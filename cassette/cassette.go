// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

// Package cassette records a live run's HTTP calls to a vendor and answers
// them again from the recording, with no network and no credentials.
//
// A live run (kindtest.Live) records each kind's calls to a cassette as it
// passes; a replay (kindtest.Replay) sends the same steps through a Player,
// which answers each request with the recorded response, in order, and
// fails on the first request that is not the one recorded. So a replay is a
// test of the runtime against what the vendor really answered: quirks the
// fake does not know (a field read back in another form, a replica that
// lags a write) included. The live run that re-records is what keeps it
// current; a changed cassette in its diff is the API having changed.
//
// Nothing secret is written: request headers (where the credentials go) are
// never recorded, and every credential value, the base URL and the run's
// names are replaced by placeholders ({{cred:KEY}}, {{baseURL}}, {{run}},
// {{name}}) before a cassette is written. Values a run saves from a
// response ({{var:NAME}}) are kept with the kind's cassette as they were
// when it was recorded, so a kind replays as recorded even when the run's
// before calls were re-recorded since.
package cassette

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var placeholderRE = regexp.MustCompile(`\{\{[A-Za-z_]+(?::[A-Za-z0-9_.|-]+)?\}\}`)

// Interaction is one request and the response the API gave it.
type Interaction struct {
	Method string `json:"method"`
	// Path is relative to the connection's base URL, with the query.
	Path string `json:"path"`
	// Body is the request body as sent (placeholders in it); empty for none.
	Body string `json:"body,omitempty"`
	// Multipart: the body was multipart/form-data, whose boundary differs
	// on every request, so it is not compared.
	Multipart bool `json:"multipart,omitempty"`
	Status    int  `json:"status"`
	// ContentType is the response's.
	ContentType string `json:"contentType,omitempty"`
	Response    string `json:"response,omitempty"`
	// Link is the response's Link header: where the next page of a list is
	// (GitHub's, GitLab's rel="next"), so a replay pages as the run did.
	Link string `json:"link,omitempty"`
}

// Cassette is one segment of a live run: one kind, or the run's before or
// after calls.
type Cassette struct {
	// Recorded is when (RFC 3339, UTC): a replay's clock, so {{date:N}}
	// fills the date the recording was made with.
	Recorded string `json:"recorded"`
	// Target is what the run tested against (live.yaml's target, the API's
	// version).
	Target string `json:"target,omitempty"`
	// Vars are the run's saved values ({{var:NAME}}) as the kind saw them.
	Vars         map[string]any `json:"vars,omitempty"`
	Interactions []Interaction  `json:"interactions"`
}

// Load reads a cassette; a missing file is (nil, nil).
func Load(path string) (*Cassette, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c Cassette
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // a saved ID of 20815322 stays 20815322, not 2.0815322e+07
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for k, v := range c.Vars {
		if n, ok := v.(json.Number); ok {
			if i, err := n.Int64(); err == nil {
				c.Vars[k] = i
			} else if f, err := n.Float64(); err == nil {
				c.Vars[k] = f
			}
		}
	}
	return &c, nil
}

// Save writes a cassette, indented, so a re-recording diffs line by line.
func (c *Cassette) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// minScrub is the shortest value Scrubber replaces: a shorter one ("1",
// "on") would be found in text it has nothing to do with. A credential that
// short is not scrubbed; Leaks does not look for it either.
const minScrub = 4

// Scrubber swaps known values for placeholders (when recording) and
// placeholders for values (when replaying). Redact is the other half:
// what the run does not know by value.
type Scrubber struct {
	byValue map[string]string // value → placeholder
	secret  map[string]bool   // values added with AddSecret: scrubbed before any other
}

// NewScrubber returns an empty Scrubber.
func NewScrubber() *Scrubber {
	return &Scrubber{byValue: map[string]string{}, secret: map[string]bool{}}
}

// Add says value stands for placeholder. Values shorter than minScrub are
// ignored. The first placeholder added for a value wins.
func (s *Scrubber) Add(value, placeholder string) {
	if s == nil || len(value) < minScrub {
		return
	}
	if _, ok := s.byValue[value]; !ok {
		s.byValue[value] = placeholder
	}
}

// AddSecret is Add for a secret, in every encoding it may appear in
// (URL-encoded, JSON-escaped, base64), each with its own placeholder
// ({{cred:apiKey|url}}), so a replay fills each the same way.
func (s *Scrubber) AddSecret(value, placeholder string) {
	if s == nil {
		return
	}
	s.Add(value, placeholder)
	if len(value) >= minScrub {
		s.secret[value] = true
	}
	base := strings.TrimSuffix(placeholder, "}}")
	for _, e := range encoded(value) {
		s.Add(e[1], base+"|"+e[0]+"}}")
	}
}

// Scrub replaces every value with its placeholder. Where two values
// overlap, the longer wins (a kind's name, hfl-live-...-01, over the run's
// name it starts with; a long secret over a short value inside it).
func (s *Scrubber) Scrub(text string) string {
	if s == nil || len(s.byValue) == 0 || text == "" {
		return text
	}
	type hit struct{ at, end int }
	vals := make([]string, 0, len(s.byValue))
	for v := range s.byValue {
		vals = append(vals, v)
	}
	// Secrets first, so no other value (the base URL) can take part of one
	// and leave its tail; then the longest.
	sort.Slice(vals, func(i, j int) bool {
		if s.secret[vals[i]] != s.secret[vals[j]] {
			return s.secret[vals[i]]
		}
		if len(vals[i]) != len(vals[j]) {
			return len(vals[i]) > len(vals[j])
		}
		return vals[i] < vals[j]
	})
	taken := make([]bool, len(text))
	var hits []hit
	ph := map[int]string{}
	for _, v := range vals {
		for from := 0; ; {
			i := strings.Index(text[from:], v)
			if i < 0 {
				break
			}
			at, end := from+i, from+i+len(v)
			free := true
			for j := at; j < end; j++ {
				if taken[j] {
					free = false
					break
				}
			}
			if free {
				for j := at; j < end; j++ {
					taken[j] = true
				}
				hits = append(hits, hit{at, end})
				ph[at] = s.byValue[v]
			}
			from = at + 1
		}
	}
	if len(hits) == 0 {
		return text
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].at < hits[j].at })
	var b strings.Builder
	last := 0
	for _, h := range hits {
		b.WriteString(text[last:h.at])
		b.WriteString(ph[h.at])
		last = h.end
	}
	b.WriteString(text[last:])
	return b.String()
}

// Unscrub replaces every placeholder with its value. A placeholder with no
// value is left as it is.
func (s *Scrubber) Unscrub(text string) string {
	if s == nil || !strings.Contains(text, "{{") {
		return text
	}
	byPlaceholder := make(map[string]string, len(s.byValue))
	for v, p := range s.byValue {
		byPlaceholder[p] = v
	}
	return placeholderRE.ReplaceAllStringFunc(text, func(p string) string {
		if v, ok := byPlaceholder[p]; ok {
			return v
		}
		// {{cred:KEY|json}} with no value of its own: a replay's stand-in
		// for KEY reads the same in that encoding, so it was never added.
		if base, enc, ok := strings.Cut(strings.TrimSuffix(p, "}}"), "|"); ok {
			if v, ok := byPlaceholder[base+"}}"]; ok {
				for _, e := range encoded(v) {
					if e[0] == enc {
						return e[1]
					}
				}
				return v
			}
		}
		return p
	})
}

// Scrubbed returns c as it may be written: every known value replaced by its
// placeholder, then everything Redact finds redacted (values that are whole
// path segments, identifiers, aside), in paths, bodies, responses and vars,
// and each value it redacted whole redacted wherever else it is too. See
// ScrubbedWith.
func (c *Cassette) Scrubbed(s *Scrubber) *Cassette { return c.ScrubbedWith(s, nil, nil) }

// Placeholders lists the distinct {{...}} placeholders in a cassette, so a
// replay can give each a value.
func (c *Cassette) Placeholders() []string {
	seen := map[string]bool{}
	for _, in := range c.Interactions {
		for _, s := range []string{in.Path, in.Body, in.Response} {
			for _, m := range placeholderRE.FindAllString(s, -1) {
				seen[m] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// encRE is a placeholder's escaping ({{cred:KEY|json}}): whether a value has
// an escaped form depends on the value (a PEM key's newlines; a stand-in has
// none), so comparisons leave it out. A base64 form always differs from the
// value, so {{cred:KEY|b64}} is compared as it is: a credential sent raw
// where it was sent encoded fails.
var encRE = regexp.MustCompile(`\|(url|path|json)\}\}`)

func withoutEncodings(s string) string { return encRE.ReplaceAllString(s, "}}") }
