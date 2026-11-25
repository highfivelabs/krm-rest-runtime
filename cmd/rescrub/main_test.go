// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/cassette"
)

func TestGivenValues(t *testing.T) {
	env := map[string]string{"A": "one\n two \n\n", "B": "three"}
	got, err := givenValues("A, B", func(k string) string { return env[k] }, strings.NewReader("four\r\n\nfive\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"one", "two", "three", "four", "five"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
	if _, err := givenValues("MISSING", func(string) string { return "" }, nil); err == nil {
		t.Error("an empty variable is not an error")
	}
	if got, err := givenValues("", nil, nil); err != nil || got != nil {
		t.Errorf("none: %v %v", got, err)
	}
}

func TestLiveIdentities(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "live.yaml")
	if err := os.WriteFile(p, []byte(`
secret: s
project: p
redactSubdomains: [workers.dev]
before:
- {method: GET, path: /user, save: {me: login, repo: id}}
kinds:
- kind: X
  unknownToRescrub: true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := liveIdentities(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(id.RedactVars, []string{"me"}) || !reflect.DeepEqual(id.RedactSubdomains, []string{"workers.dev"}) {
		t.Errorf("identities %+v", id)
	}
	if id, err := liveIdentities(filepath.Join(dir, "none.yaml")); err != nil || len(id.RedactVars) != 0 {
		t.Errorf("no live.yaml: %+v %v", id, err)
	}
}

// Committed cassettes that hold a login (a saved var, a path, a field no
// rule names) and records that quote it: rescrub with live.yaml's
// identities finds it in the cassettes themselves; a value given on
// standard input is redacted in a rehearsal's too. -check writes nothing
// and never prints the values.
func TestRescrubIdentities(t *testing.T) {
	const login = "jane-plainname"
	const other = "acme-team-owner"
	root := t.TempDir()
	live := filepath.Join(root, "hack", "live", "cassettes")
	fake := filepath.Join(root, "hack", "fake", "cassettes")
	write := func(p string, c *cassette.Cassette) {
		if err := c.Save(p); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(live, "_before.json"), &cassette.Cassette{Recorded: "2026-11-01T00:00:00Z", Interactions: []cassette.Interaction{
		{Method: "GET", Path: "/user", Status: 200, Response: `{"login":"{{redacted}}","id":1}`}}})
	write(filepath.Join(live, "Key.repo.example.hfl.dev.json"), &cassette.Cassette{Recorded: "2026-11-01T00:00:00Z", Vars: map[string]any{"me": login}, Interactions: []cassette.Interaction{
		{Method: "GET", Path: "/keys/7", Status: 200, Response: `{"added_by":"` + login + `","by":"` + other + `"}`}}})
	write(filepath.Join(live, "Member.team.example.hfl.dev.json"), &cassette.Cassette{Recorded: "2026-11-01T00:00:00Z", Vars: map[string]any{"me": login}, Interactions: []cassette.Interaction{
		{Method: "GET", Path: "/teams/42/memberships/" + login, Status: 200, Response: `{"url":"https://x/teams/42/memberships/` + login + `"}`}}})
	write(filepath.Join(fake, "Key.repo.example.hfl.dev.json"), &cassette.Cassette{Recorded: "2026-11-01T00:00:00Z", Vars: map[string]any{"me": "rehearsal-login"}, Interactions: []cassette.Interaction{
		{Method: "GET", Path: "/keys/7", Status: 200, Response: `{"added_by":"rehearsal-login","note":"` + other + `"}`}}})
	records := filepath.Join(root, "hack", "live", "records.json")
	if err := os.WriteFile(records, []byte("{\n  \"Key.repo.example.hfl.dev\": {\n    \"result\": \"fail\",\n    \"reason\": \"403: "+login+" may not\"\n  }\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	liveYAML := filepath.Join(root, "hack", "live", "live.yaml")
	if err := os.WriteFile(liveYAML, []byte("secret: s\nproject: p\nbefore:\n- {method: GET, path: /user, save: {me: login}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ids, err := liveIdentities(liveYAML)
	if err != nil {
		t.Fatal(err)
	}
	run := func(check bool) (int, string) {
		var out bytes.Buffer
		n := 0
		var liveKnown *cassette.Known
		for _, d := range []struct {
			dir    string
			shared bool
		}{{live, true}, {fake, false}} {
			shared := d.shared
			mk := func() *cassette.Known {
				k := cassette.NewKnown()
				if shared {
					k.Identify(ids)
				}
				k.AddIdentity(other)
				return k
			}
			m, known, err := rescrub(d.dir, nil, d.shared, mk, check, &out)
			if err != nil {
				t.Fatal(err)
			}
			n += m
			if shared {
				liveKnown = known
			}
		}
		m, err := redactRecords(records, liveKnown, check, &out)
		if err != nil {
			t.Fatal(err)
		}
		return n + m, out.String()
	}

	n, out := run(true)
	if n != 4 || strings.Contains(out, login) || strings.Contains(out, other) {
		t.Errorf("check: %d changed (want 4: two live, the rehearsal's, the records), output:\n%s", n, out)
	}
	data, _ := os.ReadFile(filepath.Join(live, "Member.team.example.hfl.dev.json"))
	if !strings.Contains(string(data), login) {
		t.Fatal("-check wrote")
	}

	if n, out := run(false); n != 4 {
		t.Fatalf("%d changed:\n%s", n, out)
	}
	for _, p := range []string{filepath.Join(live, "Key.repo.example.hfl.dev.json"), filepath.Join(live, "Member.team.example.hfl.dev.json"), filepath.Join(fake, "Key.repo.example.hfl.dev.json"), records} {
		data, _ := os.ReadFile(p)
		if strings.Contains(string(data), login) || strings.Contains(string(data), other) {
			t.Errorf("%s keeps a value:\n%s", p, data)
		}
	}
	// The rehearsal's own values name no one: kept.
	if data, _ := os.ReadFile(filepath.Join(fake, "Key.repo.example.hfl.dev.json")); !strings.Contains(string(data), "rehearsal-login") {
		t.Errorf("the rehearsal's value was redacted:\n%s", data)
	}
	m, _ := cassette.Load(filepath.Join(live, "Member.team.example.hfl.dev.json"))
	if m.Vars["me"] != "{{id:1}}" || m.Interactions[0].Path != "/teams/42/memberships/{{id:1}}" {
		t.Errorf("member: var %v, path %s", m.Vars["me"], m.Interactions[0].Path)
	}
	// Again: nothing left to change.
	if n, out := run(true); n != 0 {
		t.Errorf("second run changes %d:\n%s", n, out)
	}
}
