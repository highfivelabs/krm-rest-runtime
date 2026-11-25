// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package kindtest

import "testing"

// A schema with no types is not structural: ValidCRDs must say so.
func TestValidCRDsRejectsANonStructuralSchema(t *testing.T) {
	ft := &testing.T{}
	done := make(chan struct{})
	go func() { defer close(done); ValidCRDs(ft, "testdata/badcrds") }()
	<-done
	if !ft.Failed() {
		t.Fatal("ValidCRDs accepted a schema without types")
	}
}
