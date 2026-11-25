// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package kindtest

// Live mode: the steps EveryKind runs against the fake (create, read back,
// update, drift repair, delete) run against the vendor's real API, for the
// kinds a vendor repository lists in hack/live/live.yaml. See docs/live.md.
//
// This file is the part with no credentials: live.yaml's shape and its
// check. The part that runs is in live_run.go, compiled only with -tags live.

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/highfivelabs/krm-rest-runtime/cassette"
	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// Environment variables of live mode.
const (
	// EnvLive=1 turns live mode on; without it every live test skips.
	EnvLive = "KRM_LIVE"
	// EnvLiveCreds holds the Connection's credentials as JSON, instead of
	// Secret Manager (a local Vault dev server, a GitLab container).
	EnvLiveCreds = "KRM_LIVE_CREDS"
	// EnvLiveKinds narrows a run to some kinds: "KvSecret,TokenAuthBackendRole".
	EnvLiveKinds = "KRM_LIVE_KINDS"
	// EnvLiveRun names the run (default hfl-live-<UTC YYMMDDhhmm>-<4 hex>).
	EnvLiveRun = "KRM_LIVE_RUN"
	// EnvLiveRecords is where results are written (default hack/live/records.json).
	EnvLiveRecords = "KRM_LIVE_RECORDS"
	// EnvLiveSweep=1 lets LiveSweep delete every leftover, whatever its age.
	EnvLiveSweep = "KRM_LIVE_SWEEP"
	// EnvLiveProject names the Secret Manager project the live credentials
	// are read from (or live.yaml's project, which it overrides). The public
	// repositories name none.
	EnvLiveProject = "KRM_LIVE_PROJECT"
	// EnvLiveCassettes is where a live run writes, and Replay reads, the
	// cassettes (default hack/live/cassettes); "off": a live run records none.
	EnvLiveCassettes = "KRM_LIVE_CASSETTES"
	// EnvLiveRequire=1: no credentials fails the run instead of skipping it
	// (a scheduled re-record that cannot reach its secret is broken).
	EnvLiveRequire = "KRM_LIVE_REQUIRE"
)

// LivePrefix starts the name of everything a live run makes, so leftovers
// can be found and swept. Its timestamp (UTC YYMMDDhhmm) says how old it is.
const LivePrefix = "hfl-live-"

// SweepAge is how old a leftover must be before a run sweeps it (younger
// ones may belong to a run still going).
var SweepAge = 2 * time.Hour

// LiveConfig is a vendor's hack/live/live.yaml.
type LiveConfig struct {
	// Secret is the Secret Manager secret holding the Connection's
	// credentials, a JSON object with the vendor's credential keys (and
	// optionally baseURL), in Project.
	Secret  string `json:"secret"`
	Project string `json:"project"`
	// BaseURL, if the secret names none and the vendor has no default.
	BaseURL string `json:"baseURL,omitempty"`
	// Target says what a run tests against (recorded): "Vault dev server".
	Target string `json:"target,omitempty"`
	// Version is a call whose Field records the API's version.
	Version *LiveCall `json:"version,omitempty"`
	// Before runs once before any kind (mounts, a parent group); After
	// runs once at the end, whatever happened.
	Before []LiveCall `json:"before,omitempty"`
	After  []LiveCall `json:"after,omitempty"`
	// Kinds is the allowlist, in the order they run. A kind not listed is
	// not run live.
	Kinds []LiveKind `json:"kinds"`
	// Sweep lists where leftovers (named LivePrefix...) are found.
	Sweep []LiveSweepSpec `json:"sweep,omitempty"`
	// Connections are further Connections a kind may run on (its
	// connection:): another of the vendor's APIs on another host (Grafana
	// Cloud's, OnCall's, Confluent's Schema Registry), or other credentials
	// (Cloudflare's user token for Page Rules). Their values are templates
	// filled from the same secret ({{cred:KEY}}), so a run still reads one.
	Connections map[string]LiveConnection `json:"connections,omitempty"`
	// Identities (redactVars, redactFields, redactSubdomains) name what in
	// the run's calls names the account or a person: each value found is
	// redacted wherever it is, in every cassette, log line and record,
	// however plain it looks. A var saved from a login, username, handle or
	// email field is one without being named.
	cassette.Identities
}

// identities is the run's Identities, with every var a call saves from a
// person's login, username, handle or email (cassette.IdentityField)
// added to RedactVars.
func (c *LiveConfig) identities() cassette.Identities {
	id := c.Identities
	id.RedactVars = append([]string(nil), id.RedactVars...)
	seen := map[string]bool{}
	for _, v := range id.RedactVars {
		seen[v] = true
	}
	add := func(calls []LiveCall) {
		for _, call := range calls {
			for name, at := range call.Save {
				if cassette.IdentityField(at) && !seen[name] {
					seen[name] = true
					id.RedactVars = append(id.RedactVars, name)
				}
			}
		}
	}
	add(c.Before)
	for _, k := range c.Kinds {
		add(k.Before)
	}
	sort.Strings(id.RedactVars)
	return id
}

// identityVar: a saved var whose value names the account or a person.
func (c *LiveConfig) identityVar(name string) bool {
	for _, v := range c.identities().RedactVars {
		if v == name {
			return true
		}
	}
	return false
}

// LiveConnection is a Connection a kind runs on instead of the run's.
type LiveConnection struct {
	// BaseURL ("" = the run's) and Creds, the Connection Secret's keys.
	BaseURL string            `json:"baseURL,omitempty"`
	Creds   map[string]string `json:"creds"`
}

// LiveKind is one kind's live test.
type LiveKind struct {
	Kind  string `json:"kind"`
	Group string `json:"group,omitempty"`
	// Skip, a reason: listed but not run (a prerequisite we lack).
	Skip string `json:"skip,omitempty"`
	// FakeOnly, a reason: never run live, because the real feature is
	// sold only by contract (an Enterprise plan, a sales-only add-on) and
	// no test account will have it. The fake API is its test for good.
	// ParseLiveConfig makes it a Skip ("fake only: <reason>"), so every
	// run skips it; the rehearsal still runs it on the fake.
	FakeOnly string `json:"fakeOnly,omitempty"`
	// Spec is merged over the kind's sample (a null removes a sample
	// field); with ReplaceSample it replaces it. Strings are templates:
	// {{name}} (this kind's unique name), {{name_}} (the same with
	// underscores), {{NAME}} / {{NAME_}} (upper-case), {{run}},
	// {{cred:KEY}}, {{env:VAR}}, {{var:NAME}}, {{date:N}} (N days from
	// today, YYYY-MM-DD: a token's expiry).
	Spec          map[string]any `json:"spec,omitempty"`
	ReplaceSample bool           `json:"replaceSample,omitempty"`
	// Change is the update step: fields and their new values. Without it
	// the first plain string body field gets "changed-<field>".
	Change map[string]any `json:"change,omitempty"`
	// Drift is the value the drift step writes behind the controller's
	// back to the first Change field (default "hfl-live-drift" for strings).
	Drift any `json:"drift,omitempty"`
	// NoDrift, a reason: the drift step is not run.
	NoDrift string `json:"noDrift,omitempty"`
	// NoUpdate, a reason: the update and drift steps are not run (the
	// account has nothing the field can change to: GitHub reads an
	// organization owner's team role back as maintainer, whatever is sent).
	NoUpdate string `json:"noUpdate,omitempty"`
	// DriftFull: the drift step sends the object as read with the one
	// field changed, for an update call that replaces the whole object
	// (Vault's KV); by default it sends only the changed field.
	DriftFull bool `json:"driftFull,omitempty"`
	// Before and After run around this kind (After whatever happened).
	Before []LiveCall `json:"before,omitempty"`
	After  []LiveCall `json:"after,omitempty"`
	// Stays, a reason: the API keeps answering a read after the delete
	// (a singleton reset to defaults), so "gone" is not checked.
	Stays string `json:"stays,omitempty"`
	// DeleteWait is how long a delete may take to show (default 60s).
	DeleteWait string `json:"deleteWait,omitempty"`
	// BusyWait is how long the update and drift steps retry while the API
	// answers 409 because the resource is still being made (a Confluent
	// Flink compute pool refuses an update until it is provisioned).
	BusyWait string `json:"busyWait,omitempty"`
	// Connection names one of Connections: the kind runs on it, and skips
	// (naming the secret keys) where the secret lacks what it needs.
	Connection string `json:"connection,omitempty"`
	// ReplayAnyBody, a reason: Replay does not compare this kind's request
	// bodies with the recording (a hook seals a secret with a fresh nonce
	// each time, so no two bodies are alike).
	ReplayAnyBody string `json:"replayAnyBody,omitempty"`
}

// LiveCall is one REST call, relative to the Connection's base URL.
type LiveCall struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   any    `json:"body,omitempty"`
	// Headers are sent with this call only (Atlas's versioned media types).
	Headers map[string]string `json:"headers,omitempty"`
	// OK lists non-2xx statuses that count as success (an existing mount).
	OK []int `json:"ok,omitempty"`
	// Save keeps response fields (slash paths; a number indexes a list:
	// users/0/id) as {{var:NAME}}.
	Save map[string]string `json:"save,omitempty"`
	// Field, on Version, is the response field holding the version.
	Field string `json:"field,omitempty"`
}

// LiveSweepSpec finds leftovers: GET List, the array (or object, whose keys
// are names) at Items, each element's Name; DELETE Delete with {id} filled
// from ID (default Name). Before, if set, is POSTed first, {id} filled the
// same way (Okta deletes an app, zone or hook only once deactivated).
type LiveSweepSpec struct {
	List   string `json:"list"`
	Items  string `json:"items,omitempty"`
	Name   string `json:"name,omitempty"`
	ID     string `json:"id,omitempty"`
	Before string `json:"before,omitempty"`
	Delete string `json:"delete"`
}

// LiveRecord is the last live result of one kind (hack/live/records.json,
// keyed by Kind.group). The generator marks a kind's readiness "live" when
// its record's Result is "pass".
type LiveRecord struct {
	Result string   `json:"result"` // pass or fail
	Steps  []string `json:"steps"`
	Reason string   `json:"reason,omitempty"`
	Run    string   `json:"run"`
	Date   string   `json:"date"`
	Target string   `json:"target,omitempty"`
	// LastPass is the date it last passed, kept through later failures.
	LastPass string `json:"lastPass,omitempty"`
}

// FakeOnlyPrefix starts the Skip of a kind live.yaml marks fakeOnly.
const FakeOnlyPrefix = "fake only: "

// ParseLiveConfig reads a live.yaml.
func ParseLiveConfig(data []byte) (*LiveConfig, error) {
	var c LiveConfig
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return nil, err
	}
	if c.Secret == "" {
		return nil, fmt.Errorf("live.yaml: secret is required")
	}
	for i := range c.Kinds {
		k := &c.Kinds[i]
		if _, ok := c.Connections[k.Connection]; k.Connection != "" && !ok {
			return nil, fmt.Errorf("live.yaml: %s names connection %q, which connections: does not define", k.Kind, k.Connection)
		}
		if k.FakeOnly != "" {
			if k.Skip != "" {
				return nil, fmt.Errorf("live.yaml: %s has both skip and fakeOnly: keep one", k.Kind)
			}
			k.Skip = FakeOnlyPrefix + k.FakeOnly
		}
	}
	return &c, nil
}

func findKind(f *descriptor.File, lk *LiveKind) *descriptor.Kind {
	for i := range f.Kinds {
		k := &f.Kinds[i]
		if k.Kind == lk.Kind && (lk.Group == "" || k.Group == lk.Group) {
			return k
		}
	}
	return nil
}

// ValidLiveConfig checks a live.yaml without credentials: it parses, and
// every kind it lists is in types.yaml. Vendor repositories run it with
// their fake tests, so the allowlist cannot drift from the kinds.
func ValidLiveConfig(t *testing.T, typesYAML, liveYAML []byte) {
	t.Helper()
	f, err := descriptor.Parse(typesYAML)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseLiveConfig(liveYAML)
	if err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Kinds {
		lk := &cfg.Kinds[i]
		if k := findKind(f, lk); k == nil {
			t.Errorf("live.yaml lists %s %s, which types.yaml does not have", lk.Kind, lk.Group)
		} else if k.IsDraft() {
			t.Errorf("live.yaml lists %s", k.DraftReason())
		}
		if lk.DeleteWait != "" {
			if _, err := time.ParseDuration(lk.DeleteWait); err != nil {
				t.Errorf("live.yaml %s: deleteWait: %v", lk.Kind, err)
			}
		}
		if lk.BusyWait != "" {
			if _, err := time.ParseDuration(lk.BusyWait); err != nil {
				t.Errorf("live.yaml %s: busyWait: %v", lk.Kind, err)
			}
		}
	}
}
