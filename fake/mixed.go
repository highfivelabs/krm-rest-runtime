// SPDX-License-Identifier: Apache-2.0

package fake

import (
	"net/http"
	"strings"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// serveMixed answers a mixed kind's REST call (descriptor.Kind.IsMixed) on
// the object its GraphQL calls keep (GraphQLKey, by the ID the path names):
// GitHub's organization made by createEnterpriseOrganization is read,
// changed and deleted at /orgs/{org}; GitLab's epic board made and changed
// over GraphQL is read at /groups/{id}/epic_boards/{board_id}.
func (s *Server) serveMixed(w http.ResponseWriter, rt *route, method, path string, body map[string]any) {
	k := rt.kind
	params := routeParams(rt.re, rt.path, path)
	if rt.role == collection {
		obj := clone(body)
		s.nextID++
		id := newID(k, s.nextID)
		setPath(obj, k.ID.From, id)
		part, _ := k.ID.Part(id)
		params[k.ID.Param] = part
		key := GraphQLKey(k, params)
		s.store[key] = obj
		s.owner[key] = owned{k, params}
		s.reply(w, http.StatusCreated, k, obj)
		return
	}
	key := GraphQLKey(k, params)
	obj, ok := s.store[key]
	switch method {
	case http.MethodGet:
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		s.reply(w, http.StatusOK, k, obj)
	case http.MethodDelete:
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		delete(s.store, key)
		delete(s.owner, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		for _, f := range k.CreateOnly {
			if _, has := descriptor.PickField(body, f); has {
				http.Error(w, `{"message":"`+f+`: doesn't match any valid key"}`, http.StatusBadRequest)
				return
			}
		}
		mergeInto(obj, body)
		s.reply(w, http.StatusOK, k, obj)
	}
}

// newID is the ID the fake gives the nth object a create makes: n, or,
// where the kind's ID is part of a longer value (ID.Match), that value
// with n in the ID's place.
func newID(k *descriptor.Kind, n int) string {
	id := idText(n)
	if k.ID != nil && k.ID.Match != "" {
		return strings.ReplaceAll(k.ID.Match, "{"+k.ID.Param+"}", id)
	}
	return id
}

// createdAt is where an upsert made by a POST to a collection lives: its
// item path, from the create's path parameters and what the create's body
// templates name (descriptor.MatchTemplate: a ref "refs/heads/{branch}"
// sent as "refs/heads/feature/x" names branch feature/x). False where the
// create is on the item's own path.
func createdAt(k *descriptor.Kind, rt *route, path string, body map[string]any) (string, bool) {
	if k.Create == nil || rt.op != k.Create || k.Create.Path == k.ItemPath() || k.Read.Find != nil {
		return "", false
	}
	vals := routeParams(rt.re, rt.path, path)
	for f, t := range k.Create.Body {
		tmpl, isStr := t.(string)
		got, _ := body[f].(string)
		if !isStr || got == "" {
			continue
		}
		if m, ok := descriptor.MatchTemplate(tmpl, got); ok {
			for n, v := range m {
				vals[n] = v
			}
		}
	}
	p, err := rest.Expand(k.ItemPath(), vals)
	return p, err == nil
}
