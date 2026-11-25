// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

//go:build !live

package kindtest

import (
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/reconciler"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// Live, built without -tags live, skips: the code that reads credentials
// and calls a vendor's API is not in this binary (live_run.go).
func Live(t *testing.T, typesYAML []byte, vendor *rest.Vendor, hooks map[string][]reconciler.Hook, liveYAML []byte) {
	t.Helper()
	t.Skip("live tests are not built: run go test -tags live with " + EnvLive + "=1 (docs/live.md in krm-rest-runtime)")
}

// LiveSweep, built without -tags live, skips like Live.
func LiveSweep(t *testing.T, vendor *rest.Vendor, liveYAML []byte) {
	t.Helper()
	t.Skip("live sweep is not built: run go test -tags live with " + EnvLive + "=1 " + EnvLiveSweep + "=1")
}
