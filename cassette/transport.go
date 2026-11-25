// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package cassette

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"sync"
)

// relPath is a request's path and query relative to base (a base URL that
// may have a path of its own: https://api.cloudflare.com/client/v4).
func relPath(base string, req *http.Request) string {
	u := req.URL.String()
	if base != "" && strings.HasPrefix(u, base) {
		return strings.TrimPrefix(u, base)
	}
	p := req.URL.EscapedPath()
	if req.URL.RawQuery != "" {
		p += "?" + req.URL.RawQuery
	}
	return p
}

func readBody(req *http.Request) (string, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return "", nil
	}
	b, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return "", err
	}
	req.Body = io.NopCloser(bytes.NewReader(b))
	return string(b), nil
}

func isMultipart(req *http.Request) bool {
	return strings.HasPrefix(req.Header.Get("Content-Type"), "multipart/")
}

// Recorder is an http.RoundTripper that sends every request on, and keeps
// it and its response in the current segment. Request headers are never
// kept.
type Recorder struct {
	// Next sends the requests (http.DefaultTransport if nil).
	Next http.RoundTripper
	// Base is the connection's base URL; paths are kept relative to it.
	Base string

	mu  sync.Mutex
	cur []Interaction
	on  bool
}

// Start begins a segment, dropping anything not yet taken.
func (r *Recorder) Start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cur, r.on = nil, true
}

// Peek is what the segment has recorded so far.
func (r *Recorder) Peek() []Interaction {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Interaction(nil), r.cur...)
}

// Stop ends the segment and returns what it recorded; calls until the next
// Start are sent but not kept (a sweep, a version call).
func (r *Recorder) Stop() []Interaction {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.cur
	r.cur, r.on = nil, false
	return out
}

// RoundTrip implements http.RoundTripper.
func (r *Recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	next := r.Next
	if next == nil {
		next = http.DefaultTransport
	}
	r.mu.Lock()
	on := r.on
	r.mu.Unlock()
	if !on {
		return next.RoundTrip(req)
	}
	in := Interaction{Method: req.Method, Path: relPath(r.Base, req), Multipart: isMultipart(req)}
	body, err := readBody(req)
	if err != nil {
		return nil, err
	}
	if !in.Multipart {
		in.Body = body
	}
	resp, err := next.RoundTrip(req)
	if err != nil {
		return nil, err // a transport error is not an answer; nothing to replay
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	in.Status, in.ContentType, in.Response = resp.StatusCode, resp.Header.Get("Content-Type"), string(data)
	in.Link = resp.Header.Get("Link")
	r.mu.Lock()
	if r.on {
		r.cur = append(r.cur, in)
	}
	r.mu.Unlock()
	return resp, nil
}

// Player is an http.RoundTripper that answers from a cassette, in order,
// and never reaches the network. A request that is not the next recorded
// one is answered 599 and kept in Err, which names the difference.
type Player struct {
	// Base is the base URL the replay's clients use.
	Base string
	// Values fills the cassette's placeholders.
	Values *Scrubber
	// AnyBody: request bodies are not compared, only methods and paths.
	AnyBody bool

	mu   sync.Mutex
	c    *Cassette
	next int
	err  error
	keep *keeper           // whole segments of the cassette's paths: identifiers, never redacted
	sent map[string]string // secrets the replay sent, by field name: refilled in responses
	// sentAt: values the replay sent that redaction removes (a person's
	// name), by JSON path: refilled in responses at the same path
	sentAt map[string]string
}

// Load starts playing c from its first interaction.
func (p *Player) Load(c *Cassette) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.c, p.next, p.err, p.keep, p.sent, p.sentAt = c, 0, nil, nil, map[string]string{}, map[string]string{}
	if c != nil {
		p.keep = identifiers(c.Interactions)
		// {{id:N}}: a secret that was also an ID; the replay's own stand-in.
		if p.Values == nil {
			p.Values = NewScrubber()
		}
		for _, ph := range c.Placeholders() {
			if n, ok := strings.CutPrefix(strings.Trim(ph, "{}"), "id:"); ok && !strings.Contains(n, "|") {
				p.Values.AddSecret("replay-id-"+n, ph)
			}
		}
	}
}

// Err is the first mismatch, if any.
func (p *Player) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Left is how many recorded interactions were never asked for.
func (p *Player) Left() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.c == nil {
		return 0
	}
	return len(p.c.Interactions) - p.next
}

// Next is the next recorded interaction, nil at the end.
func (p *Player) Next() *Interaction {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.c == nil || p.next >= len(p.c.Interactions) {
		return nil
	}
	in := p.c.Interactions[p.next]
	return &in
}

// RoundTrip implements http.RoundTripper.
func (p *Player) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := readBody(req)
	if err != nil {
		return nil, err
	}
	got := Interaction{Method: req.Method, Path: relPath(p.Base, req), Body: body, Multipart: isMultipart(req)}
	p.mu.Lock()
	defer p.mu.Unlock()
	fail := func(format string, args ...any) (*http.Response, error) {
		if p.err == nil {
			p.err = fmt.Errorf(format, args...)
		}
		return respond(req, 599, "text/plain", "cassette: "+p.err.Error()), nil
	}
	if p.err != nil {
		return fail("")
	}
	if p.c == nil || p.next >= len(p.c.Interactions) {
		n := 0
		if p.c != nil {
			n = len(p.c.Interactions)
		}
		return fail("request %d, %s %s, is past the end of the cassette (%d recorded)", n+1, got.Method, got.Path, n)
	}
	want := p.c.Interactions[p.next]
	n := p.next + 1
	p.next++
	// Compared as recorded: the request scrubbed with the replay's values
	// (so each credential is its placeholder, compared exactly) and
	// redacted by the same rules. A value the recording has redacted is not
	// compared: whatever the replay sends there matches it (a value the
	// recording redacted because it was redacted elsewhere in the run).
	path := redactPathExcept(p.Values.Scrub(got.Path), p.keep)
	// The recording redacted again by today's rules, so a rule that
	// redacts more does not break every cassette recorded before it.
	wantPath := redactPathExcept(want.Path, p.keep)
	if want.Method != got.Method || !samePath(withoutEncodings(wantPath), withoutEncodings(path)) {
		return fail("request %d: %s %s, recorded %s %s", n, got.Method, path, want.Method, wantPath)
	}
	if want.Multipart != got.Multipart {
		return fail("request %d (%s %s): multipart %v, recorded %v", n, got.Method, path, got.Multipart, want.Multipart)
	}
	if !p.AnyBody && !got.Multipart {
		wantBody := redactExcept(want.Body, p.keep)
		body := redactExcept(p.Values.Scrub(got.Body), p.keep)
		if !sameBody(withoutEncodings(wantBody), withoutEncodings(body), got.Body, p.sentAt) {
			return fail("request %d (%s %s): body %s, recorded %s", n, got.Method, path, short(body), short(wantBody))
		}
	}
	sentSecrets(got.Body, p.sent)
	sentRedacted(got.Body, redactExcept(got.Body, p.keep), p.sentAt)
	out := respond(req, want.Status, want.ContentType, refill(p.Values.Unscrub(want.Response), p.sent, p.sentAt))
	if want.Link != "" {
		out.Header.Set("Link", p.Values.Unscrub(want.Link))
	}
	return out, nil
}

func respond(req *http.Request, status int, contentType, body string) *http.Response {
	h := http.Header{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// samePath compares a recorded path (a) and a request's (b), with their
// query parameters in any order. Where a has {{redacted}}, b may have
// anything within the segment or parameter.
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	ap, aq, _ := strings.Cut(a, "?")
	bp, bq, _ := strings.Cut(b, "?")
	as, bs := strings.Split(ap, "/"), strings.Split(bp, "/")
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if !samePart(as[i], bs[i]) {
			return false
		}
	}
	as, bs = strings.Split(aq, "&"), strings.Split(bq, "&")
	if len(as) != len(bs) {
		return false
	}
	used := make([]bool, len(bs))
	for _, s := range as { // an exact match first, then one {{redacted}} allows
		found := -1
		for j, t := range bs {
			if !used[j] && s == t {
				found = j
				break
			}
		}
		for j, t := range bs {
			if found < 0 && !used[j] && samePart(s, t) {
				found = j
			}
		}
		if found < 0 {
			return false
		}
		used[found] = true
	}
	return true
}

// samePart: a recorded path segment or query parameter (a) and a request's
// (b): the same, or b has anything where a has {{redacted}}.
func samePart(a, b string) bool {
	if a == b {
		return true
	}
	if !strings.Contains(a, Redacted) {
		return false
	}
	parts := strings.Split(a, Redacted)
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	ok, _ := regexp.MatchString("^"+strings.Join(parts, "[^/&]*")+"$", b)
	return ok
}

// sameBody compares a recorded body (a) and a request's (b): JSON as
// values (key order and spacing aside), anything else as text. A recorded
// {{redacted}} matches whatever b has there; for JSON, what the request
// sent there (in raw, the body as sent) is added to sentAt by its path, so
// a response that redacted it too answers it (refill).
func sameBody(a, b, raw string, sentAt map[string]string) bool {
	if a == b {
		return true
	}
	var av, bv any
	da, db := json.NewDecoder(strings.NewReader(a)), json.NewDecoder(strings.NewReader(b))
	da.UseNumber() // IDs past 2^53 compare digit by digit
	db.UseNumber()
	if da.Decode(&av) != nil || db.Decode(&bv) != nil {
		return strings.Contains(a, Redacted) && sameText(a, b)
	}
	if !strings.Contains(a, Redacted) {
		return reflect.DeepEqual(av, bv)
	}
	var rv any
	dr := json.NewDecoder(strings.NewReader(raw))
	dr.UseNumber()
	_ = dr.Decode(&rv)
	seen := map[string]string{}
	var same func(a, b, r any, at string) bool
	same = func(a, b, r any, at string) bool {
		if a == Redacted {
			if s, ok := r.(string); ok && s != Redacted {
				seen[at] = s
			}
			return b != nil
		}
		switch x := a.(type) {
		case map[string]any:
			y, ok := b.(map[string]any)
			if !ok || len(x) != len(y) {
				return false
			}
			rm, _ := r.(map[string]any)
			for k, e := range x {
				f, ok := y[k]
				if !ok || !same(e, f, rm[k], at+"/"+k) {
					return false
				}
			}
			return true
		case []any:
			y, ok := b.([]any)
			if !ok || len(x) != len(y) {
				return false
			}
			rl, _ := r.([]any)
			for i := range x {
				var ri any
				if i < len(rl) {
					ri = rl[i]
				}
				if !same(x[i], y[i], ri, at+"/[]") {
					return false
				}
			}
			return true
		}
		return reflect.DeepEqual(a, b)
	}
	if !same(av, bv, rv, "") {
		return false
	}
	for k, v := range seen {
		sentAt[k] = v
	}
	return true
}

// sameText: text b is a recorded text a, with anything where a has
// {{redacted}}.
func sameText(a, b string) bool {
	parts := strings.Split(a, Redacted)
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	ok, _ := regexp.MatchString("(?s)^"+strings.Join(parts, ".*?")+"$", b)
	return ok
}

func short(s string) string {
	if s == "" {
		return "(none)"
	}
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
