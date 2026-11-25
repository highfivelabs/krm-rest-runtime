// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package cassette

// Identities: values that name the test account or a person, which a
// public repository's cassettes must not hold, however plain they look
// (a login is often a person's name, an account's subdomain too). The
// secret rules (redact.go) leave them: they are words, not credentials, and
// a value that is plain words is kept and compared. A run says which they
// are (live.yaml's redactVars, redactFields, redactSubdomains, and any var
// saved from a person's login); each one found is redacted wherever it is,
// in every cassette, log line and record of the run, as a secret redacted
// whole is (Known).

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// Identities says what in a run's calls names the account or a person. A
// live.yaml holds it (its fields are live.yaml's top-level keys).
type Identities struct {
	// RedactVars are saved vars ({{var:NAME}}) whose values name the
	// account or a person: GitHub's authenticated login (save: {me: login}).
	RedactVars []string `json:"redactVars,omitempty"`
	// RedactFields are JSON fields (the last part of a dotted name) whose
	// string values do, wherever they are in a body or a response: a deploy
	// key's added_by.
	RedactFields []string `json:"redactFields,omitempty"`
	// RedactSubdomains are domains under which the label just before the
	// domain is the account's own (Cloudflare's workers.dev: a Worker's URL
	// is https://<script>.<account>.workers.dev): that label, in any host.
	RedactSubdomains []string `json:"redactSubdomains,omitempty"`
}

// identityFields: a value saved from a field of one of these names (the
// last part of its path; lower case, without _ and -) names a person: the
// authenticated user's login (GET /user's login, GET /me's username).
var identityFields = map[string]bool{"login": true, "username": true, "userlogin": true, "handle": true, "email": true, "useremail": true}

// IdentityField says whether a value saved from a response's field at path
// (a slash path: user/login) names a person, so the var it is saved as is
// an identity whether or not redactVars names it.
func IdentityField(path string) bool {
	parts := strings.Split(path, "/")
	return identityFields[normName(parts[len(parts)-1])]
}

// identityRules is Identities, ready to find values with.
type identityRules struct {
	vars, fields map[string]bool
	hosts        []*regexp.Regexp
}

func (id Identities) rules() *identityRules {
	if len(id.RedactVars)+len(id.RedactFields)+len(id.RedactSubdomains) == 0 {
		return nil
	}
	r := &identityRules{vars: map[string]bool{}, fields: map[string]bool{}}
	for _, v := range id.RedactVars {
		r.vars[v] = true
	}
	for _, f := range id.RedactFields {
		parts := strings.Split(f, ".")
		r.fields[parts[len(parts)-1]] = true
	}
	for _, d := range id.RedactSubdomains {
		d = strings.Trim(strings.ToLower(d), ".")
		if d == "" {
			continue
		}
		// The label just before .<domain>, the domain ending there.
		r.hosts = append(r.hosts, regexp.MustCompile(`(?i)([a-z0-9](?:[a-z0-9-]*[a-z0-9])?)\.`+regexp.QuoteMeta(d)+`(?:[^a-z0-9.\-]|\.?$|\.[^a-z0-9])`))
	}
	return r
}

// find adds to out each identity c holds: its vars named so, the values of
// fields named so in its bodies and responses, the account's label in hosts
// under the domains.
func (r *identityRules) find(c *Cassette, out map[string]bool) {
	if r == nil {
		return
	}
	for name, v := range c.Vars {
		if s, ok := v.(string); ok && r.vars[name] {
			out[s] = true
		}
	}
	texts := []string{c.Target}
	for _, in := range c.Interactions {
		texts = append(texts, in.Path, in.Body, in.Response, in.Link)
		if len(r.fields) > 0 {
			fieldValues(in.Body, r.fields, out)
			fieldValues(in.Response, r.fields, out)
		}
	}
	for _, re := range r.hosts {
		for _, t := range texts {
			for _, m := range re.FindAllStringSubmatch(t, -1) {
				out[m[1]] = true
			}
		}
	}
}

// fieldValues adds to out every string under a field named in fields, in
// JSON text (JSON held in a string too).
func fieldValues(text string, fields map[string]bool, out map[string]bool) {
	t := strings.TrimSpace(text)
	if t == "" || t[0] != '{' && t[0] != '[' {
		return
	}
	dec := json.NewDecoder(strings.NewReader(t))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return
	}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if s, ok := e.(string); ok {
					if fields[k] {
						out[s] = true
					} else {
						fieldValues(s, fields, out)
					}
					continue
				}
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
}

// identity: a value redacted as an identity: long enough not to be found
// in text it has nothing to do with, and holding no placeholder (a value
// that is already one).
func identity(v string) bool {
	return len(v) >= minScrub && !strings.Contains(v, "{{")
}

// Identify makes k find, in every cassette it is given (ScrubbedWith), the
// identities id names, and redact each one found everywhere, as it does a
// secret redacted whole.
func (k *Known) Identify(id Identities) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rules = id.rules()
}

// AddIdentity adds values that name the account or a person (a saved
// login, a value cmd/rescrub was given): each is redacted wherever it is,
// as a whole word or number, however plain it looks. A value shorter than
// four characters, or holding a placeholder, is left.
func (k *Known) AddIdentity(vals ...string) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if identity(v) {
			k.ident[v] = true
		}
	}
}

// Identities lists the identity values k knows, sorted. For tests and
// counts: never print them.
func (k *Known) Identities() []string {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]string, 0, len(k.ident))
	for v := range k.ident {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// identify adds the identities c holds, by k's rules.
func (k *Known) identify(c *Cassette) {
	if k == nil {
		return
	}
	k.mu.Lock()
	r := k.rules
	k.mu.Unlock()
	if r == nil {
		return
	}
	found := map[string]bool{}
	r.find(c, found)
	vals := make([]string, 0, len(found))
	for v := range found {
		vals = append(vals, v)
	}
	k.AddIdentity(vals...)
}
