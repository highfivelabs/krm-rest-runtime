// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"net/http"
	"strings"
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/rest"
)

const exampleLive = `
secret: krm-live-example
target: the fake API
kinds:
- kind: Widget
  spec: {name: "{{name}}"}
  change: {color: green}
- kind: Secret
  spec: {secretName: "{{name_}}"}
  change: {visibility: private}
- kind: Settings
  change: {default_branch: trunk}
- kind: Record
  spec: {name: "{{name}}"}
- kind: Role
  spec: {name: "{{name_}}"}
- kind: Grant
- kind: Widget
  group: things.example.hfl.dev
  skip: listed twice to check skip
`

func exampleVendor() *rest.Vendor {
	return &rest.Vendor{Name: "example", CredentialKeys: []string{"token"},
		Authorize: func(r *http.Request, c map[string]string) { r.Header.Set("Authorization", "Bearer "+c["token"]) }}
}

func TestValidLiveConfig(t *testing.T) {
	ValidLiveConfig(t, typesYAML, []byte(exampleLive))
	ft := &testing.T{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ValidLiveConfig(ft, typesYAML, []byte("secret: s\nproject: p\nkinds:\n- kind: Nope\n"))
	}()
	<-done
	if !ft.Failed() {
		t.Error("a kind types.yaml lacks was accepted")
	}
}

// live.yaml's identities: those it names, and every var a call saves from
// a person's login (here GET /user's), whether or not it names it.
func TestLiveConfigIdentities(t *testing.T) {
	cfg, err := ParseLiveConfig([]byte(`
secret: s
project: p
redactVars: [owner]
redactFields: [added_by]
redactSubdomains: [workers.dev]
before:
- {method: GET, path: /user, save: {me: login, myID: id}}
kinds:
- kind: Widget
  before:
  - {method: GET, path: /whoami, save: {mail: data/email, team: data/slug}}
`))
	if err != nil {
		t.Fatal(err)
	}
	id := cfg.identities()
	if got := strings.Join(id.RedactVars, ","); got != "mail,me,owner" {
		t.Errorf("redactVars %s", got)
	}
	if len(id.RedactFields) != 1 || len(id.RedactSubdomains) != 1 {
		t.Errorf("identities %+v", id)
	}
	if !cfg.identityVar("me") || cfg.identityVar("myID") || cfg.identityVar("team") {
		t.Error("identityVar")
	}
}

// A fakeOnly kind is skipped by every live run, saying so; it cannot also
// carry a skip.
func TestLiveConfigFakeOnly(t *testing.T) {
	cfg, err := ParseLiveConfig([]byte("secret: s\nkinds:\n- kind: Widget\n  fakeOnly: Enterprise only, sold by contract\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Kinds[0].Skip; got != "fake only: Enterprise only, sold by contract" {
		t.Errorf("skip %q", got)
	}
	if _, err := ParseLiveConfig([]byte("secret: s\nkinds:\n- kind: Widget\n  skip: x\n  fakeOnly: y\n")); err == nil || !strings.Contains(err.Error(), "both skip and fakeOnly") {
		t.Errorf("skip and fakeOnly together: %v", err)
	}
}
