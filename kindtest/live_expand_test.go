// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

//go:build live

package kindtest

import "testing"

func TestExpandUpperCase(t *testing.T) {
	r := &liveRun{id: "hfl-live-2611160000-ab12"}
	local := map[string]string{
		"name":  "hfl-live-2611160000-ab12-01",
		"name_": "hfl_live_2611160000_ab12_01",
		"NAME":  "HFL-LIVE-2611160000-AB12-01",
		"NAME_": "HFL_LIVE_2611160000_AB12_01",
	}
	if got := r.expand("{{NAME}}", local); got != "HFL-LIVE-2611160000-AB12-01" {
		t.Fatalf("{{NAME}} = %q", got)
	}
	if got := r.expand("{{NAME_}}", local); got != "HFL_LIVE_2611160000_AB12_01" {
		t.Fatalf("{{NAME_}} = %q", got)
	}
	if got := r.expand("{{RUN}}", local); got != "HFL-LIVE-2611160000-AB12" {
		t.Fatalf("{{RUN}} = %q", got)
	}
	if got := r.expand("{{RUN_}}", local); got != "HFL_LIVE_2611160000_AB12" {
		t.Fatalf("{{RUN_}} = %q", got)
	}
}
