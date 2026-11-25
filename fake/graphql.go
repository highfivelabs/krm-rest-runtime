// SPDX-License-Identifier: Apache-2.0

package fake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// gqlRole is what a GraphQL operation does to its kind's object.
type gqlRole int

const (
	gqlCreate gqlRole = iota
	gqlRead
	gqlUpdate
	gqlDelete
)

// gqlRoute is one GraphQL operation the fake answers, by its name.
type gqlRoute struct {
	kind *descriptor.Kind
	op   *descriptor.Op
	role gqlRole
}

// addGraphQL registers a GraphQL kind's operations by name, and its
// endpoint.
func (s *Server) addGraphQL(k *descriptor.Kind) {
	for _, c := range []struct {
		op   *descriptor.Op
		role gqlRole
	}{{k.Create, gqlCreate}, {&k.Read, gqlRead}, {k.Update, gqlUpdate}, {k.Delete, gqlDelete}} {
		if c.op == nil || c.op.GraphQL == nil {
			continue
		}
		s.gql[c.op.GraphQL.Operation] = gqlRoute{kind: k, op: c.op, role: c.role}
		s.gqlPaths[c.op.Path] = true
	}
}

// GraphQLKey is where the fake keeps a GraphQL kind's object: a CRUD
// object by its ID, any other by the path parameters every one of its
// operations names (a group's dependency proxy settings by the group).
func GraphQLKey(k *descriptor.Kind, params map[string]string) string {
	if k.Shape == descriptor.CRUD && k.ID != nil {
		return "graphql:" + k.Kind + ":" + params[k.ID.Param]
	}
	var parts []string
	for _, n := range keyParams(k) {
		parts = append(parts, n+"="+params[n])
	}
	return "graphql:" + k.Kind + ":" + strings.Join(parts, "&")
}

// keyParams are the path parameters every operation of k names.
func keyParams(k *descriptor.Kind) []string {
	count := map[string]int{}
	ops := 0
	for _, op := range []*descriptor.Op{k.Create, &k.Read, k.Update, k.Delete} {
		if op == nil || op.GraphQL == nil {
			continue
		}
		ops++
		names, _ := op.GraphQL.Names()
		for _, n := range names {
			if k.IsParam(n) {
				count[n]++
			}
		}
	}
	var out []string
	for n, c := range count {
		if c == ops {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// serveGraphQL answers a GraphQL request by its operation's name, as the
// kind's descriptor says the API does: the object at the operation's
// result, an empty list at its errors; a missing object is a top-level
// NOT_FOUND error (an update or delete) or a null result (a read).
func (s *Server) serveGraphQL(w http.ResponseWriter, req *http.Request) {
	var in struct {
		Query         string         `json:"query"`
		OperationName string         `json:"operationName"`
		Variables     map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		http.Error(w, `{"message":"bad json"}`, http.StatusBadRequest)
		return
	}
	rt, ok := s.gql[in.OperationName]
	if !ok || strings.TrimSpace(in.Query) != strings.TrimSpace(rt.op.GraphQL.Query) {
		gqlErrors(w, fmt.Sprintf("fake: no operation %q with this document", in.OperationName), nil)
		return
	}
	s.Calls["GRAPHQL "+in.OperationName]++
	k, g := rt.kind, rt.op.GraphQL
	params, body := g.Extract(in.Variables, k.IsParam)
	key := GraphQLKey(k, params)
	switch rt.role {
	case gqlCreate:
		obj := clone(body)
		id := descriptor.Dig(obj, k.ID.From)
		if id == nil || id == "" {
			s.nextID++
			id = fmt.Sprintf("gid://fake/%s/%d", k.Kind, s.nextID)
			if k.ID.Match != "" {
				id = newID(k, s.nextID) // gid://gitlab/Boards::EpicBoard/<n>
			}
			setPath(obj, k.ID.From, id)
		}
		part, _ := k.ID.Part(idText(id))
		params[k.ID.Param] = part
		key = GraphQLKey(k, params)
		s.store[key] = obj
		s.owner[key] = owned{k, params}
		s.gqlReply(w, k, g, obj)
	case gqlRead:
		if k.Read.Find != nil {
			var out []any
			for key, obj := range s.store {
				o, ok := s.owner[key]
				if !ok || o.kind != k {
					continue
				}
				match := true
				for n, v := range params {
					if got, has := o.params[n]; has && got != v {
						match = false
					}
				}
				if match {
					out = append(out, s.answer(k, obj))
				}
			}
			sortByKey(out)
			if out == nil {
				out = []any{}
			}
			s.gqlData(w, g, out)
			return
		}
		obj, ok := s.store[key]
		if g.Exists != "" {
			// The read answers whether it exists (isCatalogResource).
			out := map[string]any{}
			if ok {
				out = s.answer(k, obj)
			}
			setPath(out, g.Exists, ok)
			s.gqlData(w, g, out)
			return
		}
		if !ok {
			if k.Shape != descriptor.Settings {
				s.gqlData(w, g, nil)
				return
			}
			obj = map[string]any{}
		}
		s.gqlReply(w, k, g, obj)
	case gqlUpdate:
		obj, ok := s.store[key]
		if !ok && k.Shape == descriptor.CRUD {
			gqlErrors(w, "fake: no such object", g)
			return
		}
		for _, f := range k.CreateOnly {
			if _, has := descriptor.PickField(body, f); has {
				gqlErrors(w, "fake: "+f+" can only be set on create", nil)
				return
			}
		}
		if !ok {
			obj = map[string]any{}
			s.owner[key] = owned{k, params}
		}
		mergeInto(obj, body)
		s.store[key] = obj
		s.gqlReply(w, k, g, obj)
	case gqlDelete:
		if _, ok := s.store[key]; !ok {
			gqlErrors(w, "fake: no such object", g)
			return
		}
		delete(s.store, key)
		delete(s.owner, key)
		s.gqlData(w, g, nil)
	}
}

// answer is obj as a read answers it: with server fields, without
// write-only ones, compare fields in the API's form.
func (s *Server) answer(k *descriptor.Kind, obj map[string]any) map[string]any {
	return withoutWriteOnly(k, "", readBack(k, withServerFields(k, obj)))
}

func (s *Server) gqlReply(w http.ResponseWriter, k *descriptor.Kind, g *descriptor.GraphQL, obj map[string]any) {
	s.gqlData(w, g, s.answer(k, obj))
}

// gqlData answers {"data": ...} with v at the operation's result and an
// empty list at its errors.
func (s *Server) gqlData(w http.ResponseWriter, g *descriptor.GraphQL, v any) {
	data := map[string]any{}
	if g.Result != "" {
		setPath(data, g.Result, v)
	}
	if g.Errors != "" {
		setPath(data, g.Errors, []any{})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// gqlErrors answers a 200 with a top-level error, as GraphQL APIs do; with
// the operation g, a missing object's: coded NOT_FOUND, the operation's
// field null.
func gqlErrors(w http.ResponseWriter, msg string, g *descriptor.GraphQL) {
	e := map[string]any{"message": msg}
	var data any
	if g != nil {
		e["extensions"] = map[string]any{"code": "NOT_FOUND"}
		root, _, _ := strings.Cut(g.Result+g.Errors, "/")
		if root != "" {
			data = map[string]any{root: nil}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "errors": []any{e}})
}
