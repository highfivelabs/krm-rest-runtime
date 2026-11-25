// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// The MCP server's fixture types and CRDs: the Widget's live spec fills its
// integer width from a saved value, which is checked as 0.
func TestExampleProblems(t *testing.T) {
	types, err := os.ReadFile("../mcpserver/testdata/types.yaml")
	if err != nil {
		t.Fatal(err)
	}
	live, err := os.ReadFile("../mcpserver/testdata/live/live.yaml")
	if err != nil {
		t.Fatal(err)
	}
	live = []byte(strings.Replace(string(live), "width: 10", `width: "{{var:width}}"`, 1))
	got, err := ExampleProblems(types, live, "../mcpserver/testdata/crds")
	if err != nil {
		t.Fatal(err)
	}
	// Settings has no CRD in the fixture; Widget and Gadget are valid.
	if len(got) != 1 || !strings.Contains(got[0], "Settings.things.example.hfl.dev: no CRD") {
		t.Errorf("problems = %q, want only Settings' missing CRD", got)
	}

	bad := []byte(strings.Replace(string(live), "color: red", "color: green", 1))
	got, _ = ExampleProblems(types, bad, "../mcpserver/testdata/crds")
	if !strings.Contains(strings.Join(got, "\n"), `Widget.things.example.hfl.dev live spec: spec.color: Unsupported value: "green"`) {
		t.Errorf("a refused live spec was not reported: %q", got)
	}
}

// The fixture's Gadget with its Widget: valid, and made and deleted on the
// fake API, the Widget first.
func TestExampleFileProblems(t *testing.T) {
	types, err := os.ReadFile("../mcpserver/testdata/types.yaml")
	if err != nil {
		t.Fatal(err)
	}
	v := &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}
	file := "../mcpserver/testdata/examples/Gadget/with-Widget.yaml"
	got, err := ExampleFileProblems(types, v, nil, file, "../mcpserver/testdata/crds")
	if err != nil || len(got) != 0 {
		t.Fatalf("problems = %q, %v", got, err)
	}

	// Without its reference, the Gadget breaks its CRD's rule.
	data, _ := os.ReadFile(file)
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte(strings.Replace(string(data), "  fieldRefs:\n    widgetID: {kind: Widget, name: example-widget}\n", "", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ = ExampleFileProblems(types, v, nil, bad, "../mcpserver/testdata/crds")
	if !strings.Contains(strings.Join(got, "\n"), "spec.widgetID is required") {
		t.Errorf("a Gadget with no widgetID: %q", got)
	}
}
