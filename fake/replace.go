// SPDX-License-Identifier: Apache-2.0

package fake

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// CallKey is how Server.Calls counts a request: its method, its route's path
// pattern and, for a route that needs them, its query parameters.
func CallKey(method, pattern string, query map[string]string) string {
	key := method + " " + pattern
	if len(query) > 0 {
		q := url.Values{}
		for k, v := range query {
			q.Set(k, v)
		}
		key += "?" + q.Encode()
	}
	return key
}

// ReplaceCalls is how many times kind k was re-created in place.
func (s *Server) ReplaceCalls(k *descriptor.Kind) int {
	if k.Replace == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Calls[CallKey(k.Replace.Method, compile(k.Replace.Path).String(), k.Replace.Query)]
}

func queryMatches(want map[string]string, got url.Values) bool {
	for k, v := range want {
		if descriptor.IsTemplate(v) {
			// Filled from the spec (?environment={environment.id}): sent.
			if got.Get(k) == "" {
				return false
			}
		} else if got.Get(k) != v {
			return false
		}
	}
	return true
}

// replace answers a create-or-replace: the resource with the same identity
// (its caller-chosen ID, or else the same name) is replaced whole, keeping
// its ID; if there is none, it is created.
func (s *Server) replace(w http.ResponseWriter, k *descriptor.Kind, coll string, body map[string]any, params map[string]string) {
	obj := clone(body)
	key := ""
	var id any
	if k.ID.IsTemplate() {
		id, _ = k.ID.Compose(obj)
	} else {
		id = descriptor.Dig(obj, k.ID.From)
	}
	if id == nil || id == "" {
		id = obj[k.ID.Param]
	}
	if id != nil && id != "" {
		key = coll + "/" + url.PathEscape(idText(id))
	} else if name, ok := obj["name"].(string); ok && name != "" {
		for p, old := range s.store {
			if strings.HasPrefix(p, coll+"/") && !strings.Contains(p[len(coll)+1:], "/") && strings.EqualFold(fmt.Sprint(old["name"]), name) {
				key = p
				if oldID := descriptor.Dig(old, k.ID.From); oldID != nil {
					setPath(obj, k.ID.From, oldID)
				}
				break
			}
		}
	}
	if key == "" {
		s.nextID++
		setPath(obj, k.ID.From, s.nextID)
		key = coll + "/" + url.PathEscape(fmt.Sprint(s.nextID))
	}
	s.store[key] = obj
	s.owner[key] = owned{k, params}
	s.reply(w, http.StatusOK, k, obj)
}
