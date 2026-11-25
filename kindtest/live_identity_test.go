// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

//go:build live

package kindtest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GitHub's team membership of the run's own user, whose login a before
// call saves (save: {me: login}).
const memberTypes = `
vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: ExampleConnection}
kinds:
- kind: TeamMembership
  group: team.example.hfl.dev
  version: v1alpha1
  plural: teammemberships
  shape: upsert
  read: {method: GET, path: "/teams/{team_id}/memberships/{username}"}
  update: {method: PUT, path: "/teams/{team_id}/memberships/{username}"}
  delete: {method: DELETE, path: "/teams/{team_id}/memberships/{username}"}
  params:
  - {name: team_id, field: teamID}
  - {name: username, field: username}
  sample: {teamID: "1", username: someone, role: member}
  serverFields: {url: "https://example.com/"}
  readiness: live
`

const memberLive = `
secret: s
project: p
redactSubdomains: [workers.dev]
before:
- {method: GET, path: /user, save: {me: login}}
kinds:
- kind: TeamMembership
  spec: {teamID: "42", username: "{{var:me}}", role: member}
  change: {role: maintainer}
  noDrift: not needed here
`

// The run's own login, saved by a before call from a login field, is an
// identity: no cassette and no record holds it (a path, a URL in a
// response, a saved var, an error), nor the account's workers.dev label,
// and a replay of the cassettes passes.
func TestLiveRedactsIdentities(t *testing.T) {
	const login = "jane-plainname"
	const sub = "janes-account"
	role := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var in map[string]string
		_ = json.Unmarshal(b, &in)
		w.Header().Set("Content-Type", "application/json")
		member := "/teams/42/memberships/" + login
		answer := func() {
			_, _ = io.WriteString(w, `{"role":"`+role+`","url":"https://api.example.com`+member+`","site":"https://`+sub+`.workers.dev"}`)
		}
		switch {
		case r.URL.Path == "/user":
			_, _ = io.WriteString(w, `{"login":"`+login+`","id":1}`)
		case r.URL.Path == member && r.Method == "PUT":
			role = in["role"]
			answer()
		case r.URL.Path == member && r.Method == "GET" && role != "":
			answer()
		case r.URL.Path == member && r.Method == "DELETE":
			role = ""
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"message":"Not Found: `+r.URL.Path+`"}`)
		}
	}))
	dir := filepath.Join(t.TempDir(), "cassettes")
	records := filepath.Join(t.TempDir(), "records.json")
	t.Setenv(EnvLive, "1")
	t.Setenv(EnvLiveCreds, `{"token":"sekrit-test-token","baseURL":"`+srv.URL+`"}`)
	t.Setenv(EnvLiveRecords, records)
	t.Setenv(EnvLiveRun, "hfl-live-2611220000-idn1")
	t.Setenv(EnvLiveCassettes, dir)

	t.Run("live", func(t *testing.T) { Live(t, []byte(memberTypes), exampleVendor(), nil, []byte(memberLive)) })
	srv.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != 2 {
		t.Fatalf("cassettes: %v, want _before and the kind's", files)
	}
	for _, p := range append(files, records) {
		data, _ := os.ReadFile(p)
		if strings.Contains(string(data), login) || strings.Contains(string(data), sub) {
			t.Errorf("%s keeps the login or the subdomain:\n%s", filepath.Base(p), data)
		}
	}
	data, _ := os.ReadFile(filepath.Join(dir, "TeamMembership.team.example.hfl.dev.json"))
	if !strings.Contains(string(data), "/memberships/{{id:1}}") {
		t.Errorf("the login in a path is not an ID:\n%s", data)
	}

	t.Run("replay", func(t *testing.T) { Replay(t, []byte(memberTypes), exampleVendor(), nil, []byte(memberLive)) })
}
