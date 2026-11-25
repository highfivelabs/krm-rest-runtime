// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/cassette"
	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// A rehearsal records every kind against the fake (the listed ones with
// their live specs, the rest with their samples), the same bytes each time,
// replays what it recorded, and writes no records.
func TestRehearseRecordsThenReplays(t *testing.T) {
	records := filepath.Join(t.TempDir(), "records.json")
	t.Setenv(EnvLiveRecords, records)
	record := func(dir string) {
		t.Helper()
		t.Setenv(EnvLiveCassettes, dir)
		t.Setenv(EnvFakeRecord, "1")
		t.Run("record", func(t *testing.T) { Rehearse(t, typesYAML, exampleVendor(), nil, []byte(exampleLive)) })
	}
	a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	record(a)
	record(b)
	if t.Failed() {
		t.FailNow()
	}

	f, _ := descriptor.Parse(typesYAML)
	for i := range f.Kinds {
		name := kindKey(&f.Kinds[i]) + ".json"
		c, err := cassette.Load(filepath.Join(a, name))
		if err != nil || c == nil {
			t.Errorf("%s: no cassette (%v)", name, err)
			continue
		}
		if c.Target != FakeTarget || c.Recorded != "2000-01-01T00:00:00Z" {
			t.Errorf("%s: target %q recorded %q", name, c.Target, c.Recorded)
		}
		if strings.Contains(string(mustRead(t, filepath.Join(a, name))), "rehearsal-token") {
			t.Errorf("%s: the stand-in credential is in it, not its placeholder", name)
		}
		if string(mustRead(t, filepath.Join(a, name))) != string(mustRead(t, filepath.Join(b, name))) {
			t.Errorf("%s: recording again changed it", name)
		}
	}
	if _, err := os.Stat(filepath.Join(a, "README.md")); err != nil {
		t.Error("no README.md beside the cassettes")
	}
	if _, err := os.Stat(records); !os.IsNotExist(err) {
		t.Errorf("a rehearsal wrote records (%v): readiness is the real API's alone", err)
	}

	t.Setenv(EnvFakeRecord, "")
	t.Setenv(EnvLiveCassettes, a)
	Rehearse(t, typesYAML, exampleVendor(), nil, []byte(exampleLive))
}

// Without cassettes, a rehearsal skips and says how to record them.
func TestRehearseWithoutCassettesSkips(t *testing.T) {
	t.Setenv(EnvLiveCassettes, filepath.Join(t.TempDir(), "none"))
	ft := &testing.T{}
	done := make(chan struct{})
	go func() { defer close(done); Rehearse(ft, typesYAML, exampleVendor(), nil, []byte(exampleLive)) }()
	<-done
	if !ft.Skipped() || ft.Failed() {
		t.Fatalf("skipped=%v failed=%v", ft.Skipped(), ft.Failed())
	}
}

// Every kind, each once: the listed first (a skip cleared), then the rest.
func TestRehearsalConfig(t *testing.T) {
	f, _ := descriptor.Parse(typesYAML)
	cfg, err := rehearsalConfig(f, []byte(exampleLive))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Kinds) != len(f.Kinds) {
		t.Fatalf("%d kinds, want %d", len(cfg.Kinds), len(f.Kinds))
	}
	if cfg.Kinds[0].Kind != "Widget" || cfg.Kinds[0].Skip != "" || cfg.Kinds[0].Change["color"] != "green" {
		t.Errorf("first: %+v", cfg.Kinds[0])
	}
	seen := map[string]bool{}
	for _, lk := range cfg.Kinds {
		key := lk.Kind + "." + lk.Group
		if seen[key] {
			t.Errorf("%s twice", key)
		}
		seen[key] = true
	}
	if _, err := rehearsalConfig(f, []byte("kinds:\n- kind: Nope\n")); err == nil {
		t.Error("a kind types.yaml lacks was accepted")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
