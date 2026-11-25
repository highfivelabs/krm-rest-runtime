// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package descriptor

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Rotate describes a token that expires and is replaced before it does, as
// Terraform's grafana_*_rotating_token types do: each token is made with a
// name of the spec's prefix and its expiry, and lives the spec's lifetime;
// once the time left is within the spec's window, a new one is made (its
// secret outputs written to spec.writeSecretRef), then the old one is
// deleted. The reconciler requeues the object for that time rather than
// waiting for the next resync. The spec fields it names are the kind's
// QueryFields: never sent, never compared. Deleting the object deletes the
// last token only where the spec's DeleteOnDelete field is true (else it is
// left to expire, as the provider's delete_on_destroy).
type Rotate struct {
	// Prefix is the spec field each token's name starts with; the name is
	// "<prefix>-<expiry in Unix seconds>".
	Prefix string `json:"prefix"`
	// Lifetime is the spec field saying how long a token lives: a duration
	// ("24h", "1h30m"), or with Seconds a number of seconds.
	Lifetime string `json:"lifetime"`
	// Window is the spec field saying how long before its expiry a token is
	// replaced, in the same unit; at most the lifetime.
	Window string `json:"window"`
	// Seconds: Lifetime and Window are whole seconds, not durations.
	Seconds bool `json:"seconds,omitempty"`
	// DeleteOnDelete is the spec field that, true, deletes the last token
	// when the object is deleted.
	DeleteOnDelete string `json:"deleteOnDelete,omitempty"`
	// Name is the body field the create sets to the token's name.
	Name string `json:"name"`
	// ExpiresAt is the body field the create sets to its expiry (RFC 3339),
	// where the API takes one (Grafana Cloud's access policy tokens).
	ExpiresAt string `json:"expiresAt,omitempty"`
	// TTL is the body field the create sets to its lifetime in seconds,
	// where the API takes that (Grafana's service account tokens).
	TTL string `json:"ttl,omitempty"`
}

// Fields are the spec fields Rotate names.
func (r *Rotate) Fields() []string {
	out := []string{r.Prefix, r.Lifetime, r.Window}
	if r.DeleteOnDelete != "" {
		out = append(out, r.DeleteOnDelete)
	}
	return out
}

func (k *Kind) validateRotate() error {
	r := k.Rotate
	if k.Shape != CRUD || k.Create == nil || k.Delete == nil || k.IsGraphQL() {
		return fmt.Errorf("rotate is for a REST crud kind with a create and a delete (a token)")
	}
	if r.Prefix == "" || r.Lifetime == "" || r.Window == "" || r.Name == "" || r.ExpiresAt == "" && r.TTL == "" {
		return fmt.Errorf("rotate needs prefix, lifetime, window, name, and expiresAt or ttl")
	}
	for _, f := range r.Fields() {
		found := false
		for _, q := range k.QueryFields {
			found = found || q == f
		}
		if !found {
			return fmt.Errorf("rotate: %s is not among queryFields (never sent, never compared)", f)
		}
	}
	return nil
}

// Durations are the token lifetime and rotation window spec sets, checked.
func (r *Rotate) Durations(spec map[string]any) (lifetime, window time.Duration, err error) {
	get := func(f string) (time.Duration, error) {
		v, ok := spec[f]
		if !ok || v == nil || Text(v) == "" {
			return 0, fmt.Errorf("spec.%s is required", f)
		}
		var d time.Duration
		if r.Seconds {
			n, err := strconv.ParseInt(Text(v), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("spec.%s: a whole number of seconds: %v", f, Text(v))
			}
			d = time.Duration(n) * time.Second
		} else {
			if d, err = time.ParseDuration(strings.TrimSpace(Text(v))); err != nil {
				return 0, fmt.Errorf("spec.%s: a duration such as 24h or 1h30m: %v", f, err)
			}
		}
		if d < 0 {
			return 0, fmt.Errorf("spec.%s may not be negative", f)
		}
		return d, nil
	}
	if lifetime, err = get(r.Lifetime); err != nil {
		return 0, 0, err
	}
	if window, err = get(r.Window); err != nil {
		return 0, 0, err
	}
	if window > lifetime {
		return 0, 0, fmt.Errorf("spec.%s cannot be more than spec.%s", r.Window, r.Lifetime)
	}
	return lifetime, window, nil
}

// CreateBody is body with the fields Rotate sets on the create of a token
// that expires at expires, made at now: its name, and its expiry or its
// lifetime in seconds.
func (r *Rotate) CreateBody(body, spec map[string]any, now, expires time.Time) map[string]any {
	out := make(map[string]any, len(body)+3)
	for f, v := range body {
		out[f] = v
	}
	out[r.Name] = fmt.Sprintf("%s-%d", Text(spec[r.Prefix]), expires.Unix())
	if r.ExpiresAt != "" {
		out[r.ExpiresAt] = expires.UTC().Format(time.RFC3339)
	}
	if r.TTL != "" {
		out[r.TTL] = int64(expires.Sub(now) / time.Second)
	}
	return out
}
