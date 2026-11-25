// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

//go:build live

package kindtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	apifake "github.com/highfivelabs/krm-rest-runtime/fake"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// Live mode against an API that answers as the descriptors say (the fake,
// over HTTP): every step runs, everything is deleted, a record per kind.
func TestLiveAgainstAnHTTPAPI(t *testing.T) {
	f, err := descriptor.Parse(typesYAML)
	if err != nil {
		t.Fatal(err)
	}
	api := apifake.New(f)
	srv := httptest.NewServer(api)
	defer srv.Close()
	records := filepath.Join(t.TempDir(), "records.json")
	t.Setenv(EnvLive, "1")
	t.Setenv(EnvLiveCassettes, t.TempDir())
	t.Setenv(EnvLiveCreds, `{"token":"t","baseURL":"`+srv.URL+`"}`)
	t.Setenv(EnvLiveRecords, records)
	t.Setenv(EnvLiveRun, "hfl-live-2611150000-test")

	t.Run("live", func(t *testing.T) { Live(t, typesYAML, exampleVendor(), nil, []byte(exampleLive)) })

	data, err := os.ReadFile(records)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]LiveRecord
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"Widget.things.example.hfl.dev":   "create read update drift delete gone",
		"Secret.things.example.hfl.dev":   "create read update drift delete gone",
		"Settings.things.example.hfl.dev": "create read update drift delete",
		"Record.things.example.hfl.dev":   "create read update drift delete gone",
		"Role.core.example.hfl.dev":       "create read delete gone",
		"Grant.core.example.hfl.dev":      "create read delete",
	}
	if len(got) != len(want) {
		t.Errorf("records: %d, want %d: %s", len(got), len(want), data)
	}
	for k, steps := range want {
		r := got[k]
		if r.Result != "pass" || strings.Join(r.Steps, " ") != steps || r.Run != "hfl-live-2611150000-test" || r.LastPass == "" {
			t.Errorf("%s: %+v, want pass with steps %q", k, r, steps)
		}
	}
	for _, p := range api.Paths() {
		if !strings.HasSuffix(p, "/settings") { // settings stay, restored
			t.Errorf("left behind: %s", p)
		}
	}
}

// A failing kind is recorded as a failure with the reason, and the last
// pass is kept; what it made is still deleted.
func TestLiveRecordsAFailure(t *testing.T) {
	f, _ := descriptor.Parse(typesYAML)
	api := apifake.New(f)
	// The real API ignores color: the update never shows on read.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			r.Body = http.NoBody
		}
		api.ServeHTTP(w, r)
	}))
	defer srv.Close()
	records := filepath.Join(t.TempDir(), "records.json")
	old := `{"Widget.things.example.hfl.dev": {"result": "pass", "steps": [], "run": "old", "date": "x", "lastPass": "2026-11-01T00:00:00Z"}}`
	if err := os.WriteFile(records, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvLive, "1")
	t.Setenv(EnvLiveCassettes, t.TempDir())
	t.Setenv(EnvLiveCreds, `{"token":"t","baseURL":"`+srv.URL+`"}`)
	t.Setenv(EnvLiveRecords, records)
	t.Setenv(EnvLiveKinds, "Widget")
	cfg := "secret: s\nproject: p\nkinds:\n- kind: Widget\n  change: {color: green}\n"

	ft := &testing.T{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// t.Run on a bare T is not allowed; run the kind directly.
		r := liveSetup(t, exampleVendor(), []byte(cfg))
		k := &f.Kinds[0]
		inner := make(chan struct{})
		go func() {
			defer close(inner)
			r.one(ft, f, k, &r.cfg.Kinds[0], "hfl-live-2611150000-x-01", nil)
		}()
		<-inner
		r.writeRecords(t)
	}()
	<-done
	if !ft.Failed() {
		t.Fatal("an update the API ignores passed")
	}
	data, _ := os.ReadFile(records)
	var got map[string]LiveRecord
	_ = json.Unmarshal(data, &got)
	r := got["Widget.things.example.hfl.dev"]
	if r.Result != "fail" || !strings.Contains(r.Reason, "update") || r.LastPass != "2026-11-01T00:00:00Z" {
		t.Errorf("record: %+v", r)
	}
}

// Without KRM_LIVE=1, or without credentials, live tests skip and say why.
func TestLiveSkipsAndSaysWhy(t *testing.T) {
	cases := []struct {
		name, live, want string
	}{
		{"off", "", "set KRM_LIVE=1"},
		{"no project", "1", "set $KRM_LIVE_PROJECT to the Secret Manager project holding secret krm-live-example"},
		{"no secret", "1", "secret krm-live-example in project example-project"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(EnvLive, c.live)
			if c.name == "no secret" {
				t.Setenv(EnvLiveProject, "example-project")
			} else {
				t.Setenv(EnvLiveProject, "")
			}
			t.Setenv(EnvLiveCreds, "")
			t.Setenv("PATH", t.TempDir()) // no gcloud
			ft := &testing.T{}
			done := make(chan struct{})
			go func() { defer close(done); Live(ft, typesYAML, exampleVendor(), nil, []byte(exampleLive)) }()
			<-done
			if !ft.Skipped() || ft.Failed() {
				t.Fatalf("skipped=%v failed=%v", ft.Skipped(), ft.Failed())
			}
			if c.live == "1" {
				cfg, _ := ParseLiveConfig([]byte(exampleLive))
				cfg.Project = os.Getenv(EnvLiveProject)
				if _, why := liveCredentials(cfg); !strings.Contains(why, c.want) {
					t.Errorf("skip reason %q does not name %q", why, c.want)
				}
			}
		})
	}
}

// $KRM_LIVE_CREDS is for a sandbox on this machine: pointed anywhere else,
// the run refuses, so a real account's token never sits in the environment.
func TestLiveCredsFromEnvOnlyForLoopback(t *testing.T) {
	for _, c := range []struct {
		url  string
		pass bool
	}{
		{"http://127.0.0.1:18200/v1", true},
		{"http://localhost:8080/api/v4", true},
		{"http://[::1]:8200/v1", true},
		{"https://api.github.com", false},
		{"https://127.0.0.1.example.com/v1", false},
	} {
		if got := loopback(c.url); got != c.pass {
			t.Errorf("loopback(%q) = %v, want %v", c.url, got, c.pass)
		}
	}
	t.Setenv(EnvLive, "1")
	t.Setenv(EnvLiveCassettes, t.TempDir())
	t.Setenv(EnvLiveCreds, `{"token":"t","baseURL":"https://api.example.com"}`)
	ft := &testing.T{}
	done := make(chan struct{})
	go func() { defer close(done); liveSetup(ft, exampleVendor(), []byte(exampleLive)) }()
	<-done
	if !ft.Failed() {
		t.Error("credentials from the environment for a remote API were accepted")
	}
}

// Putting settings back sends body fields only, never a path parameter the
// read also returns (Atlas refuses cloud and geography in the body).
func TestLiveRestoreSendsBodyFieldsOnly(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = json.NewDecoder(req.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	k := &descriptor.Kind{
		Kind: "Limit", Shape: descriptor.Settings,
		Read:   descriptor.Op{Method: "GET", Path: "/orgs/{org}/limits"},
		Update: &descriptor.Op{Method: "PATCH", Path: "/orgs/{org}/limits"},
		Params: []descriptor.Param{{Name: "org", Field: "org"}},
	}
	r := &liveRun{api: rest.New(&rest.Vendor{Name: "test"}, srv.URL, nil)}
	spec := map[string]any{"org": "acme", "limit": 1.0}
	original := map[string]any{"org": "acme", "limit": 3.0, "endpoint": "x"}
	if err := r.forceDelete(t.Context(), k, map[string]string{"org": "acme"}, spec, original); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["limit"] != 3.0 {
		t.Fatalf("restore sent %v, want {limit: 3}", got)
	}
}

// The sweep deletes only old leftovers named hfl-live-..., making its
// before call first.
func TestLiveSweepCallsBeforeThenDeletes(t *testing.T) {
	var calls []string
	recent := time.Now().UTC().Format("0601021504")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[{"name":"hfl-live-2601010000-abcd-01","id":"z1"},{"name":"hfl-live-` + recent + `-abcd-01","id":"z2"},{"name":"prod-zone","id":"z3"}]`))
			return
		}
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv(EnvLive, "1")
	t.Setenv(EnvLiveCassettes, t.TempDir())
	t.Setenv(EnvLiveSweep, "")
	t.Setenv(EnvLiveCreds, `{"token":"t","baseURL":"`+srv.URL+`"}`)
	cfg := "secret: s\nproject: p\nkinds: []\nsweep:\n- {list: /zones, name: name, id: id, before: \"/zones/{id}/deactivate\", delete: \"/zones/{id}\"}\n"
	r := liveSetup(t, exampleVendor(), []byte(cfg))
	r.sweep(t, false)
	want := "POST /zones/z1/deactivate,DELETE /zones/z1"
	if got := strings.Join(calls, ","); got != want {
		t.Errorf("calls %q, want %q (z2 is too new, z3 not a leftover)", got, want)
	}
}
