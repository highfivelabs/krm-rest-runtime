// SPDX-License-Identifier: Apache-2.0

package descriptor

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// GraphQL makes an operation a GraphQL request instead of a REST call: a
// POST to Op.Path, the vendor's GraphQL endpoint (GitLab's /api/graphql,
// GitHub's /graphql; see rest.Vendor.RESTPath for how it is found from the
// Connection's base URL), of {"query", "operationName", "variables"}.
//
// GitLab's compliance frameworks, saved replies and dependency proxy
// settings, and GitHub's branch protection rules, have no REST API.
type GraphQL struct {
	// Operation is the operation's name in Query, sent as operationName:
	// the fake API answers by it, so each is unique in a vendor.
	Operation string `json:"operation"`
	// Query is the document: one query or mutation.
	Query string `json:"query"`
	// Variables are the request's variables, as templates filled from the
	// spec (Fill): "{field}" alone is that path parameter's value, or the
	// body field's value as it is (a number stays a number; a field the
	// spec leaves out leaves the variable out); text around placeholders
	// ("gid://gitlab/Group/{groupId}") makes a string of path parameters;
	// "$body" is the whole body (the spec less its path parameters);
	// objects and lists are filled member by member; any other value is
	// sent as it is.
	Variables map[string]any `json:"variables,omitempty"`
	// Result is where in the answer's data the object is, as a slash path
	// (createComplianceFramework/framework); for a find read, the list
	// (project/targetBranchRules/nodes). Null or absent on a read: the
	// object does not exist.
	Result string `json:"result,omitempty"`
	// Errors is where a mutation answers its own errors (GitLab's payload
	// errors: [String!]!, createComplianceFramework/errors): a 200 whose
	// list there is not empty failed, as does one with top-level errors.
	Errors string `json:"errors,omitempty"`
	// Exists, on a read, is the field of the result (a slash path) that
	// says whether the object exists: anything but true there is not found
	// (GitLab answers project { isCatalogResource } whether or not the
	// project is a CI/CD catalog resource).
	Exists string `json:"exists,omitempty"`
}

// BodyTemplate is the variable template that stands for the whole body.
const BodyTemplate = "$body"

var wholeRE = regexp.MustCompile(`^\{([^{}]+)\}$`)

// Request is the request body sent: the document, its operation's name,
// and the variables filled from params and body.
func (g *GraphQL) Request(params map[string]string, body map[string]any) (map[string]any, error) {
	vars, err := fillMap(g.Variables, params, body)
	if err != nil {
		return nil, fmt.Errorf("graphql %s: %w", g.Operation, err)
	}
	out := map[string]any{"query": g.Query, "operationName": g.Operation}
	if len(vars) > 0 {
		out["variables"] = vars
	}
	return out, nil
}

func fillMap(m map[string]any, params map[string]string, body map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(m))
	for k, t := range m {
		v, ok, err := fill(t, params, body)
		if err != nil {
			return nil, err
		}
		if ok {
			out[k] = v
		}
	}
	return out, nil
}

// fill is one template's value; ok is false where it is left out (a body
// field the spec does not set).
func fill(t any, params map[string]string, body map[string]any) (any, bool, error) {
	switch tt := t.(type) {
	case string:
		if tt == BodyTemplate {
			if body == nil {
				return map[string]any{}, true, nil
			}
			return body, true, nil
		}
		if m := wholeRE.FindStringSubmatch(tt); m != nil {
			if v, ok := params[m[1]]; ok && v != "" {
				return v, true, nil
			}
			if v, ok := body[m[1]]; ok && v != nil {
				return v, true, nil
			}
			return nil, false, nil
		}
		if !templateRE.MatchString(tt) {
			return tt, true, nil
		}
		var missing []string
		out := templateRE.ReplaceAllStringFunc(tt, func(p string) string {
			name := p[1 : len(p)-1]
			if v, ok := params[name]; ok && v != "" {
				return v
			}
			if v, ok := body[name]; ok && v != nil {
				return Text(v)
			}
			missing = append(missing, name)
			return p
		})
		if len(missing) > 0 {
			return nil, false, fmt.Errorf("%q: no value for %s", tt, strings.Join(missing, ", "))
		}
		return out, true, nil
	case map[string]any:
		m, err := fillMap(tt, params, body)
		return m, err == nil, err
	case []any:
		out := make([]any, 0, len(tt))
		for _, e := range tt {
			v, ok, err := fill(e, params, body)
			if err != nil {
				return nil, false, err
			}
			if ok {
				out = append(out, v)
			}
		}
		return out, true, nil
	default:
		return t, true, nil
	}
}

// Extract is Request's inverse, for the fake API: the path parameters
// (names isParam accepts) and body fields that variables were filled from.
func (g *GraphQL) Extract(vars map[string]any, isParam func(string) bool) (map[string]string, map[string]any) {
	params, body := map[string]string{}, map[string]any{}
	extractMap(g.Variables, vars, isParam, params, body)
	return params, body
}

func extractMap(t, v map[string]any, isParam func(string) bool, params map[string]string, body map[string]any) {
	for k, tt := range t {
		if vv, ok := v[k]; ok && vv != nil {
			extract(tt, vv, isParam, params, body)
		}
	}
}

func extract(t, v any, isParam func(string) bool, params map[string]string, body map[string]any) {
	switch tt := t.(type) {
	case string:
		if tt == BodyTemplate {
			if m, ok := v.(map[string]any); ok {
				for k, x := range m {
					body[k] = x
				}
			}
			return
		}
		if m := wholeRE.FindStringSubmatch(tt); m != nil {
			if isParam(m[1]) {
				params[m[1]] = Text(v)
			} else {
				body[m[1]] = v
			}
			return
		}
		names := Placeholders(tt)
		if len(names) == 0 {
			return
		}
		parts := templateRE.Split(tt, -1)
		for i := range parts {
			parts[i] = regexp.QuoteMeta(parts[i])
		}
		re := regexp.MustCompile("^" + strings.Join(parts, "(.+?)") + "$")
		s, _ := v.(string)
		if got := re.FindStringSubmatch(s); got != nil {
			for i, n := range names {
				if isParam(n) {
					params[n] = got[i+1]
				} else {
					body[n] = got[i+1]
				}
			}
		}
	case map[string]any:
		if m, ok := v.(map[string]any); ok {
			extractMap(tt, m, isParam, params, body)
		}
	case []any:
		if l, ok := v.([]any); ok {
			for i := range tt {
				if i < len(l) {
					extract(tt[i], l[i], isParam, params, body)
				}
			}
		}
	}
}

// Names are the placeholders the variables name, sorted; body says whether
// they take the whole body too.
func (g *GraphQL) Names() (names []string, body bool) {
	seen := map[string]bool{}
	var walk func(any)
	walk = func(t any) {
		switch tt := t.(type) {
		case string:
			if tt == BodyTemplate {
				body = true
				return
			}
			for _, n := range Placeholders(tt) {
				seen[n] = true
			}
		case map[string]any:
			for _, x := range tt {
				walk(x)
			}
		case []any:
			for _, x := range tt {
				walk(x)
			}
		}
	}
	walk(map[string]any(g.Variables))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, body
}

// interpolated are the placeholders of templates with text around them,
// which only path parameters may fill.
func (g *GraphQL) interpolated() []string {
	var out []string
	var walk func(any)
	walk = func(t any) {
		switch tt := t.(type) {
		case string:
			if tt != BodyTemplate && !wholeRE.MatchString(tt) {
				out = append(out, Placeholders(tt)...)
			}
		case map[string]any:
			for _, x := range tt {
				walk(x)
			}
		case []any:
			for _, x := range tt {
				walk(x)
			}
		}
	}
	walk(map[string]any(g.Variables))
	return out
}

// IsGraphQL reports whether any of the kind's operations is a GraphQL
// request; IsMixed, whether others are REST calls.
func (k *Kind) IsGraphQL() bool {
	for _, op := range []*Op{k.Create, &k.Read, k.Update, k.Delete} {
		if op != nil && op.GraphQL != nil {
			return true
		}
	}
	return false
}

// IsMixed reports whether the kind makes GraphQL requests and REST calls
// both: GitHub makes an enterprise's organization over GraphQL
// (createEnterpriseOrganization) and changes and deletes it over REST
// (/orgs/{org}); GitLab reads an epic board over REST only.
func (k *Kind) IsMixed() bool {
	if !k.IsGraphQL() {
		return false
	}
	for _, op := range []*Op{k.Create, &k.Read, k.Update, k.Delete} {
		if op != nil && op.GraphQL == nil {
			return true
		}
	}
	return false
}

// IsParam reports whether name is a path parameter of the kind, or its ID.
func (k *Kind) IsParam(name string) bool {
	if k.ID != nil && k.ID.Param == name {
		return true
	}
	_, ok := k.ParamField(name)
	return ok
}

func (k *Kind) validateGraphQL() error {
	var ops []*Op // the GraphQL ones; a mixed kind's others are REST calls
	for _, op := range []*Op{k.Create, &k.Read, k.Update, k.Delete} {
		if op != nil && op.GraphQL != nil {
			ops = append(ops, op)
		}
	}
	switch {
	case k.Shape == Assoc:
		return fmt.Errorf("graphql: not for assoc kinds")
	case k.IsMixed() && k.Shape != CRUD:
		return fmt.Errorf("graphql: a kind of GraphQL and REST calls both is crud (its object is found by its ID)")
	case k.Envelope != "" || k.BodyEnvelope != "" || k.BodyArray || k.BodyField != "" || k.Multipart != nil || k.UpdateID != "":
		return fmt.Errorf("graphql: the variables shape the request and result says where the object is: no envelope, bodyEnvelope, bodyArray, bodyField, multipart or updateID")
	case k.Replace != nil || len(k.BeforeDelete) > 0:
		return fmt.Errorf("graphql: no replace or beforeDelete")
	}
	for _, op := range ops {
		if op == nil {
			continue
		}
		g := op.GraphQL
		if op.Method != "POST" || !strings.HasPrefix(op.Path, "/") || len(Placeholders(op.Path)) > 0 {
			return fmt.Errorf("graphql %s: a POST to the endpoint's path (/api/graphql), not %s %s", g.Operation, op.Method, op.Path)
		}
		if g.Operation == "" || g.Query == "" || !strings.Contains(g.Query, g.Operation) {
			return fmt.Errorf("graphql: an operation needs its name and a document that names it (%q)", g.Operation)
		}
		if len(op.Query) > 0 || op.Find != nil && op != &k.Read || op.SendBody || len(op.Body) > 0 {
			return fmt.Errorf("graphql %s: no query, sendBody or body (the variables say what is sent)", g.Operation)
		}
		if g.Exists != "" && op != &k.Read {
			return fmt.Errorf("graphql %s: exists is for a read", g.Operation)
		}
		for _, n := range g.interpolated() {
			if !k.IsParam(n) {
				return fmt.Errorf("graphql %s: {%s} is inside text, which only a path parameter or the ID may fill", g.Operation, n)
			}
		}
	}
	return nil
}
