// SPDX-License-Identifier: Apache-2.0

// Package mcpserver serves a vendor's kinds over the Model Context Protocol,
// so an agent can find a kind, read every field of it, get a tested
// example, check a manifest and see the REST calls it makes, without
// reading the repository. It answers from the same types.yaml and CRDs the
// controllers run from, plus the live tests' config and records where the
// repository has them. Nothing it does reaches the vendor's real API.
//
// A vendor repository runs it with a few lines in cmd/mcp:
//
//	mcpserver.Main(mcpserver.Options{Vendor: extensions.Vendor, Types: extensions.Types, Hooks: extensions.Hooks})
//
// and Main reads config/crds, hack/live/live.yaml and hack/live/records.json
// from --dir (default: the working directory).
package mcpserver

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/reconciler"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// Options for New and Main.
type Options struct {
	Vendor *rest.Vendor
	Types  []byte // the vendor's types.yaml
	Hooks  map[string][]reconciler.Hook
	// CRDs is a directory of CRD files (config/crds). Without it,
	// describe_kind has no field list and validate checks less.
	CRDs fs.FS
	// Live and Records are hack/live/live.yaml and hack/live/records.json,
	// if the repository has them.
	Live    []byte
	Records []byte
	// Examples is the repository's examples/ (gen/examples.py): files
	// that show a kind with the kinds it needs.
	Examples fs.FS
	// Version is reported to clients.
	Version string
}

// New builds the server.
func New(o Options) (*mcp.Server, error) {
	c, err := loadCatalog(o)
	if err != nil {
		return nil, err
	}
	name := c.file.Vendor + "-krm-extensions"
	v := o.Version
	if v == "" {
		v = "dev"
	}
	s := mcp.NewServer(&mcp.Implementation{Name: name, Title: c.file.Vendor + " KRM kinds", Version: v}, &mcp.ServerOptions{
		Instructions: fmt.Sprintf("Kubernetes kinds that manage %s configuration through its REST API "+
			"(High Five Labs' %s; the controllers run on krm-rest-runtime). Each object reaches %s "+
			"through the Connection named \"default\" in its namespace, which names a Secret of credentials. "+
			"Start with list_kinds or find_kinds_for_terraform, then describe_kind and example; "+
			"check what you write with validate_manifest and plan_on_fake. Nothing here calls the real %s API.",
			c.file.Vendor, name, c.file.Vendor, c.file.Vendor),
	})
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}

	mcp.AddTool(s, &mcp.Tool{Name: "list_kinds", Annotations: ro,
		Description: "List every kind this provider has: its apiVersion, shape (crud, upsert, settings, assoc), the Terraform resource types it stands in for, whether it has passed against the real API, and a one-line description. Filter by text (matched against kind, group, Terraform type and description) or API group."},
		func(_ context.Context, _ *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, listOut, error) {
			return nil, c.list(in), nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "find_kinds_for_terraform", Annotations: ro,
		Description: "Given Terraform resource types (e.g. cloudflare_access_rule), name the kinds that stand in for each."},
		func(_ context.Context, _ *mcp.CallToolRequest, in tfIn) (*mcp.CallToolResult, tfOut, error) {
			return nil, c.forTerraform(in.Types), nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "describe_kind", Annotations: ro,
		Description: "Everything about one kind: its description, every spec and status field (type, required, description, enum, default, which URL parameter it fills, write-only and create-only fields), the REST calls behind create, read, update and delete, its Terraform equivalents, and its live-test record. Name the kind as Kind, Kind.group, or \"group/version, Kind\"."},
		func(_ context.Context, _ *mcp.CallToolRequest, in kindIn) (*mcp.CallToolResult, *KindDoc, error) {
			k, err := c.find(in.Kind)
			if err != nil {
				return nil, nil, err
			}
			return nil, c.describe(k, in.Fields), nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "example", Annotations: ro,
		Description: "A manifest for one kind, with the Secret and Connection it needs. Prefers the spec the live tests ran against the real API (tested: live, with the date), then the descriptor's sample, which the every-kind test runs on the fake API (tested: fake), then one built from the CRD's required fields (tested: none). <placeholders> are values to fill in. withRelated lists the repository's examples that show the kind with the kinds it needs, or that need it, joined by spec.fieldRefs (each tested on the fake API)."},
		func(_ context.Context, _ *mcp.CallToolRequest, in exampleIn) (*mcp.CallToolResult, *Example, error) {
			k, err := c.find(in.Kind)
			if err != nil {
				return nil, nil, err
			}
			ex, err := c.example(k, !in.WithoutConnection, o)
			return nil, ex, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "validate_manifest", Annotations: ro,
		Description: "Check a YAML or JSON manifest (one or more documents) against each kind's CRD schema as the API server would, and against what the controller needs: required fields, types, enums, the fields that fill URL parameters, and fields the CRD does not have (which the API server silently drops)."},
		func(_ context.Context, _ *mcp.CallToolRequest, in manifestIn) (*mcp.CallToolResult, *Validation, error) {
			v, err := c.validate(in.Manifest)
			return nil, v, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "plan_on_fake", Annotations: ro,
		Description: "Run the real controller on a manifest against an in-memory fake of the API (built from the same descriptors) and return, for each object, the REST calls it made (method, path, query, JSON body, status), whether it became Ready, and its status. With delete: true, then delete the objects and return those calls too. Shows exactly what the controller sends; it does not prove the vendor's API accepts it."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in planIn) (*mcp.CallToolResult, *Plan, error) {
			p, err := c.plan(ctx, in.Manifest, in.Delete, o)
			return nil, p, err
		})
	return s, nil
}

func ptr[T any](v T) *T { return &v }

type listIn struct {
	Query string `json:"query,omitempty" jsonschema:"text to match against kind, group, Terraform type and description"`
	Group string `json:"group,omitempty" jsonschema:"only kinds of this API group"`
}

// KindSummary is one row of list_kinds.
type KindSummary struct {
	Kind       string   `json:"kind"`
	APIVersion string   `json:"apiVersion"`
	Shape      string   `json:"shape"`
	Terraform  []string `json:"terraform,omitempty"`
	// Readiness is "live" once the kind passed against the real API.
	Readiness   string `json:"readiness,omitempty"`
	Description string `json:"description,omitempty"`
}

type listOut struct {
	Vendor string        `json:"vendor"`
	Count  int           `json:"count"`
	Kinds  []KindSummary `json:"kinds"`
}

func (c *catalog) list(in listIn) listOut {
	q := strings.ToLower(strings.TrimSpace(in.Query))
	out := listOut{Vendor: c.file.Vendor, Kinds: []KindSummary{}}
	for i := range c.file.Kinds {
		k := &c.file.Kinds[i]
		if in.Group != "" && k.Group != in.Group {
			continue
		}
		desc := c.description(k)
		if q != "" && !strings.Contains(strings.ToLower(strings.Join(append([]string{k.Kind, k.Group, desc}, k.Terraform...), " ")), q) {
			continue
		}
		out.Kinds = append(out.Kinds, KindSummary{Kind: k.Kind, APIVersion: k.Group + "/" + k.Version, Shape: string(k.Shape),
			Terraform: k.Terraform, Readiness: c.readiness(k), Description: firstSentence(desc, 200)})
	}
	sort.Slice(out.Kinds, func(a, b int) bool {
		return out.Kinds[a].APIVersion+out.Kinds[a].Kind < out.Kinds[b].APIVersion+out.Kinds[b].Kind
	})
	out.Count = len(out.Kinds)
	return out
}

func (c *catalog) readiness(k *descriptor.Kind) string {
	if r, ok := c.records[key(k.Kind, k.Group)]; ok && r.Result == "pass" {
		return "live"
	}
	if k.Readiness != "" {
		return k.Readiness
	}
	return "fake"
}

type tfIn struct {
	Types []string `json:"types" jsonschema:"Terraform resource types, e.g. cloudflare_access_rule"`
}

type tfOut struct {
	// Kinds maps each Terraform type to the kinds (Kind.group) that stand
	// in for it; an empty list means none here.
	Kinds map[string][]string `json:"kinds"`
}

func (c *catalog) forTerraform(tfTypes []string) tfOut {
	out := tfOut{Kinds: map[string][]string{}}
	for _, t := range tfTypes {
		out.Kinds[t] = []string{}
		for i := range c.file.Kinds {
			k := &c.file.Kinds[i]
			for _, kt := range k.Terraform {
				if kt == t {
					out.Kinds[t] = append(out.Kinds[t], key(k.Kind, k.Group))
				}
			}
		}
	}
	return out
}

type kindIn struct {
	Kind string `json:"kind" jsonschema:"Kind, Kind.group, or group/version, Kind"`
	// Fields limits the field list to paths starting with one of these.
	Fields []string `json:"fields,omitempty" jsonschema:"only fields whose path starts with one of these (e.g. configuration)"`
}

// Operation is one REST call behind a kind.
type Operation struct {
	Op     string            `json:"op"`
	Method string            `json:"method"`
	Path   string            `json:"path"`
	Query  map[string]string `json:"query,omitempty"`
	// GraphQL is the operation's document, for a GraphQL call (a POST of
	// it and its variables to Path).
	GraphQL string `json:"graphql,omitempty"`
}

// KindDoc is describe_kind's answer.
type KindDoc struct {
	Kind        string      `json:"kind"`
	APIVersion  string      `json:"apiVersion"`
	Shape       string      `json:"shape"`
	Description string      `json:"description,omitempty"`
	Terraform   []string    `json:"terraform,omitempty"`
	Readiness   string      `json:"readiness"`
	Live        *LiveStatus `json:"live,omitempty"`
	Operations  []Operation `json:"operations"`
	// IDFrom is the create response field that holds the server's ID.
	IDFrom  string   `json:"idFrom,omitempty"`
	Spec    []Field  `json:"spec,omitempty"`
	Status  []Field  `json:"status,omitempty"`
	Notes   []string `json:"notes,omitempty"`
	Connect string   `json:"connection"`
	// Open, for a draft (readiness "draft"): what keeps it from being a
	// kind. Its operations are the generator's candidates; nothing runs it.
	Open []string `json:"open,omitempty"`
}

// LiveStatus is the kind's last live-test result.
type LiveStatus struct {
	Result   string   `json:"result,omitempty"`
	Date     string   `json:"date,omitempty"`
	LastPass string   `json:"lastPass,omitempty"`
	Target   string   `json:"target,omitempty"`
	Steps    []string `json:"steps,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	// Skipped is why the live tests list the kind but do not run it.
	Skipped string `json:"skipped,omitempty"`
	// FakeOnly: the real feature is sold only by contract, so the kind is
	// never run live; the fake API is its only test (Skipped says why).
	FakeOnly bool `json:"fakeOnly,omitempty"`
}

func (c *catalog) describe(k *descriptor.Kind, only []string) *KindDoc {
	d := &KindDoc{Kind: k.Kind, APIVersion: k.Group + "/" + k.Version, Shape: string(k.Shape), Description: c.description(k),
		Terraform: k.Terraform, Readiness: c.readiness(k),
		Connect: fmt.Sprintf("spec.connectionRef.name (default \"default\"): a %s/%s %s in the same namespace", c.file.Connection.Group, c.file.Connection.Version, c.file.Connection.Kind)}
	ops := []struct {
		name string
		op   *descriptor.Op
	}{{"create", k.Create}, {"read", &k.Read}, {"update", k.Update}, {"delete", k.Delete}}
	for _, o := range ops {
		if o.op != nil && o.op.Method != "" {
			op := Operation{Op: o.name, Method: o.op.Method, Path: o.op.Path, Query: o.op.Query}
			if o.op.GraphQL != nil {
				op.GraphQL = o.op.GraphQL.Query
			}
			d.Operations = append(d.Operations, op)
		}
	}
	for i := range k.BeforeDelete {
		b := &k.BeforeDelete[i]
		d.Operations = append(d.Operations, Operation{Op: "beforeDelete", Method: b.Method, Path: b.Path, Query: b.Query})
	}
	if k.IsDraft() && k.Draft != nil {
		d.Open = k.Draft.Open
		d.Notes = append(d.Notes, "a draft: never reconciled, and its CRD is not installed by default (config/crds/drafts); its operations are candidates, unconfirmed")
	}
	if k.ID != nil {
		d.IDFrom = k.ID.From
	}
	if r, ok := c.records[key(k.Kind, k.Group)]; ok {
		d.Live = &LiveStatus{Result: r.Result, Date: r.Date, LastPass: r.LastPass, Target: r.Target, Steps: r.Steps, Reason: r.Reason}
	}
	if lk := c.liveKind(k); lk != nil && lk.Skip != "" {
		if d.Live == nil {
			d.Live = &LiveStatus{}
		}
		d.Live.Skipped = lk.Skip
		d.Live.FakeOnly = lk.FakeOnly != ""
	}
	spec, status := c.fields(k)
	d.Spec, d.Status = filter(spec, only), filter(status, only)
	if spec == nil {
		d.Notes = append(d.Notes, "no CRD loaded: fields are not listed")
	}
	switch k.Shape {
	case descriptor.Settings:
		d.Notes = append(d.Notes, "settings: a singleton that always exists; deleting the object leaves the settings as they are")
		if k.Unread != "" {
			d.Notes = append(d.Notes, "unread: never read ("+k.Unread+"); written when the spec changes and only then, so a change made outside Kubernetes is not seen or put back")
		}
	case descriptor.Assoc:
		d.Notes = append(d.Notes, "assoc: a link with no identity; deleting the object undoes the link")
	case descriptor.SetShape:
		d.Notes = append(d.Notes, "set: spec."+k.Set.Field+" lists every element (by "+k.Set.KeyNames()+"); the controller adds, changes and removes elements until the API holds exactly those, removing any it does not list; deleting the object removes its elements")
		if b := k.Set.Batch; b != nil {
			d.Notes = append(d.Notes, "set.batch: spec."+b.Field+" (by "+b.Key+") is added to in one call; its elements are removed only all together, so removing or changing one is refused unless the list is empty, which removes them all; left out, the list is left as it is")
		}
	case descriptor.SQLShape:
		d.Notes = append(d.Notes, "sql ("+k.SQL.Mode+"): managed with SQL statements sent to "+k.Read.Path+"; create: "+k.SQL.Create+"; delete: "+k.SQL.Delete)
	}
	if k.Update == nil && k.Shape == descriptor.CRUD {
		if k.Replace != nil {
			d.Notes = append(d.Notes, "no update call: set spec.replaceOnChange: true to apply a changed spec by re-creating the resource in place")
		} else {
			d.Notes = append(d.Notes, "no update call: a changed spec is reported as drift and nothing is sent; delete and recreate the object")
		}
	}
	if len(k.CreateOnly) > 0 {
		d.Notes = append(d.Notes, "create-only fields ("+strings.Join(k.CreateOnly, ", ")+") are sent on create only")
	}
	if len(k.WriteOnly) > 0 {
		d.Notes = append(d.Notes, "write-only fields ("+strings.Join(k.WriteOnly, ", ")+") are never read back; fill them from a Secret with spec.secretRefs")
	}
	if len(k.Params) > 0 {
		var fs []string
		for _, p := range k.Params {
			fs = append(fs, p.Field)
		}
		d.Notes = append(d.Notes, "a field another object of this provider makes ("+strings.Join(fs, ", ")+", or any other) can be filled from it: "+
			"spec.fieldRefs: {<field>: {kind, name}} takes that object's status.id once it is Ready (fieldPath picks another field)")
	}
	return d
}

func filter(fields []Field, only []string) []Field {
	if len(only) == 0 {
		return fields
	}
	var out []Field
	for _, f := range fields {
		for _, p := range only {
			if f.Path == p || strings.HasPrefix(f.Path, p+".") || strings.HasPrefix(f.Path, p+"[]") {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

type exampleIn struct {
	Kind              string `json:"kind" jsonschema:"Kind, Kind.group, or group/version, Kind"`
	WithoutConnection bool   `json:"withoutConnection,omitempty" jsonschema:"leave out the Secret and Connection"`
}

type manifestIn struct {
	Manifest string `json:"manifest" jsonschema:"YAML or JSON, one or more documents"`
}

type planIn struct {
	Manifest string `json:"manifest" jsonschema:"YAML or JSON, one or more documents; a Connection and its Secret are made up if left out"`
	Delete   bool   `json:"delete,omitempty" jsonschema:"also delete the objects and return the delete calls"`
}

// Main serves over stdio until the client goes away. --dir is the vendor
// repository (default: the working directory), where config/crds and
// hack/live are read from when Options does not hold them.
func Main(o Options) {
	dir := flag.String("dir", ".", "the vendor repository: config/crds and hack/live are read from here")
	flag.Parse()
	if err := Load(&o, *dir); err != nil {
		log.Fatal(err)
	}
	s, err := New(o)
	if err != nil {
		log.Fatal(err)
	}
	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

// Load fills the Options a repository holds on disk under dir: CRDs from
// config/crds, and hack/live/live.yaml and records.json. What is missing
// is left empty.
func Load(o *Options, dir string) error {
	if o.CRDs == nil {
		if st, err := os.Stat(filepath.Join(dir, "config", "crds")); err == nil && st.IsDir() {
			o.CRDs = os.DirFS(filepath.Join(dir, "config", "crds"))
		}
	}
	read := func(dst *[]byte, rel string) error {
		if len(*dst) > 0 {
			return nil
		}
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		*dst = data
		return err
	}
	if o.Examples == nil {
		if st, err := os.Stat(filepath.Join(dir, "examples")); err == nil && st.IsDir() {
			o.Examples = os.DirFS(filepath.Join(dir, "examples"))
		}
	}
	if err := read(&o.Live, filepath.Join("hack", "live", "live.yaml")); err != nil {
		return err
	}
	return read(&o.Records, filepath.Join("hack", "live", "records.json"))
}
