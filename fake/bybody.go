// SPDX-License-Identifier: Apache-2.0

package fake

import (
	"net/http"
	"strings"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// BodyKey is where the fake keeps an object of a kind that names it in
// its calls' bodies (descriptor.Kind.ReadsByBody): by kind, path parameters
// and ID, as its paths do not name it.
func BodyKey(k *descriptor.Kind, params map[string]string) string {
	var parts []string
	for _, p := range k.Params {
		parts = append(parts, p.Name+"="+params[p.Name])
	}
	if k.ID == nil {
		return "body:" + k.Kind + ":" + strings.Join(parts, "&") // an upsert: named by its params
	}
	return "body:" + k.Kind + ":" + strings.Join(parts, "&") + ":" + params[k.ID.Param]
}

// serveByBody answers a call of a kind whose read, update and delete name
// the object in their bodies, as an RPC API's do (Grafana IRM's
// SeveritiesService.GetSeverityByID {"severityID": ...}; Vault's
// .../secret-id-accessor/lookup {"secret_id_accessor": ...}): the create
// gives it an ID; the others find it by the ID their body's template names,
// read from the body as sent (raw) where the call sends it bare.
func (s *Server) serveByBody(w http.ResponseWriter, rt *route, path string, body map[string]any, raw any) {
	k := rt.kind
	params := routeParams(rt.re, rt.path, path)
	op := rt.op
	if op == k.Create && k.ID == nil {
		// An upsert named by its body (Fleet Management's collector id).
		inBody(k, params, body)
		key := BodyKey(k, params)
		if _, ok := s.store[key]; ok {
			http.Error(w, `{"message":"already exists"}`, http.StatusConflict)
			return
		}
		s.store[key] = clone(body)
		s.owner[key] = owned{k, params}
		s.replyOp(w, http.StatusCreated, k, op, s.store[key])
		return
	}
	if op == k.Create {
		obj := clone(body)
		id := descriptor.Dig(obj, k.ID.From)
		if id == nil || id == "" {
			s.nextID++
			id = idText(s.nextID)
			setPath(obj, k.ID.From, id)
		}
		s.fillCarried(k, obj)
		params[k.ID.Param] = idText(id)
		key := BodyKey(k, params)
		s.store[key] = obj
		s.owner[key] = owned{k, params}
		s.replyOp(w, http.StatusCreated, k, op, obj)
		return
	}
	if op == &k.Read && k.Read.Find != nil {
		// A POST listing them all (Grafana IRM's GetRoles {}): every object
		// of the kind made with these parameters.
		var out []any
		for key, obj := range s.store {
			if o, ok := s.owner[key]; ok && o.kind == k && agree(o.params, params) {
				out = append(out, obj)
			}
		}
		sortByKey(out)
		s.replyList(w, k.ListAt(), out)
		return
	}
	key := bodyNamed(rt, params, body, raw)
	obj, ok := s.store[key]
	if !ok || k.ID != nil && params[k.ID.Param] == "" {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	switch op {
	case &k.Read:
		s.replyOp(w, http.StatusOK, k, op, obj)
	case k.Delete:
		delete(s.store, key)
		delete(s.owner, key)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	default:
		if !s.carries(w, op, obj, body) {
			return
		}
		for _, f := range k.CreateOnly {
			if _, has := descriptor.PickField(body, f); has {
				http.Error(w, `{"message":"`+f+`: cannot be changed"}`, http.StatusBadRequest)
				return
			}
		}
		mergeInto(obj, body)
		s.bumpCarried(k, obj)
		s.replyOp(w, http.StatusOK, k, op, obj)
	}
}

// bodyNamed is the key of the object a read, update or delete names in its
// body (the ID its body template stands for), with params, the path's,
// given that ID.
func bodyNamed(rt *route, params map[string]string, body map[string]any, raw any) string {
	k, op := rt.kind, rt.op
	sent := any(body)
	if op == &k.Read || op.BareBody {
		sent = raw
	}
	templateValues(op.Body, sent, params)
	if m, ok := sent.(map[string]any); ok {
		inBody(k, params, m)
	}
	if op == k.Update && k.UpdateID != "" && params[k.ID.Param] == "" {
		if id := descriptor.Dig(body, strings.ReplaceAll(k.UpdateID, ".", "/")); id != nil {
			params[k.ID.Param] = idText(id)
		}
	}
	return BodyKey(k, params)
}

// pickByBody chooses, among routes of several kinds on one URL (Vault's
// generated and custom SecretIDs, both looked up and destroyed at
// .../secret-id-accessor/...), the kind that holds the object the body
// names; else the first.
func (s *Server) pickByBody(rs []*route, path string, body map[string]any, raw any) *route {
	for _, r := range rs {
		if !r.kind.ReadsByBody() || r.op == r.kind.Create {
			continue
		}
		if _, ok := s.store[bodyNamed(r, routeParams(r.re, r.path, path), body, raw)]; ok {
			return r
		}
	}
	return rs[0]
}

// inBody fills params the path does not name from the body fields that
// are params too (Param.InBody).
func inBody(k *descriptor.Kind, params map[string]string, body map[string]any) {
	for _, p := range k.Params {
		if params[p.Name] != "" || !p.InBody {
			continue
		}
		if v := p.In(body); v != nil {
			params[p.Name] = idText(v)
		}
	}
}
