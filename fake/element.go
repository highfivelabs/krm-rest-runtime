// SPDX-License-Identifier: Apache-2.0

package fake

import (
	"encoding/json"
	"net/http"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// bareList is where the fake keeps a list the read answers bare (an Element
// kind with no List: GitHub's custom property values).
const bareList = ""

// seedHolders notes, for each Element kind, the resource its sample's
// element goes in: the fake makes it, empty, the first time it is asked
// for, as the vendor (or another kind) would have made it before.
func (s *Server) seedHolders(f *descriptor.File) {
	s.seeds = map[string]*descriptor.Kind{}
	// A settings kind on the path another kind's resource is read at
	// (Okta's domain verification and certificate, on the Domain's path)
	// acts on that resource: it is not there until made, so the fake holds
	// one for the kind's sample.
	made := map[string]bool{} // path patterns a CRUD or upsert kind reads
	for i := range f.Kinds {
		if k := &f.Kinds[i]; (k.Shape == descriptor.CRUD || k.Shape == descriptor.Upsert) && k.Read.GraphQL == nil && k.SQL == nil {
			made[compile(k.Read.Path).String()] = true
		}
	}
	s.riders = map[*descriptor.Kind]bool{}
	for i := range f.Kinds {
		k := &f.Kinds[i]
		// One that writes the resource on its own path (a GitLab user's
		// avatar, PUT /users/{id}) keeps it as a settings kind does.
		ownWrite := k.Update != nil && k.Update.Path == k.Read.Path && k.State == nil
		if k.Shape == descriptor.Settings && k.Element == nil && k.Read.GraphQL == nil && k.SQL == nil && !ownWrite && made[compile(k.Read.Path).String()] {
			s.riders[k] = true
		}
	}
	for i := range f.Kinds {
		k := &f.Kinds[i]
		if k.Element == nil && !s.riders[k] {
			continue
		}
		vals := map[string]string{}
		for _, p := range k.Params {
			vals[p.Name] = idText(k.Sample[p.Field])
		}
		if p, err := rest.Expand(k.Read.Path, vals); err == nil {
			s.seeds[p] = k
		}
	}
}

// HoldFor has the fake make, the first time it is asked for, the resource
// that an Element kind's object with this spec goes in (or a settings
// kind's that acts on another kind's resource), as seedHolders does for the
// kind's sample: a rehearsal of a live.yaml spec names its own (GitHub's
// custom property values of the run's repository). Nothing is made where
// something is stored already, nor for another kind, nor where a path
// parameter has no value in spec (a fieldRef's).
func (s *Server) HoldFor(k *descriptor.Kind, spec map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k.Element == nil && !s.riders[k] {
		return
	}
	vals := map[string]string{}
	for _, p := range k.Params {
		v := p.In(spec)
		if v == nil || idText(v) == "" {
			return
		}
		vals[p.Name] = idText(v)
	}
	p, err := rest.Expand(k.Read.Path, vals)
	if err != nil {
		return
	}
	if _, stored := s.store[p]; !stored {
		s.seeds[p] = k
	}
}

// seed makes the resource an Element kind's sample goes in (or a settings
// kind's on another kind's path), if path is it
// and it is not there yet.
func (s *Server) seed(path string) {
	k, ok := s.seeds[path]
	if !ok {
		return
	}
	delete(s.seeds, path) // once: deleting it later leaves it deleted
	if _, stored := s.store[path]; stored {
		return
	}
	obj := map[string]any{}
	if k.Element == nil {
		// Another kind's resource, which this one acts on: answered as this
		// kind's (without the other's server fields, which would answer
		// the state it asks for).
		s.store[path] = obj
		s.owner[path] = owned{k, routeParams(compile(k.Read.Path), k.Read.Path, path)}
		return
	}
	e := k.Element
	for _, l := range e.Lists() {
		setPath(obj, l, []any{})
	}
	if len(e.In) == 0 {
		if e.List == "" {
			obj[bareList] = []any{}
		} else {
			setPath(obj, e.List, []any{})
		}
	}
	s.store[path] = obj
	s.owner[path] = owned{k, map[string]string{}}
}

// serveElement answers an Element kind's read and write of the resource
// holding its list: a whole write replaces the lists it sends; a patch
// merges each element it sends by key, and removes one sent with the
// Unset fields.
func (s *Server) serveElement(w http.ResponseWriter, rt *route, method, path string, body map[string]any) {
	k := rt.kind
	e := k.Element
	obj, ok := s.store[path]
	if !ok {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	if method != http.MethodGet {
		obj = clone(obj)
		if e.Patch == "" {
			mergeInto(obj, body)
		} else {
			at := e.List
			if at == "" {
				at = bareList
			}
			list, _ := descriptor.Dig(obj, at).([]any)
			if at == bareList {
				list, _ = obj[bareList].([]any)
			}
			list = append([]any{}, list...)
			sent, _ := body[e.Patch].([]any)
			for _, x := range sent {
				m, _ := x.(map[string]any)
				if m == nil {
					continue
				}
				j := e.Find(list, m[e.Key])
				switch {
				case isUnset(e, m):
					if j >= 0 {
						list = append(list[:j], list[j+1:]...)
					}
				case j >= 0:
					list[j] = m
				default:
					list = append(list, m)
				}
			}
			if at == bareList {
				obj[bareList] = list
			} else {
				setPath(obj, at, list)
			}
		}
		s.store[path] = obj
	}
	if l, bare := obj[bareList]; bare && len(e.In) == 0 && e.List == "" {
		if l == nil {
			l = []any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(l)
		return
	}
	s.reply(w, http.StatusOK, k, obj)
}

// isUnset reports whether a patched element is a removal: it holds every
// Unset field with its value.
func isUnset(e *descriptor.Element, m map[string]any) bool {
	if len(e.Unset) == 0 {
		return false
	}
	for f, v := range e.Unset {
		got, ok := m[f]
		if !ok {
			return false
		}
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(v)
		if string(a) != string(b) {
			return false
		}
	}
	return true
}

// serveState answers a State call: the resource the call names (its path's
// placeholders are the item's) is put in the route's state. A settings
// resource is there before anything is written; another must exist.
func (s *Server) serveState(w http.ResponseWriter, rt *route, path string) {
	k := rt.kind
	item, ok := samePlaceholders(rt.re, rt.path, path, k.ItemPath())
	if !ok {
		// A call naming fewer parameters than the read (Okta's
		// DELETE /governance/api/v2/request-sequences/{sequenceId}, read
		// under its resource): the kind's object its parameters agree with.
		want := routeParams(rt.re, rt.path, path)
		for p, o := range s.owner {
			if o.kind == k && agree(o.params, want) {
				item, ok = p, true
				break
			}
		}
	}
	if rt.method == http.MethodDelete {
		// The state is "gone" (an app token revoked by its DELETE). A
		// settings kind's resource is there before anything is written.
		if _, stored := s.store[item]; ok && stored {
			delete(s.store, item)
			delete(s.owner, item)
		} else if k.Shape != descriptor.Settings {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !ok {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	obj, stored := s.store[item]
	if !stored && k.Shape != descriptor.Settings {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	obj = clone(obj)
	obj[k.State.Field] = rt.state
	s.store[item] = obj
	if _, has := s.owner[item]; !has {
		s.owner[item] = owned{k, routeParams(compile(k.ItemPath()), k.ItemPath(), item)}
	}
	s.reply(w, http.StatusOK, k, obj)
}

// addStateRoutes adds a route for each of a State kind's calls.
func (s *Server) addStateRoutes(k *descriptor.Kind) {
	if k.State == nil {
		return
	}
	for _, v := range k.State.Values() {
		op := k.State.Calls[v]
		s.routes = append(s.routes, route{method: op.Method, path: op.Path, re: compile(op.Path), kind: k, role: stateCall, query: op.Query, op: &op, state: v})
		for i := range k.State.Before[v] {
			b := &k.State.Before[v][i]
			s.routes = append(s.routes, route{method: b.Method, path: b.Path, re: compile(b.Path), kind: k, role: stateBefore, query: b.Query, op: b})
		}
	}
}

// elementOwnsPath reports whether a route is an Element kind's read or
// write of the resource holding its list.
func elementOwnsPath(rt *route) bool {
	return rt.kind.Element != nil && rt.role == item
}
