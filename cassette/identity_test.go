// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package cassette

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// A login is plain words, which the secret rules keep. Named an identity
// (redactVars), it is redacted wherever it is: a saved var, a field no rule
// names (a deploy key's added_by), a URL; where it is a whole segment of a
// path it is an ID ({{id:N}}), so a replay sends its stand-in there, and
// the replay of the cassette passes.
func TestIdentityVarRedactedEverywhere(t *testing.T) {
	const login = "jane-plainname"
	k := NewKnown()
	k.Identify(Identities{RedactVars: []string{"me"}})
	c := (&Cassette{Vars: map[string]any{"me": login, "team": "builders"}, Interactions: []Interaction{
		{Method: "PUT", Path: "/teams/42/memberships/" + login, Body: `{"role":"maintainer"}`, Status: 200,
			Response: `{"url":"https://api.example.com/teams/42/memberships/` + login + `","role":"maintainer"}`},
		{Method: "GET", Path: "/repos/o/r/keys/7", Status: 200, Response: `{"id":7,"added_by":"` + login + `","title":"jane-plainname-key"}`},
	}}).ScrubbedWith(NewScrubber(), nil, k)
	if all := cassetteText(c); strings.Contains(all, login+`"`) || strings.Contains(all, "/"+login) || strings.Contains(all, `"`+login) {
		t.Fatalf("the login is left:\n%s", all)
	}
	if c.Interactions[0].Path != "/teams/42/memberships/{{id:1}}" || c.Vars["me"] != "{{id:1}}" {
		t.Errorf("path %s, var %v: want {{id:1}} in both", c.Interactions[0].Path, c.Vars["me"])
	}
	if c.Vars["team"] != "builders" {
		t.Errorf("another var changed: %v", c.Vars["team"])
	}
	if !strings.Contains(c.Interactions[1].Response, `"added_by":"{{id:1}}"`) {
		t.Errorf("response %s", c.Interactions[1].Response)
	}
	// An ID is replaced wherever it is, inside a longer word too.
	if !strings.Contains(c.Interactions[1].Response, `"title":"{{id:1}}-key"`) {
		t.Errorf("title %s", c.Interactions[1].Response)
	}

	p := &Player{Base: "http://r", Values: NewScrubber()}
	p.Load(c)
	me := p.Values.Unscrub(c.Vars["me"].(string))
	if me == login || strings.Contains(me, "{{") {
		t.Fatalf("replay's me: %q, want a stand-in", me)
	}
	hc := &http.Client{Transport: p}
	do(t, hc, "PUT", "http://r/teams/42/memberships/"+me, `{"role":"maintainer"}`)
	do(t, hc, "GET", "http://r/repos/o/r/keys/7", "")
	if p.Err() != nil || p.Left() != 0 {
		t.Fatalf("replay: %v, %d left", p.Err(), p.Left())
	}
}

// A short identity (under eight characters) in a path is redacted there,
// not given an ID; a replay that sends the {{redacted}} it read matches it.
func TestShortIdentityInPath(t *testing.T) {
	k := NewKnown()
	k.AddIdentity("jdoe")
	c := (&Cassette{Vars: map[string]any{"me": "jdoe"}, Interactions: []Interaction{
		{Method: "GET", Path: "/users/jdoe/keys", Status: 200, Response: `[{"owner":"jdoe","note":"jdoes and jdoe-x"}]`},
	}}).ScrubbedWith(NewScrubber(), nil, k)
	in := c.Interactions[0]
	if in.Path != "/users/{{redacted}}/keys" || c.Vars["me"] != Redacted {
		t.Errorf("path %s, var %v", in.Path, c.Vars["me"])
	}
	if in.Response != `[{"owner":"{{redacted}}","note":"jdoes and {{redacted}}-x"}]` {
		t.Errorf("response %s", in.Response)
	}
	p := &Player{Base: "http://r", Values: NewScrubber()}
	p.Load(c)
	do(t, &http.Client{Transport: p}, "GET", "http://r/users/%7B%7Bredacted%7D%7D/keys", "")
	if p.Err() != nil {
		t.Fatal(p.Err())
	}
}

// redactSubdomains: the account's label of a domain (a Worker's URL,
// https://<script>.<account>.workers.dev) is found in one cassette and
// redacted in every cassette of the run, wherever it is.
func TestRedactSubdomains(t *testing.T) {
	const sub = "janes-account"
	k := NewKnown()
	k.Identify(Identities{RedactSubdomains: []string{"workers.dev"}})
	s := NewScrubber()
	s.Add("hfl-live-2611220000-ab12-01", "{{name}}")
	other := (&Cassette{Interactions: []Interaction{
		{Method: "GET", Path: "/accounts/1/workers/subdomain", Status: 200, Response: `{"result":{"subdomain":"` + sub + `"}}`},
	}}).ScrubbedWith(NewScrubber(), nil, k)
	c := (&Cassette{Interactions: []Interaction{
		{Method: "POST", Path: "/accounts/1/workers/workers", Body: `{"name":"hfl-live-2611220000-ab12-01"}`, Status: 200,
			Response: `{"result":{"url":"https://hfl-live-2611220000-ab12-01.` + sub + `.workers.dev","preview":"https://` + sub + `.workers.dev/x"}}`},
	}}).ScrubbedWith(s, nil, k)
	if got := c.Interactions[0].Response; got != `{"result":{"url":"https://{{name}}.{{redacted}}.workers.dev","preview":"https://{{redacted}}.workers.dev/x"}}` {
		t.Errorf("response %s", got)
	}
	// The cassette written before the run found it: redacted at the end.
	other, changed := k.Apply(other)
	if !changed || strings.Contains(cassetteText(other), sub) {
		t.Errorf("earlier cassette: changed %v:\n%s", changed, cassetteText(other))
	}
	// A domain that only starts like it is another.
	k2 := NewKnown()
	k2.Identify(Identities{RedactSubdomains: []string{"workers.dev"}})
	(&Cassette{Interactions: []Interaction{{Method: "GET", Path: "/x", Status: 200, Response: `{"u":"https://abc.workers.devices.example.com"}`}}}).ScrubbedWith(NewScrubber(), nil, k2)
	if got := k2.Identities(); len(got) != 0 {
		t.Errorf("found %v in another domain", got)
	}
}

// redactFields: a field's value names a person, wherever the field is.
func TestRedactIdentityFields(t *testing.T) {
	k := NewKnown()
	k.Identify(Identities{RedactFields: []string{"key.added_by"}})
	c := (&Cassette{Interactions: []Interaction{
		{Method: "GET", Path: "/keys/7", Status: 200, Response: `{"id":7,"added_by":"plainperson","note":"made by plainperson"}`},
		{Method: "GET", Path: "/keys", Status: 200, Response: `{"body":"{\"added_by\":\"otherperson\"}"}`},
	}}).ScrubbedWith(NewScrubber(), nil, k)
	if all := cassetteText(c); strings.Contains(all, "plainperson") || strings.Contains(all, "otherperson") {
		t.Fatalf("left:\n%s", all)
	}
	if got := k.Identities(); !reflect.DeepEqual(got, []string{"otherperson", "plainperson"}) {
		t.Errorf("identities %v", got)
	}
}

func TestAddIdentityLeavesShortAndPlaceholders(t *testing.T) {
	k := NewKnown()
	k.AddIdentity("abc", "{{redacted}}", " ", "  jdoe  ")
	if got := k.Identities(); !reflect.DeepEqual(got, []string{"jdoe"}) {
		t.Errorf("identities %v", got)
	}
	if got := k.Redact("user jdoe failed; jdoes did not"); got != "user {{redacted}} failed; jdoes did not" {
		t.Errorf("log line %q", got)
	}
}

func TestIdentityField(t *testing.T) {
	for path, want := range map[string]bool{"login": true, "user/login": true, "data/0/user_name": true, "result/email": true, "id": false, "slug": false, "name": false} {
		if got := IdentityField(path); got != want {
			t.Errorf("IdentityField(%q) = %v", path, got)
		}
	}
}

// No identities: a cassette is scrubbed exactly as before.
func TestNoIdentitiesChangesNothing(t *testing.T) {
	in := &Cassette{Vars: map[string]any{"me": "jane-plainname"}, Interactions: []Interaction{
		{Method: "GET", Path: "/teams/42/memberships/jane-plainname", Status: 200, Response: `{"login":"jane-plainname"}`},
	}}
	a := in.ScrubbedWith(NewScrubber(), nil, nil)
	k := NewKnown()
	k.Identify(Identities{})
	b := in.ScrubbedWith(NewScrubber(), nil, k)
	if !reflect.DeepEqual(a, b) || a.Vars["me"] != "jane-plainname" {
		t.Errorf("changed: %+v / %+v", a, b)
	}
}
