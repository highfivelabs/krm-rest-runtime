// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

//go:build live

package kindtest

// The part of a live run that reaches a vendor: credentials (Secret Manager
// or, for a sandbox on this machine, the environment), and the Live and
// LiveSweep entry points. Compiled only with -tags live, so go test ./...
// and task check never read a credential or reach a vendor. The steps a run
// takes are in live_harness.go, shared with Replay.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/reconciler"
	"github.com/highfivelabs/krm-rest-runtime/rest"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// liveSetup decides whether live mode runs and, if so, connects. It skips,
// naming the secret and project, when there are no credentials.
func liveSetup(t *testing.T, vendor *rest.Vendor, liveYAML []byte) *liveRun {
	t.Helper()
	if os.Getenv(EnvLive) != "1" {
		t.Skipf("live tests are off: set %s=1 to run them against the real API (docs/live.md in krm-rest-runtime)", EnvLive)
	}
	cfg, err := ParseLiveConfig(liveYAML)
	if err != nil {
		t.Fatal(err)
	}
	if p := os.Getenv(EnvLiveProject); p != "" {
		cfg.Project = p
	}
	raw, skip := liveCredentials(cfg)
	if skip != "" {
		if os.Getenv(EnvLiveRequire) == "1" {
			t.Fatal(skip) // a scheduled run with no credentials is a failure, not a quiet skip
		}
		t.Skip(skip)
	}
	fromEnv := os.Getenv(EnvLiveCreds) != ""
	creds := map[string]string{}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("live: credentials (secret %s in project %s, or $%s) are not a JSON object: %v", cfg.Secret, cfg.Project, EnvLiveCreds, err)
	}
	for k, v := range obj {
		creds[k] = fmt.Sprint(v)
	}
	var missing []string
	for _, k := range vendor.CredentialKeys {
		if creds[k] == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		keys := make([]string, 0, len(creds))
		for k := range creds {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Fatalf("live: credentials lack %v (have %v); secret %s in project %s must hold %v", missing, keys, cfg.Secret, cfg.Project, vendor.CredentialKeys)
	}
	maskInActions(creds)
	baseURL := creds["baseURL"]
	delete(creds, "baseURL")
	if baseURL == "" {
		baseURL = cfg.BaseURL
	}
	r := newLiveRun(cfg, vendor, creds, baseURL, os.Getenv(EnvLiveRun))
	if r.api.BaseURL == "" {
		t.Fatalf("live: no base URL: put baseURL in secret %s or live.yaml", cfg.Secret)
	}
	if fromEnv && !loopback(r.api.BaseURL) {
		t.Fatalf("live: $%s may only point at a sandbox on this machine (127.0.0.1, localhost, ::1), not %s: a real account's credentials come from secret %s in project %s", EnvLiveCreds, r.api.BaseURL, cfg.Secret, cfg.Project)
	}
	getenv, masked := r.getenv, map[string]bool{}
	r.getenv = func(k string) string { // {{env:VAR}} values are secrets too: masked once
		v := getenv(k)
		if !masked[k] {
			masked[k] = true
			maskInActions(map[string]string{k: v})
		}
		return v
	}
	r.recordCassettes()
	r.versionCall(t)
	t.Logf("live: run %s against %s (%s)", r.id, r.api.BaseURL, r.target)
	return r
}

// maskInActions tells GitHub Actions to hide each value in the job's log:
// it masks only its own secrets, not those read from Secret Manager.
func maskInActions(values map[string]string) {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return
	}
	for _, v := range values {
		for _, line := range strings.Split(v, "\n") {
			if line = strings.TrimSpace(line); len(line) >= 4 {
				fmt.Printf("::add-mask::%s\n", line)
			}
		}
	}
}

// liveCredentials returns the credentials JSON, or why live mode skips.
func liveCredentials(cfg *LiveConfig) ([]byte, string) {
	if v := os.Getenv(EnvLiveCreds); v != "" {
		return []byte(v), ""
	}
	if cfg.Project == "" {
		return nil, fmt.Sprintf("live: no credentials: set $%s to the Secret Manager project holding secret %s (or set $%s)", EnvLiveProject, cfg.Secret, EnvLiveCreds)
	}
	where := fmt.Sprintf("secret %s in project %s", cfg.Secret, cfg.Project)
	if _, err := exec.LookPath("gcloud"); err != nil {
		return nil, fmt.Sprintf("live: no credentials: gcloud is not installed, so %s cannot be read (or set $%s)", where, EnvLiveCreds)
	}
	var stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gcloud", "secrets", "versions", "access", "latest", "--secret="+cfg.Secret, "--project="+cfg.Project, "--quiet") //nolint:gosec // argv, not a shell: the secret and project names are single arguments to gcloud
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		switch {
		case strings.Contains(msg, "NOT_FOUND") || strings.Contains(msg, "not found"):
			return nil, fmt.Sprintf("live: no credentials: %s does not exist yet (docs/live.md says what to put in it)", where)
		case strings.Contains(msg, "PERMISSION_DENIED"):
			return nil, fmt.Sprintf("live: no credentials: no permission to read %s: %s", where, firstLine(msg))
		default:
			return nil, fmt.Sprintf("live: no credentials: reading %s failed: %s", where, firstLine(msg))
		}
	}
	return out, ""
}

// loopback says whether a base URL is on this machine: a sandbox (a Vault
// dev server, a GitLab container), the only target $KRM_LIVE_CREDS may name.
func loopback(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Live runs every kind of liveYAML's allowlist against the real API: create,
// read back (nothing to change), update, read back, drift repaired, delete,
// gone. It skips, saying why, unless KRM_LIVE=1 and credentials are found.
// Each kind's result is written to hack/live/records.json, and each passing
// kind's calls to its cassette in hack/live/cassettes/ (what Replay plays).
func Live(t *testing.T, typesYAML []byte, vendor *rest.Vendor, hooks map[string][]reconciler.Hook, liveYAML []byte) {
	t.Helper()
	f, err := descriptor.Parse(typesYAML)
	if err != nil {
		t.Fatal(err)
	}
	r := liveSetup(t, vendor, liveYAML)
	r.sweep(t, false)
	r.runKinds(t, f, hooks)
}

// LiveSweep deletes every leftover a live run made; with KRM_LIVE_SWEEP=1
// whatever its age, otherwise only those older than SweepAge.
func LiveSweep(t *testing.T, vendor *rest.Vendor, liveYAML []byte) {
	t.Helper()
	if os.Getenv(EnvLiveSweep) != "1" {
		t.Skipf("sweep is off: set %s=1 (and %s=1) to delete every live-test leftover", EnvLiveSweep, EnvLive)
	}
	r := liveSetup(t, vendor, liveYAML)
	r.sweep(t, true)
}

// stampRE finds a live run's name and time in a leftover's name, whatever
// the API did to its case (Snowflake uppercases, GitHub variables too).
var stampRE = regexp.MustCompile(`(?i)hfl[-_]live[-_](\d{10})`)

// sweep deletes leftovers named LivePrefix...: those older than SweepAge,
// or all of them.
func (r *liveRun) sweep(t *testing.T, all bool) {
	ctx := context.Background()
	for _, s := range r.cfg.Sweep {
		path := r.expand(s.List, nil)
		resp, err := r.api.Do(ctx, "GET", path, nil)
		if err != nil {
			t.Logf("sweep: GET %s: %v", path, err)
			continue
		}
		type item struct{ name, id string }
		var items []item
		switch v := descriptor.Dig(resp, s.Items).(type) {
		case []any:
			for _, e := range v {
				m, _ := e.(map[string]any)
				name := reconciler.IDString(descriptor.Dig(m, s.Name))
				id := name
				if s.ID != "" {
					id = reconciler.IDString(descriptor.Dig(m, s.ID))
				}
				items = append(items, item{name, id})
			}
		case map[string]any:
			for k := range v {
				items = append(items, item{k, strings.TrimSuffix(k, "/")})
			}
		}
		for _, it := range items {
			m := stampRE.FindStringSubmatch(it.name)
			if m == nil {
				continue
			}
			if low := strings.ToLower(it.name); strings.Contains(low, r.id) || strings.Contains(low, strings.ReplaceAll(r.id, "-", "_")) {
				continue
			}
			made, err := time.Parse("0601021504", m[1])
			if err != nil || (!all && time.Since(made) < SweepAge) {
				continue
			}
			if s.Before != "" {
				pre, err := rest.Expand(r.expand(s.Before, nil), map[string]string{"id": it.id})
				if err != nil {
					continue
				}
				if _, err := r.api.Do(ctx, "POST", pre, nil); err != nil && !errors.Is(err, rest.ErrNotFound) {
					t.Logf("sweep: POST %s: %v", pre, err)
					continue
				}
			}
			del, err := rest.Expand(r.expand(s.Delete, nil), map[string]string{"id": it.id})
			if err != nil {
				continue
			}
			if _, err := r.api.Do(ctx, "DELETE", del, nil); err != nil && !errors.Is(err, rest.ErrNotFound) {
				t.Logf("sweep: DELETE %s: %v", del, err)
				continue
			}
			t.Logf("sweep: deleted %s (%s)", it.name, del)
		}
	}
}

var _ client.Object = &unstructured.Unstructured{}
