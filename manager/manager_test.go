// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package manager

import (
	"strings"
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// The manager runs no controller for a draft, whatever groups it runs.
func TestControllersLeaveOutDrafts(t *testing.T) {
	f := &descriptor.File{Kinds: []descriptor.Kind{
		{Kind: "Widget", Group: "things.example.hfl.dev", Readiness: "fake"},
		{Kind: "Assignment", Group: "things.example.hfl.dev", Readiness: descriptor.ReadinessDraft, Draft: &descriptor.Draft{Open: []string{"x"}}},
		{Kind: "Trigger", Group: "event.example.hfl.dev", Readiness: "live"},
	}}
	run, drafts := Controllers(f, nil)
	var names []string
	for _, k := range run {
		names = append(names, k.Kind)
	}
	if strings.Join(names, ",") != "Widget,Trigger" || strings.Join(drafts, ",") != "Assignment.things.example.hfl.dev" {
		t.Errorf("run %v, drafts %v", names, drafts)
	}
	run, drafts = Controllers(f, map[string]bool{"things.example.hfl.dev": true})
	if len(run) != 1 || run[0].Kind != "Widget" || len(drafts) != 1 {
		t.Errorf("groups: run %d, drafts %v", len(run), drafts)
	}
}
