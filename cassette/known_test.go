// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package cassette

import (
	"net/http"
	"strings"
	"testing"
)

func cassetteText(c *Cassette) string {
	var b strings.Builder
	b.WriteString(c.Target)
	for _, in := range c.Interactions {
		b.WriteString("\n" + in.Path + "\n" + in.Body + "\n" + in.Response + "\n" + in.Link)
	}
	for _, v := range c.Vars {
		if s, ok := v.(string); ok {
			b.WriteString("\n" + s)
		}
	}
	return b.String()
}

// Vault's AppRole SecretID: its accessor is redacted under its name, and so
// it is in the 404 that quotes it, in a later call's query and in a saved
// var, with the same placeholder.
func TestRedactedValueRedactedEverywhere(t *testing.T) {
	const acc = "711b62c9-37e4-409b-4477-cd80f74ae768"
	c := (&Cassette{Vars: map[string]any{"last": acc}, Interactions: []Interaction{
		{Method: "POST", Path: "/auth/a/role/r/secret-id", Status: 200, Response: `{"data":{"secret_id":"0a1b2c3d-4e5f","secret_id_accessor":"` + acc + `"}}`},
		{Method: "POST", Path: "/auth/a/role/r/secret-id-accessor/lookup", Body: `{"secret_id_accessor":"` + acc + `"}`, Status: 404,
			Response: `{"data":{"error":"failed to find accessor entry for secret_id_accessor: \"` + acc + `\""}}`},
		{Method: "GET", Path: "/audit?ref=" + acc, Body: `{"note":"` + acc + `"}`, Status: 400, ContentType: "text/plain", Response: "no entry " + acc},
	}}).Scrubbed(NewScrubber())
	all := cassetteText(c)
	if strings.Contains(all, acc) || strings.Contains(all, "711b62c9") {
		t.Fatalf("the accessor is left:\n%s", all)
	}
	if got := c.Interactions[2].Path; got != "/audit?ref="+Redacted {
		t.Errorf("path %s", got)
	}
	if c.Vars["last"] != Redacted {
		t.Errorf("var %v", c.Vars["last"])
	}

	// A replay reads {{redacted}} and sends it back: its calls are the ones
	// recorded, the query's escaped placeholder included.
	p := &Player{Base: "http://r", Values: NewScrubber()}
	p.Load(c)
	hc := &http.Client{Transport: p}
	do(t, hc, "POST", "http://r/auth/a/role/r/secret-id", "")
	do(t, hc, "POST", "http://r/auth/a/role/r/secret-id-accessor/lookup", `{"secret_id_accessor":"{{redacted}}"}`)
	if st, _ := do(t, hc, "GET", "http://r/audit?ref=%7B%7Bredacted%7D%7D", `{"note":"{{redacted}}"}`); st != 400 || p.Err() != nil {
		t.Fatalf("replay: %d %v", st, p.Err())
	}
}

// A kind's named fields (secretOutputs, redact) are redacted wherever their
// values are: Vault's token accessor, in its read's and delete's bodies and
// in an error.
func TestScrubbedWithFields(t *testing.T) {
	const acc = "n4xYctRALiA53ntOMjMQEc5N"
	c := (&Cassette{Interactions: []Interaction{
		{Method: "POST", Path: "/auth/token/create", Body: `{"ttl":"1h"}`, Status: 200, Response: `{"auth":{"accessor":"` + acc + `","client_token":"hvs.CAESIabcdefgh"}}`},
		{Method: "POST", Path: "/auth/token/lookup-accessor", Body: `{"accessor":"` + acc + `"}`, Status: 200, Response: `{"data":{"accessor":"` + acc + `","id":""}}`},
		{Method: "POST", Path: "/auth/token/lookup-accessor", Body: `{"accessor":"` + acc + `"}`, Status: 400, ContentType: "text/plain", Response: "invalid accessor " + acc},
	}}).ScrubbedWith(NewScrubber(), []string{"auth.client_token", "accessor"}, nil)
	if all := cassetteText(c); strings.Contains(all, acc) || strings.Contains(all, "hvs.") {
		t.Fatalf("left:\n%s", all)
	}
	if c.Interactions[1].Body != `{"accessor":"{{redacted}}"}` {
		t.Errorf("body %s", c.Interactions[1].Body)
	}
	p := &Player{Base: "http://r", Values: NewScrubber()}
	p.Load(c)
	hc := &http.Client{Transport: p}
	do(t, hc, "POST", "http://r/auth/token/create", `{"ttl":"1h"}`)
	do(t, hc, "POST", "http://r/auth/token/lookup-accessor", `{"accessor":"{{redacted}}"}`)
	if st, _ := do(t, hc, "POST", "http://r/auth/token/lookup-accessor", `{"accessor":"{{redacted}}"}`); st != 400 || p.Err() != nil {
		t.Fatalf("replay: %d %v", st, p.Err())
	}

	// Without the field named, an accessor is not a secret: nothing changes.
	plain := (&Cassette{Interactions: []Interaction{{Method: "POST", Path: "/x", Body: `{"accessor":"` + acc + `"}`, Status: 204}}}).Scrubbed(NewScrubber())
	if !strings.Contains(plain.Interactions[0].Body, acc) {
		t.Errorf("redacted unasked: %s", plain.Interactions[0].Body)
	}
}

// A named field whose value is also an ID in a path is written {{id:N}}
// everywhere, so its paths still match, not redacted in bodies and left in
// paths.
func TestFieldThatIsAnID(t *testing.T) {
	const key = "k3y-0123456789"
	c := (&Cassette{Interactions: []Interaction{
		{Method: "POST", Path: "/keys", Status: 201, Response: `{"key":"` + key + `"}`},
		{Method: "GET", Path: "/keys/" + key, Status: 200, Response: `{"key":"` + key + `"}`},
	}}).ScrubbedWith(NewScrubber(), []string{"key"}, nil)
	if all := cassetteText(c); strings.Contains(all, key) || c.Interactions[1].Path != "/keys/{{id:1}}" || c.Interactions[0].Response != `{"key":"{{id:1}}"}` {
		t.Fatalf("got:\n%s", all)
	}
}

// What one cassette's redaction learns is applied to the others of the
// run: the before calls' cassette, written first, and a later kind's.
func TestKnownAcrossCassettes(t *testing.T) {
	const sec = "s3cr3t-value-123"
	known := NewKnown()
	before := (&Cassette{Interactions: []Interaction{{Method: "GET", Path: "/x", Status: 200, Response: `{"note":"` + sec + `"}`}}}).ScrubbedWith(NewScrubber(), nil, known)
	if !strings.Contains(before.Interactions[0].Response, sec) {
		t.Fatal("redacted before anything said it was a secret")
	}
	kind := (&Cassette{Interactions: []Interaction{{Method: "POST", Path: "/y", Body: `{"password":"` + sec + `"}`, Status: 201, Response: `{"echo":"` + sec + `"}`}}}).ScrubbedWith(NewScrubber(), nil, known)
	if strings.Contains(cassetteText(kind), sec) || known.Len() != 1 {
		t.Fatalf("kind: %s (%d known)", cassetteText(kind), known.Len())
	}
	after := (&Cassette{Interactions: []Interaction{{Method: "DELETE", Path: "/y?v=" + sec, Status: 204}}}).ScrubbedWith(NewScrubber(), nil, known)
	if after.Interactions[0].Path != "/y?v="+Redacted {
		t.Errorf("a later cassette: %s", after.Interactions[0].Path)
	}
	out, changed := known.Apply(before)
	if !changed || out.Interactions[0].Response != `{"note":"{{redacted}}"}` || before.Interactions[0].Response == out.Interactions[0].Response {
		t.Errorf("applied to an earlier one: %v %s (input %s)", changed, out.Interactions[0].Response, before.Interactions[0].Response)
	}
	if _, changed := known.Apply(out); changed {
		t.Error("applied twice, changed twice")
	}
	if got := known.Redact("failed: " + sec); got != "failed: "+Redacted {
		t.Errorf("text %q", got)
	}
}

// A known value that is a whole path segment in a cassette is an ID there:
// {{id:N}}, numbered after the IDs the cassette has.
func TestKnownIDNumberedAfterExisting(t *testing.T) {
	const sec = "token-abcdef12345"
	known := NewKnown()
	known.add(map[string]bool{sec: true})
	c := &Cassette{Vars: map[string]any{"t": sec}, Interactions: []Interaction{
		{Method: "GET", Path: "/a/{{id:1}}", Status: 200},
		{Method: "GET", Path: "/t/" + sec, Status: 200, Response: `{"t":"` + sec + `"}`},
	}}
	out, _ := known.Apply(c)
	if out.Interactions[1].Path != "/t/{{id:2}}" || out.Interactions[1].Response != `{"t":"{{id:2}}"}` || out.Vars["t"] != "{{id:2}}" || out.Interactions[0].Path != "/a/{{id:1}}" {
		t.Fatalf("got %+v %v", out.Interactions, out.Vars)
	}
}

// A secret the replay sends under its name, and somewhere else too (a
// query, a field of another name), is compared as the recording has it:
// redacted in both places.
func TestPlayerRedactsWhatItSends(t *testing.T) {
	const pw = "Corr3ct-Horse-9"
	c := (&Cassette{Interactions: []Interaction{
		{Method: "POST", Path: "/users", Body: `{"password":"` + pw + `","hint":"` + pw + `"}`, Status: 201, Response: `{}`},
		{Method: "GET", Path: "/check?p=" + pw, Status: 200, Response: `{}`},
	}}).Scrubbed(NewScrubber())
	if all := cassetteText(c); strings.Contains(all, pw) {
		t.Fatalf("left:\n%s", all)
	}
	for _, sent := range []string{pw, "An0ther-Pass-77"} { // the recording's value, or another
		p := &Player{Base: "http://r", Values: NewScrubber()}
		p.Load(c)
		hc := &http.Client{Transport: p}
		do(t, hc, "POST", "http://r/users", `{"password":"`+sent+`","hint":"`+sent+`"}`)
		if st, _ := do(t, hc, "GET", "http://r/check?p="+sent, ""); st != 200 || p.Err() != nil {
			t.Errorf("sent %s: %d %v", sent, st, p.Err())
		}
	}
	// A recording made before: the value in clear where it was not
	// redacted then. Its replay still matches.
	old := &Cassette{Interactions: []Interaction{
		{Method: "POST", Path: "/users", Body: `{"password":"{{redacted}}","hint":"` + pw + `"}`, Status: 201, Response: `{}`},
		{Method: "GET", Path: "/check?p=" + pw, Status: 200, Response: `{}`},
	}}
	p := &Player{Base: "http://r", Values: NewScrubber()}
	p.Load(old)
	hc := &http.Client{Transport: p}
	do(t, hc, "POST", "http://r/users", `{"password":"`+pw+`","hint":"`+pw+`"}`)
	if st, _ := do(t, hc, "GET", "http://r/check?p="+pw, ""); st != 200 || p.Err() != nil {
		t.Errorf("an older recording: %d %v", st, p.Err())
	}
}

// A secret's value written after its name in text is redacted; words,
// placeholders, URLs, enums and other names' values are not.
func TestRedactNamedInText(t *testing.T) {
	for in, want := range map[string]string{
		`failed to find accessor entry for secret_id_accessor: "711b62c9-37e4-409b"`: `failed to find accessor entry for secret_id_accessor: "{{redacted}}"`,
		`bad request: api_key=AbC123xyz789 rejected`:                                 `bad request: api_key={{redacted}} rejected`,
		`client_secret: must be 32 characters`:                                       `client_secret: must be 32 characters`,
		`token_type: service`:                                                        `token_type: service`,
		`password: {{redacted}}`:                                                     `password: {{redacted}}`,
		`secret_url: https://example.com/a1b2c3d4`:                                   `secret_url: https://example.com/a1b2c3d4`,
		`request_id: 0a1b2c3d-4e5f-6789`:                                             `request_id: 0a1b2c3d-4e5f-6789`,
	} {
		if got := Redact(in); got != want {
			t.Errorf("%s\n got %s\nwant %s", in, got, want)
		}
	}
	// In a JSON field's text too, and the value is redacted elsewhere then.
	c := (&Cassette{Interactions: []Interaction{
		{Method: "GET", Path: "/a", Status: 404, Response: `{"errors":["no such secret_id: 9z8y7x6w5v4u"]}`},
		{Method: "GET", Path: "/b", Status: 200, Response: `{"last":"9z8y7x6w5v4u"}`},
	}}).Scrubbed(NewScrubber())
	if all := cassetteText(c); strings.Contains(all, "9z8y7x6w5v4u") {
		t.Errorf("left:\n%s", all)
	}
}

// Scrubbing again what was scrubbed (cmd/rescrub on a committed cassette)
// changes nothing more.
func TestScrubbedWithIdempotent(t *testing.T) {
	c := (&Cassette{Vars: map[string]any{"k": "v"}, Interactions: []Interaction{
		{Method: "POST", Path: "/t", Body: `{"secret":"abcdefgh1234"}`, Status: 201, Response: `{"id":"tok-1234567890","api_key":"zyxw98765432"}`},
		{Method: "GET", Path: "/t/tok-1234567890", Status: 200, Response: `{"msg":"key zyxw98765432 and abcdefgh1234"}`},
	}}).ScrubbedWith(NewScrubber(), []string{"id"}, nil)
	again := c.ScrubbedWith(NewScrubber(), []string{"id"}, nil)
	if !sameCassette(c, again) {
		t.Errorf("once:\n%s\ntwice:\n%s", cassetteText(c), cassetteText(again))
	}
	if strings.Contains(cassetteText(c), "zyxw98765432") || strings.Contains(cassetteText(c), "abcdefgh1234") {
		t.Errorf("left:\n%s", cassetteText(c))
	}
}

// A known value is redacted where it is a whole word or number, not inside
// a longer one.
func TestKnownWholeWords(t *testing.T) {
	r := redactor([]string{"270000007", "s3cr3t-abc"}, nil)
	for in, want := range map[string]string{
		`{"id":"270000007"}`:            `{"id":"{{redacted}}"}`,
		`/x/2700000071/270000007?a=1`:   `/x/2700000071/{{redacted}}?a=1`,
		`key s3cr3t-abc, s3cr3t-abcdef`: `key {{redacted}}, s3cr3t-abcdef`,
		`xs3cr3t-abc.`:                  `xs3cr3t-abc.`,
		`Bearer s3cr3t-abc`:             `Bearer {{redacted}}`,
	} {
		if got := r.Scrub(in); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}

// A value the recording redacted (because it was redacted elsewhere in the
// run) is not compared: the replay's own value there matches, and a
// response that redacted it too is answered with what the replay sent.
// Anything else still is compared.
func TestPlayerDoesNotCompareRedacted(t *testing.T) {
	c := &Cassette{Interactions: []Interaction{
		{Method: "POST", Path: "/handles", Body: `{"data":{"attributes":{"team_id":"{{redacted}}","name":"h"}}}`, Status: 201,
			Response: `{"data":{"id":"1","attributes":{"team_id":"{{redacted}}","name":"h"}}}`},
		{Method: "GET", Path: "/teams/{{redacted}}/handles?team={{redacted}}&page=2", Status: 200, Response: `{}`},
		{Method: "POST", Path: "/form", Body: "team={{redacted}}&x=1", Status: 204},
	}}
	play := func(steps ...[3]string) (*Player, []string) {
		p := &Player{Base: "http://r", Values: NewScrubber()}
		p.Load(c)
		hc := &http.Client{Transport: p}
		var bodies []string
		for _, s := range steps {
			_, b := do(t, hc, s[0], "http://r"+s[1], s[2])
			bodies = append(bodies, b)
		}
		return p, bodies
	}
	p, bodies := play(
		[3]string{"POST", "/handles", `{"data":{"attributes":{"team_id":"0000-1111","name":"h"}}}`},
		[3]string{"GET", "/teams/0000-1111/handles?page=2&team=0000-1111", ""},
		[3]string{"POST", "/form", "team=0000-1111&x=1"},
	)
	if p.Err() != nil || p.Left() != 0 {
		t.Fatalf("replay: %v", p.Err())
	}
	if !strings.Contains(bodies[0], `"team_id":"0000-1111"`) {
		t.Errorf("not refilled: %s", bodies[0])
	}
	for _, bad := range [][3]string{
		{"POST", "/handles", `{"data":{"attributes":{"team_id":"0000-1111","name":"other"}}}`},
		{"POST", "/handles", `{"data":{"attributes":{"name":"h"}}}`},
	} {
		if p, _ := play(bad); p.Err() == nil {
			t.Errorf("%s matched", bad[2])
		}
	}
	p, _ = play([3]string{"POST", "/handles", `{"data":{"attributes":{"team_id":"x","name":"h"}}}`}, [3]string{"GET", "/teams/x/y/handles?page=2&team=x", ""})
	if p.Err() == nil {
		t.Error("a redacted segment matched two")
	}
}
