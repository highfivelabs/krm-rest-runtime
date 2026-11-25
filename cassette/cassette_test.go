// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package cassette

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func do(t *testing.T, c *http.Client, method, url, body string, header ...string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rd)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Recorded against a server, scrubbed, saved, loaded, and replayed with
// other values for the placeholders and no server at all.
func TestRecordScrubReplay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sekrit-token" {
			w.WriteHeader(401)
			return
		}
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case "POST":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":"x1","account":"acct-1234","echo":` + string(b) + `}`))
		case "DELETE":
			w.WriteHeader(204)
		default:
			_, _ = w.Write([]byte(`{"id":"x1","name":"hfl-live-2611200000-ab12-01"}`))
		}
	}))
	defer srv.Close()
	base := srv.URL + "/v4"

	rec := &Recorder{Base: base}
	c := &http.Client{Transport: rec}
	auth := []string{"Authorization", "Bearer sekrit-token"}
	do(t, c, "GET", base+"/version", "", auth...) // before Start: sent, not kept
	rec.Start()
	do(t, c, "POST", base+"/accounts/acct-1234/things", `{"name":"hfl-live-2611200000-ab12-01"}`, auth...)
	do(t, c, "GET", base+"/accounts/acct-1234/things/x1?b=2&a=1", "", auth...)
	do(t, c, "DELETE", base+"/accounts/acct-1234/things/x1", "", auth...)
	got := rec.Stop()
	if len(got) != 3 {
		t.Fatalf("recorded %d, want 3", len(got))
	}

	s := NewScrubber()
	s.Add("acct-1234", "{{cred:accountId}}")
	s.Add("sekrit-token", "{{cred:token}}")
	s.Add("hfl-live-2611200000-ab12", "{{run}}")
	s.Add("hfl-live-2611200000-ab12-01", "{{name}}")
	s.Add("on", "{{cred:short}}") // too short to scrub
	cas := (&Cassette{Recorded: "2026-11-20T00:00:00Z", Interactions: got}).Scrubbed(s)
	path := filepath.Join(t.TempDir(), "Thing.json")
	if err := cas.Save(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := Load(path)
	for _, in := range raw.Interactions {
		all := in.Path + in.Body + in.Response
		for _, leak := range []string{"acct-1234", "sekrit", "hfl-live-2611200000", "Authorization"} {
			if strings.Contains(all, leak) {
				t.Errorf("cassette keeps %q: %+v", leak, in)
			}
		}
	}
	if !strings.Contains(raw.Interactions[0].Body, "{{name}}") || !strings.Contains(raw.Interactions[0].Path, "{{cred:accountId}}") {
		t.Errorf("not scrubbed to placeholders: %+v", raw.Interactions[0])
	}
	if ph := strings.Join(raw.Placeholders(), " "); ph != "{{cred:accountId}} {{name}}" {
		t.Errorf("placeholders %q", ph)
	}

	// Replay: other values, no server, no credentials.
	v := NewScrubber()
	v.Add("replay-accountId", "{{cred:accountId}}")
	v.Add("hfl-live-0001010000-0000-07", "{{name}}")
	p := &Player{Base: "http://replay.invalid", Values: v}
	p.Load(raw)
	rc := &http.Client{Transport: p}
	st, body := do(t, rc, "POST", "http://replay.invalid/accounts/replay-accountId/things", `{ "name": "hfl-live-0001010000-0000-07" }`)
	if st != 201 || !strings.Contains(body, `"account":"replay-accountId"`) || !strings.Contains(body, "hfl-live-0001010000-0000-07") {
		t.Errorf("replayed POST: %d %s", st, body)
	}
	if st, _ := do(t, rc, "GET", "http://replay.invalid/accounts/replay-accountId/things/x1?a=1&b=2", ""); st != 200 {
		t.Errorf("query in another order: %d (%v)", st, p.Err())
	}
	if p.Left() != 1 {
		t.Errorf("left %d, want 1", p.Left())
	}
	if st, _ := do(t, rc, "DELETE", "http://replay.invalid/accounts/replay-accountId/things/x1", ""); st != 204 || p.Err() != nil || p.Left() != 0 {
		t.Errorf("DELETE %d err %v left %d", st, p.Err(), p.Left())
	}
	if st, _ := do(t, rc, "GET", "http://replay.invalid/extra", ""); st != 599 || p.Err() == nil || !strings.Contains(p.Err().Error(), "past the end") {
		t.Errorf("past the end: %d %v", st, p.Err())
	}
}

// A request other than the recorded one fails, naming the difference, and
// every request after it fails too.
func TestPlayerMismatch(t *testing.T) {
	cas := &Cassette{Interactions: []Interaction{
		{Method: "PATCH", Path: "/things/1", Body: `{"color":"green"}`, Status: 200, Response: `{}`},
		{Method: "GET", Path: "/things/1", Status: 200, Response: `{}`},
	}}
	for name, tc := range map[string]struct{ method, path, body, want string }{
		"method": {"PUT", "/things/1", `{"color":"green"}`, "PUT /things/1, recorded PATCH /things/1"},
		"path":   {"PATCH", "/things/2", `{"color":"green"}`, "recorded PATCH /things/1"},
		"body":   {"PATCH", "/things/1", `{"color":"blue"}`, `body {"color":"blue"}, recorded {"color":"green"}`},
	} {
		t.Run(name, func(t *testing.T) {
			p := &Player{Base: "http://r", Values: NewScrubber()}
			p.Load(cas)
			c := &http.Client{Transport: p}
			if st, _ := do(t, c, tc.method, "http://r"+tc.path, tc.body); st != 599 {
				t.Errorf("status %d", st)
			}
			if p.Err() == nil || !strings.Contains(p.Err().Error(), tc.want) {
				t.Errorf("err %v, want %q", p.Err(), tc.want)
			}
			if st, _ := do(t, c, "GET", "http://r/things/1", ""); st != 599 {
				t.Errorf("after a mismatch: %d", st)
			}
		})
	}
}

// Multipart bodies differ in their boundary every time: not compared.
func TestMultipartNotCompared(t *testing.T) {
	cas := &Cassette{Interactions: []Interaction{{Method: "PUT", Path: "/s", Multipart: true, Status: 200}}}
	p := &Player{Base: "http://r", Values: NewScrubber()}
	p.Load(cas)
	if st, _ := do(t, &http.Client{Transport: p}, "PUT", "http://r/s", "--b1\r\n...", "Content-Type", "multipart/form-data; boundary=b1"); st != 200 {
		t.Errorf("status %d: %v", st, p.Err())
	}
}

// Secret-named fields are redacted at any depth, in camel and snake case;
// other fields, placeholders and non-JSON are left; large IDs keep every digit.
func TestRedact(t *testing.T) {
	in := `{"name":"n","api_key":"cfat_live123456","data":{"attributes":{"clientSecret":"s3cr3t-value"}},"configuration":{"tokenSigningKey":"-----BEGIN PRIVATE KEY-----"},"token_endpoint_auth_method":"client_secret_post","key_id":"3380204578043523366","password":"{{cred:password}}","list":[{"private_key":"pk-9f8e7d"}]}`
	out := Redact(in)
	for _, leak := range []string{"cfat_", "s3cr3t", "BEGIN PRIVATE", "pk-9f8e7d"} {
		if strings.Contains(out, leak) {
			t.Errorf("kept %q: %s", leak, out)
		}
	}
	for _, keep := range []string{`"client_secret_post"`, `"3380204578043523366"`, `"{{cred:password}}"`, `"name":"n"`} {
		if !strings.Contains(out, keep) {
			t.Errorf("lost %s: %s", keep, out)
		}
	}
	nested := Redact(`{"secrets":{"authentication":{"id":"a1","key":"9964976384819da7"}},"result":{"pem":"-----BEGIN RSA PRIVATE KEY-----\nMII"},"public":"-----BEGIN PUBLIC KEY-----"}`)
	if strings.Contains(nested, "9964976384819da7") || strings.Contains(nested, "PRIVATE KEY") || !strings.Contains(nested, "PUBLIC KEY") {
		t.Errorf("nested secrets: %s", nested)
	}
	shaped := Redact(`{"data":{"attributes":{"key":"ddapp_KDFtvYibArDFGQyX","name":"k"}},"parameters":{"routing_key":"R02A26XZ0RJF"},"key":"2W0h4kaKP+mM/2d8EHx"}`)
	if strings.Contains(shaped, "ddapp_") || strings.Contains(shaped, "R02A26") || !strings.Contains(shaped, "2W0h4kaKP") {
		t.Errorf("shaped tokens: %s", shaped)
	}
	if Redact("not json") != "not json" || Redact(`{"a":1}`) != `{"a":1}` {
		t.Error("changed what had nothing to redact")
	}
	if got := Redact(`{"id":20815322123456789,"token":"abcdefgh"}`); !strings.Contains(got, "20815322123456789") {
		t.Errorf("a large number lost digits: %s", got)
	}
}

// A replay matches a request whose secret differs from the recording's: the
// recording keeps only {{redacted}}, and the request is redacted the same way.
func TestPlayerComparesRedacted(t *testing.T) {
	cas := (&Cassette{Interactions: []Interaction{{Method: "POST", Path: "/x", Body: `{"name":"a","api_key":"recorded-secret"}`, Status: 201, Response: `{"id":1,"client_secret":"minted-secret"}`}}}).Scrubbed(NewScrubber())
	if all := cas.Interactions[0].Body + cas.Interactions[0].Response; strings.Contains(all, "recorded-secret") || strings.Contains(all, "minted-secret") {
		t.Fatalf("not redacted: %+v", cas.Interactions[0])
	}
	p := &Player{Base: "http://r", Values: NewScrubber()}
	p.Load(cas)
	if st, _ := do(t, &http.Client{Transport: p}, "POST", "http://r/x", `{"name":"a","api_key":"replay-env-KEY"}`); st != 201 {
		t.Errorf("status %d: %v", st, p.Err())
	}
}

// Saved values keep whole numbers whole, however large, and string vars are
// scrubbed like everything else.
func TestVarsRoundTrip(t *testing.T) {
	s := NewScrubber()
	s.Add("hfl-live-2611202356-cfb2", "{{run}}")
	c := (&Cassette{Vars: map[string]any{"team": int64(20815322), "big": int64(3380204578043523366), "project": "hfl-live-2611202356-cfb2"}}).Scrubbed(s)
	path := filepath.Join(t.TempDir(), "c.json")
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	got, _ := Load(path)
	if got.Vars["team"] != int64(20815322) || got.Vars["big"] != int64(3380204578043523366) || got.Vars["project"] != "{{run}}" {
		t.Errorf("vars %#v", got.Vars)
	}
}

// A secret that is also an ID in later paths stays (it is in the path
// anyway); a secret the API echoes is answered with the replay's own value.
func TestIdentifiersKeptAndEchoesRefilled(t *testing.T) {
	c := (&Cassette{Interactions: []Interaction{
		{Method: "POST", Path: "/embeds", Body: `{"credentials":{"client_secret":"real-client-secret"}}`, Status: 201, Response: `{"token":"tok-abc123456","credentials":{"client_secret":"real-client-secret"}}`},
		{Method: "GET", Path: "/embeds/tok-abc123456", Status: 200, Response: `{"token":"tok-abc123456"}`},
	}}).Scrubbed(NewScrubber())
	all := c.Interactions[0].Response + c.Interactions[1].Path + c.Interactions[1].Response
	if strings.Contains(all, "tok-abc123456") || !strings.Contains(c.Interactions[1].Path, "{{id:1}}") || strings.Contains(c.Interactions[0].Response, "real-client-secret") {
		t.Fatalf("recorded: %+v", c.Interactions)
	}
	p := &Player{Base: "http://r", Values: NewScrubber()}
	p.Load(c)
	hc := &http.Client{Transport: p}
	_, body := do(t, hc, "POST", "http://r/embeds", `{"credentials":{"client_secret":"replay-client"}}`)
	if !strings.Contains(body, `"client_secret":"replay-client"`) || !strings.Contains(body, `"token":"replay-id-1"`) || p.Err() != nil {
		t.Fatalf("replayed: %s (%v)", body, p.Err())
	}
	if st, _ := do(t, hc, "GET", "http://r/embeds/replay-id-1", ""); st != 200 || p.Err() != nil {
		t.Errorf("the ID's path: %d %v", st, p.Err())
	}
}

// Each leak the security review found is closed: a token in a query, a
// form body, JSON inside a string, a Bearer header value, an email address,
// a person's name; and what is not a secret is kept and compared.
func TestRedactFindings(t *testing.T) {
	for name, tc := range map[string]struct{ in, leak, keep string }{
		"query":            {"/v1/hooks?api_key=9f8e7d6c5b&page=2", "9f8e7d6c5b", "page=2"},
		"token in url":     {"/v1/x?next=https://h/cb?t=ghp_abcdEFGH1234", "ghp_abcd", "next="},
		"form":             {"client_id=app1&client_secret=s3cr3tVal&grant_type=client_credentials", "s3cr3tVal", "grant_type=client_credentials"},
		"json in a string": {`{"custom_headers":"{\"Authorization\":\"Bearer abc123XYZ\"}","name":"n"}`, "abc123XYZ", `"name":"n"`},
		"bearer":           {`{"headers":{"Authorization":"Bearer abc123XYZ"}}`, "abc123XYZ", "headers"},
		"token inside":     {`{"note":"use token ghp_abcdEFGH1234 here"}`, "ghp_abcd", "use token"},
		"email":            {`{"handle":"jane@acme-corp.com","test":"hfl-live-1-u@example.com"}`, "jane@", "u@example.com"},
		"person":           {`{"author_name":"Jane Roe","name":"dash"}`, "Roe", `"dash"`},
		"secret_string":    {`{"secret_string":"Xy9-real-value"}`, "Xy9-real", "secret_string"},
		"secret words":     {`{"secret_string":"correct-horse-battery-staple"}`, "horse", "secret_string"},
		"signed url":       {`{"download":"https://b.s3.amazonaws.com/o?X-Amz-Signature=abc123def&X-Amz-Date=1"}`, "abc123def", "X-Amz-Date=1"},
		"url password":     {`{"dsn":"x","conn":"postgres://app:hunter22@db.example.net:5432/db"}`, "hunter22", "postgres://app:"},
		"mongodb srv":      {`{"uri":"mongodb+srv://u:p4ssw0rd@cluster0.abc.mongodb.net/?retryWrites=true"}`, "p4ssw0rd", "cluster0.abc.mongodb.net"},
		"slack hook":       {`{"url":"https://hooks.slack.com/services/T000/B000/XXXXsecretXXXX"}`, "XXXXsecret", `"url"`},
		"header text":      {`{"headers":"Authorization: Basic dXNlcjpwYXNz"}`, "dXNlcjpwYXNz", "headers"},
		"plain kept":       {`{"token_endpoint_auth_method":"client_secret_post","token_type":"Bearer"}`, "", "client_secret_post"},
	} {
		t.Run(name, func(t *testing.T) {
			out := Redact(tc.in)
			if strings.HasPrefix(tc.in, "/") {
				out = RedactPath(tc.in)
			}
			if tc.leak != "" && strings.Contains(out, tc.leak) {
				t.Errorf("kept %q: %s", tc.leak, out)
			}
			if !strings.Contains(out, tc.keep) {
				t.Errorf("lost %q: %s", tc.keep, out)
			}
		})
	}
}

// Where values overlap the longer is scrubbed, so no tail of a secret is
// left; a secret is scrubbed in each encoding, each to its own placeholder.
func TestScrubOverlapAndEncodings(t *testing.T) {
	s := NewScrubber()
	s.Add("abcd", "{{cred:a}}")
	s.Add("bcdefghij", "{{cred:b}}")
	if got := s.Scrub("xabcdefghij"); got != "xa{{cred:b}}" {
		t.Errorf("overlap: %s", got)
	}
	s = NewScrubber()
	s.AddSecret("p+ss/w=rd", "{{cred:pw}}")
	got := s.Scrub("q=p%2Bss%2Fw%3Drd raw=p+ss/w=rd b64=cCtzcy93PXJk")
	if strings.Contains(got, "ss") || !strings.Contains(got, "{{cred:pw|url}}") || !strings.Contains(got, "{{cred:pw|b64}}") {
		t.Errorf("encodings: %s", got)
	}
	r := NewScrubber()
	r.AddSecret("replay+pw", "{{cred:pw}}")
	if u := r.Unscrub("{{cred:pw|url}}"); u != "replay%2Bpw" {
		t.Errorf("unscrub encoded: %s", u)
	}
}

// A cassette that still holds a known secret, in any encoding, or a token
// by its shape, is reported, without the value.
func TestLeaks(t *testing.T) {
	c := &Cassette{Interactions: []Interaction{
		{Method: "POST", Path: "/x?k=p%2Bss%2Fw%3Drd", Body: `{"a":"ok"}`, Response: `{"t":"cfat_abcdef123456"}`},
	}}
	leaks := c.Leaks(map[string]string{"cred:pw": "p+ss/w=rd"})
	all := strings.Join(leaks, "\n")
	if len(leaks) != 2 || !strings.Contains(all, "request 1 path: the value of cred:pw") || !strings.Contains(all, "request 1 response: a token") || strings.Contains(all, "p+ss") {
		t.Errorf("leaks: %q", leaks)
	}
	if l := (&Cassette{Interactions: []Interaction{{Path: "/x", Body: `{"pw":"{{cred:pw}}"}`}}}).Leaks(map[string]string{"cred:pw": "p+ss/w=rd"}); len(l) != 0 {
		t.Errorf("a placeholder is not a leak: %q", l)
	}
}

// A replay sending the wrong credential fails: credentials are compared as
// placeholders, not redacted away. Switching to multipart fails too.
func TestReplayComparesCredentials(t *testing.T) {
	rec := NewScrubber()
	rec.AddSecret("real-key-A", "{{cred:keyA}}")
	rec.AddSecret("real-key-B", "{{cred:keyB}}")
	cas := (&Cassette{Interactions: []Interaction{{Method: "POST", Path: "/i", Body: `{"api_key":"real-key-A"}`, Status: 201, Response: `{}`}}}).Scrubbed(rec)
	if !strings.Contains(cas.Interactions[0].Body, "{{cred:keyA}}") {
		t.Fatalf("recorded %s", cas.Interactions[0].Body)
	}
	play := func(body string, header ...string) int {
		v := NewScrubber()
		v.AddSecret("replay-keyA", "{{cred:keyA}}")
		v.AddSecret("replay-keyB", "{{cred:keyB}}")
		p := &Player{Base: "http://r", Values: v}
		p.Load(cas)
		st, _ := do(t, &http.Client{Transport: p}, "POST", "http://r/i", body, header...)
		return st
	}
	if st := play(`{"api_key":"replay-keyA"}`); st != 201 {
		t.Errorf("right key: %d", st)
	}
	if st := play(`{"api_key":"replay-keyB"}`); st != 599 {
		t.Errorf("wrong key replayed green: %d", st)
	}
	if st := play("--b\r\n", "Content-Type", "multipart/form-data; boundary=b"); st != 599 {
		t.Errorf("multipart where JSON was recorded: %d", st)
	}
}

// A secret-named value is kept as an identifier only when it is a whole path
// segment of eight characters or more, and never when it looks like a token.
func TestIdentifiersWholeSegments(t *testing.T) {
	keep := identifiers([]Interaction{{Path: "/embeds/abcdef123456/x?q=1"}, {Path: "/t/ghp_abcdEFGH1234"}})
	if !keep.keeps("abcdef123456") || keep.keeps("abcdef") || keep.keeps("ghp_abcdEFGH1234") || keep.keeps("embeds/abcdef123456") {
		t.Error("identifiers")
	}
}

// A secret recorded JSON-escaped (a PEM key's newlines) replays against a
// stand-in that has no escaped form of its own.
func TestEncodedPlaceholderReplays(t *testing.T) {
	rec := NewScrubber()
	rec.AddSecret("-----BEGIN KEY-----\nabc\n-----END KEY-----", "{{env:K}}")
	cas := (&Cassette{Interactions: []Interaction{{Method: "POST", Path: "/p", Body: `{"key":"-----BEGIN KEY-----\nabc\n-----END KEY-----"}`, Status: 201, Response: `{"echo":"{{env:K|json}}"}`}}}).Scrubbed(rec)
	if !strings.Contains(cas.Interactions[0].Body, "{{env:K|json}}") {
		t.Fatalf("recorded %s", cas.Interactions[0].Body)
	}
	v := NewScrubber()
	v.AddSecret("replay-env-K", "{{env:K}}")
	p := &Player{Base: "http://r", Values: v}
	p.Load(cas)
	st, body := do(t, &http.Client{Transport: p}, "POST", "http://r/p", `{"key":"replay-env-K"}`)
	if st != 201 || !strings.Contains(body, `"echo":"replay-env-K"`) {
		t.Errorf("%d %s %v", st, body, p.Err())
	}
}

// Round-two findings, each pinned.
func TestRoundTwoFindings(t *testing.T) {
	t.Run("an email in a path is not an identifier", func(t *testing.T) {
		c := (&Cassette{Interactions: []Interaction{{Method: "PUT", Path: "/invites/alice.smith@corp.com", Body: `{"invitee":"alice.smith@corp.com"}`, Status: 200}}}).Scrubbed(NewScrubber())
		if all := c.Interactions[0].Path + c.Interactions[0].Body; strings.Contains(all, "alice.smith") {
			t.Errorf("kept: %+v", c.Interactions[0])
		}
		p := &Player{Base: "http://r", Values: NewScrubber()}
		p.Load(c)
		if st, _ := do(t, &http.Client{Transport: p}, "PUT", "http://r/invites/alice.smith@corp.com", `{"invitee":"alice.smith@corp.com"}`); st != 200 {
			t.Errorf("the same request: %d %v", st, p.Err())
		}
	})
	t.Run("config under a secret-named object is compared", func(t *testing.T) {
		rec := `{"credentials":{"client":{"client_id":"0oa1Xyz9"},"userNameTemplate":{"type":"BUILT_IN"}},"endpoints":{"token":{"url":"https://idp.example.com/token","binding":"HTTP-POST"}},"access_token_url":"https://x.example.com/t"}`
		out := Redact(rec)
		for _, keep := range []string{"BUILT_IN", "https://idp.example.com/token", "HTTP-POST", "https://x.example.com/t"} {
			if !strings.Contains(out, keep) {
				t.Errorf("hid %q: %s", keep, out)
			}
		}
		c := (&Cassette{Interactions: []Interaction{{Method: "POST", Path: "/idps", Body: rec, Status: 200}}}).Scrubbed(NewScrubber())
		p := &Player{Base: "http://r", Values: NewScrubber()}
		p.Load(c)
		changed := strings.Replace(rec, "https://idp.example.com/token", "https://evil.example.org/other", 1)
		if st, _ := do(t, &http.Client{Transport: p}, "POST", "http://r/idps", changed); st != 599 {
			t.Errorf("a changed token url replayed green")
		}
	})
	t.Run("a person's login in their URLs", func(t *testing.T) {
		out := Redact(`{"owner":{"login":"jroe-dev","type":"User","html_url":"https://github.com/jroe-dev","repos_url":"https://api.github.com/users/jroe-dev/repos","id":42}}`)
		if strings.Contains(out, "jroe-dev") || !strings.Contains(out, `"id":42`) {
			t.Errorf("%s", out)
		}
	})
	t.Run("JSON:API person attributes", func(t *testing.T) {
		out := Redact(`{"data":{"type":"users","id":"u1","attributes":{"name":"Jane Roe","handle":"jroe"}}}`)
		if strings.Contains(out, "Jane Roe") || !strings.Contains(out, `"id":"u1"`) {
			t.Errorf("%s", out)
		}
	})
	t.Run("base64 is compared as base64", func(t *testing.T) {
		rec := NewScrubber()
		rec.AddSecret("real-key-value", "{{cred:k}}")
		c := (&Cassette{Interactions: []Interaction{{Method: "POST", Path: "/k", Body: `{"blob":"cmVhbC1rZXktdmFsdWU="}`, Status: 200}}}).Scrubbed(rec)
		if !strings.Contains(c.Interactions[0].Body, "{{cred:k|b64}}") {
			t.Fatalf("recorded %s", c.Interactions[0].Body)
		}
		v := NewScrubber()
		v.AddSecret("replay-k", "{{cred:k}}")
		p := &Player{Base: "http://r", Values: v}
		p.Load(c)
		if st, _ := do(t, &http.Client{Transport: p}, "POST", "http://r/k", `{"blob":"replay-k"}`); st != 599 {
			t.Errorf("raw where base64 was recorded replayed green")
		}
	})
	t.Run("a secret before any other value", func(t *testing.T) {
		s := NewScrubber()
		s.Add("https://api.acme.io", "{{baseURL}}")
		s.AddSecret("acme.io/Zk39xQ", "{{cred:k}}")
		if got := s.Scrub("https://api.acme.io/Zk39xQ"); strings.Contains(got, "Zk39xQ") {
			t.Errorf("%s", got)
		}
	})
	t.Run("a strong secret word wins over a descriptive ending", func(t *testing.T) {
		out := Redact(`{"secret_id":"7c9e6679-7425-40de-944b-e07fc1f90ae7","api_key_id":"kid-123456","access_token_url":"https://x.example.com/t"}`)
		if strings.Contains(out, "7c9e6679") || !strings.Contains(out, "kid-123456") || !strings.Contains(out, "https://x.example.com/t") {
			t.Errorf("%s", out)
		}
	})
	t.Run("a token prefix inside a word is not a token", func(t *testing.T) {
		if out := Redact(`{"cursor":"abcghs_zzzzzzzz","t":"x ghp_abcdEFGH1234"}`); !strings.Contains(out, "abcghs_zzzzzzzz") || strings.Contains(out, "ghp_abcd") || !strings.Contains(out, `"x {{redacted}}"`) {
			t.Errorf("%s", out)
		}
	})
}

// A field redaction changed in what the replay sent (a person's name, an
// email address in the text) is answered back as sent, at its own path,
// through an envelope only the response has, and at its latest value.
func TestRefillRedactedAsSent(t *testing.T) {
	at := map[string]string{}
	sent := `{"user":{"type":"user","name":"Ada Lovelace","email":"ada@corp.dev"},"type":"AWS"}`
	sentRedacted(sent, redactExcept(sent, nil), at)
	got := refill(`{"user":{"name":"`+Redacted+`","email":"`+Redacted+`","type":"user"},"type":"AWS"}`, nil, at)
	if !strings.Contains(got, `"name":"Ada Lovelace"`) || !strings.Contains(got, `"email":"ada@corp.dev"`) || !strings.Contains(got, `"type":"AWS"`) {
		t.Errorf("refilled %s", got)
	}
	// The drift step sends one field alone, unredacted: it is the latest.
	sentRedacted(`{"user":{"email":"drift"}}`, `{"user":{"email":"drift"}}`, at)
	if got := refill(`{"user":{"email":"`+Redacted+`"}}`, nil, at); !strings.Contains(got, `"email":"drift"`) {
		t.Errorf("after drift: %s", got)
	}
	// An email address redacted in the text, read back inside data.
	at = map[string]string{}
	sent = `{"service_account_email":"sa@proj.iam.gserviceaccount.com","ttl":"1h"}`
	sentRedacted(sent, redactExcept(sent, nil), at)
	got = refill(`{"data":{"service_account_email":"`+RedactedEmail+`","ttl":"1h"}}`, nil, at)
	if !strings.Contains(got, `sa@proj.iam.gserviceaccount.com`) {
		t.Errorf("enveloped: %s", got)
	}
	// Another field of the same name is not refilled with it.
	if got := refill(`{"credentials":{"type":"`+Redacted+`"}}`, nil, map[string]string{"/type": "AWS", "/credentials/type": "AWSAssumeRole"}); !strings.Contains(got, `"type":"AWSAssumeRole"`) {
		t.Errorf("same name, other path: %s", got)
	}
}

// A secret redacted inside a value's text (a URL's password) is answered as sent.
func TestRefillRedactedInText(t *testing.T) {
	at := map[string]string{}
	sent := `{"url":"https://user:hunter2hunter2@example.com/x.git"}`
	sentRedacted(sent, redactExcept(sent, nil), at)
	rec := redactExcept(`{"id":1,"url":"https://user:hunter2hunter2@example.com/x.git"}`, nil)
	if !strings.Contains(rec, Redacted) {
		t.Skipf("the password is not redacted in %s", rec)
	}
	if got := refill(rec, nil, at); !strings.Contains(got, "hunter2hunter2") {
		t.Errorf("refilled %s", got)
	}
}

// A response's Link header is recorded with the base URL as its
// placeholder, and a replay answers it on the replay's base URL.
func TestLinkHeaderRecordedAndReplayed(t *testing.T) {
	rec := &Cassette{Interactions: []Interaction{{Method: "GET", Path: "/teams", Status: 200, ContentType: "application/json",
		Response: `[]`, Link: `<https://api.example.com/teams?page=2>; rel="next"`}}}
	s := NewScrubber()
	s.Add("https://api.example.com", "{{baseURL}}")
	out := rec.Scrubbed(s)
	if got := out.Interactions[0].Link; got != `<{{baseURL}}/teams?page=2>; rel="next"` {
		t.Fatalf("recorded %q", got)
	}
	p := &Player{Base: "http://replay.invalid"}
	p.Values = NewScrubber()
	p.Values.Add("http://replay.invalid", "{{baseURL}}")
	p.Load(out)
	req, _ := http.NewRequest("GET", "http://replay.invalid/teams", nil)
	resp, err := p.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get("Link"); got != `<http://replay.invalid/teams?page=2>; rel="next"` {
		t.Errorf("replayed Link %q", got)
	}
}

// A kind's secret outputs are redacted in its cassette whatever their names.
func TestRedactFields(t *testing.T) {
	c := &Cassette{Interactions: []Interaction{{Method: "POST", Path: "/turn", Body: `{"name":"x"}`, Status: 201,
		Response: `{"result":{"uid":"u1","key":"0123456789abcdef0123456789abcdef","name":"x"}}`}}}
	out := c.RedactFields([]string{"result.key"})
	if strings.Contains(out.Interactions[0].Response, "0123456789abcdef") || !strings.Contains(out.Interactions[0].Response, `"uid":"u1"`) {
		t.Errorf("response %s", out.Interactions[0].Response)
	}
	if c.Interactions[0].Response == out.Interactions[0].Response {
		t.Error("the input was changed in place, or nothing was redacted")
	}
}
