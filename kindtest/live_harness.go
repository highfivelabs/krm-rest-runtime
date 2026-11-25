// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package kindtest

// The steps of a live run, shared by Live (the real API, live_run.go,
// compiled only with -tags live) and Replay (a run's cassettes, replay.go).
// Nothing here reads a credential: Live hands it the credentials it read;
// Replay hands it stand-ins and a transport that never reaches a network.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/go-logr/logr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/highfivelabs/krm-rest-runtime/cassette"
	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	apifake "github.com/highfivelabs/krm-rest-runtime/fake"
	"github.com/highfivelabs/krm-rest-runtime/reconciler"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// liveRun is one live run's (or replay's) shared state.
type liveRun struct {
	cfg     *LiveConfig
	vendor  *rest.Vendor
	creds   map[string]string
	baseURL string
	id      string
	target  string
	vars    map[string]any
	api     *rest.Client

	// transport carries every call to the API, the reconciler's included:
	// a cassette.Recorder (live, recording) or a cassette.Player (replay).
	transport http.RoundTripper
	recorder  *cassette.Recorder
	player    *cassette.Player
	cassettes string // the cassettes' directory; "" records none
	// The run's clock. Live: deadlines on the wall clock, {{date:N}} from
	// the time a segment began (so it is the date its Recorded says).
	// Replay: a clock that sleep advances (no waiting, deadlines still
	// reached), {{date:N}} from the recording's Recorded.
	vclock   time.Time // replay: the virtual clock; zero in live runs
	dateBase time.Time // {{date:N}}'s today
	segStart time.Time // live: when the segment began (its cassette's Recorded)
	sleep    func(time.Duration)
	beforeOK bool // live: the run's before calls passed, so its after cassette may be written
	// getenv fills {{env:VAR}} (os.Getenv; a replay's stand-ins); envUsed
	// is every VAR a template named, so its value is scrubbed like a
	// credential's.
	getenv  func(string) string
	envUsed map[string]bool
	// rehearsal: a run against the fake API (Rehearse), not the vendor's:
	// it writes no records (readiness is the real API's alone).
	rehearsal bool
	// kindCreds: a kind on another Connection (LiveKind.Connection) gets
	// its Secret from these; the run's creds are still scrubbed.
	kindCreds map[string]string
	// secretFields: the running kind's secret outputs and the fields it
	// names to redact (descriptor's redact), redacted in its cassette
	// whatever their names (a TURN app's "key", a Vault token's accessor).
	secretFields []string
	// known: every value the run's redaction took out whole, redacted
	// wherever else it is, in every cassette, log line and record. A
	// rehearsal's is each segment's own: the fake answers the same sample
	// values to every kind, and they are no secrets.
	known *cassette.Known
	// written: the cassettes this run wrote, redacted again at its end
	// with what later segments taught known.
	written []string
	stub    *setupStub // a rehearsal's recording: answers setup calls the fake has no route for

	mu      sync.Mutex
	records map[string]LiveRecord
}

// newLiveRun is a run with the credentials it was given; id names it
// (empty: hfl-live-<UTC YYMMDDhhmm>-<4 hex>).
func newLiveRun(cfg *LiveConfig, vendor *rest.Vendor, creds map[string]string, baseURL, id string) *liveRun {
	r := &liveRun{cfg: cfg, vendor: vendor, creds: creds, baseURL: baseURL, id: id, vars: map[string]any{}, records: map[string]LiveRecord{}, sleep: time.Sleep, getenv: os.Getenv, envUsed: map[string]bool{}, known: cassette.NewKnown()}
	if r.id == "" {
		b := make([]byte, 2)
		_, _ = rand.Read(b)
		r.id = LivePrefix + time.Now().UTC().Format("0601021504") + "-" + hex.EncodeToString(b)
	}
	r.api = rest.New(vendor, baseURL, creds)
	r.target = cfg.Target
	r.known.Identify(cfg.identities())
	return r
}

// versionCall appends the API's version to the target (live.yaml version:).
func (r *liveRun) versionCall(t *testing.T) {
	v := r.cfg.Version
	if v == nil {
		return
	}
	resp, err := r.api.Do(context.Background(), orGet(v.Method), v.Path, nil)
	if err == nil {
		r.target = strings.TrimSpace(r.target + " " + fmt.Sprint(descriptor.Dig(resp, v.Field)))
	} else {
		t.Logf("live: version call %s: %v", v.Path, err)
	}
}

func orGet(m string) string {
	if m == "" {
		return "GET"
	}
	return m
}

// safe is text for the test log: scrubbed and redacted like a cassette. A
// failure quotes the API's answers, and a scheduled run's log is as public
// as its repository.
func (r *liveRun) safe(name, text string) string {
	r.learn(name)
	return r.known.Redact(cassette.Redact(r.values(name).Scrub(text)))
}

// learn adds to known what the segment's calls so far will have redacted
// whole when its cassette is written, so a failure that quotes one (logged,
// or in a record, before the cassette is written) is redacted as it will be.
// A replay's Player adds what the replay sends.
func (r *liveRun) learn(name string) {
	if r.recorder == nil {
		return
	}
	if ins := r.recorder.Peek(); len(ins) > 0 {
		(&cassette.Cassette{Interactions: ins}).ScrubbedWith(r.values(name), r.secretFields, r.known)
	}
}

// now is the clock deadlines are measured on.
func (r *liveRun) now() time.Time {
	if !r.vclock.IsZero() {
		return r.vclock
	}
	return time.Now()
}

// today is {{date:N}}'s base.
func (r *liveRun) today() time.Time {
	if !r.dateBase.IsZero() {
		return r.dateBase
	}
	return time.Now()
}

// stalled: a replay already failed, so waiting for the API to change is
// pointless (every call answers the same error).
func (r *liveRun) stalled() bool { return r.player != nil && r.player.Err() != nil }

// setTransport sends every call, the reconciler's too, through rt.
func (r *liveRun) setTransport(rt http.RoundTripper) {
	r.transport = rt
	r.api.HTTP.Transport = rt
}

// newClient is the reconciler's client: rest.New, through the run's transport.
func (r *liveRun) newClient(v *rest.Vendor, baseURL string, creds map[string]string) *rest.Client {
	c := rest.New(v, baseURL, creds)
	if r.transport != nil {
		c.HTTP.Transport = r.transport
	}
	return c
}

// cassetteDir is where cassettes are kept ($KRM_LIVE_CASSETTES, default
// hack/live/cassettes); "" when KRM_LIVE_CASSETTES=off.
func cassetteDir() string {
	d := os.Getenv(EnvLiveCassettes)
	switch d {
	case "off":
		return ""
	case "":
		return filepath.Join("hack", "live", "cassettes")
	}
	return d
}

// recordCassettes records the run's calls; each passing kind's are written
// to its cassette.
func (r *liveRun) recordCassettes() {
	r.cassettes = cassetteDir()
	if r.cassettes == "" {
		return
	}
	r.recorder = &cassette.Recorder{Base: r.api.BaseURL}
	r.setTransport(r.recorder)
}

// values is the run's placeholders and their values: the credentials and
// the {{env:VAR}} values a template used (each in every encoding), the base
// URL and its host's tenant label, the run's names and (for a kind) the
// kind's names. Recording, they are the real values to hide; replaying, the
// stand-ins to fill in.
func (r *liveRun) values(name string) *cassette.Scrubber {
	s := cassette.NewScrubber()
	if name != "" {
		s.Add(name, "{{name}}")
		s.Add(strings.ReplaceAll(name, "-", "_"), "{{name_}}")
		s.Add(strings.ToUpper(name), "{{NAME}}")
		s.Add(strings.ToUpper(strings.ReplaceAll(name, "-", "_")), "{{NAME_}}")
	}
	s.Add(r.id, "{{run}}")
	s.Add(strings.ReplaceAll(r.id, "-", "_"), "{{run_}}")
	s.Add(strings.ToUpper(r.id), "{{RUN}}")
	s.Add(strings.ToUpper(strings.ReplaceAll(r.id, "-", "_")), "{{RUN_}}")
	secrets := r.secrets()
	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		keys = append(keys, k)
	}
	sort.Strings(keys) // the first placeholder for a value wins: the same one every run
	for _, k := range keys {
		s.AddSecret(secrets[k], "{{"+k+"}}")
	}
	s.Add(r.api.BaseURL, "{{baseURL}}")
	if t := r.tenant(); t != "" {
		s.Add(t, "{{tenant}}")
	}
	return s
}

// secrets are the run's credentials and used {{env:VAR}} values, by
// placeholder name (cred:KEY, env:VAR).
func (r *liveRun) secrets() map[string]string {
	out := map[string]string{}
	for k, v := range r.creds {
		out["cred:"+k] = v
	}
	r.mu.Lock()
	envs := make([]string, 0, len(r.envUsed))
	for k := range r.envUsed {
		envs = append(envs, k)
	}
	r.mu.Unlock()
	for _, k := range envs {
		if v := r.getenv(k); v != "" {
			out["env:"+k] = v
		}
	}
	return out
}

// tenant is the base URL host's first label when it names an account
// (integrator-1234567 in integrator-1234567.okta.com: a hyphen or a digit,
// eight characters or more), which the API also uses in other hosts
// (integrator-1234567-admin.okta.com) and names. A replay's is
// replay-tenant.
func (r *liveRun) tenant() string {
	if r.player != nil {
		return "replay-tenant"
	}
	u, err := url.Parse(r.api.BaseURL)
	if err != nil {
		return ""
	}
	label, _, _ := strings.Cut(u.Hostname(), ".")
	if len(label) >= 8 && strings.ContainsAny(label, "-0123456789") && net.ParseIP(u.Hostname()) == nil {
		return label
	}
	return ""
}

const (
	segBefore = "_before"
	segAfter  = "_after"
)

func kindKey(k *descriptor.Kind) string { return k.Kind + "." + k.Group }

// errNoCassette: a replay of a segment that was never recorded.
var errNoCassette = errors.New("no cassette")

// begin starts a segment (a kind, or the run's before or after calls).
func (r *liveRun) begin(seg, name string, anyBody bool) error {
	switch {
	case r.player != nil:
		c, err := cassette.Load(filepath.Join(r.cassettes, seg+".json"))
		if err != nil {
			return err
		}
		if c == nil {
			return errNoCassette
		}
		if t, err := time.Parse(time.RFC3339, c.Recorded); err == nil {
			r.dateBase = t
		}
		r.mu.Lock()
		for _, p := range c.Placeholders() {
			if k, ok := strings.CutPrefix(strings.Trim(p, "{}"), "env:"); ok {
				k, _, _ = strings.Cut(k, "|")
				r.envUsed[k] = true
			}
		}
		r.mu.Unlock()
		r.player.Values = r.values(name)
		r.player.AnyBody = anyBody
		r.player.Load(c) // first: it gives {{id:N}} its stand-ins, which saved vars may hold
		r.mu.Lock()
		for k, v := range c.Vars {
			if str, ok := v.(string); ok {
				v = r.player.Values.Unscrub(str) // a saved name holds the replay's run, not the recording's
			}
			r.vars[k] = v
		}
		r.mu.Unlock()
		r.target = c.Target
	case r.recorder != nil:
		r.segStart = time.Now().UTC().Truncate(time.Second)
		if !r.vclock.IsZero() {
			r.segStart = r.vclock // a rehearsal: the same date every recording, so re-recording changes nothing
		}
		r.dateBase = r.segStart // {{date:N}} is the date the cassette says
		if r.rehearsal {
			r.known = cassette.NewKnown()
		}
		r.recorder.Start()
	}
	return nil
}

// end ends a segment. Recording, a segment that passed (ok) is written to
// its cassette, unless a secret is left in it; replaying, a call that was
// not the one recorded, or a recorded call never made, fails the test.
func (r *liveRun) end(t *testing.T, seg, name string, ok bool) {
	t.Helper()
	switch {
	case r.player != nil:
		if err := r.player.Err(); err != nil {
			t.Errorf("replay %s: %v\n(the runtime now calls the API differently from the recording: if that is meant, re-record it with a live run, docs/live.md \"Replay\")", seg, err)
		} else if left := r.player.Left(); left > 0 && !t.Failed() {
			next := r.player.Next()
			t.Errorf("replay %s: %d recorded calls were never made; the next was %s %s", seg, left, next.Method, next.Path)
		}
		r.player.Load(nil)
	case r.recorder != nil:
		got := r.recorder.Stop()
		r.dateBase = time.Time{}
		if !ok || ((seg == segBefore || seg == segAfter) && len(got) == 0) {
			return
		}
		if seg == segAfter && !r.beforeOK {
			return // its calls use IDs the before calls saved this run: written together or not at all
		}
		r.mu.Lock()
		vars := map[string]any{}
		for k, v := range r.vars {
			vars[k] = v
		}
		r.mu.Unlock()
		c := &cassette.Cassette{Recorded: r.segStart.Format(time.RFC3339), Target: r.target, Interactions: got}
		if seg != segBefore && len(vars) > 0 {
			c.Vars = vars // a kind's (or the after calls') IDs, as they were when recorded
		}
		if c.Interactions == nil {
			c.Interactions = []cassette.Interaction{}
		}
		out := c.ScrubbedWith(r.values(name), r.secretFields, r.known)
		if leaks := out.Leaks(r.secrets()); len(leaks) > 0 {
			t.Errorf("live: cassette %s not written: a secret would be left in it:\n  %s", seg, strings.Join(leaks, "\n  "))
			// The kind's record said pass; without its cassette it is not one.
			r.mu.Lock()
			if rec, ok := r.records[seg]; ok {
				rec.Result, rec.Reason, rec.LastPass = "fail", "cassette withheld: a secret would be left in it ("+strings.Join(leaks, "; ")+")", ""
				r.records[seg] = rec
			}
			r.mu.Unlock()
			return
		}
		path := filepath.Join(r.cassettes, seg+".json")
		if err := out.Save(path); err != nil {
			t.Errorf("live: writing cassette %s: %v", path, err)
			return
		}
		r.mu.Lock()
		r.written = append(r.written, path)
		r.mu.Unlock()
	}
}

// redactWritten redacts, in every cassette the run wrote, each value a
// later segment's redaction took out (a secret a kind's create answered
// that the before calls' cassette also has).
func (r *liveRun) redactWritten(t *testing.T) {
	r.mu.Lock()
	paths := append([]string(nil), r.written...)
	r.mu.Unlock()
	for _, path := range paths {
		c, err := cassette.Load(path)
		if err != nil || c == nil {
			t.Errorf("live: reading cassette %s again: %v", path, err)
			continue
		}
		if out, changed := r.known.Apply(c); changed {
			if err := out.Save(path); err != nil {
				t.Errorf("live: writing cassette %s: %v", path, err)
			}
		}
	}
}

// runKinds runs the run's before calls, every listed kind, and (whatever
// happened) its after calls.
func (r *liveRun) runKinds(t *testing.T, f *descriptor.File, hooks map[string][]reconciler.Hook) {
	t.Helper()
	ctx := context.Background()
	only := map[string]bool{}
	for _, k := range strings.Split(os.Getenv(EnvLiveKinds), ",") {
		if k = strings.TrimSpace(k); k != "" {
			only[k] = true
		}
	}
	t.Cleanup(func() {
		if r.player != nil {
			defer r.player.Load(nil)
		}
		if err := r.begin(segAfter, "", false); err == nil {
			ok := true
			for _, c := range r.cfg.After {
				if err := r.call(ctx, c); err != nil {
					ok = false
					t.Errorf("live: after %s %s: %s", c.Method, r.expand(c.Path, nil), r.safe("", fmt.Sprint(err)))
				}
			}
			r.end(t, segAfter, "", ok)
		} else if !errors.Is(err, errNoCassette) {
			t.Errorf("replay %s: %v", segAfter, err)
		}
		if r.recorder != nil && !r.rehearsal {
			r.redactWritten(t)
		}
		if r.player == nil && !r.rehearsal {
			r.writeRecords(t)
		}
	})
	switch err := r.begin(segBefore, "", false); {
	case errors.Is(err, errNoCassette) && len(r.cfg.Before) > 0:
		t.Skipf("replay: no %s.json in %s: a live run records it", segBefore, r.cassettes)
	case err != nil && !errors.Is(err, errNoCassette):
		t.Fatal(err)
	}
	for _, c := range r.cfg.Before {
		if err := r.call(ctx, c); err != nil {
			r.end(t, segBefore, "", false)
			t.Fatalf("live: before %s %s: %s", c.Method, r.expand(c.Path, nil), r.safe("", fmt.Sprint(err)))
		}
	}
	r.end(t, segBefore, "", true)
	r.beforeOK = true
	for i := range r.cfg.Kinds {
		lk := &r.cfg.Kinds[i]
		k := findKind(f, lk)
		if k == nil {
			t.Errorf("live.yaml lists %s.%s, which types.yaml does not have", lk.Kind, lk.Group)
			continue
		}
		name := fmt.Sprintf("%s-%02d", r.id, i+1)
		t.Run(kindKey(k), func(t *testing.T) {
			if k.IsDraft() {
				t.Skip(k.DraftReason())
			}
			if len(only) > 0 && !only[k.Kind] {
				t.Skipf("not in %s", EnvLiveKinds)
			}
			if lk.Skip != "" {
				t.Skip(lk.Skip)
			}
			seg := kindKey(k)
			if lk.Connection != "" {
				restore, skip := r.useConnection(lk.Connection)
				if skip != "" {
					t.Skip(skip)
				}
				t.Cleanup(restore) // registered before the segment's end: runs after it
			}
			if missing := r.missingCreds(lk); len(missing) > 0 {
				t.Skipf("live: secret %s lacks %s, which this kind's live.yaml entry names", r.cfg.Secret, strings.Join(missing, ", "))
			}
			r.secretFields = append(append([]string(nil), k.SecretOutputs...), k.Redact...)
			t.Cleanup(func() { r.secretFields = nil }) // after the segment's end, which uses them
			if err := r.begin(seg, name, lk.ReplayAnyBody != ""); errors.Is(err, errNoCassette) {
				t.Skipf("replay: no cassette %s.json in %s: a passing live run of the kind records it", seg, r.cassettes)
			} else if err != nil {
				t.Fatal(err)
			}
			// Registered first, so it runs last: after the kind's own
			// cleanups (its after calls, a forced delete).
			t.Cleanup(func() { r.end(t, seg, name, !t.Failed() && !t.Skipped()) })
			r.one(t, f, k, lk, name, hooks[k.Kind])
		})
	}
}

// one runs one kind's live steps.
func (r *liveRun) one(t *testing.T, f *descriptor.File, k *descriptor.Kind, lk *LiveKind, name string, hooks []reconciler.Hook) {
	// The reconciler logs to its context's logger: none here (a test reads
	// the steps' results, not the reconciler's log).
	ctx := ctrllog.IntoContext(context.Background(), logr.Discard())
	var steps []string
	reason := ""
	failf := func(format string, args ...any) {
		t.Helper()
		reason = fmt.Sprintf(format, args...)
		t.Fatal(r.safe(name, reason)) // a reason quotes the API: the log is public too
	}
	defer func() {
		if t.Skipped() || r.player != nil || r.rehearsal {
			return
		}
		r.record(k, name, !t.Failed(), steps, reason)
	}()

	local := map[string]string{
		"name":  name,
		"name_": strings.ReplaceAll(name, "-", "_"),
		"NAME":  strings.ToUpper(name),
		"NAME_": strings.ToUpper(strings.ReplaceAll(name, "-", "_")),
	}
	t.Cleanup(func() {
		for _, c := range lk.After {
			if err := r.callLocal(ctx, c, local); err != nil {
				t.Errorf("after %s %s: %s", c.Method, c.Path, r.safe(name, fmt.Sprint(err)))
			}
		}
	})
	for _, c := range lk.Before {
		if err := r.callLocal(ctx, c, local); err != nil {
			failf("before %s %s: %v", c.Method, r.expand(c.Path, local), err)
		}
	}

	spec := map[string]any{}
	if !lk.ReplaceSample {
		spec = runtime.DeepCopyJSONValue(map[string]any(k.Sample)).(map[string]any)
		if spec == nil {
			spec = map[string]any{}
		}
	}
	for f, v := range lk.Spec {
		if v == nil {
			delete(spec, f)
			continue
		}
		spec[f] = r.template(v, local)
	}
	if r.stub != nil {
		// A rehearsal: the resource an element goes in, which the account
		// has already (the run's repository's custom property values), is
		// where this spec names it, not only where the sample does.
		if api, ok := r.stub.fake.(*apifake.Server); ok {
			api.HoldFor(k, spec)
		}
	}

	// The Kubernetes side is in memory; only the vendor API is real.
	gvk := k.GVK()
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(gvk, meta.RESTScopeNamespace)
	mapper.Add(f.ConnectionGVK(), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	data := map[string][]byte{}
	creds := r.creds
	if r.kindCreds != nil {
		creds = r.kindCreds
	}
	for key, v := range creds {
		data[key] = []byte(v)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "creds"}, Data: data}
	conn := &unstructured.Unstructured{}
	conn.SetGroupVersionKind(f.ConnectionGVK())
	conn.SetNamespace(ns)
	conn.SetName("default")
	_ = unstructured.SetNestedField(conn.Object, r.api.BaseURL, "spec", "baseURL")
	_ = unstructured.SetNestedField(conn.Object, "creds", "spec", "secretRef", "name")
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(ns)
	obj.SetName("sample")
	obj.Object["spec"] = spec
	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).
		WithObjects(secret, conn, obj).WithStatusSubresource(obj).Build()
	rec := &reconciler.Reconciler{Client: c, Kind: k, Vendor: r.vendor, Connection: f.ConnectionGVK(), Hooks: hooks, NewClient: r.newClient, Kinds: reconciler.KindsByName(f)}
	if !r.vclock.IsZero() { // a replay or a rehearsal: nothing to wait for
		rec.Wait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
		if k.Rotate != nil {
			// A token's name and expiry are of the run's clock: the same
			// in every recording and its replay.
			rec.Now = r.now
		}
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "sample"}}

	// Whatever happens, what was made is deleted.
	var params map[string]string
	created, deleted := false, false
	var original map[string]any
	t.Cleanup(func() {
		if !created || deleted {
			return
		}
		if err := r.forceDelete(ctx, k, params, spec, original); err != nil && k.Shape == descriptor.Settings {
			t.Logf("cleanup: could not put back the settings as they were: %v", err)
		} else if err != nil {
			t.Errorf("cleanup: %s may be left behind (the next run sweeps it if named %s...): %s", k.Kind, LivePrefix, r.safe(name, fmt.Sprint(err)))
		}
	})

	get := func(step string) *unstructured.Unstructured {
		t.Helper()
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvk)
		if err := c.Get(ctx, req.NamespacedName, got); err != nil {
			failf("%s: get: %v", step, err)
		}
		return got
	}
	// reconcile runs the controller once and returns the Ready reason.
	var busy time.Duration
	if lk.BusyWait != "" {
		busy, _ = time.ParseDuration(lk.BusyWait)
	}
	reconcile := func(step string) (*unstructured.Unstructured, string) {
		t.Helper()
		var got *unstructured.Unstructured
		var cond map[string]any
		settleBy := r.now().Add(settleWait(k))
		for deadline := r.now().Add(busy); ; {
			if _, err := rec.Reconcile(ctx, req); err != nil {
				got := get(step)
				failf("%s: reconcile: %v (%s)", step, err, condMessage(got))
			}
			got = get(step)
			conds, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
			if len(conds) == 0 {
				failf("%s: no Ready condition", step)
			}
			cond, _ = conds[0].(map[string]any)
			// An asynchronous object waits for its status (sql.settle).
			if cond["reason"] == reconciler.SettlingReason && !r.now().After(settleBy) && !r.stalled() {
				r.sleep(reconciler.SettleRetry)
				continue
			}
			// Still being made: the API refuses changes until it is (busyWait).
			if cond["status"] == "True" || step == "create" || !strings.Contains(fmt.Sprint(cond["message"]), "HTTP 409") || r.now().After(deadline) || r.stalled() {
				break
			}
			t.Logf("%s: busy (409), retrying", step)
			r.sleep(10 * time.Second)
		}
		if cond["status"] != "True" {
			failf("%s: not Ready: %v: %v", step, cond["reason"], cond["message"])
		}
		reason, _ := cond["reason"].(string)
		return got, reason
	}
	wantReason := func(step, got string, want ...string) {
		t.Helper()
		for _, w := range want {
			if got == w {
				return
			}
		}
		failf("%s: Ready reason %s, want %v%s", step, got, want, r.differs(ctx, k, params, spec))
	}
	paramsOf := func(got *unstructured.Unstructured) map[string]string {
		p := map[string]string{}
		for _, pp := range k.Params {
			p[pp.Name] = fmt.Sprint(pp.In(spec))
		}
		if k.ID != nil {
			if id, _, _ := unstructured.NestedString(got.Object, "status", "id"); id != "" {
				p[k.ID.Param] = id
			}
		}
		if k.SQL != nil {
			if id, _, _ := unstructured.NestedString(got.Object, "status", "id"); id != "" {
				p["@id"] = id
			}
		}
		_ = k.FillQuery(spec, p)
		return p
	}

	if k.Shape == descriptor.Settings && k.Unread == "" {
		params = paramsOf(obj)
		if o, err := r.readObject(ctx, k, params, spec); err == nil {
			original = o
		} else if !errors.Is(err, rest.ErrNotFound) {
			failf("settings: reading the current settings to restore them later: %v", err)
		}
	}

	// 1. Create, then read back: nothing should differ.
	created = true
	got, _ := reconcile("create")
	params = paramsOf(got)
	steps = append(steps, "create")
	got, why := reconcile("read")
	wantReason("read (right after create: the API should answer what was sent)", why, "Unchanged")
	steps = append(steps, "read")

	// 2. Update, and 3. drift repair, where the kind can be updated.
	if lk.NoUpdate != "" {
		t.Logf("update: not run: %s", lk.NoUpdate)
	} else if (k.Update != nil && k.Shape != descriptor.Assoc && k.Shape != descriptor.SetShape) || sqlUpdatable(k) {
		change := map[string]any{}
		for f, v := range lk.Change {
			change[f] = r.template(v, local)
		}
		if len(change) == 0 {
			if f := changeable(k, spec); f != "" {
				change[f] = "changed-" + f
			}
		}
		if len(change) == 0 {
			t.Logf("update: no field to change (set change: in live.yaml)")
		} else {
			fields := make([]string, 0, len(change))
			for f, v := range change {
				_ = unstructured.SetNestedField(got.Object, runtime.DeepCopyJSONValue(v), "spec", f)
				spec[f] = v
				fields = append(fields, f)
			}
			sort.Strings(fields)
			if err := c.Update(ctx, got); err != nil {
				failf("update: %v", err)
			}
			_, why = reconcile("update")
			wantReason("update", why, "Updated")
			_, why = reconcile("read after update")
			wantReason("read after update", why, "Unchanged")
			steps = append(steps, "update")

			field := fields[0]
			drift := lk.Drift
			if drift == nil {
				if _, ok := change[field].(string); ok {
					drift = "hfl-live-drift"
				}
			}
			switch {
			case lk.NoDrift != "":
				t.Logf("drift: not run: %s", lk.NoDrift)
			case k.Unread != "":
				t.Logf("drift: not run: the kind never reads the resource: %s", k.Unread)
			case drift == nil:
				t.Logf("drift: not run: %s is not a string; set drift: in live.yaml", field)
			default:
				// Change it behind the controller's back, the way a person in
				// the console would, and check the controller puts it back.
				if err := r.outOfBand(ctx, k, params, spec, field, r.template(drift, local), lk.DriftFull); err != nil {
					failf("drift: changing %s behind the controller's back: %v", field, err)
				}
				_, why = reconcile("drift")
				wantReason("drift (the controller should see the change and put it back)", why, "Updated")
				_, why = reconcile("read after drift")
				wantReason("read after drift", why, "Unchanged")
				steps = append(steps, "drift")
			}
		}
	}

	// 4. Delete, and check it is gone.
	got = get("delete")
	if err := c.Delete(ctx, got); err != nil {
		failf("delete: %v", err)
	}
	for settleBy := r.now().Add(settleWait(k)); ; {
		if _, err := rec.Reconcile(ctx, req); err != nil {
			failf("delete: reconcile: %v", err)
		}
		// An asynchronous object is torn down before it is dropped.
		if err := c.Get(ctx, req.NamespacedName, got); err != nil || k.SQL == nil || k.SQL.Settle == nil || r.now().After(settleBy) || r.stalled() {
			break
		}
		r.sleep(reconciler.SettleRetry)
	}
	if err := c.Get(ctx, req.NamespacedName, got); err == nil {
		failf("delete: the object still exists (%s)", condMessage(got))
	}
	switch {
	case lk.Stays != "":
		deleted = true
		t.Logf("delete: not checked gone: %s", lk.Stays)
		steps = append(steps, "delete")
	case k.Stays != "":
		deleted = true
		t.Logf("delete: not checked gone: %s", k.Stays)
		steps = append(steps, "delete")
	case k.StateDeletes():
		deleted = true // left in the API, in the state deleting the object puts it in
		t.Logf("delete: left in the API, %s %s", k.State.Field, k.State.Delete)
		steps = append(steps, "delete")
	case k.Unread != "":
		deleted = true // never read: left as it is, as written last
		t.Logf("delete: left as it is: %s", k.Unread)
		steps = append(steps, "delete")
	case k.Forget != "":
		deleted = true // let go, not deleted: the API cannot delete it
		t.Logf("delete: let go, left in the API: %s", k.Forget)
		steps = append(steps, "delete")
	case k.Shape == descriptor.Settings:
		deleted = false // settings stay: the cleanup restores what was there
		steps = append(steps, "delete")
	case k.Shape == descriptor.Assoc:
		deleted = true
		steps = append(steps, "delete")
	case k.SQL.PerField():
		// What the settings are on stays: gone is each field it set unset.
		obs, err := r.readObject(ctx, k, params, spec)
		if err != nil && !errors.Is(err, rest.ErrNotFound) {
			failf("delete: reading the settings: %v", err)
		}
		for f := range bodyOf(k, spec) {
			if k.SQL.Settable(f) && k.SQL.Readable(f) && obs[f] != nil {
				failf("delete: %s is still set: %v", f, obs[f])
			}
		}
		deleted = true
		steps = append(steps, "delete", "gone")
	case k.Shape == descriptor.SetShape:
		// Gone: none of its elements is left.
		left, err := reconciler.ListSet(ctx, r.api, k, params)
		if err != nil && !errors.Is(err, rest.ErrNotFound) {
			failf("delete: listing the elements: %v", err)
		}
		want, _ := reconciler.SetElements(k, bodyOf(k, spec))
		for _, e := range want {
			key, _ := k.Set.KeyOf(e)
			if _, ok := left[key]; ok {
				failf("delete: %s %v is still in the API", k.Set.KeyNames(), key)
			}
		}
		deleted = true
		steps = append(steps, "delete", "gone")
	default:
		wait := 60 * time.Second
		if lk.DeleteWait != "" {
			if d, err := time.ParseDuration(lk.DeleteWait); err == nil {
				wait = d
			}
		}
		deadline := r.now().Add(wait)
		for {
			_, err := r.readObject(ctx, k, params, spec)
			if errors.Is(err, rest.ErrNotFound) {
				break
			}
			if r.stalled() {
				failf("delete: %v", err)
			}
			if r.now().After(deadline) {
				failf("delete: still readable after %s (err %v)", wait, err)
			}
			r.sleep(2 * time.Second)
		}
		deleted = true
		steps = append(steps, "delete", "gone")
	}
}

// differs says which field the API answers differently from what was sent
// (why a read reports drift right after a write).
func (r *liveRun) differs(ctx context.Context, k *descriptor.Kind, params map[string]string, spec map[string]any) string {
	obs, err := r.readObject(ctx, k, params, spec)
	if err != nil {
		return fmt.Sprintf(" (reading it: %v)", err)
	}
	body := bodyOf(k, spec)
	skip := func(f string) bool {
		_, returned := obs[f]
		return k.IsWriteOnly(f) || (k.CompareReturnedOnly && !strings.Contains(f, ".") && !returned)
	}
	f := reconciler.Drift(body, reconciler.Normalize(k, body, obs), skip)
	if f == "" {
		return ""
	}
	top := strings.SplitN(strings.SplitN(f, ".", 2)[0], "[", 2)[0]
	sent, _ := json.Marshal(body[top])
	got, _ := json.Marshal(obs[top])
	return fmt.Sprintf(": the API answers %s = %s, sent %s", top, got, sent)
}

func condMessage(o *unstructured.Unstructured) string {
	conds, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	if len(conds) == 0 {
		return "no conditions"
	}
	c, _ := conds[0].(map[string]any)
	return fmt.Sprintf("%v: %v", c["reason"], c["message"])
}

// readObject reads a kind's object the way the reconciler does.
func (r *liveRun) readObject(ctx context.Context, k *descriptor.Kind, params map[string]string, spec map[string]any) (map[string]any, error) {
	if k.SQL != nil {
		return r.sqlRead(ctx, k, params, spec)
	}
	if k.Read.GraphQL != nil {
		return reconciler.GraphQLRead(ctx, r.api, k, params)
	}
	if k.Element != nil {
		return reconciler.ReadElement(ctx, r.api, k, params, bodyOf(k, spec))
	}
	path, err := rest.Expand(k.Read.Path, params)
	if err != nil {
		return nil, err
	}
	if fd := k.Read.Find; fd != nil {
		return reconciler.FindPagedWith(ctx, r.api, k, rest.WithQuery(path, k.Read.QueryOf(params)), params[fd.Param], params)
	}
	resp, err := r.api.DoWith(ctx, k.Read.Method, rest.WithQuery(path, k.Read.QueryOf(params)), reconciler.ReadBody(k, params), k.Read.HeadersOf(params))
	if err != nil {
		return nil, err
	}
	obs := reconciler.UnwrapResponse(k, resp)
	if k.Read.IsAbsent(obs) {
		return nil, rest.ErrNotFound
	}
	return obs, nil
}

// outOfBand writes field=value through the kind's update call, as a person
// editing it in the vendor's console would.
func (r *liveRun) outOfBand(ctx context.Context, k *descriptor.Kind, params map[string]string, spec map[string]any, field string, value any, full bool) error {
	if k.SQL != nil {
		return r.sqlOutOfBand(ctx, k, params, spec, field, value)
	}
	if k.Element != nil {
		return r.elementOutOfBand(ctx, k, params, spec, field, value)
	}
	body := map[string]any{field: value}
	if full {
		// A full replace: send what is there now, with the one field changed.
		// Fields a read never returns (Vault's MFA secret_key, api_token) come
		// from the spec, as a person in the console types them in again.
		cur, err := r.readObject(ctx, k, params, spec)
		if err != nil {
			return err
		}
		body = map[string]any{}
		own := map[string]bool{"connectionRef": true, "secretRefs": true, reconciler.WriteSecretRef: true}
		for _, p := range k.Params {
			own[p.Field] = !p.InBody
		}
		for _, f := range k.QueryFields {
			own[f] = true
		}
		for f, v := range spec {
			if !own[f] {
				body[f] = v
			}
		}
		for f, v := range cur {
			body[f] = v
		}
		body[field] = value
	}
	for _, p := range k.Params {
		if p.InBody && p.In(body) == nil {
			// It names the object (Datadog's v1 Azure integration, by
			// tenant_name; a custom forecast, by attributes.budget_uid).
			body = descriptor.MergeAt(body, strings.ReplaceAll(p.Field, ".", "/"), p.In(spec))
		}
	}
	body = descriptor.WithoutFields(body, k.CreateOnly) // the update refuses them, as it does the controller's
	if len(k.Update.Carry) > 0 {
		// What the update must send as the API has it (a resourceVersion),
		// read as the console does before it saves.
		cur, err := r.readObject(ctx, k, params, spec)
		if err != nil {
			return err
		}
		body = reconciler.Carried(k.Update, body, cur, nil)
	}
	if full {
		for _, f := range k.OmitUnchanged {
			if f != field {
				delete(body, f)
			}
		}
	}
	if k.Update.GraphQL != nil {
		_, err := reconciler.GraphQLCall(ctx, r.api, k.Update, params, body)
		return err
	}
	path, err := rest.Expand(k.Update.Path, params)
	if err != nil {
		return err
	}
	params = r.withCurrent(ctx, k, params)
	_, err = r.api.DoWith(ctx, k.Update.Method, rest.WithQuery(path, k.Update.QueryOf(params)), reconciler.Request(k, k.Update, reconciler.OpBody(k.Update, reconciler.WithUpdateID(k, body, params), params, nil)), k.Update.HeadersOf(params))
	return err
}

// elementOutOfBand changes field of an Element kind's element to value,
// in the resource holding it, as a person editing that resource would.
func (r *liveRun) elementOutOfBand(ctx context.Context, k *descriptor.Kind, params map[string]string, spec map[string]any, field string, value any) error {
	resp, err := reconciler.ReadHolder(ctx, r.api, k, params)
	if err != nil {
		return err
	}
	body := bodyOf(k, spec)
	body[field] = value
	send, err := reconciler.ElementWrite(k, resp, body, true)
	if err != nil {
		return err
	}
	path, err := rest.Expand(k.Update.Path, params)
	if err != nil {
		return err
	}
	_, err = r.api.DoWith(ctx, k.Update.Method, rest.WithQuery(path, k.Update.QueryOf(params)), reconciler.Request(k, k.Update, send), k.Update.HeadersOf(params))
	return err
}

// withCurrent is params with the kind's {current.field} templates filled
// from a read of the resource, as the reconciler fills them before an
// update or a delete; params itself where the kind has none.
func (r *liveRun) withCurrent(ctx context.Context, k *descriptor.Kind, params map[string]string) map[string]string {
	if !k.HasCurrent() {
		return params
	}
	out := make(map[string]string, len(params))
	for p, v := range params {
		out[p] = v
	}
	// nil spec: only sql kinds read by their spec, and none has {current.field} templates
	if cur, err := r.readObject(ctx, k, params, nil); err == nil {
		k.FillCurrent(cur, out)
	}
	return out
}

// forceDelete removes what a failed test left: the kind's delete call, or
// for settings, the original values put back.
func (r *liveRun) forceDelete(ctx context.Context, k *descriptor.Kind, params map[string]string, spec, original map[string]any) error {
	if k.SQL != nil {
		return r.sqlForceDelete(ctx, k, params, spec)
	}
	if k.Shape == descriptor.Settings {
		if original == nil || k.Update == nil {
			return nil
		}
		// Only body fields: a path parameter the read also returns (Atlas's
		// cloud, geography) is refused in the body.
		body := map[string]any{}
		for f := range bodyOf(k, spec) {
			if v, ok := original[f]; ok {
				body[f] = v
			}
		}
		if k.Update.GraphQL != nil {
			_, err := reconciler.GraphQLCall(ctx, r.api, k.Update, params, body)
			return err
		}
		path, err := rest.Expand(k.Update.Path, params)
		if err != nil {
			return err
		}
		if k.Order != nil {
			// A reorder call is the order with the call's own body, its
			// {current.field} values from the API's answer now.
			rp, _ := rest.Expand(k.Read.Path, params)
			cur, err := r.api.DoWith(ctx, k.Read.Method, rest.WithQuery(rp, k.Read.QueryOf(params)), nil, k.Read.HeadersOf(params))
			if err != nil {
				return err
			}
			m, _ := cur.(map[string]any)
			body = reconciler.OpBody(k.Update, body, params, m)
		}
		_, err = r.api.DoWith(ctx, k.Update.Method, rest.WithQuery(path, k.Update.QueryOf(params)), reconciler.Request(k, k.Update, body), k.Update.HeadersOf(params))
		return err
	}
	if k.Delete == nil {
		return nil
	}
	if k.Shape == descriptor.SetShape {
		return reconciler.DeleteSet(ctx, r.api, k, params, bodyOf(k, spec))
	}
	if k.Delete.GraphQL != nil {
		if k.ID != nil && params[k.ID.Param] == "" {
			return nil // a CRUD object never given an ID was never made
		}
		if _, err := reconciler.GraphQLCall(ctx, r.api, k.Delete, params, nil); err != nil && !errors.Is(err, rest.ErrNotFound) {
			return err
		}
		return nil
	}
	if k.Shape != descriptor.Assoc {
		if err := reconciler.BeforeDelete(ctx, r.api, k, params); err != nil {
			return err
		}
	}
	path, err := rest.Expand(k.Delete.Path, params)
	if err != nil {
		return err // a CRUD object never given an ID was never made
	}
	var body any
	query := k.Delete.QueryOf(params)
	if k.Shape == descriptor.Assoc {
		// The link the spec describes, undone as the reconciler undoes it.
		link := bodyOf(k, spec)
		if k.Delete.SendBody {
			body = reconciler.Request(k, k.Delete, link)
		} else {
			body = reconciler.Request(k, k.Delete, nil)
		}
		query = k.Delete.QueryFrom(link, params)
	} else {
		params = r.withCurrent(ctx, k, params)
		body = reconciler.DeleteBody(k, params, bodyOf(k, spec))
		if len(k.Delete.Carry) > 0 {
			// What the delete names as the API has it now (a blob sha).
			cur, err := r.readObject(ctx, k, params, spec)
			if errors.Is(err, rest.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			body = reconciler.Request(k, k.Delete, reconciler.Carried(k.Delete, reconciler.DeleteFields(k, params, bodyOf(k, spec)), cur, spec))
		}
	}
	if _, err := r.api.DoWith(ctx, k.Delete.Method, rest.WithQuery(path, query), body, k.Delete.HeadersOf(params)); err != nil && !errors.Is(err, rest.ErrNotFound) {
		return err
	}
	return nil
}

func (r *liveRun) call(ctx context.Context, c LiveCall) error { return r.callLocal(ctx, c, nil) }

func (r *liveRun) callLocal(ctx context.Context, c LiveCall, local map[string]string) error {
	if r.stub != nil {
		r.stub.on.Store(true) // a setup call: a stand-in answers where the fake has no route
		defer r.stub.on.Store(false)
	}
	var body any
	if c.Body != nil {
		body = r.template(runtime.DeepCopyJSONValue(c.Body), local)
	}
	resp, err := r.api.DoWith(ctx, c.Method, r.expand(c.Path, local), body, c.Headers)
	if err != nil {
		var ae *rest.APIError
		ok := false
		for _, s := range c.OK {
			if (errors.As(err, &ae) && ae.Status == s) || (s == 404 && errors.Is(err, rest.ErrNotFound)) {
				ok = true
			}
		}
		if !ok {
			return err
		}
	}
	for name, at := range c.Save {
		v := dig(resp, at)
		if v == nil && r.rehearsal {
			v = "rehearsal-" + name // the fake's stand-in answer has no such field: a stand-in value
		}
		if v == nil {
			return fmt.Errorf("save %s: response has no %s", name, at)
		}
		if f, ok := v.(float64); ok && f == float64(int64(f)) {
			v = int64(f) // an ID: 1000000, not 1e+06
		}
		r.mu.Lock()
		r.vars[name] = v
		r.mu.Unlock()
		if s, ok := v.(string); ok && !r.rehearsal && r.cfg.identityVar(name) {
			r.known.AddIdentity(s) // the account's or a person's: redacted from now on, the before calls' cassette too
		}
	}
	return nil
}

var tmplRE = regexp.MustCompile(`\{\{([a-zA-Z_]+)(?::([A-Za-z0-9_.-]+))?\}\}`)

// template fills {{...}} placeholders in every string of v. A string that is
// exactly one {{var:NAME}} takes the saved value's own type (a number stays
// a number).
func (r *liveRun) template(v any, local map[string]string) any {
	switch x := v.(type) {
	case string:
		if m := tmplRE.FindStringSubmatch(x); m != nil && m[0] == x && m[1] == "var" {
			r.mu.Lock()
			val, ok := r.vars[m[2]]
			r.mu.Unlock()
			if ok {
				return val
			}
		}
		return r.expand(x, local)
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = r.template(e, local)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = r.template(e, local)
		}
		return out
	default:
		return v
	}
}

func (r *liveRun) expand(s string, local map[string]string) string {
	return tmplRE.ReplaceAllStringFunc(s, func(m string) string {
		p := tmplRE.FindStringSubmatch(m)
		switch p[1] {
		case "run":
			return r.id
		case "run_":
			return strings.ReplaceAll(r.id, "-", "_")
		case "RUN":
			return strings.ToUpper(r.id)
		case "RUN_":
			return strings.ToUpper(strings.ReplaceAll(r.id, "-", "_"))
		case "name", "name_", "NAME", "NAME_":
			if v, ok := local[p[1]]; ok {
				return v
			}
		case "cred":
			return r.creds[p[2]]
		case "env":
			r.mu.Lock()
			r.envUsed[p[2]] = true
			r.mu.Unlock()
			return r.getenv(p[2])
		case "date":
			var days int
			if _, err := fmt.Sscanf(p[2], "%d", &days); err == nil {
				return r.today().UTC().AddDate(0, 0, days).Format("2006-01-02")
			}
		case "var":
			r.mu.Lock()
			v, ok := r.vars[p[2]]
			r.mu.Unlock()
			if ok {
				return fmt.Sprint(v)
			}
		}
		return m
	})
}

func (r *liveRun) record(k *descriptor.Kind, name string, pass bool, steps []string, reason string) {
	// A reason quotes the API's answers and what was sent: scrubbed and
	// redacted like a cassette, since records.json is committed too.
	reason = r.safe(name, reason)
	r.mu.Lock()
	defer r.mu.Unlock()
	key := k.Kind + "." + k.Group
	now := time.Now().UTC().Format(time.RFC3339)
	rec := LiveRecord{Result: "fail", Steps: steps, Reason: reason, Run: r.id, Date: now, Target: r.target}
	if steps == nil {
		rec.Steps = []string{}
	}
	if pass {
		rec.Result, rec.Reason, rec.LastPass = "pass", "", now
	}
	r.records[key] = rec
}

// writeRecords merges this run's results into the records file, one record
// per kind (a kind not run keeps its old one).
func (r *liveRun) writeRecords(t *testing.T) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.records) == 0 {
		return
	}
	path := os.Getenv(EnvLiveRecords)
	if path == "" {
		path = filepath.Join("hack", "live", "records.json")
	}
	all := map[string]LiveRecord{}
	if data, err := os.ReadFile(path); err == nil { //nolint:gosec // the test run chooses this path (an env override of the repository default)
		if err := json.Unmarshal(data, &all); err != nil {
			t.Errorf("live: %s: %v", path, err)
			return
		}
	}
	for k, v := range r.records {
		if v.Result != "pass" {
			v.LastPass = all[k].LastPass
		}
		v.Reason = r.known.Redact(v.Reason) // what a later segment taught
		all[k] = v
	}
	data, _ := json.MarshalIndent(all, "", "  ")                   // map keys are sorted
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // hack/live/records.json is committed, readable like the rest of the checkout
		t.Errorf("live: %v", err)
		return
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil { //nolint:gosec // hack/live/records.json is committed, readable like the rest of the checkout
		t.Errorf("live: %v", err)
		return
	}
	t.Logf("live: %d results written to %s", len(r.records), path)
}

// dig is descriptor.Dig that also indexes lists.
func dig(v any, path string) any {
	if path == "" {
		return v
	}
	for _, part := range strings.Split(path, "/") {
		switch x := v.(type) {
		case map[string]any:
			v = x[part]
		case []any:
			var i int
			if _, err := fmt.Sscanf(part, "%d", &i); err != nil || i < 0 || i >= len(x) {
				return nil
			}
			v = x[i]
		default:
			return nil
		}
	}
	return v
}

// useConnection points the run at one of live.yaml's connections for a
// kind: its base URL (live only: a replay or a rehearsal answers on the
// run's) and its credentials. It returns how to put the run's back, or why
// the kind skips: the secret lacks keys the connection needs.
func (r *liveRun) useConnection(name string) (restore func(), skip string) {
	c := r.cfg.Connections[name]
	stand := r.player != nil || r.rehearsal
	// The secret's keys the connection's templates name, and which it lacks.
	need := map[string]bool{}
	for _, tmpl := range append([]string{c.BaseURL}, mapValues(c.Creds)...) {
		for _, m := range credRE.FindAllStringSubmatch(tmpl, -1) {
			if r.creds[m[1]] == "" {
				need[m[1]] = true
			}
		}
	}
	if len(need) > 0 && !stand {
		missing := make([]string, 0, len(need))
		for k := range need {
			missing = append(missing, k)
		}
		sort.Strings(missing)
		return nil, fmt.Sprintf("live: connection %s: secret %s lacks %s (add them to it)", name, r.cfg.Secret, strings.Join(missing, ", "))
	}
	creds := map[string]string{}
	for k, tmpl := range c.Creds {
		v := r.expand(tmpl, nil)
		if v == "" || strings.Contains(v, "{{") {
			v = "rehearsal-" + k // a replay's or a rehearsal's stand-in
		}
		creds[k] = v
	}
	base := r.api.BaseURL
	if c.BaseURL != "" && !stand {
		base = r.expand(c.BaseURL, nil)
	}
	oldAPI := r.api
	api := rest.New(r.vendor, base, creds)
	if r.transport != nil {
		api.HTTP.Transport = r.transport
	}
	r.api, r.kindCreds = api, creds
	oldBase := ""
	if r.recorder != nil {
		oldBase, r.recorder.Base = r.recorder.Base, api.BaseURL
	}
	return func() {
		r.api, r.kindCreds = oldAPI, nil
		if r.recorder != nil {
			r.recorder.Base = oldBase
		}
	}, ""
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// missingCreds is the {{cred:KEY}} templates a live kind's entry names that
// the run's secret lacks: a live run skips the kind, naming them, rather
// than send an empty value. A replay or a rehearsal has stand-ins for all.
func (r *liveRun) missingCreds(lk *LiveKind) []string {
	if r.player != nil || r.rehearsal {
		return nil
	}
	raw, err := json.Marshal(lk)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range credRE.FindAllStringSubmatch(string(raw), -1) {
		if _, ok := r.creds[m[1]]; !ok && !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// settleWait is how long a step waits for an asynchronous object's status
// (sql.settle): its timeout; none for other kinds.
func settleWait(k *descriptor.Kind) time.Duration {
	if k.SQL == nil || k.SQL.Settle == nil {
		return 0
	}
	return k.SQL.Settle.TimeoutOf()
}
