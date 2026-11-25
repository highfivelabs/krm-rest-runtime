// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package cassette

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Known is every value a run's redaction took out whole: a secret under
// its field's name, a kind's secret output, a credential by its shape, a
// secret's value written after its name in an error. Once one is redacted
// anywhere it is redacted everywhere it is, in every encoding, in every
// cassette of the run: the API's error that quotes it ("failed to find
// accessor entry for secret_id_accessor: ..."), a later call that sends it
// back, a saved var. A replay sees the same placeholder wherever the value
// was, so the calls it makes from it are the ones recorded. Safe for
// concurrent use.
type Known struct {
	mu   sync.Mutex
	vals map[string]bool
	// ident: values that name the account or a person (identity.go),
	// redacted however plain or short (four characters or more) they are;
	// rules finds them in each cassette.
	ident map[string]bool
	rules *identityRules
}

// NewKnown returns an empty Known.
func NewKnown() *Known { return &Known{vals: map[string]bool{}, ident: map[string]bool{}} }

func (k *Known) add(vals map[string]bool) {
	if k == nil || len(vals) == 0 {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for v := range vals {
		k.vals[v] = true
	}
}

// Len is how many values are known.
func (k *Known) Len() int {
	if k == nil {
		return 0
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.vals) + len(k.ident)
}

// values are the known values (identities among them) but those skip says
// are identifiers (each noted for {{id:N}} instead: a replay sends its
// stand-in where the recording had it).
func (k *Known) values(skip *keeper) (vals []string, ident map[string]bool) {
	if k == nil {
		return nil, nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]string, 0, len(k.vals)+len(k.ident))
	for v := range k.vals {
		if skip.keeps(v) {
			skip.note(v)
			continue
		}
		out = append(out, v)
	}
	ident = map[string]bool{}
	for v := range k.ident {
		if skip.keeps(v) {
			skip.note(v)
			continue
		}
		if !k.vals[v] {
			out = append(out, v)
		}
		ident[v] = true
	}
	sort.Strings(out)
	return out, ident
}

// replacer replaces each of a set of values, in each of its encodings, with
// Redacted, where it is not part of a longer word or number (the fake's ID
// 270000007 is not in 2700000071). The longest value is replaced first.
type replacer struct{ forms []string }

// redactor is the replacer of vals; nil (which replaces nothing) for none.
// A value's forms shorter than minKnown are left, an identity's (ident)
// shorter than minScrub.
func redactor(vals []string, ident map[string]bool) *replacer {
	if len(vals) == 0 {
		return nil
	}
	seen := map[string]bool{}
	r := &replacer{}
	for _, v := range vals {
		least := minKnown
		if ident[v] {
			least = minScrub
		}
		for _, e := range encodings(v) {
			if len(e) >= least && !seen[e] {
				seen[e] = true
				r.forms = append(r.forms, e)
			}
		}
	}
	sort.Slice(r.forms, func(i, j int) bool {
		if len(r.forms[i]) != len(r.forms[j]) {
			return len(r.forms[i]) > len(r.forms[j])
		}
		return r.forms[i] < r.forms[j]
	})
	return r
}

func wordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// Scrub replaces the values in text.
func (r *replacer) Scrub(text string) string {
	if r == nil || text == "" {
		return text
	}
	for _, f := range r.forms {
		if !strings.Contains(text, f) {
			continue
		}
		var b strings.Builder
		last := 0
		for from := 0; ; {
			i := strings.Index(text[from:], f)
			if i < 0 {
				break
			}
			at, end := from+i, from+i+len(f)
			from = at + 1
			if wordByte(f[0]) && at > 0 && wordByte(text[at-1]) || wordByte(f[len(f)-1]) && end < len(text) && wordByte(text[end]) {
				continue // part of a longer word or number
			}
			b.WriteString(text[last:at])
			b.WriteString(Redacted)
			last, from = end, end
		}
		if last > 0 {
			b.WriteString(text[last:])
			text = b.String()
		}
	}
	return text
}

// Redact redacts every known value in text (a log line, a record's reason).
func (k *Known) Redact(text string) string {
	return redactor(k.values(nil)).Scrub(text)
}

// Apply is c with every known value redacted wherever it is: a cassette
// written before the run knew them. A known value that is a whole segment
// of c's paths is an identifier there, written as {{id:N}}, as Scrubbed
// writes one. It says whether anything changed.
func (k *Known) Apply(c *Cassette) (*Cassette, bool) {
	out := *c
	out.Interactions = append([]Interaction(nil), c.Interactions...)
	if c.Vars != nil {
		out.Vars = make(map[string]any, len(c.Vars))
		for n, v := range c.Vars {
			out.Vars[n] = v
		}
	}
	keep := identifiers(out.Interactions)
	finish(&out, keep, k)
	return &out, !sameCassette(c, &out)
}

func sameCassette(a, b *Cassette) bool {
	if a.Target != b.Target || len(a.Interactions) != len(b.Interactions) || len(a.Vars) != len(b.Vars) {
		return false
	}
	for i := range a.Interactions {
		if a.Interactions[i] != b.Interactions[i] {
			return false
		}
	}
	for n, v := range a.Vars {
		if fmt.Sprint(b.Vars[n]) != fmt.Sprint(v) {
			return false
		}
	}
	return true
}

// ScrubbedWith is Scrubbed, and then: every string under a JSON field named
// as one of fields (a kind's secret outputs, and what it names to redact)
// redacted, and every value redaction took out whole, here or earlier in
// the run (known: nil for this cassette alone), redacted wherever else it
// is. Known learns this cassette's values.
//
// A cassette that was scrubbed already (a committed one) is redacted again
// by today's rules: ScrubbedWith(NewScrubber(), fields, known) is what
// cmd/rescrub does.
func (c *Cassette) ScrubbedWith(s *Scrubber, fields []string, known *Known) *Cassette {
	out := &Cassette{Recorded: c.Recorded, Target: s.Scrub(c.Target), Interactions: make([]Interaction, len(c.Interactions))}
	for i, in := range c.Interactions {
		in.Path = s.Scrub(in.Path)
		in.Body = s.Scrub(in.Body)
		in.Response = s.Scrub(in.Response)
		in.Link = s.Scrub(in.Link)
		out.Interactions[i] = in
	}
	if known == nil {
		known = NewKnown()
	}
	// The identities it holds, before redaction takes some of them (a
	// login redacted in a person object is still in a path).
	known.identify(&Cassette{Target: out.Target, Interactions: out.Interactions, Vars: scrubbedVars(s, c.Vars)})
	keep := identifiers(out.Interactions)
	for i := range out.Interactions {
		in := &out.Interactions[i]
		in.Path = redactPathExcept(in.Path, keep)
		in.Body = redactFields(redactExcept(in.Body, keep), fields, keep)
		in.Response = redactFields(redactExcept(in.Response, keep), fields, keep)
		in.Link = redactExcept(in.Link, keep)
	}
	if c.Vars != nil {
		out.Vars = map[string]any{}
		for k, v := range c.Vars {
			if str, ok := v.(string); ok {
				str = s.Scrub(str)
				if secretName(k) && looksSecret(k, str) || shaped(str) {
					if keep.keeps(str) {
						keep.note(str)
					} else {
						keep.took(str)
						str = Redacted
					}
				}
				v = redactText(str, keep)
			}
			out.Vars[k] = v
		}
	}
	finish(out, keep, known)
	return out
}

// scrubbedVars is vars with each string scrubbed by s.
func scrubbedVars(s *Scrubber, vars map[string]any) map[string]any {
	if vars == nil {
		return nil
	}
	out := make(map[string]any, len(vars))
	for k, v := range vars {
		if str, ok := v.(string); ok {
			v = s.Scrub(str)
		}
		out[k] = v
	}
	return out
}

// finish redacts every known value (keep's found ones added) wherever it
// is in out, then writes each secret that is also an ID (keep's ids) as
// {{id:N}}, everywhere it is, not as the value: a replay fills it with a
// stand-in. New IDs are numbered after any out has already.
func finish(out *Cassette, keep *keeper, known *Known) {
	known.add(keep.found)
	if r := redactor(known.values(keep)); r != nil {
		out.Target = r.Scrub(out.Target)
		for i := range out.Interactions {
			in := &out.Interactions[i]
			in.Path, in.Body, in.Response, in.Link = r.Scrub(in.Path), r.Scrub(in.Body), r.Scrub(in.Response), r.Scrub(in.Link)
		}
		for k, v := range out.Vars {
			if str, ok := v.(string); ok {
				out.Vars[k] = r.Scrub(str)
			}
		}
	}
	if len(keep.ids) == 0 {
		return
	}
	ids := make([]string, 0, len(keep.ids))
	for v := range keep.ids {
		ids = append(ids, v)
	}
	sort.Strings(ids)
	first := out.maxID() + 1
	idS := NewScrubber()
	for i, v := range ids {
		idS.AddSecret(v, fmt.Sprintf("{{id:%d}}", first+i))
	}
	out.Target = idS.Scrub(out.Target)
	for i := range out.Interactions {
		in := &out.Interactions[i]
		in.Path, in.Body, in.Response, in.Link = idS.Scrub(in.Path), idS.Scrub(in.Body), idS.Scrub(in.Response), idS.Scrub(in.Link)
	}
	for k, v := range out.Vars {
		if str, ok := v.(string); ok {
			out.Vars[k] = idS.Scrub(str)
		}
	}
}

var idPlaceholderRE = regexp.MustCompile(`\{\{id:(\d+)`)

// maxID is the highest N of the {{id:N}} placeholders c holds (0: none).
func (c *Cassette) maxID() int {
	n := 0
	see := func(s string) {
		for _, m := range idPlaceholderRE.FindAllStringSubmatch(s, -1) {
			if i, err := strconv.Atoi(m[1]); err == nil && i > n {
				n = i
			}
		}
	}
	see(c.Target)
	for _, in := range c.Interactions {
		see(in.Path)
		see(in.Body)
		see(in.Response)
	}
	for _, v := range c.Vars {
		if s, ok := v.(string); ok {
			see(s)
		}
	}
	return n
}
