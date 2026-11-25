// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/highfivelabs/krm-rest-runtime/crdcheck"
	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/kindtest"
)

// catalog is everything the tools answer from: the descriptors, and, when
// the vendor repository supplies them, its CRDs and its live-test config and
// records.
type catalog struct {
	file    *descriptor.File
	crds    map[string]*apiextensionsv1.CustomResourceDefinition // by "Kind.group"
	live    *kindtest.LiveConfig
	records map[string]kindtest.LiveRecord // by "Kind.group"
	// examples is the repository's examples/ directory, if any.
	examples fs.FS
}

func key(kind, group string) string { return kind + "." + group }

func loadCatalog(o Options) (*catalog, error) {
	f, err := descriptor.Parse(o.Types)
	if err != nil {
		return nil, fmt.Errorf("types.yaml: %w", err)
	}
	c := &catalog{file: f, crds: map[string]*apiextensionsv1.CustomResourceDefinition{}, records: map[string]kindtest.LiveRecord{}}
	if o.CRDs != nil {
		if c.crds, err = crdcheck.Load(o.CRDs); err != nil {
			return nil, err
		}
	}
	c.examples = o.Examples
	if len(o.Live) > 0 {
		lc, err := kindtest.ParseLiveConfig(o.Live)
		if err != nil {
			return nil, fmt.Errorf("live.yaml: %w", err)
		}
		c.live = lc
	}
	if len(o.Records) > 0 {
		if err := json.Unmarshal(o.Records, &c.records); err != nil {
			return nil, fmt.Errorf("records.json: %w", err)
		}
	}
	return c, nil
}

// find resolves a kind by name ("Widget"), "Kind.group" or apiVersion/kind.
// A bare name that more than one group has is an error naming them.
func (c *catalog) find(name string) (*descriptor.Kind, error) {
	name = strings.TrimSpace(name)
	if gv, kind, ok := strings.Cut(name, ","); ok { // "group/version, Kind"
		name = strings.TrimSpace(kind) + "." + strings.Split(strings.TrimSpace(gv), "/")[0]
	}
	var matches []*descriptor.Kind
	for i := range c.file.Kinds {
		k := &c.file.Kinds[i]
		if key(k.Kind, k.Group) == name {
			return k, nil
		}
		if strings.EqualFold(k.Kind, name) {
			matches = append(matches, k)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return nil, fmt.Errorf("no kind %q in %s; list_kinds lists them", name, c.file.Vendor)
	}
	var names []string
	for _, k := range matches {
		names = append(names, key(k.Kind, k.Group))
	}
	return nil, fmt.Errorf("%q is in more than one group; name one of: %s", name, strings.Join(names, ", "))
}

// byGVK finds the kind a manifest names.
func (c *catalog) byGVK(apiVersion, kind string) *descriptor.Kind {
	for i := range c.file.Kinds {
		k := &c.file.Kinds[i]
		if k.Group+"/"+k.Version == apiVersion && k.Kind == kind {
			return k
		}
	}
	return nil
}

// schema is the kind's openAPIV3Schema at its version, or nil without CRDs.
func (c *catalog) schema(k *descriptor.Kind) *apiextensionsv1.JSONSchemaProps {
	return crdcheck.Schema(c.crds[key(k.Kind, k.Group)], k.Version)
}

func (c *catalog) liveKind(k *descriptor.Kind) *kindtest.LiveKind {
	if c.live == nil {
		return nil
	}
	for i := range c.live.Kinds {
		lk := &c.live.Kinds[i]
		if lk.Kind == k.Kind && (lk.Group == "" || lk.Group == k.Group) {
			return lk
		}
	}
	return nil
}

// Field is one spec or status field, flattened to a dotted path
// (items of a list are "[]").
type Field struct {
	Path        string   `json:"path"`
	Type        string   `json:"type,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Default     string   `json:"default,omitempty"`
	Format      string   `json:"format,omitempty"`
	// PathParam names the URL placeholder the field fills.
	PathParam string `json:"pathParam,omitempty"`
	WriteOnly bool   `json:"writeOnly,omitempty"`
	// CreateOnly: sent on create only; a change is drift that only
	// re-creating applies.
	CreateOnly bool `json:"createOnly,omitempty"`
	// AnyValue: the schema accepts any value here.
	AnyValue bool `json:"anyValue,omitempty"`
}

func flatten(s *apiextensionsv1.JSONSchemaProps, prefix string, out []Field, depth int) []Field {
	if s == nil || depth > 12 {
		return out
	}
	req := map[string]bool{}
	for _, r := range s.Required {
		req[r] = true
	}
	names := make([]string, 0, len(s.Properties))
	for n := range s.Properties {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := s.Properties[n]
		fp := n
		if prefix != "" {
			fp = prefix + "." + n
		}
		f := Field{Path: fp, Type: typeOf(&p), Required: req[n], Description: strings.TrimSpace(p.Description), Format: p.Format}
		for _, e := range p.Enum {
			var v any
			if json.Unmarshal(e.Raw, &v) == nil {
				f.Enum = append(f.Enum, fmt.Sprint(v))
			}
		}
		if p.Default != nil {
			f.Default = string(p.Default.Raw)
		}
		if p.XPreserveUnknownFields != nil && *p.XPreserveUnknownFields && len(p.Properties) == 0 {
			f.AnyValue = true
		}
		out = append(out, f)
		switch {
		case len(p.Properties) > 0:
			out = flatten(&p, fp, out, depth+1)
		case p.Items != nil && p.Items.Schema != nil && len(p.Items.Schema.Properties) > 0:
			out = flatten(p.Items.Schema, fp+"[]", out, depth+1)
		}
	}
	return out
}

func typeOf(p *apiextensionsv1.JSONSchemaProps) string {
	t := p.Type
	if p.XIntOrString {
		return "int|string"
	}
	switch t {
	case "array":
		if p.Items != nil && p.Items.Schema != nil {
			it := p.Items.Schema.Type
			if it == "" && len(p.Items.Schema.Properties) > 0 {
				it = "object"
			}
			if it != "" {
				return "[]" + it
			}
		}
	case "object", "":
		if len(p.Properties) == 0 && p.AdditionalProperties != nil && p.AdditionalProperties.Schema != nil {
			if at := p.AdditionalProperties.Schema.Type; at != "" {
				return "map[string]" + at
			}
			return "map"
		}
		if t == "" && len(p.Properties) > 0 {
			return "object"
		}
	}
	if t == "" {
		return "any"
	}
	return t
}

// fields is the kind's spec and status fields from its CRD, marked with
// what the descriptor says about each.
func (c *catalog) fields(k *descriptor.Kind) (spec, status []Field) {
	s := c.schema(k)
	if s == nil {
		return nil, nil
	}
	params := map[string]string{}
	for _, p := range k.Params {
		params[p.Field] = p.Name
	}
	if sp, ok := s.Properties["spec"]; ok {
		spec = flatten(&sp, "", nil, 0)
		for i := range spec {
			f := &spec[i]
			f.PathParam = params[f.Path]
			f.WriteOnly = k.IsWriteOnly(f.Path)
			f.CreateOnly = k.IsCreateOnly(f.Path)
		}
	}
	if st, ok := s.Properties["status"]; ok {
		status = flatten(&st, "", nil, 0)
	}
	return spec, status
}

func (c *catalog) description(k *descriptor.Kind) string {
	if s := c.schema(k); s != nil {
		return strings.TrimSpace(s.Description)
	}
	return ""
}

// firstSentence shortens a description for a listing.
func firstSentence(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if i := strings.Index(s, ". "); i > 0 && i < max {
		return s[:i+1]
	}
	if len(s) > max {
		return strings.TrimSpace(s[:max-1]) + "…"
	}
	return s
}
