// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/highfivelabs/krm-rest-runtime/cassette"
	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// The cassettes in testdata/cassettes were recorded by a live run against
// the fake over HTTP (TestLiveRecordsThenReplays, KRM_UPDATE_TESTDATA=1).
// Replaying them needs no tag, no network and no credentials.
func TestReplayRecordedCassettes(t *testing.T) {
	t.Setenv(EnvLiveCassettes, filepath.Join("testdata", "cassettes"))
	Replay(t, typesYAML, exampleVendor(), nil, []byte(exampleLive))
}

// With no cassettes, a replay skips and says how they are made.
func TestReplayWithoutCassettesSkips(t *testing.T) {
	t.Setenv(EnvLiveCassettes, filepath.Join(t.TempDir(), "none"))
	ft := &testing.T{}
	done := make(chan struct{})
	go func() { defer close(done); Replay(ft, typesYAML, exampleVendor(), nil, []byte(exampleLive)) }()
	<-done
	if !ft.Skipped() || ft.Failed() {
		t.Fatalf("skipped=%v failed=%v", ft.Skipped(), ft.Failed())
	}
}

// replayOne replays one kind from dir into a bare T and returns it.
func replayOne(t *testing.T, dir, live string) *testing.T {
	t.Helper()
	t.Setenv(EnvLiveCassettes, dir)
	f, _ := descriptor.Parse(typesYAML)
	r := newReplay(t, exampleVendor(), []byte(live))
	k := &f.Kinds[0] // Widget
	ft := &testing.T{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		name := replayRun + "-01"
		if err := r.begin(kindKey(k), name, r.cfg.Kinds[0].ReplayAnyBody != ""); err != nil {
			t.Error(err)
			return
		}
		defer func() { r.end(ft, kindKey(k), name, !ft.Failed()) }()
		r.one(ft, f, k, &r.cfg.Kinds[0], name, nil)
	}()
	<-done
	return ft
}

// The runtime sending what it did not send when recorded fails the replay:
// here the recording's update carried a field the runtime no longer sends.
func TestReplayFailsOnAChangedCall(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join("testdata", "cassettes", "Widget.things.example.hfl.dev.json")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(data), `\"color\":\"green\",`, `\"color\":\"green\",\"size\":\"L\",`, 1)
	if changed == string(data) {
		t.Fatal("the update is not in the cassette as expected")
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.Base(src)), []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	live := "secret: s\nproject: p\nkinds:\n- kind: Widget\n  spec: {name: \"{{name}}\"}\n  change: {color: green}\n"
	if ft := replayOne(t, dir, live); !ft.Failed() {
		t.Error("a replay whose update differs from the recording passed")
	}
	// With replayAnyBody the bodies are not compared, so it passes.
	if ft := replayOne(t, dir, live+"  replayAnyBody: test\n"); ft.Failed() {
		t.Error("replayAnyBody still compared the bodies")
	}
}

// A replay whose delete never reads as gone fails at once, naming the call,
// not after spinning to the delete's deadline.
func TestReplayFailsFastWhenStillReadable(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join("testdata", "cassettes", "Widget.things.example.hfl.dev.json")
	c, err := cassette.Load(src)
	if err != nil || c == nil {
		t.Fatal(err)
	}
	last := &c.Interactions[len(c.Interactions)-1]
	if last.Method != "GET" || last.Status != 404 {
		t.Fatalf("the last call is not the gone read: %s %d", last.Method, last.Status)
	}
	last.Status, last.Response = 200, c.Interactions[len(c.Interactions)-3].Response
	if err := c.Save(filepath.Join(dir, filepath.Base(src))); err != nil {
		t.Fatal(err)
	}
	live := "secret: s\nproject: p\nkinds:\n- kind: Widget\n  spec: {name: \"{{name}}\"}\n  change: {color: green}\n  deleteWait: 10m\n"
	start := time.Now()
	if ft := replayOne(t, dir, live); !ft.Failed() {
		t.Error("a delete that never read as gone replayed green")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %s", d)
	}
}

// A saved value holding a secret ID ({{id:N}}) replays with the ID's
// stand-in, also when no call of the kind saves it again.
func TestReplayFillsSecretIDVars(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join("testdata", "cassettes", "Widget.things.example.hfl.dev.json")
	c, err := cassette.Load(src)
	if err != nil || c == nil {
		t.Fatal(err)
	}
	c.Interactions = append([]cassette.Interaction{{Method: "GET", Path: "/probe/{{id:1}}", Status: 200, Response: `{}`}}, c.Interactions...)
	c.Vars = map[string]any{"probe": "{{id:1}}"}
	if err := c.Save(filepath.Join(dir, filepath.Base(src))); err != nil {
		t.Fatal(err)
	}
	live := "secret: s\nproject: p\nkinds:\n- kind: Widget\n  before:\n  - {method: GET, path: \"/probe/{{var:probe}}\"}\n  spec: {name: \"{{name}}\"}\n  change: {color: green}\n"
	if ft := replayOne(t, dir, live); ft.Failed() {
		t.Error("a secret-ID var replayed as its literal placeholder")
	}
}
