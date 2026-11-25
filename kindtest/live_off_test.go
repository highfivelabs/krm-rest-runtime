// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

//go:build !live

package kindtest

import "testing"

// Built without -tags live, Live skips even with KRM_LIVE=1 and
// credentials set: the code that would use them is not compiled in.
func TestLiveNotBuiltSkips(t *testing.T) {
	t.Setenv(EnvLive, "1")
	t.Setenv(EnvLiveCreds, `{"token":"t","baseURL":"http://127.0.0.1:1"}`)
	ft := &testing.T{}
	done := make(chan struct{})
	go func() { defer close(done); Live(ft, typesYAML, exampleVendor(), nil, []byte(exampleLive)) }()
	<-done
	if !ft.Skipped() || ft.Failed() {
		t.Fatalf("skipped=%v failed=%v", ft.Skipped(), ft.Failed())
	}
}
