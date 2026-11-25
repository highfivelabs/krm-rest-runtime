// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/highfivelabs/krm-rest-runtime/cassette"
	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	apifake "github.com/highfivelabs/krm-rest-runtime/fake"
	"github.com/highfivelabs/krm-rest-runtime/reconciler"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// EnvFakeRecord=1 makes Rehearse record its cassettes again, against the
// fake API, instead of replaying them.
const EnvFakeRecord = "KRM_FAKE_RECORD"

// FakeCassetteDir is where Rehearse keeps its cassettes: apart from
// hack/live/cassettes, which hold only what the real API answered.
const FakeCassetteDir = "hack/fake/cassettes"

// FakeTarget is every rehearsal cassette's target: what answered.
const FakeTarget = "krm-rest-runtime fake API (built from types.yaml; not the vendor's API)"

// rehearsalDate is every rehearsal cassette's recording date, so that
// recording again changes nothing unless a call or an answer changed.
var rehearsalDate = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// Rehearse runs the live steps (create, read back, update, drift repair,
// delete) of every kind against the fake API: the kinds liveYAML lists,
// with their live specs, then every other kind with its sample. It needs no
// account, so it covers the kinds no live run has reached yet.
//
// With KRM_FAKE_RECORD=1 it records each kind's calls and the fake's
// answers to hack/fake/cassettes/<Kind>.<group>.json; otherwise it replays
// them as strictly as Replay does a live run's, so a change in what the
// runtime sends fails the kind, naming the call. The cassettes are a full
// request and response example of every kind to build against, in the same
// format and with the same placeholders as a live run's; their target says
// the fake answered them. They prove only that the runtime and the
// descriptors agree, nothing about the real API: no records are written and
// no kind's readiness changes.
func Rehearse(t *testing.T, typesYAML []byte, vendor *rest.Vendor, hooks map[string][]reconciler.Hook, liveYAML []byte) {
	t.Helper()
	f, err := descriptor.Parse(typesYAML)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := rehearsalConfig(f, liveYAML)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.FromSlash(FakeCassetteDir)
	if d := os.Getenv(EnvLiveCassettes); d != "" && d != "off" {
		dir = d
	}

	if os.Getenv(EnvFakeRecord) != "1" {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			t.Skipf("rehearsal: no cassettes in %s yet: record them with %s=1 go test -run %s", dir, EnvFakeRecord, t.Name())
		}
		r := newReplayFrom(t, vendor, cfg, dir)
		r.rehearsal = true
		r.runKinds(t, f, hooks)
		return
	}

	// Record: the fake API, credentials that are only stand-ins, the run's
	// name fixed and a clock that only sleeping moves (the fake answers at
	// once; a deadline is still reached).
	stub := &setupStub{fake: apifake.New(f), nextID: 280000000}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	creds := map[string]string{}
	for _, k := range vendor.CredentialKeys {
		creds[k] = "rehearsal-" + k
	}
	for _, m := range credRE.FindAllStringSubmatch(string(liveYAML), -1) {
		creds[m[1]] = "rehearsal-" + m[1]
	}
	v := *vendor
	v.Authorize, v.AuthorizeE = nil, nil // no token exchange: the fake takes any credential
	r := newLiveRun(cfg, &v, creds, srv.URL, replayRun)
	r.rehearsal = true
	r.stub = stub
	r.target = FakeTarget
	r.getenv = func(k string) string { return "rehearsal-env-" + k }
	r.vclock = rehearsalDate
	r.sleep = func(d time.Duration) { r.vclock = r.vclock.Add(d) }
	if err := os.RemoveAll(dir); err != nil { // a kind that is gone leaves no cassette behind
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.cassettes = dir
	r.recorder = &cassette.Recorder{Base: r.api.BaseURL}
	r.setTransport(r.recorder)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(fakeReadme), 0o644); err != nil {
		t.Fatal(err)
	}
	r.runKinds(t, f, hooks)
}

// credRE finds the {{cred:KEY}} templates a live.yaml names.
var credRE = regexp.MustCompile(`\{\{cred:([A-Za-z0-9_]+)`)

// rehearsalConfig is liveYAML with every kind of f: those it lists first,
// in its order (a skipped one too: its prerequisite is the real account's;
// one listed twice, once),
// then the rest with their samples, in types.yaml's order.
func rehearsalConfig(f *descriptor.File, liveYAML []byte) (*LiveConfig, error) {
	cfg := &LiveConfig{}
	if len(liveYAML) > 0 {
		c, err := ParseLiveConfig(liveYAML)
		if err != nil {
			return nil, err
		}
		cfg = c
	}
	out := *cfg
	out.Target = FakeTarget
	out.Version = nil
	out.Kinds = nil
	listed := map[string]bool{}
	for _, lk := range cfg.Kinds {
		k := findKind(f, &lk)
		if k == nil {
			return nil, fmt.Errorf("live.yaml lists %s.%s, which types.yaml does not have", lk.Kind, lk.Group)
		}
		if listed[kindKey(k)] {
			continue // listed again (with a skip): rehearsed once, as first listed
		}
		if k.IsDraft() {
			return nil, fmt.Errorf("live.yaml lists %s", k.DraftReason())
		}
		listed[kindKey(k)] = true
		lk.Skip = ""
		lk.Group = k.Group
		out.Kinds = append(out.Kinds, lk)
	}
	// Drafts last, each skipped with its reason: the kinds before them keep
	// their places (a run's object names count them).
	for _, drafts := range []bool{false, true} {
		for i := range f.Kinds {
			k := &f.Kinds[i]
			if !listed[kindKey(k)] && k.IsDraft() == drafts {
				out.Kinds = append(out.Kinds, LiveKind{Kind: k.Kind, Group: k.Group})
			}
		}
	}
	return &out, nil
}

const fakeReadme = `# Rehearsal cassettes (fake API)

Written by krm-rest-runtime's kindtest.Rehearse (KRM_FAKE_RECORD=1 go test
-run TestEveryKindRehearsal .), replayed by the same test in every go test.

Each file is one kind's live steps (create, read back, update, drift repair,
delete) as the runtime sends them and as the runtime's fake API answers: a
full request and response example of every kind, in the same format and with
the same placeholders as a live run's cassettes (hack/live/cassettes).

The fake answers what the descriptors say, not what the vendor does: these
are not recordings of the real API, and no kind's readiness depends on
them. Where a kind has a live cassette, that one shows the real answers.
`

// setupStub is the fake API a rehearsal records against. A live run's setup
// calls (live.yaml before and after: a project to work in, a user to
// assign, the caller's own user) make what a real account needs before a
// kind can be tried; the fake needs none of it, and has no route for much
// of it. So while one is made, a stand-in answers instead of the fake: a
// write echoes its body with an ID, a read answers an ID, a delete answers
// 204, each marked as a stand-in. The kinds' own calls are the fake's alone.
type setupStub struct {
	fake   http.Handler
	on     atomic.Bool
	mu     sync.Mutex
	nextID int
}

// StubMarker marks a stand-in's answer.
const StubMarker = "rehearsal stand-in: the fake API has no such resource"

func (s *setupStub) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if !s.on.Load() {
		s.fake.ServeHTTP(w, req)
		return
	}
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	if req.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	out := map[string]any{}
	if m, ok := decodeObject(body); ok {
		out = m
	}
	s.mu.Lock()
	s.nextID++
	if _, ok := out["id"]; !ok {
		out["id"] = s.nextID
	}
	s.mu.Unlock()
	out["_stub"] = StubMarker
	status := http.StatusOK
	if req.Method == http.MethodPost {
		status = http.StatusCreated
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(out)
}

func decodeObject(data []byte) (map[string]any, bool) {
	var m map[string]any
	if len(data) == 0 || json.Unmarshal(data, &m) != nil {
		return nil, false
	}
	return m, true
}
