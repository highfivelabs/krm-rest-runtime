// SPDX-License-Identifier: Apache-2.0

package fake

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// check refuses a request that lacks what its call must send, as the real
// API would: a field of the call's Outer beside the envelope (Datadog's
// "meta"), a templated header, or a {current.field} header or query value
// other than the stored resource's (Grafana Assistant's X-Resource-Scope
// must name the scope the object has now). "" when the request is fine.
func (s *Server) check(rt *route, req *http.Request, path string, raw any) string {
	op := rt.op
	if op == nil {
		return ""
	}
	if len(op.Outer) > 0 {
		m, _ := raw.(map[string]any)
		for f := range op.Outer {
			if _, ok := m[f]; !ok {
				return f + " is required beside the body"
			}
		}
	}
	cur := map[string]any{"current": s.store[path]}
	for h, v := range op.Headers {
		if !descriptor.IsTemplate(v) {
			continue
		}
		got := req.Header.Get(h)
		if !descriptor.IsCurrent(v) {
			if got == "" {
				return "header " + h + " is required"
			}
			continue
		}
		// What the resource holds now, where it holds the field.
		if want, ok := (&descriptor.ID{From: v}).Compose(cur); ok && got != want {
			return "header " + h + ": " + got + " does not match the resource's " + want
		}
	}
	for q, v := range op.Query {
		if want, ok := (&descriptor.ID{From: v}).Compose(cur); descriptor.IsCurrent(v) && ok && req.URL.Query().Get(q) != want {
			return "query " + q + ": " + req.URL.Query().Get(q) + " does not match the resource's " + want
		}
	}
	return ""
}

// retarget is the route and stored path a request really acts on, where
// its method and URL alone cannot tell: a delete sent as a POST of a fixed
// body to the path the writes use too (Grafana's {"permission": ""}), or
// on a path that names the item only in its body (Datadog's bulk delete);
// an update on another path than the item's, the ID in its body (Grafana
// Synthetic Monitoring's POST /check/update), or on the very path that
// creates (Datadog's PUT /cost/budget, an update when the body has an ID).
func (s *Server) retarget(rt *route, method, path string, body map[string]any) (*route, string) {
	k := rt.kind
	if rt.role == assocAdd || rt.role == assocList || rt.role == assocDel || rt.role == list || rt.role == stateCall || k.Element != nil {
		return rt, path
	}
	if fd := k.Read.Find; fd != nil && k.Create != nil && method != http.MethodGet && rt.op != nil && !strings.Contains(rt.op.Path, "{"+fd.Param+"}") {
		// Named by a body field on the collection (Datadog's v1 Azure
		// integration, by tenant_name): kept under the collection by that
		// name, as one object per name.
		for _, p := range k.Params {
			if v, ok := body[p.Field]; ok && p.Name == fd.Param && p.InBody {
				path = strings.TrimSuffix(path, "/") + "/" + url.PathEscape(idText(v))
			}
		}
	}
	if del := k.Delete; del != nil && (del.Method != http.MethodDelete || namedByBody(k)) && k.Stays == "" &&
		del.Method == method && compile(del.Path).MatchString(path) && bodyFits(del.Body, body) {
		out := *rt
		out.role, out.op, out.re, out.path = removal, del, compile(del.Path), del.Path
		return &out, path
	}
	if op := rt.op; op != nil && k.ID == nil && k.Read.Find == nil && (op == k.Update || op == k.Create) && namedInBody(k, op) &&
		op.Method == method && compile(op.Path).MatchString(path) && k.Shape != descriptor.Assoc {
		// An upsert written on another path, which names the item by a body
		// field (Datadog's PUT /cost/budget/custom-forecast, by
		// data.attributes.budget_uid): kept where it is read.
		if p, ok := storedAt(k, routeParams(compile(op.Path), op.Path, path), body); ok {
			return rt, p
		}
	}
	if upd := k.Update; upd != nil && k.ID != nil && upd.Method == method &&
		!strings.Contains(upd.Path, "{"+k.ID.Param+"}") && compile(upd.Path).MatchString(path) {
		id := descriptor.Dig(body, strings.ReplaceAll(k.UpdateID, ".", "/"))
		if k.UpdateID == "" || id == nil || id == "" {
			return rt, path // a create on the same path (no ID yet)
		}
		vals := routeParams(compile(upd.Path), upd.Path, path)
		vals[k.ID.Param] = idText(id)
		if p, ok := storedAt(k, vals, body); ok {
			if _, stored := s.store[p]; stored {
				out := *rt
				out.role, out.op, out.re, out.path = item, upd, compile(p), p
				return &out, p
			}
		}
		if c := k.Create; c != nil && c.Method == method && compile(c.Path).MatchString(path) {
			// Not there yet: the same call creates it (Asserts' PUT of a
			// rules file, named in its body).
			out := *rt
			out.role, out.op, out.re, out.path = collection, c, compile(c.Path), c.Path
			return &out, path
		}
	}
	return rt, path
}

// namedByBody: a DELETE whose path does not name the item, which its
// fixed body does (Datadog's DELETE /api/v2/team/connections of
// {"data": [{"id": ...}]}): a removal, as a delete that is a write is.
func namedByBody(k *descriptor.Kind) bool {
	del := k.Delete
	return del.Body != nil && k.ID != nil && !strings.Contains(del.Path, "{"+k.ID.Param+"}") && !strings.Contains(del.Path, "{+"+k.ID.Param+"}")
}

// storedAt is the path the fake keeps an item at, from path parameters
// and the ID in vals, the rest from the body (a parameter that is a body
// field under another name, Param.InBody): its read path, or for a kind
// read from a list, its delete path.
func storedAt(k *descriptor.Kind, vals map[string]string, body map[string]any) (string, bool) {
	tmpl := k.ItemPath()
	if k.Read.Find != nil {
		if k.Delete == nil {
			return "", false
		}
		tmpl = k.Delete.Path
	}
	all := make(map[string]string, len(vals))
	for n, v := range vals {
		all[n] = v
	}
	for _, n := range placeholderRE.FindAllStringSubmatch(tmpl, -1) {
		if _, ok := all[n[1]]; ok {
			continue
		}
		v := bodyParam(k, n[1], body)
		if v == nil {
			return "", false
		}
		all[n[1]] = idText(v)
	}
	p, err := rest.Expand(tmpl, all)
	return p, err == nil
}

// namedInBody: op's path lacks a placeholder of the item's path that a body
// field fills (Param.InBody): the body names the item.
func namedInBody(k *descriptor.Kind, op *descriptor.Op) bool {
	for _, p := range k.Params {
		if p.InBody && strings.Contains(k.ItemPath(), "{"+p.Name+"}") && !strings.Contains(op.Path, "{"+p.Name+"}") {
			return true
		}
	}
	return false
}

// bodyParam is the body's value for path parameter name: its field, or the
// body field (dotted for a nested one) that fills it (Param.InBody).
func bodyParam(k *descriptor.Kind, name string, body map[string]any) any {
	for _, p := range k.Params {
		if p.Name == name && p.InBody {
			return p.In(body)
		}
	}
	return body[name]
}

// bodyFits: body holds every field of a delete's fixed body (a {param}
// template matches any value).
func bodyFits(want map[string]any, body map[string]any) bool {
	for f, v := range want {
		got, ok := body[f]
		if !ok {
			return false
		}
		if str, isStr := v.(string); isStr && descriptor.IsTemplate(str) {
			continue
		}
		if wm, isMap := v.(map[string]any); isMap {
			gm, _ := got.(map[string]any)
			if !bodyFits(wm, gm) {
				return false
			}
			continue
		}
		if _, isList := v.([]any); isList {
			continue // the items it names (templates): matched by remove
		}
		if !reflect.DeepEqual(v, got) {
			return false
		}
	}
	return true
}

// templateValues collects, where want holds a {param} template, the value
// got has in its place.
func templateValues(want, got any, out map[string]string) {
	switch w := want.(type) {
	case string:
		if descriptor.IsTemplate(w) && strings.HasPrefix(w, "{") && strings.HasSuffix(w, "}") && got != nil {
			out[w[1:len(w)-1]] = idText(got)
		}
	case map[string]any:
		g, _ := got.(map[string]any)
		for f, v := range w {
			templateValues(v, g[f], out)
		}
	case []any:
		g, _ := got.([]any)
		for i, v := range w {
			if i < len(g) {
				templateValues(v, g[i], out)
			}
		}
	}
}

// remove answers a delete that is a write: the item it names, by its path
// or its body, is gone.
func (s *Server) remove(w http.ResponseWriter, rt *route, path string, body map[string]any) {
	k := rt.kind
	key := path
	if _, ok := s.store[key]; !ok {
		vals := routeParams(rt.re, rt.path, path)
		templateValues(k.Delete.Body, body, vals)
		if p, err := rest.Expand(k.Read.Path, vals); err == nil {
			key = p
		}
		if _, ok := s.store[key]; !ok && k.Read.Find != nil && k.Create != nil && k.ID != nil && vals[k.ID.Param] != "" {
			// Read from a list, made on a collection no path of which names
			// it: kept under the collection by its ID, as a create keeps it.
			if c, err := rest.Expand(k.Create.Path, vals); err == nil {
				key = c + "/" + url.PathEscape(vals[k.ID.Param])
			}
		}
	}
	if _, ok := s.store[key]; !ok && body != nil {
		// Named by its body alone (Vault's secrets sync association removed
		// by {mount, secret_name}): the kind's object made with these
		// parameters that holds each of the body's fields.
		want := routeParams(rt.re, rt.path, path)
		for p, o := range s.owner {
			if (o.kind == k || sameCreate(o.kind, k)) && agree(o.params, want) && holdsAll(s.store[p], body) {
				key = p
				break
			}
		}
	}
	if _, ok := s.store[key]; !ok {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	delete(s.prepared, key)
	delete(s.store, key)
	delete(s.owner, key)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

// holdsAll reports whether obj has each of body's fields, the same value.
func holdsAll(obj, body map[string]any) bool {
	for f, v := range body {
		if !reflect.DeepEqual(obj[f], v) {
			return false
		}
	}
	return obj != nil
}

// hasTemplates reports whether any query value is a {field} template.
func hasTemplates(q map[string]string) bool {
	for _, v := range q {
		if descriptor.IsTemplate(v) {
			return true
		}
	}
	return false
}

// linkMatches: the link holds, for each templated query parameter, the
// value the request names.
func linkMatches(q map[string]string, got url.Values, link map[string]any) bool {
	for name, v := range q {
		if !descriptor.IsTemplate(v) {
			continue
		}
		want, ok := (&descriptor.ID{From: v}).Compose(link)
		if !ok || want != got.Get(name) {
			return false
		}
	}
	return true
}

// undoRoute is the route of an assoc's undo where it shares the create's
// method and path and only its body's wrapper tells them apart
// (PagerDuty's change_tags: {"add": [...]} or {"remove": [...]}).
func (s *Server) undoRoute(rt *route, req *http.Request) *route {
	k := rt.kind
	if rt.role != assocAdd || k.Delete == nil || k.Delete.BodyEnvelope == "" || req.Body == nil {
		return rt
	}
	data, _ := io.ReadAll(req.Body)
	req.Body = io.NopCloser(bytes.NewReader(data))
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return rt
	}
	if _, undo := m[k.Delete.BodyEnvelope]; !undo {
		return rt
	}
	for i := range s.routes {
		if r := &s.routes[i]; r.kind == k && r.role == assocDel {
			return r
		}
	}
	return rt
}
