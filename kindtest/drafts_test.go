// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

//go:embed testdata/drafts/types.yaml
var draftTypesYAML []byte

var draftVendor = &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}

// Drafts are skipped, each with its reason; the kinds around them run (a
// draft that ran would fail: the reconciler refuses it).
func TestEveryKindSkipsDrafts(t *testing.T) {
	EveryKind(t, draftTypesYAML, draftVendor, nil)
}

// A rehearsal runs the drafts last (skipped), so the kinds keep the
// object names their cassettes hold; a live.yaml may not list one.
func TestRehearsalConfigDrafts(t *testing.T) {
	f, err := descriptor.Parse(draftTypesYAML)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := rehearsalConfig(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, k := range cfg.Kinds {
		got = append(got, k.Kind)
	}
	if want := "Widget Gadget WidgetAssignment Trigger"; strings.Join(got, " ") != want {
		t.Errorf("order %v, want %s", got, want)
	}
	live := []byte("secret: s\nproject: p\nkinds:\n- kind: Trigger\n")
	if _, err := rehearsalConfig(f, live); err == nil || !strings.Contains(err.Error(), "draft") {
		t.Errorf("a live.yaml listing a draft: %v", err)
	}
	ft := &testing.T{}
	done := make(chan struct{})
	go func() { defer close(done); ValidLiveConfig(ft, draftTypesYAML, live) }()
	<-done
	if !ft.Failed() {
		t.Error("ValidLiveConfig accepted a draft")
	}
}

// ValidCRDs checks the drafts' CRDs (config/crds/drafts) too.
func TestValidCRDsChecksDrafts(t *testing.T) {
	ValidCRDs(t, "testdata/drafts/crds")
	ft := &testing.T{}
	done := make(chan struct{})
	go func() { defer close(done); ValidCRDs(ft, "testdata/drafts/badcrds") }()
	<-done
	if !ft.Failed() {
		t.Fatal("ValidCRDs passed a draft CRD whose schema is not structural")
	}
}

// A draft's sample is checked against its CRD in drafts/, as a kind's is.
func TestExamplesOfDrafts(t *testing.T) {
	problems, err := ExampleProblems(draftTypesYAML, nil, "testdata/drafts/crds")
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) > 0 {
		t.Errorf("problems: %v", problems)
	}
}

// plan_on_fake never runs a draft: it is noted and skipped.
func TestRunManifestSkipsDrafts(t *testing.T) {
	f, err := descriptor.Parse(draftTypesYAML)
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{"apiVersion": "things.example.hfl.dev/v1alpha1", "kind": "WidgetAssignment",
		"metadata": map[string]any{"name": "a", "namespace": "team-a"}, "spec": map[string]any{"org": "acme", "assignee": "x"}}
	widget := map[string]any{"apiVersion": "things.example.hfl.dev/v1alpha1", "kind": "Widget",
		"metadata": map[string]any{"name": "w", "namespace": "team-a"}, "spec": map[string]any{"org": "acme", "name": "w1"}}
	run, err := RunManifest(context.Background(), f, draftVendor, nil, []map[string]any{doc, widget}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Objects) != 1 || run.Objects[0].Kind != "Widget.things.example.hfl.dev" || !strings.Contains(strings.Join(run.Notes, " "), "is a draft") {
		t.Errorf("objects %v, notes %v", run.Objects, run.Notes)
	}
}
