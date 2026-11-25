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

// A Vault-style token: made by a create that answers its accessor (its ID)
// and the token, looked up and revoked by the accessor.
const tokenTypes = `
vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: ExampleConnection}
kinds:
- kind: Token
  group: token.example.hfl.dev
  version: v1alpha1
  plural: tokens
  shape: crud
  create: {method: POST, path: /auth/token/create, envelope: auth}
  read: {method: POST, path: /auth/token/lookup-accessor, body: {accessor: "{id}"}}
  delete: {method: POST, path: /auth/token/revoke-accessor, body: {accessor: "{id}"}}
  id: {param: id, from: accessor}
  envelope: data
  compareReturnedOnly: true
  writeOnly: [ttl]
  secretOutputs: [client_token]
  redact: [accessor]
  sample: {ttl: 1h}
  readiness: live
`

const tokenLive = `
secret: s
project: p
before:
- {method: GET, path: /sys/hint}
kinds:
- kind: Token
  spec: {ttl: 1h}
`

// A kind's redact field is redacted in its cassette, and its value
// everywhere else the run recorded it: the error that quotes it, the calls
// that send it, and the before calls' cassette, written before the run knew
// it. A replay of them passes.
func TestLiveRedactsAKnownValueEverywhere(t *testing.T) {
	const acc = "Acc3ss0r-0123456789"
	live := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var in map[string]string
		_ = json.Unmarshal(b, &in)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/sys/hint":
			_, _ = io.WriteString(w, `{"note":"next accessor `+acc+`"}`)
		case "/auth/token/create":
			live = true
			_, _ = io.WriteString(w, `{"auth":{"accessor":"`+acc+`","client_token":"hvs.0123456789abcdef"}}`)
		case "/auth/token/lookup-accessor":
			if !live || in["accessor"] != acc {
				w.WriteHeader(404)
				_, _ = io.WriteString(w, `{"errors":["no token for accessor `+in["accessor"]+`"]}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":{"accessor":"`+acc+`","ttl":3600}}`)
		case "/auth/token/revoke-accessor":
			live = false
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	dir := filepath.Join(t.TempDir(), "cassettes")
	t.Setenv(EnvLive, "1")
	t.Setenv(EnvLiveCreds, `{"token":"sekrit-test-token","baseURL":"`+srv.URL+`"}`)
	t.Setenv(EnvLiveRecords, filepath.Join(t.TempDir(), "records.json"))
	t.Setenv(EnvLiveRun, "hfl-live-2611220000-red1")
	t.Setenv(EnvLiveCassettes, dir)

	t.Run("live", func(t *testing.T) { Live(t, []byte(tokenTypes), exampleVendor(), nil, []byte(tokenLive)) })
	srv.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != 2 {
		t.Fatalf("cassettes: %v, want _before and the Token's", files)
	}
	for _, p := range files {
		data, _ := os.ReadFile(p)
		if strings.Contains(string(data), "Acc3ss0r") || strings.Contains(string(data), "hvs.") {
			t.Errorf("%s keeps the accessor or the token:\n%s", filepath.Base(p), data)
		}
		if !strings.Contains(string(data), "{{redacted}}") {
			t.Errorf("%s redacts nothing:\n%s", filepath.Base(p), data)
		}
	}

	t.Run("replay", func(t *testing.T) { Replay(t, []byte(tokenTypes), exampleVendor(), nil, []byte(tokenLive)) })
}
