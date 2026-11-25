// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/highfivelabs/krm-rest-runtime/rest"
)

var testVendor = &rest.Vendor{
	Name: "example", DefaultBaseURL: "https://api.example.test/v1", CredentialKeys: []string{"apiToken"},
	Authorize: func(r *http.Request, c map[string]string) { r.Header.Set("Authorization", "Bearer "+c["apiToken"]) },
}

// connect serves the fixture vendor and returns a client session on it.
func connect(t *testing.T) *mcp.ClientSession {
	t.Helper()
	types, err := os.ReadFile("testdata/types.yaml")
	if err != nil {
		t.Fatal(err)
	}
	o := Options{Vendor: testVendor, Types: types, CRDs: os.DirFS("testdata/crds"), Examples: os.DirFS("testdata/examples")}
	o.Live, _ = os.ReadFile("testdata/live/live.yaml")
	o.Records, _ = os.ReadFile("testdata/live/records.json")
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// call calls a tool and decodes its structured result into out; it fails
// the test on a tool error unless wantErr.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		return res
	}
	data, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, data)
	}
	return res
}

func errText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestTools(t *testing.T) {
	cs := connect(t)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
		if tl.InputSchema == nil || tl.Description == "" {
			t.Errorf("%s: no input schema or description", tl.Name)
		}
	}
	sort.Strings(names)
	want := "describe_kind example find_kinds_for_terraform list_kinds plan_on_fake validate_manifest"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("tools = %s, want %s", got, want)
	}
}

func TestListKinds(t *testing.T) {
	cs := connect(t)
	var out listOut
	call(t, cs, "list_kinds", map[string]any{}, &out)
	if out.Vendor != "example" || out.Count != 3 {
		t.Fatalf("list_kinds = %+v", out)
	}
	w := out.Kinds[2]
	if w.Kind != "Widget" || w.Readiness != "live" || w.Terraform[0] != "example_widget" || !strings.HasPrefix(w.Description, "Widget: a thing") {
		t.Errorf("Widget row = %+v", w)
	}
	call(t, cs, "list_kinds", map[string]any{"query": "org_settings"}, &out)
	if out.Count != 1 || out.Kinds[0].Kind != "Settings" {
		t.Errorf("query org_settings = %+v", out)
	}
}

func TestFindKindsForTerraform(t *testing.T) {
	cs := connect(t)
	var out tfOut
	call(t, cs, "find_kinds_for_terraform", map[string]any{"types": []string{"example_widget", "example_nope"}}, &out)
	if got := out.Kinds["example_widget"]; len(got) != 1 || got[0] != "Widget.things.example.hfl.dev" {
		t.Errorf("example_widget → %v", got)
	}
	if got, ok := out.Kinds["example_nope"]; !ok || len(got) != 0 {
		t.Errorf("example_nope → %v, %v", got, ok)
	}
}

func TestDescribeKind(t *testing.T) {
	cs := connect(t)
	var d KindDoc
	call(t, cs, "describe_kind", map[string]any{"kind": "Widget"}, &d)
	if d.APIVersion != "things.example.hfl.dev/v1alpha1" || d.Live == nil || d.Live.Result != "pass" || d.IDFrom != "id" {
		t.Fatalf("describe_kind = %+v", d)
	}
	if len(d.Operations) != 4 || d.Operations[0].Op != "create" || d.Operations[0].Path != "/orgs/{org}/widgets" {
		t.Errorf("operations = %+v", d.Operations)
	}
	fields := map[string]Field{}
	for _, f := range d.Spec {
		fields[f.Path] = f
	}
	if f := fields["org"]; !f.Required || f.PathParam != "org" {
		t.Errorf("org = %+v", f)
	}
	if f := fields["color"]; strings.Join(f.Enum, ",") != "blue,red" || f.Description != "Its color." {
		t.Errorf("color = %+v", f)
	}
	if f := fields["size.width"]; !f.Required || f.Type != "integer" {
		t.Errorf("size.width = %+v", f)
	}
	if f := fields["token"]; !f.WriteOnly {
		t.Errorf("token = %+v", f)
	}
	var some KindDoc
	call(t, cs, "describe_kind", map[string]any{"kind": "Widget.things.example.hfl.dev", "fields": []string{"size"}}, &some)
	if len(some.Spec) != 3 {
		t.Errorf("fields [size] = %+v", some.Spec)
	}
	var settings KindDoc
	call(t, cs, "describe_kind", map[string]any{"kind": "things.example.hfl.dev/v1alpha1, Settings"}, &settings)
	if settings.Live == nil || settings.Live.Skipped == "" || len(settings.Notes) == 0 {
		t.Errorf("Settings = %+v", settings)
	}
	if settings.Live.FakeOnly {
		t.Errorf("Settings is skipped, not fake only: %+v", settings.Live)
	}
	var gadget KindDoc
	call(t, cs, "describe_kind", map[string]any{"kind": "Gadget"}, &gadget)
	if gadget.Live == nil || !gadget.Live.FakeOnly || !strings.HasPrefix(gadget.Live.Skipped, "fake only: ") {
		t.Errorf("Gadget live = %+v", gadget.Live)
	}
	if res := call(t, cs, "describe_kind", map[string]any{"kind": "Doohickey"}, nil); !res.IsError || !strings.Contains(errText(res), "list_kinds") {
		t.Errorf("Doohickey: %+v", res)
	}
}

func TestExample(t *testing.T) {
	cs := connect(t)
	var ex Example
	call(t, cs, "example", map[string]any{"kind": "Widget"}, &ex)
	if ex.Tested != "live" || ex.LiveDate == "" || ex.Target != "Example sandbox org" {
		t.Fatalf("example = %+v", ex)
	}
	for _, want := range []string{"kind: Secret", "apiToken: <apiToken>", "kind: Connection", "baseURL: https://api.example.test/v1",
		"name: example-widget", "org: <org>", "color: red", "width: 10"} {
		if !strings.Contains(ex.Manifest, want) {
			t.Errorf("manifest has no %q:\n%s", want, ex.Manifest)
		}
	}
	if len(ex.WithRelated) != 1 || ex.WithRelated[0].File != "examples/Gadget/with-Widget.yaml" || strings.Join(ex.WithRelated[0].Kinds, ",") != "Widget,Gadget" {
		t.Errorf("withRelated = %+v", ex.WithRelated)
	}
	if len(ex.Prerequisites) != 1 || ex.Prerequisites[0] != "POST /orgs/<org>/teams" {
		t.Errorf("prerequisites = %v", ex.Prerequisites)
	}
	// The example is valid and runs on the fake.
	var v Validation
	call(t, cs, "validate_manifest", map[string]any{"manifest": ex.Manifest}, &v)
	if !v.Valid || len(v.Problems) != 0 {
		t.Errorf("the example does not validate: %+v", v)
	}
	var settings Example
	call(t, cs, "example", map[string]any{"kind": "Settings", "withoutConnection": true}, &settings)
	if settings.Tested != "fake" || strings.Contains(settings.Manifest, "kind: Connection") || !strings.Contains(settings.Manifest, "default_branch: main") {
		t.Errorf("Settings example = %+v", settings)
	}
}

func TestRequiredSpec(t *testing.T) {
	types, _ := os.ReadFile("testdata/types.yaml")
	c, err := loadCatalog(Options{Types: types, CRDs: os.DirFS("testdata/crds")})
	if err != nil {
		t.Fatal(err)
	}
	k, _ := c.find("Widget")
	spec := c.requiredSpec(k)
	if spec["org"] != "<org>" || spec["name"] != "<name>" || len(spec) != 2 {
		t.Errorf("requiredSpec = %v (size is optional, so size.width is not filled)", spec)
	}
}

func TestValidateManifest(t *testing.T) {
	cs := connect(t)
	bad := `
apiVersion: things.example.hfl.dev/v1alpha1
kind: Widget
metadata: {name: w}
spec:
  name: w
  color: green
  size: {height: 3}
  colour: blue
---
apiVersion: things.example.hfl.dev/v1alpha1
kind: Doohickey
metadata: {name: d}
`
	var v Validation
	call(t, cs, "validate_manifest", map[string]any{"manifest": bad}, &v)
	if v.Valid || v.Objects != 2 {
		t.Fatalf("validate = %+v", v)
	}
	got := map[string]string{}
	for _, p := range v.Problems {
		got[p.Field+"|"+p.Severity] = p.Message
	}
	for _, want := range []string{"spec.org|error", "spec.color|error", "spec.size.width|error", "spec.colour|warning"} {
		if _, ok := got[want]; !ok {
			t.Errorf("no problem %s in %+v", want, v.Problems)
		}
	}
	unknown := false
	for _, p := range v.Problems {
		if p.Doc == 1 && strings.Contains(p.Message, "not one of example's kinds") {
			unknown = true
		}
	}
	if !unknown {
		t.Errorf("Doohickey not reported: %+v", v.Problems)
	}
}

func TestPlanOnFake(t *testing.T) {
	cs := connect(t)
	manifest := `
apiVersion: things.example.hfl.dev/v1alpha1
kind: Widget
metadata: {name: w, namespace: team-a}
spec:
  org: acme
  name: w1
  color: blue
  secretRefs: {token: {name: widget-token, key: token}}
`
	var p Plan
	call(t, cs, "plan_on_fake", map[string]any{"manifest": manifest, "delete": true}, &p)
	if len(p.Objects) != 1 {
		t.Fatalf("plan = %+v", p)
	}
	o := p.Objects[0]
	if !o.Ready || o.Error != "" {
		t.Fatalf("not Ready: %+v", o)
	}
	if len(o.Create) == 0 || o.Create[0].Method != "POST" || o.Create[0].Path != "/orgs/acme/widgets" {
		t.Fatalf("create calls = %+v", o.Create)
	}
	body, _ := o.Create[0].Body.(map[string]any)
	if body["name"] != "w1" || body["token"] != "<secret widget-token/token>" || body["org"] != nil {
		t.Errorf("create body = %v (org fills the path, not the body; token comes from the Secret)", body)
	}
	if o.Status["id"] == nil {
		t.Errorf("status = %v, want the server's id", o.Status)
	}
	if n := len(o.Delete); n == 0 || o.Delete[n-1].Method != "DELETE" {
		t.Errorf("delete calls = %+v", o.Delete)
	}
	if len(p.Notes) != 2 {
		t.Errorf("notes = %v, want the made-up Connection and the placeholder Secret", p.Notes)
	}
	if res := call(t, cs, "plan_on_fake", map[string]any{"manifest": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\n"}, nil); !res.IsError {
		t.Errorf("a manifest with no vendor objects planned: %+v", res)
	}
}

func TestLoad(t *testing.T) {
	var o Options
	if err := Load(&o, "testdata/no-such-repo"); err != nil || o.CRDs != nil || o.Live != nil {
		t.Fatalf("Load of a missing dir = %v, %+v", err, o)
	}
}

// A child that takes its parent's server-made ID through spec.fieldRefs:
// planned in passes, the parent first, whatever the manifest's order.
func TestPlanOnFakeWithReferences(t *testing.T) {
	cs := connect(t)
	manifest := `
apiVersion: things.example.hfl.dev/v1alpha1
kind: Gadget
metadata: {name: g}
spec:
  name: g1
  fieldRefs: {widgetID: {kind: Widget, name: w}}
---
apiVersion: things.example.hfl.dev/v1alpha1
kind: Widget
metadata: {name: w}
spec: {org: acme, name: w1}
`
	var p Plan
	call(t, cs, "plan_on_fake", map[string]any{"manifest": manifest, "delete": true}, &p)
	if len(p.Objects) != 2 {
		t.Fatalf("plan = %+v", p)
	}
	g, w := p.Objects[0], p.Objects[1]
	if !w.Ready || !g.Ready {
		t.Fatalf("not both Ready: widget %+v, gadget %+v", w, g)
	}
	wid, _ := w.Status["id"].(string)
	var post string
	for _, c := range g.Create {
		if c.Method == "POST" {
			post = c.Path
		}
	}
	if post != "/widgets/"+wid+"/gadgets" {
		t.Errorf("gadget POST = %q, want under widget %s; calls %+v", post, wid, g.Create)
	}
	if len(g.Delete) == 0 {
		t.Errorf("gadget delete calls = %+v", g.Delete)
	}
	var v Validation
	call(t, cs, "validate_manifest", map[string]any{"manifest": manifest}, &v)
	for _, pr := range v.Problems {
		if pr.Field == "spec.widgetID" {
			t.Errorf("widgetID comes from fieldRefs, but validate says %+v", pr)
		}
	}
}
