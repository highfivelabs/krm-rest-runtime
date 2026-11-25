// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

//go:build live

package kindtest

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/cassette"
	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	apifake "github.com/highfivelabs/krm-rest-runtime/fake"
)

// A live run records a cassette per passing kind, with nothing secret or
// run-specific in it, and a replay of them passes with the API gone.
//
// KRM_UPDATE_TESTDATA=1 also copies them to testdata/cassettes, which
// TestReplayRecordedCassettes replays in every go test ./....
func TestLiveRecordsThenReplays(t *testing.T) {
	f, err := descriptor.Parse(typesYAML)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(apifake.New(f))
	dir := filepath.Join(t.TempDir(), "cassettes")
	t.Setenv(EnvLive, "1")
	t.Setenv(EnvLiveCreds, `{"token":"sekrit-test-token","baseURL":"`+srv.URL+`"}`)
	t.Setenv(EnvLiveRecords, filepath.Join(t.TempDir(), "records.json"))
	t.Setenv(EnvLiveRun, "hfl-live-2611200000-rec1")
	t.Setenv(EnvLiveCassettes, dir)

	t.Run("live", func(t *testing.T) { Live(t, typesYAML, exampleVendor(), nil, []byte(exampleLive)) })
	srv.Close() // the API is gone; the replay needs only the cassettes

	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != 6 {
		t.Fatalf("cassettes: %v, want one per kind run (6)", files)
	}
	for _, p := range files {
		data, _ := os.ReadFile(p)
		for _, leak := range []string{"sekrit-test-token", "hfl-live-2611200000", srv.URL, "Authorization"} {
			if strings.Contains(string(data), leak) {
				t.Errorf("%s keeps %q", filepath.Base(p), leak)
			}
		}
	}
	w, _ := cassette.Load(filepath.Join(dir, "Widget.things.example.hfl.dev.json"))
	if w == nil || len(w.Interactions) < 6 || !strings.Contains(w.Interactions[0].Body, "{{name}}") {
		t.Fatalf("Widget cassette: %+v", w)
	}

	t.Run("replay", func(t *testing.T) {
		Replay(t, typesYAML, exampleVendor(), nil, []byte(exampleLive))
	})

	if os.Getenv("KRM_UPDATE_TESTDATA") == "1" {
		_ = os.RemoveAll(filepath.Join("testdata", "cassettes"))
		_ = os.MkdirAll(filepath.Join("testdata", "cassettes"), 0o755)
		for _, p := range files {
			data, _ := os.ReadFile(p)
			if err := os.WriteFile(filepath.Join("testdata", "cassettes", filepath.Base(p)), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// A failing kind's calls are not written: its last passing cassette stays.
func TestLiveKeepsCassetteOfAFailure(t *testing.T) {
	f, _ := descriptor.Parse(typesYAML)
	srv := httptest.NewServer(apifake.New(f))
	defer srv.Close()
	dir := t.TempDir()
	old := filepath.Join(dir, "Widget.things.example.hfl.dev.json")
	if err := os.WriteFile(old, []byte(`{"recorded":"old","interactions":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvLive, "1")
	t.Setenv(EnvLiveCreds, `{"token":"t","baseURL":"`+srv.URL+`"}`)
	t.Setenv(EnvLiveRecords, filepath.Join(t.TempDir(), "records.json"))
	t.Setenv(EnvLiveCassettes, dir)
	cfg := "secret: s\nproject: p\nkinds:\n- kind: Widget\n  change: {nope: x}\n" // a field the fake will not read back

	r := liveSetup(t, exampleVendor(), []byte(cfg))
	k := &f.Kinds[0]
	ft := &testing.T{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := r.begin(kindKey(k), "hfl-live-x-01", false); err != nil {
			t.Error(err)
		}
		defer r.end(ft, kindKey(k), "hfl-live-x-01", false) // what runKinds passes for a failed kind
		r.one(ft, f, k, &r.cfg.Kinds[0], "hfl-live-x-01", nil)
	}()
	<-done
	if data, _ := os.ReadFile(old); !strings.Contains(string(data), `"old"`) {
		t.Errorf("a failed kind overwrote its cassette: %s", data)
	}
}

// The run's before and after calls replay with their own saved IDs, even
// when a kind's cassette is from an older run (a kind that failed live
// keeps its last passing cassette): the after cassette keeps its vars.
func TestReplayBeforeAfterWithAStaleKind(t *testing.T) {
	f, _ := descriptor.Parse(typesYAML)
	srv := httptest.NewServer(apifake.New(f))
	dir := t.TempDir()
	t.Setenv(EnvLive, "1")
	t.Setenv(EnvLiveCreds, `{"token":"sekrit-test-token","baseURL":"`+srv.URL+`"}`)
	t.Setenv(EnvLiveRecords, filepath.Join(t.TempDir(), "records.json"))
	t.Setenv(EnvLiveCassettes, dir)
	live := `secret: s
project: p
before:
- {method: POST, path: "/orgs/acme/widgets?activate=false", body: {name: "{{run}}-parent"}, save: {pid: id}}
after:
- {method: DELETE, path: "/orgs/acme/widgets/{{var:pid}}"}
kinds:
- kind: Widget
  spec: {name: "{{name}}"}
  change: {color: green}
`
	t.Run("live", func(t *testing.T) { Live(t, typesYAML, exampleVendor(), nil, []byte(live)) })
	srv.Close()
	after, _ := cassette.Load(filepath.Join(dir, "_after.json"))
	if after == nil || after.Vars["pid"] == nil {
		t.Fatalf("_after.json keeps no vars: %+v", after)
	}
	// The kind's cassette from "an older run": other IDs saved.
	kp := filepath.Join(dir, "Widget.things.example.hfl.dev.json")
	kc, _ := cassette.Load(kp)
	kc.Vars = map[string]any{"pid": int64(999999)}
	if err := kc.Save(kp); err != nil {
		t.Fatal(err)
	}
	t.Run("replay", func(t *testing.T) { Replay(t, typesYAML, exampleVendor(), nil, []byte(live)) })
}
