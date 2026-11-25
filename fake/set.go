// SPDX-License-Identifier: Apache-2.0

package fake

import (
	"crypto/sha1" //nolint:gosec // not for security: the fake makes a stable 24-hex ID, as the vendor API does
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// serveSet answers a set kind's calls (descriptor.SetShape): its elements
// are kept each at its own path, under the list's (a repository's labels at
// /repos/{owner}/{repo}/labels/{name}), as the API's one-element kinds
// (IssueLabel) keep them, whichever kind's call made one.
func (s *Server) serveSet(w http.ResponseWriter, rt *route, method, path string, query url.Values, body map[string]any) {
	k := rt.kind
	st := k.Set
	switch rt.role {
	case setList:
		out := []any{}
		// Elements kept on paths outside the list's (an Okta user's
		// linked objects, each removed at the associated user's own path):
		// those this kind made with the list's parameters.
		outside := !strings.HasPrefix(k.Delete.Path, k.Read.Path+"/")
		want := routeParams(rt.re, rt.path, path)
		elem := compile(k.Delete.Path)
		for key, obj := range s.store {
			rest, ok := strings.CutPrefix(key, path+"/")
			if outside {
				o, mine := s.owner[key]
				switch {
				case mine:
					ok = o.kind == k && agree(o.params, want)
				default: // put there by hand: where an element of these parameters would be
					ok = elem.MatchString(key) && agree(routeParams(elem, k.Delete.Path, key), want)
				}
				rest = "x"
			}
			if !ok || rest == "" || strings.Contains(rest, "/") {
				continue
			}
			e := clone(obj)
			if st.KeyURL != "" {
				// The API names the element by a URL only.
				e = descriptor.MergeAt(map[string]any{}, st.KeyURL, "/"+url.PathEscape(descriptor.Text(obj[st.Key])))
				out = append(out, e)
				continue
			}
			if st.Key != "" && e[st.Key] == nil {
				// Written with no body (PUT .../repositories/{id}): the
				// API's list answers its key all the same.
				if v, err := url.PathUnescape(rest); err == nil {
					e[st.Key] = v
				}
			}
			if st.ReadEach != nil {
				out = append(out, e[st.Key]) // keys only (Vault's LIST)
				continue
			}
			out = append(out, withServerFields(k, e))
		}
		if st.ReadEach != nil && len(out) == 0 {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound) // Vault answers an empty LIST so
			return
		}
		sortByKey(out)
		if b := st.Batch; b != nil {
			// The batch list is in the same answer (Atlas's global writes:
			// managedNamespaces beside customZoneMapping).
			held := s.batches[path]
			if held == nil {
				held = map[string]any{}
			}
			resp := descriptor.MergeAt(nil, b.At, held)
			if st.List == "" {
				http.Error(w, `{"message":"a batch list needs the elements at set.list"}`, http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusOK, descriptor.MergeAt(resp, st.List, out))
			return
		}
		s.replyList(w, st.List, out)
	case setHolder:
		s.serveHolder(w, rt, method, path, body)
	case setAdd:
		params := routeParams(rt.re, rt.path, path)
		if len(st.Keys) > 0 {
			// Named by its keys: kept at its delete's path and query.
			if _, ok := st.KeyOf(body); !ok {
				http.Error(w, `{"message":"`+st.KeyNames()+` are required"}`, http.StatusBadRequest)
				return
			}
			at, err := SetElementPath(k, params, body)
			if err != nil {
				http.Error(w, `{"message":"no element path"}`, http.StatusBadRequest)
				return
			}
			if _, ok := s.store[at]; ok {
				http.Error(w, `{"message":"already_exists"}`, http.StatusBadRequest)
				return
			}
			s.store[at] = clone(body)
			s.owner[at] = owned{k, params}
			s.reply(w, http.StatusOK, k, body)
			return
		}
		key, ok := params[st.Param]
		if !ok {
			if v := body[st.Key]; v != nil {
				key = descriptor.Text(v)
			}
		}
		if key == "" {
			http.Error(w, `{"message":"`+st.Key+` is required"}`, http.StatusUnprocessableEntity)
			return
		}
		params[st.Param] = key
		at, err := rest.Expand(k.Delete.Path, params)
		if err != nil {
			http.Error(w, `{"message":"no element path"}`, http.StatusBadRequest)
			return
		}
		if old, ok := s.store[at]; ok && method == http.MethodPost {
			if u := k.Update; u != nil && u.Method == k.Create.Method && u.Path == k.Create.Path {
				// One write both makes and changes an element (Vault's POST
				// .../config/:name).
				mergeInto(old, body)
				s.reply(w, http.StatusOK, k, old)
				return
			}
			http.Error(w, `{"message":"already_exists"}`, http.StatusUnprocessableEntity)
			return
		}
		obj := clone(body)
		if obj[st.Key] == nil {
			obj[st.Key] = key
		}
		s.store[at] = obj
		s.owner[at] = owned{k, params}
		s.reply(w, http.StatusCreated, k, obj)
	case setElem:
		if len(st.Keys) > 0 && len(rt.op.Query) > 0 {
			// Named in the query too (?db=...&collection=...).
			q := url.Values{}
			for name := range rt.op.Query {
				q.Set(name, query.Get(name))
			}
			path += "?" + q.Encode()
		}
		if u := k.Update; u != nil && rt.op == u && u.Path != k.Delete.Path {
			if p, ok := samePlaceholders(rt.re, u.Path, path, k.Delete.Path); ok {
				path = p
			}
		}
		obj, ok := s.store[path]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		if method == http.MethodDelete {
			delete(s.store, path)
			delete(s.owner, path)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if method == http.MethodGet {
			s.replyOp(w, http.StatusOK, k, rt.op, withServerFields(k, obj)) // one element (Set.ReadEach)
			return
		}
		mergeInto(obj, body)
		s.reply(w, http.StatusOK, k, obj)
	}
}

// serveHolder answers the calls on a set kind's holder (Set.Holder): made
// where it is read, and deleted with every element of its kind made with
// the same parameters, as the API deletes a mount and what is in it.
func (s *Server) serveHolder(w http.ResponseWriter, rt *route, method, path string, body map[string]any) {
	k := rt.kind
	h := k.Set.Holder
	params := routeParams(rt.re, rt.path, path)
	at, err := rest.Expand(h.Read.Path, params)
	if err != nil {
		http.Error(w, `{"message":"no holder path"}`, http.StatusBadRequest)
		return
	}
	obj, ok := s.store[at]
	switch {
	case rt.op == &h.Create:
		if ok {
			http.Error(w, `{"message":"path is already in use"}`, http.StatusBadRequest)
			return
		}
		s.store[at] = clone(body)
		s.owner[at] = owned{k, params}
		w.WriteHeader(http.StatusNoContent)
	case !ok:
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	case method == http.MethodDelete:
		for p, o := range s.owner {
			if o.kind == k && agree(o.params, params) {
				delete(s.store, p)
				delete(s.owner, p)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(obj) // only whether it is there counts
	}
}

// Put stores obj at path, as if made outside the controllers (a test's
// drift: an element of a set no spec lists).
func (s *Server) Put(path string, obj map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store[path] = obj
}

// SetElementPath is where the fake keeps a set kind's element: its
// delete's path, and for one named by Set.Keys, the delete's query too
// (.../managedNamespaces?collection=users&db=app), filled from the element.
func SetElementPath(k *descriptor.Kind, params map[string]string, elem map[string]any) (string, error) {
	vals := make(map[string]string, len(params)+len(k.Set.Keys))
	for n, v := range params {
		vals[n] = v
	}
	if k.Set.Param != "" {
		vals[k.Set.Param] = descriptor.Text(elem[k.Set.Key])
	}
	for _, f := range k.Set.Keys {
		vals[f] = descriptor.Text(elem[f])
	}
	at, err := rest.Expand(k.Delete.Path, vals)
	if err != nil || len(k.Set.Keys) == 0 || len(k.Delete.Query) == 0 {
		return at, err
	}
	q := url.Values{}
	for name, v := range k.Delete.QueryOf(vals) {
		q.Set(name, v)
	}
	return at + "?" + q.Encode(), nil
}

// serveBatch answers a set kind's batch list's add and clear (Set.Batch):
// kept by its list's path, each element's value answered in another form
// (a 24-hex ID made from it), as Atlas answers a zone's ID for its name.
func (s *Server) serveBatch(w http.ResponseWriter, rt *route, path string, body map[string]any) {
	k := rt.kind
	b := k.Set.Batch
	at, err := rest.Expand(k.Read.Path, routeParams(rt.re, rt.path, path))
	if err != nil {
		http.Error(w, `{"message":"no list path"}`, http.StatusBadRequest)
		return
	}
	held := s.batches[at]
	if held == nil {
		held = map[string]any{}
	}
	if rt.op == &b.Clear {
		held = map[string]any{}
	} else {
		list, ok := body[b.Field].([]any)
		if !ok || len(list) == 0 {
			http.Error(w, `{"message":"`+b.Field+` is required"}`, http.StatusBadRequest)
			return
		}
		for _, e := range list {
			m, _ := e.(map[string]any)
			if m == nil || m[b.Key] == nil || m[b.Value] == nil {
				http.Error(w, `{"message":"`+b.Key+` and `+b.Value+` are required"}`, http.StatusBadRequest)
				return
			}
			held[descriptor.Text(m[b.Key])] = BatchValue(descriptor.Text(m[b.Value]))
		}
	}
	s.batches[at] = held
	writeJSON(w, http.StatusOK, descriptor.MergeAt(nil, b.At, held))
}

// BatchValue is the form the fake answers a batch list's value in: a
// 24-hex ID made from it, as Atlas answers a zone's ID for its name.
func BatchValue(v string) string {
	sum := sha1.Sum([]byte(v)) //nolint:gosec // not for security: a stable ID for a value, nothing more
	return hex.EncodeToString(sum[:12])
}

// Batch is the batch list the fake holds at a list path (Set.Batch), by
// key (for assertions).
func (s *Server) Batch(listPath string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.batches[listPath])
}

// PutBatch adds an element to the batch list at a list path, as if made
// outside the controllers (a test's drift).
func (s *Server) PutBatch(listPath, key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.batches[listPath] == nil {
		s.batches[listPath] = map[string]any{}
	}
	s.batches[listPath][key] = BatchValue(value)
}
