// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"path/filepath"
	"strings"
	"testing"
)

const connLive = `
secret: krm-live-example
project: p
connections:
  other:
    baseURL: "https://{{cred:otherHost}}.example.com/api"
    creds: {token: "{{cred:otherToken}}"}
kinds:
- kind: Widget
  spec: {name: "{{name}}"}
  change: {color: green}
- kind: Record
  connection: other
  spec: {name: "{{name}}"}
`

// A kind on another connection runs on its base URL and credentials, and
// skips, naming the keys, where the secret lacks them.
func TestUseConnection(t *testing.T) {
	cfg, err := ParseLiveConfig([]byte(connLive))
	if err != nil {
		t.Fatal(err)
	}
	r := newLiveRun(cfg, exampleVendor(), map[string]string{"token": "main-token"}, "https://main.example.com/api", "hfl-live-1")
	if _, skip := r.useConnection("other"); !strings.Contains(skip, "otherHost, otherToken") {
		t.Errorf("no keys: skip %q", skip)
	}
	r = newLiveRun(cfg, exampleVendor(), map[string]string{"token": "main-token", "otherToken": "o-token", "otherHost": "acme"}, "https://main.example.com/api", "hfl-live-1")
	restore, skip := r.useConnection("other")
	if skip != "" {
		t.Fatal(skip)
	}
	if r.api.BaseURL != "https://acme.example.com/api" || r.kindCreds["token"] != "o-token" {
		t.Errorf("base %s creds %v", r.api.BaseURL, r.kindCreds)
	}
	restore()
	if r.api.BaseURL != "https://main.example.com/api" || r.kindCreds != nil {
		t.Errorf("not put back: %s %v", r.api.BaseURL, r.kindCreds)
	}
}

// A connection is checked: a kind may name only one live.yaml defines.
func TestConnectionMustBeDefined(t *testing.T) {
	if _, err := ParseLiveConfig([]byte("secret: s\nproject: p\nkinds:\n- kind: Widget\n  connection: nope\n")); err == nil {
		t.Error("an undefined connection was accepted")
	}
}

// A rehearsal of a kind on another connection records and replays on the
// run's base, with stand-in credentials.
func TestRehearseWithConnection(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "c")
	t.Setenv(EnvLiveCassettes, dir)
	t.Setenv(EnvLiveRecords, filepath.Join(t.TempDir(), "records.json"))
	t.Setenv(EnvFakeRecord, "1")
	t.Run("record", func(t *testing.T) { Rehearse(t, typesYAML, exampleVendor(), nil, []byte(connLive)) })
	t.Setenv(EnvFakeRecord, "")
	Rehearse(t, typesYAML, exampleVendor(), nil, []byte(connLive))
}

// A live kind whose entry names a key the secret lacks is skipped, naming it.
func TestMissingCreds(t *testing.T) {
	cfg, _ := ParseLiveConfig([]byte("secret: s\nproject: p\nkinds:\n- kind: Widget\n  spec: {name: \"{{name}}\", key: \"{{cred:gcpCredentials}}\"}\n"))
	r := newLiveRun(cfg, exampleVendor(), map[string]string{"token": "t"}, "http://x", "hfl-live-1")
	if got := r.missingCreds(&cfg.Kinds[0]); len(got) != 1 || got[0] != "gcpCredentials" {
		t.Errorf("missing %v", got)
	}
	r.rehearsal = true
	if got := r.missingCreds(&cfg.Kinds[0]); got != nil {
		t.Errorf("a rehearsal has stand-ins: %v", got)
	}
}
