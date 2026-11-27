// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package cassette

// Redaction is the second of two ways a cassette keeps secrets out. The
// first, the Scrubber, replaces values the run knows (its credentials, the
// base URL, its names) with placeholders, which a replay fills and compares
// exactly. Redaction replaces what the run does not know by value, found by
// a field's name or a value's shape: an OAuth client's secret a create hands
// back, an access token inside a URL, a person's email address. It runs
// after scrubbing, on both the recording and (in a replay) every request, so
// the two are compared alike; a redacted value is not compared, so the rules
// below take only what looks secret.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Redacted replaces a secret; RedactedEmail a person's email address.
const (
	Redacted      = "{{redacted}}"
	RedactedEmail = "{{email}}"
)

// secretNames: a field (lower case, without _ and -) whose name contains
// one of these holds a secret when its value looks like one (looksSecret).
var secretNames = []string{
	"secret", "password", "passwd", "passphrase", "privatekey", "apikey", "appkey",
	"token", "signingkey", "secretkey", "accesskey", "encryptedvalue", "dsn",
	"credential", "routingkey", "servicekey", "integrationkey",
}

// alwaysNames: a field whose name contains one of these holds a secret
// whatever its value looks like ("Bearer abc", "correct-horse-battery").
var alwaysNames = []string{"authorization", "cookie", "bearer", "secret", "password", "passwd", "passphrase", "privatekey", "signature", "hmac"}

// queryNames: query parameters that carry a secret (signed URLs).
var queryNames = []string{"sig", "signature", "xamzsignature", "xamzcredential", "xamzsecuritytoken", "hmac", "accesstoken", "apikey", "token", "key"}

// personNames: fields that hold a person's name.
var personNames = []string{"authorname", "ownername", "creatorname", "fullname", "realname", "authoremail"}

// personObjects: objects that describe a person (Datadog's
// "author": {"name": ..., "handle": ...}), and their fields that name one.
var (
	personObjects = map[string]bool{"author": true, "owner": true, "creator": true, "createdby": true, "modifiedby": true, "updatedby": true, "lastmodifiedby": true, "deletedby": true}
	personFields  = map[string]bool{"name": true, "displayname": true, "handle": true, "email": true, "username": true, "avatar": true, "avatarurl": true, "icon": true, "gravatar": true, "summary": true, "login": true, "company": true, "blog": true, "location": true, "bio": true, "twitterusername": true}
)

// personTypes: an object typed as one of these is a person (GitHub's
// "type": "User", PagerDuty's "user_reference", JSON:API's "users").
var personTypes = map[string]bool{"user": true, "users": true, "userreference": true, "member": true, "person": true}

// isPerson: an object that holds a person's email address (a user record
// an API embeds) describes that person.
func isPerson(m map[string]any) bool {
	if t, ok := m["type"].(string); ok && personTypes[normName(t)] {
		return true
	}
	for _, k := range []string{"email", "handle", "login", "username", "user_email"} {
		if s, ok := m[k].(string); ok && emailRE.MatchString(s) && !reservedRE.MatchString(emailRE.FindString(s)) {
			return true
		}
	}
	return false
}

// tokenRE finds a credential by its shape, anywhere in a value: vendors'
// token prefixes (Datadog's application and service-account keys,
// Cloudflare's, GitHub's, GitLab's, Slack's, Stripe's), AWS access key IDs,
// and JWTs.
var tokenRE = regexp.MustCompile(`(?:^|[^A-Za-z0-9])((?:ddapp_|ddsat_|ddpat_|cfat_|cfut_|ghp_|gho_|ghs_|ghu_|github_pat_|glpat-|xox[abpr]-|sk_live_|rk_live_|AKIA[0-9A-Z]{16}|ASIA[0-9A-Z]{16}|eyJ[A-Za-z0-9_-]{8,}\.eyJ|hooks\.slack\.com/services/)[A-Za-z0-9_\-./]*)`)

// privateKeyRE finds a PEM private key wherever it is.
var privateKeyRE = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)

// emailRE finds an email address; one at a reserved domain (example.com,
// .test, .invalid) is a test's own, not a person's.
var (
	emailRE    = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	reservedRE = regexp.MustCompile(`(?i)@([a-z0-9-]+\.)*(example\.(com|org|net)|[a-z0-9-]+\.(test|invalid|example|localhost))$`)
)

func normName(k string) string {
	return strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(k))
}

func nameHas(key string, list []string) bool {
	k := normName(key)
	for _, n := range list {
		if strings.Contains(k, n) {
			return true
		}
	}
	return false
}

// notSecretSuffixes: a name that ends in one of these describes a secret
// rather than holding one (access_token_url, token_endpoint_auth_method,
// secret_type, client_secret_expires_at, signing_kid).
var notSecretSuffixes = []string{"url", "uri", "href", "type", "method", "binding", "scheme", "kid", "at", "id", "ids", "name", "names", "count", "version", "template"}

// strongNames: a name holding one of these holds a secret even when it ends
// like a description (secret_id is Vault's AppRole credential).
var strongNames = []string{"authorization", "cookie", "secret", "password", "passwd", "passphrase", "privatekey"}

func secretName(key string) bool {
	if nameHas(key, strongNames) {
		return true
	}
	k := normName(key)
	for _, n := range notSecretSuffixes {
		if strings.HasSuffix(k, n) {
			return false
		}
	}
	return nameHas(key, secretNames) || nameHas(key, alwaysNames)
}

// plainRE: a value that is words, not a secret ("client_secret_post",
// "basic", "oauth2"); plainValueRE: one that is a URL, an upper-case
// enum (HTTP-REDIRECT, SAML2) or a timestamp. A secret has a digit, mixed
// case or a symbol, and is none of these.
var (
	plainRE      = regexp.MustCompile(`^[a-z]+([_\-. ][a-z]+)*$`)
	plainValueRE = regexp.MustCompile(`^(https?://\S+|[A-Z][A-Z0-9]*([_\-][A-Z0-9]+)*|\d{4}-\d{2}-\d{2}([T ][0-9:.]+(Z|[+-]\d{2}:?\d{2})?)?)$`)
)

// looksSecret: a value under a secret's name that is one. A field named for
// a secret (client_secret, apiKey, password) holds one whatever it looks
// like; a field whose name only contains one (token_endpoint_auth_method),
// or any field under a secret-named object, only a value that is not plain
// words.
func looksSecret(key, v string) bool {
	if v == "" || onlyPlaceholders(v) {
		return false // only placeholders: already safe, and compared exactly
	}
	if nameHas(key, alwaysNames) {
		return true
	}
	k := normName(key)
	for _, n := range secretNames {
		if strings.HasSuffix(k, n) {
			return len(v) >= minScrub
		}
	}
	return len(v) >= 6 && !plainRE.MatchString(v) && !plainValueRE.MatchString(v)
}

// onlyPlaceholders: a value that is placeholders ({{redacted}}, {{id:1}},
// {{name}}-{{run}}) and nothing else: already safe, and compared exactly.
func onlyPlaceholders(v string) bool {
	return placeholderRE.MatchString(v) && strings.TrimSpace(placeholderRE.ReplaceAllString(v, "")) == ""
}

// urlRE finds URLs in text; headerRE header-style secrets
// ("Authorization: Basic ...", "X-Api-Key=...").
var (
	urlRE    = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://[^\s"'<>\\]+`)
	headerRE = regexp.MustCompile(`(?i)\b(authorization|cookie|set-cookie|x-api-key|api-key|x-auth-token)(\s*[:=]\s*)(bearer |basic |token )?[^\s",;]+`)
)

// redactURL redacts a URL's password (scheme://user:pass@host) and its
// query's secrets.
func redactURL(u string, keep *keeper) string {
	scheme, rest, ok := strings.Cut(u, "://")
	if !ok {
		return u
	}
	host, path, hasPath := strings.Cut(rest, "/")
	if at := strings.LastIndex(host, "@"); at >= 0 {
		if user, pass, ok := strings.Cut(host[:at], ":"); ok {
			keep.took(pass)
			host = user + ":" + Redacted + host[at:]
		}
	}
	rest = host
	if hasPath {
		rest += "/" + path
	}
	if p, q, ok := strings.Cut(rest, "?"); ok {
		rest = p + "?" + redactQuery(q, keep)
	}
	return scheme + "://" + rest
}

// shaped is whether a value is a credential by its shape alone.
func shaped(v string) bool { return tokenRE.MatchString(v) || privateKeyRE.MatchString(v) }

// redactText redacts what is found by shape in free text: tokens, private
// keys (the whole text), people's email addresses.
func redactText(s string, keep *keeper) string {
	if privateKeyRE.MatchString(s) {
		keep.took(s)
		return Redacted
	}
	s = urlRE.ReplaceAllStringFunc(s, func(u string) string { return redactURL(u, keep) })
	s = headerRE.ReplaceAllStringFunc(s, func(m string) string {
		g := headerRE.FindStringSubmatchIndex(m)
		at := g[5] // after the separator, or after "Bearer " when there is one
		if g[7] >= 0 {
			at = g[7]
		}
		keep.took(m[at:])
		return m[:g[5]] + Redacted
	})
	s = tokenRE.ReplaceAllStringFunc(s, func(m string) string {
		tok := tokenRE.FindStringSubmatch(m)[1]
		if keep.keeps(tok) {
			return m
		}
		keep.took(tok)
		return strings.TrimSuffix(m, tok) + Redacted // the character before it stays
	})
	return emailRE.ReplaceAllStringFunc(s, func(m string) string {
		if reservedRE.MatchString(m) || strings.Contains(m, "{{") {
			return m
		}
		return RedactedEmail
	})
}

// Redact redacts a body, a response or any text: in JSON, every value under
// a secret's name that looks secret (and every one under a secret-named
// object), people's names, and, in any value, credentials by their shape and
// people's email addresses; JSON held in a string is redacted too. A form
// body (a=1&b=2) is redacted by its parameters' names, anything else as
// text. Text with nothing to redact is returned as it was.
func Redact(text string) string { return redactExcept(text, nil) }

// redactExcept is Redact, leaving values keep says are identifiers.
func redactExcept(text string, keep *keeper) string {
	t := strings.TrimSpace(text)
	if t == "" {
		return text
	}
	if t[0] == '{' || t[0] == '[' {
		if out, ok := redactJSON(text, keep); ok {
			return out
		}
	}
	if form, ok := redactForm(t, keep); ok {
		return form
	}
	return redactProse(text, keep)
}

// namedRE finds a name and what separates it from a value written after it
// in text (`secret_id_accessor: "711b..."`, `api_key=abc...`): an error
// that quotes what it was sent. namedValueRE is the value.
var (
	namedRE      = regexp.MustCompile(`([A-Za-z][A-Za-z0-9_\-]*)\\?"?\s*[:=]\s*\\?"?`)
	namedValueRE = regexp.MustCompile(`^[^\s"'\\,;&<>{}()\[\]]+`)
)

// redactNamed redacts, in text, each value written after a secret's name
// that looks like one: long enough, not words, a URL, an enum or a time.
func redactNamed(s string, keep *keeper) string {
	if !strings.ContainsAny(s, ":=") {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range namedRE.FindAllStringSubmatchIndex(s, -1) {
		if m[0] < last || !secretName(s[m[2]:m[3]]) {
			continue
		}
		v := namedValueRE.FindString(s[m[1]:])
		if !knowable(v) || plainValueRE.MatchString(v) || keep.keeps(v) {
			continue
		}
		keep.took(v)
		b.WriteString(s[last:m[1]])
		b.WriteString(Redacted)
		last = m[1] + len(v)
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// redactProse is redactText for prose (a field's value, an error's text),
// where a secret's name may be written before its value; a path is not.
func redactProse(s string, keep *keeper) string {
	return redactNamed(redactText(s, keep), keep)
}

func redactJSON(text string, keep *keeper) (string, bool) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber() // large IDs keep every digit
	var v any
	if dec.Decode(&v) != nil {
		return "", false
	}
	if !redactValue(v, keep) {
		return text, true
	}
	return encodeJSON(v, text), true
}

func encodeJSON(v any, fallback string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return fallback
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// redactValue redacts in place; it says whether it changed anything.
func redactValue(v any, keep *keeper) bool {
	changed := false
	leaf := func(key, s string, under bool, parent string, person bool) (string, bool) {
		if (person || personObjects[normName(parent)]) && (personFields[normName(key)] || strings.HasSuffix(normName(key), "url")) && s != "" && !placeholderRE.MatchString(s) {
			if keep.keeps(s) {
				// A person's login that is also a path's ID (GitHub's
				// team memberships, /memberships/<login>): {{id:N}} here as
				// there, so a replay that reads it sends the same stand-in.
				keep.note(s)
				return s, false
			}
			return Redacted, true
		}
		if (under || secretName(key)) && looksSecret(key, s) || shaped(s) && !strings.Contains(s, " ") {
			if keep.keeps(s) {
				keep.note(s) // a secret that is also an ID: a placeholder, not the value
				return s, false
			}
			keep.took(s)
			return Redacted, true
		}
		if keep.keeps(s) {
			return s, false
		}
		switch {
		case nameHas(key, personNames) && !placeholderRE.MatchString(s) && s != "":
			return Redacted, true
		}
		if inner := strings.TrimSpace(s); len(inner) > 1 && (inner[0] == '{' || inner[0] == '[') {
			if out, ok := redactJSON(s, keep); ok && out != s {
				return out, true
			}
		}
		if out := redactProse(s, keep); out != s {
			return out, true
		}
		return s, false
	}
	var walk func(v any, under bool, parent string, person bool)
	walk = func(v any, under bool, parent string, inherited bool) {
		switch x := v.(type) {
		case map[string]any:
			person := inherited || isPerson(x)
			// A person's login or handle names them in the object's other
			// strings too (GitHub's html_url: .../users/<login>).
			var names []string
			if person {
				for _, k := range []string{"login", "handle", "username", "slug"} {
					if n, ok := x[k].(string); ok && len(n) >= minScrub && !placeholderRE.MatchString(n) && !keep.keeps(n) {
						names = append(names, n)
					}
				}
			}
			for k, e := range x {
				if s, ok := e.(string); ok {
					out, c := leaf(k, s, under, parent, person)
					for _, n := range names {
						if strings.Contains(out, n) {
							out, c = strings.ReplaceAll(out, n, Redacted), true
						}
					}
					if c {
						x[k], changed = out, true
					}
					continue
				}
				// JSON:API: a person's fields are in its "attributes".
				walk(e, under || secretName(k), k, person && k == "attributes")
			}
		case []any:
			for i, e := range x {
				if s, ok := e.(string); ok {
					if out, c := leaf("", s, under, parent, false); c {
						x[i], changed = out, true
					}
					continue
				}
				walk(e, under, parent, false)
			}
		}
	}
	walk(v, false, "", false)
	return changed
}

// formRE: a form-encoded body (client_id=a&client_secret=b).
var formRE = regexp.MustCompile(`^[A-Za-z0-9_.\-\[\]%]+=[^&\s]*(&[A-Za-z0-9_.\-\[\]%]+=[^&\s]*)*$`)

func redactForm(t string, keep *keeper) (string, bool) {
	if !formRE.MatchString(t) {
		return "", false
	}
	return redactQuery(t, keep), true
}

// redactQuery redacts a query string's (or form body's) values, keeping its
// parameters' order.
func redactQuery(q string, keep *keeper) string {
	parts := strings.Split(q, "&")
	for i, p := range parts {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		dv, err := url.QueryUnescape(v)
		if err != nil {
			dv = v
		}
		if keep.keeps(dv) {
			continue
		}
		switch {
		case secretName(k) && looksSecret(k, dv), nameHas(k, queryNames) && len(dv) >= minScrub, shaped(dv):
			keep.took(dv)
			parts[i] = k + "=" + Redacted
		default:
			if out := redactText(dv, keep); out != dv {
				parts[i] = k + "=" + out
			}
		}
	}
	return strings.Join(parts, "&")
}

// RedactPath redacts a request path: its query's secrets, and credentials
// and email addresses in its segments.
func RedactPath(path string) string { return redactPathExcept(path, nil) }

func redactPathExcept(path string, keep *keeper) string {
	p, q, hasQ := strings.Cut(path, "?")
	p = redactText(p, keep)
	if hasQ {
		return p + "?" + redactQuery(q, keep)
	}
	return p
}

// keeper knows the identifiers redaction leaves (identifiers), and notes the
// ones that are secrets too, so Scrubbed can give them {{id:N}}; and it
// notes each value redaction took out whole (found), so it can be redacted
// wherever else it is (Known).
type keeper struct {
	isID  func(string) bool
	ids   map[string]bool
	found map[string]bool
}

func (k *keeper) keeps(s string) bool { return k != nil && k.isID != nil && k.isID(s) }

func (k *keeper) note(s string) {
	if k != nil && k.ids != nil {
		k.ids[s] = true
	}
}

// took notes a value redaction replaced whole, if it is one to look for
// elsewhere (knowable).
func (k *keeper) took(s string) {
	if k != nil && k.found != nil && knowable(s) {
		k.found[s] = true
	}
}

// minKnown is the shortest value redacted wherever it is once it was
// redacted somewhere: a shorter one could be part of text it has nothing
// to do with.
const minKnown = 8

// knowable: a value redacted whole that is also redacted wherever else it
// is: long enough, not plain words ("client_secret_post", which names a
// method), and holding no placeholder (a value the replay fills, compared
// as it is).
func knowable(v string) bool {
	return len(v) >= minKnown && !strings.Contains(v, "{{") && !plainRE.MatchString(v)
}

// identifiers says whether a value is a whole segment of some interaction's
// path: an ID, however secret its field's name (a secure embed's token is
// its ID), and not redacted, so the paths that use it still match. Short
// values and credentials by their shape are never identifiers.
func identifiers(ins []Interaction) *keeper {
	segs := map[string]bool{}
	for _, in := range ins {
		p, _, _ := strings.Cut(in.Path, "?")
		for _, s := range strings.Split(p, "/") {
			if len(s) < 8 {
				continue
			}
			segs[s] = true
			if u, err := url.PathUnescape(s); err == nil {
				segs[u] = true
			}
		}
	}
	return &keeper{isID: func(v string) bool { return segs[v] && !shaped(v) && !emailRE.MatchString(v) && !onlyPlaceholders(v) }, ids: map[string]bool{}, found: map[string]bool{}}
}

// sentSecrets is every value a request body sends that redaction would hide,
// by field name (normalized), for refill.
func sentSecrets(body string, out map[string]string) {
	var v any
	if json.Unmarshal([]byte(body), &v) != nil {
		return
	}
	var walk func(v any, under bool)
	walk = func(v any, under bool) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if s, ok := e.(string); ok {
					if (under || secretName(k)) && looksSecret(k, s) || shaped(s) {
						out[normName(k)] = s
					}
					continue
				}
				walk(e, under || secretName(k))
			}
		case []any:
			for _, e := range x {
				walk(e, under)
			}
		}
	}
	walk(v, false)
}

// sentRedacted adds to out, by JSON path (a list's index left out), each
// string field of a sent body that redaction changes (redacted is the same
// body redacted): a person's name and email, whatever rule redacts them, so
// an API that echoes them answers the replay's values, as refill does for
// secrets. A field once redacted keeps its latest value sent, redacted or
// not (a drift step's).
func sentRedacted(body, redacted string, out map[string]string) {
	var v, r any
	if json.Unmarshal([]byte(body), &v) != nil || json.Unmarshal([]byte(redacted), &r) != nil {
		return
	}
	var walk func(v, r any, at string)
	walk = func(v, r any, at string) {
		switch x := v.(type) {
		case map[string]any:
			rm, _ := r.(map[string]any)
			for k, e := range x {
				p := at + "/" + k
				if s, ok := e.(string); ok {
					if _, seen := out[p]; (rm[k] != s || seen) && s != Redacted {
						out[p] = s
					}
					continue
				}
				walk(e, rm[k], p)
			}
		case []any:
			rl, _ := r.([]any)
			for i, e := range x {
				if i < len(rl) {
					walk(e, rl[i], at+"/[]")
				}
			}
		}
	}
	walk(v, r, "")
}

// refill puts back, in a response, each redacted field the replay sent a
// value for under the same name: an API that echoes what it was sent (an
// IdP's credentials.client.client_id) answers the replay's value, as it
// answered the recording's.
func refill(text string, sent, sentAt map[string]string) string {
	if len(sent)+len(sentAt) == 0 || !strings.Contains(text, Redacted) && !strings.Contains(text, RedactedEmail) {
		return text
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return text
	}
	changed := false
	var walk func(v any, at string)
	walk = func(v any, at string) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if str, ok := e.(string); ok && str != Redacted && (strings.Contains(str, RedactedEmail) || strings.Contains(str, Redacted)) {
					if s, ok := atPath(sentAt, at+"/"+k); ok { // redacted in the text (an email address, a URL's password): what was sent
						x[k], changed = s, true
					}
					continue
				}
				if e == Redacted {
					if s, ok := atPath(sentAt, at+"/"+k); ok {
						x[k], changed = s, true
					} else if s, ok := sent[normName(k)]; ok {
						x[k], changed = s, true
					}
					continue
				}
				walk(e, at+"/"+k)
			}
		case []any:
			for _, e := range x {
				walk(e, at+"/[]")
			}
		}
	}
	walk(v, "")
	if !changed {
		return text
	}
	return encodeJSON(v, text)
}

// atPath is what was sent at a response's path, or at the same path less
// its first segments: an envelope only the response has (Vault's data).
func atPath(sentAt map[string]string, p string) (string, bool) {
	for {
		if s, ok := sentAt[p]; ok {
			return s, true
		}
		i := strings.Index(p[1:], "/")
		if i < 0 {
			return "", false
		}
		p = p[i+1:]
	}
}

// Leaks lists where a cassette still holds a known secret (known: values by
// name, in any encoding Scrubber.AddSecret adds) or a credential by its
// shape, without the values. A recording with any is not written.
func (c *Cassette) Leaks(known map[string]string) []string {
	var out []string
	check := func(where, text string) {
		if text == "" {
			return
		}
		for name, v := range known {
			for _, form := range encodings(v) {
				if len(form) >= minScrub && strings.Contains(text, form) {
					out = append(out, where+": the value of "+name)
					break
				}
			}
		}
		if privateKeyRE.MatchString(text) {
			out = append(out, where+": a private key")
		}
		if m := tokenRE.FindStringSubmatch(text); m != nil {
			out = append(out, where+": a token ("+m[1][:min(len(m[1]), 5)]+"…)")
		}
	}
	check("target", c.Target)
	for k, v := range c.Vars {
		if s, ok := v.(string); ok {
			check("var "+k, s)
		}
	}
	for i, in := range c.Interactions {
		n := i + 1
		check(sprintf("request %d path", n), in.Path)
		check(sprintf("request %d body", n), in.Body)
		check(sprintf("request %d response", n), in.Response)
	}
	return out
}

// encoded: a value as it may appear in a request or response, by the
// encoding's name: URL-encoded (query and path), JSON-escaped, base64. Only
// forms that differ from the value itself.
func encoded(v string) [][2]string {
	var out [][2]string
	seen := map[string]bool{v: true}
	for _, e := range [][2]string{{"url", url.QueryEscape(v)}, {"path", url.PathEscape(v)}, {"json", jsonEscape(v)}, {"b64", b64(v)},
		{"b64url", base64.URLEncoding.EncodeToString([]byte(v))}, {"b64raw", base64.RawStdEncoding.EncodeToString([]byte(v))}, {"b64rawurl", base64.RawURLEncoding.EncodeToString([]byte(v))}} {
		if !seen[e[1]] {
			seen[e[1]] = true
			out = append(out, e)
		}
	}
	return out
}

// encodings: the value and its encoded forms.
func encodings(v string) []string {
	out := []string{v}
	for _, e := range encoded(v) {
		out = append(out, e[1])
	}
	return out
}

func jsonEscape(v string) string {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	return string(b[1 : len(b)-1])
}

func b64(v string) string { return base64.StdEncoding.EncodeToString([]byte(v)) }

func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

// RedactFields is the cassette with every string under a JSON field named
// as one of names (the last part of a dotted name) redacted, in each body
// and response: a secret the API makes under a name that says nothing
// (Cloudflare's TURN key, Datadog's application key: "key"), which the
// kind declares among its secret outputs. ScrubbedWith does this too, and
// redacts each such value wherever else it is.
func (c *Cassette) RedactFields(names []string) *Cassette {
	if len(names) == 0 {
		return c
	}
	out := *c
	out.Interactions = make([]Interaction, len(c.Interactions))
	for i, in := range c.Interactions {
		in.Body, in.Response = redactFields(in.Body, names, nil), redactFields(in.Response, names, nil)
		out.Interactions[i] = in
	}
	return &out
}

// redactFields redacts, in JSON text, every string under a field named as
// one of names (the last part of a dotted name), but an identifier keep
// knows, which it notes for {{id:N}}; keep notes each value it took.
func redactFields(t string, names []string, keep *keeper) string {
	if len(names) == 0 || strings.TrimSpace(t) == "" {
		return t
	}
	leaf := map[string]bool{}
	for _, n := range names {
		parts := strings.Split(n, ".")
		leaf[parts[len(parts)-1]] = true
	}
	var walk func(v any) bool
	walk = func(v any) bool {
		changed := false
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if s, ok := e.(string); ok && leaf[k] && s != "" && !onlyPlaceholders(s) {
					if keep.keeps(s) {
						keep.note(s) // also an ID: {{id:N}} everywhere, so its paths still match
						continue
					}
					keep.took(s)
					x[k], changed = Redacted, true
					continue
				}
				if walk(e) {
					changed = true
				}
			}
		case []any:
			for _, e := range x {
				if walk(e) {
					changed = true
				}
			}
		}
		return changed
	}
	dec := json.NewDecoder(strings.NewReader(t))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil || !walk(v) {
		return t
	}
	return encodeJSON(v, t)
}
