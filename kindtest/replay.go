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
	"github.com/highfivelabs/krm-rest-runtime/reconciler"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// ReplayBaseURL is the base URL a replay's clients use. Nothing listens on
// it (.invalid never resolves), and nothing is sent to it: the cassette
// answers.
const ReplayBaseURL = "http://replay.invalid"

// replayRun names a replay's run, in place of the recording's.
const replayRun = LivePrefix + "0001010000-0000"

// Replay runs the live steps of every kind in liveYAML against the vendor's
// recorded answers (hack/live/cassettes, written by passing live runs)
// instead of its API: no network, no credentials, no build tag, seconds
// rather than minutes. A call the runtime now makes differently from the
// recording fails the kind, naming the call; a kind with no cassette skips.
// Records are not written: a replay says nothing about the API as it is
// now, only about the runtime against the API as it was when recorded.
func Replay(t *testing.T, typesYAML []byte, vendor *rest.Vendor, hooks map[string][]reconciler.Hook, liveYAML []byte) {
	t.Helper()
	f, err := descriptor.Parse(typesYAML)
	if err != nil {
		t.Fatal(err)
	}
	r := newReplay(t, vendor, liveYAML)
	r.runKinds(t, f, hooks)
}

// newReplay is a run whose transport answers from the cassettes; it skips
// when there are none.
func newReplay(t *testing.T, vendor *rest.Vendor, liveYAML []byte) *liveRun {
	t.Helper()
	cfg, err := ParseLiveConfig(liveYAML)
	if err != nil {
		t.Fatal(err)
	}
	dir := cassetteDir()
	if dir == "" {
		t.Skipf("replay: %s=off", EnvLiveCassettes)
	}
	return newReplayFrom(t, vendor, cfg, dir)
}

// newReplayFrom is a run whose transport answers from the cassettes in dir.
func newReplayFrom(t *testing.T, vendor *rest.Vendor, cfg *LiveConfig, dir string) *liveRun {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		t.Skipf("replay: no cassettes in %s yet: a passing live run records them (docs/live.md in krm-rest-runtime)", dir)
	}
	if err != nil {
		t.Fatal(err)
	}

	// Stand-ins: every credential the vendor needs and every {{cred:KEY}}
	// the cassettes name, as replay-KEY; every {{env:VAR}} they name, as
	// replay-env-VAR. An env var no cassette names stays empty, as it was
	// (too short or unset) when recorded.
	creds := map[string]string{}
	envs := map[string]bool{}
	for _, k := range vendor.CredentialKeys {
		creds[k] = "replay-" + k
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		c, err := cassette.Load(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range c.Placeholders() {
			kind, key, _ := strings.Cut(strings.Trim(p, "{}"), ":")
			key, _, _ = strings.Cut(key, "|") // {{cred:KEY|url}}: KEY, encoded
			switch kind {
			case "cred":
				creds[key] = "replay-" + key
			case "env":
				envs[key] = true
			}
		}
	}

	// The recording's answers are already in the cassette: no
	// authentication (an OAuth exchange would reach the network), and no
	// waiting for a replica or a delete to catch up.
	v := *vendor
	v.Authorize, v.AuthorizeE = nil, nil
	r := newLiveRun(cfg, &v, creds, ReplayBaseURL, replayRun)
	r.cassettes = dir
	r.getenv = func(k string) string {
		if envs[k] {
			return "replay-env-" + k
		}
		return ""
	}
	// A clock that only sleeping moves: no waiting, and a deadline (a
	// delete still readable, a 409 that never clears) is still reached.
	r.vclock = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	r.sleep = func(d time.Duration) { r.vclock = r.vclock.Add(d) }
	r.player = &cassette.Player{Base: r.api.BaseURL}
	r.player.Values = r.values("")
	r.setTransport(r.player)
	return r
}
