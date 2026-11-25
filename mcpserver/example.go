// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/highfivelabs/krm-rest-runtime/crdcheck"
	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/kindtest"
)

// Example is a manifest for one kind and what stands behind it.
type Example struct {
	Kind string `json:"kind"`
	// Tested says what the spec has been run against: "live" (passed
	// against the real API on LiveDate), "live-failing" (in the live tests,
	// whose last run failed), "fake" (the descriptor's sample, which the
	// every-kind test runs on the fake API) or "none" (built from the CRD's
	// required fields; untested).
	Tested   string `json:"tested"`
	LiveDate string `json:"liveDate,omitempty"`
	Target   string `json:"target,omitempty"`
	// Prerequisites are REST calls the live test makes before this kind:
	// things the spec refers to ({{var:...}} became <...>) that have no
	// kind of their own here.
	Prerequisites []string `json:"prerequisites,omitempty"`
	Manifest      string   `json:"manifest"`
	Notes         []string `json:"notes,omitempty"`
	// WithRelated are the repository's examples (examples/<Kind>/*.yaml)
	// in which this kind appears, with the kinds it needs or that need it.
	WithRelated []Related `json:"withRelated,omitempty"`
}

// Related is one example file: several objects joined by spec.fieldRefs,
// tested on the fake API (kindtest.ValidExampleFiles).
type Related struct {
	File     string   `json:"file"`
	Kinds    []string `json:"kinds"`
	Manifest string   `json:"manifest"`
}

// related lists the example files naming kind k, its own first.
func (c *catalog) related(k *descriptor.Kind) []Related {
	if c.examples == nil {
		return nil
	}
	files, _ := fs.Glob(c.examples, "*/*.yaml")
	var own, other []Related
	for _, f := range files {
		data, err := fs.ReadFile(c.examples, f)
		if err != nil {
			continue
		}
		var kinds []string
		mentions := false
		for _, doc := range strings.Split(string(data), "\n---\n") {
			var m struct {
				APIVersion string `json:"apiVersion"`
				Kind       string `json:"kind"`
			}
			if yaml.Unmarshal([]byte(doc), &m) != nil || m.Kind == "" {
				continue
			}
			kinds = append(kinds, m.Kind)
			if m.Kind == k.Kind && m.APIVersion == k.Group+"/"+k.Version {
				mentions = true
			}
		}
		if !mentions {
			continue
		}
		r := Related{File: "examples/" + f, Kinds: kinds, Manifest: string(data)}
		if strings.HasPrefix(f, k.Kind+"/") {
			own = append(own, r)
		} else {
			other = append(other, r)
		}
	}
	if len(other) > 5 {
		other = other[:5]
	}
	return append(own, other...)
}

const exampleNS = "default"

func (c *catalog) example(k *descriptor.Kind, withConnection bool, o Options) (*Example, error) {
	ex := &Example{Kind: key(k.Kind, k.Group)}
	name := "example-" + strings.ToLower(k.Kind)
	var spec map[string]any
	if lk := c.liveKind(k); lk != nil && lk.Skip == "" {
		if !lk.ReplaceSample {
			spec = deepCopy(k.Sample)
		}
		if spec == nil {
			spec = map[string]any{}
		}
		for f, v := range lk.Spec {
			if v == nil {
				delete(spec, f)
				continue
			}
			spec[f] = fill(v, name)
		}
		ex.Tested = "live-failing"
		if r, ok := c.records[key(k.Kind, k.Group)]; ok && r.Result == "pass" {
			ex.Tested, ex.LiveDate, ex.Target = "live", r.Date, r.Target
		} else if ok && r.LastPass != "" {
			ex.Notes = append(ex.Notes, fmt.Sprintf("last passed live on %s; the latest run failed: %s", r.LastPass, r.Reason))
		} else if !ok {
			ex.Tested = "fake"
			ex.Notes = append(ex.Notes, "listed in the live tests but not yet run live")
		}
		for _, call := range append(append([]kindtest.LiveCall{}, c.live.Before...), lk.Before...) {
			ex.Prerequisites = append(ex.Prerequisites, call.Method+" "+fillString(call.Path, name))
		}
	} else if len(k.Sample) > 0 {
		spec = deepCopy(k.Sample)
		ex.Tested = "fake"
		if lk != nil {
			ex.Notes = append(ex.Notes, "not run live: "+lk.Skip)
		}
	} else {
		spec = c.requiredSpec(k)
		ex.Tested = "none"
		ex.Notes = append(ex.Notes, "built from the CRD's required fields; replace each <placeholder>")
	}

	if sch := c.schema(k); sch != nil {
		if sp, ok := sch.Properties["spec"]; ok {
			for _, f := range crdcheck.NumberPlaceholders(&sp, spec, "") {
				ex.Notes = append(ex.Notes, "spec."+f+" is a number the live test takes from a value it saves; replace the 0")
			}
		}
	}

	var docs []string
	if withConnection {
		docs = append(docs, connectionDocs(c.file, o)...)
	}
	obj := map[string]any{
		"apiVersion": k.Group + "/" + k.Version,
		"kind":       k.Kind,
		"metadata":   map[string]any{"name": name, "namespace": exampleNS},
		"spec":       spec,
	}
	y, err := yaml.Marshal(obj)
	if err != nil {
		return nil, err
	}
	docs = append(docs, string(y))
	ex.Manifest = strings.Join(docs, "---\n")
	ex.WithRelated = c.related(k)
	if len(k.WriteOnly) > 0 {
		ex.Notes = append(ex.Notes, "write-only fields ("+strings.Join(k.WriteOnly, ", ")+") are better filled from a Secret: spec.secretRefs: {<field>: {name, key}}")
	}
	return ex, nil
}

// connectionDocs are the Secret and Connection every object reaches the
// vendor through.
func connectionDocs(f *descriptor.File, o Options) []string {
	creds := map[string]any{}
	if o.Vendor != nil {
		for _, k := range o.Vendor.CredentialKeys {
			creds[k] = "<" + k + ">"
		}
	}
	secretName := f.Vendor + "-credentials"
	secret := map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata":   map[string]any{"name": secretName, "namespace": exampleNS},
		"stringData": creds,
	}
	connSpec := map[string]any{"secretRef": map[string]any{"name": secretName}}
	if o.Vendor != nil && o.Vendor.DefaultBaseURL != "" {
		connSpec["baseURL"] = o.Vendor.DefaultBaseURL
	} else {
		connSpec["baseURL"] = "<baseURL>"
	}
	conn := map[string]any{
		"apiVersion": f.Connection.Group + "/" + f.Connection.Version, "kind": f.Connection.Kind,
		"metadata": map[string]any{"name": "default", "namespace": exampleNS},
		"spec":     connSpec,
	}
	var out []string
	for _, d := range []map[string]any{secret, conn} {
		y, _ := yaml.Marshal(d)
		out = append(out, string(y))
	}
	return out
}

// requiredSpec is a spec holding a placeholder for every required field.
func (c *catalog) requiredSpec(k *descriptor.Kind) map[string]any {
	spec := map[string]any{}
	specFields, _ := c.fields(k)
	byPath := map[string]Field{}
	for _, f := range specFields {
		byPath[f.Path] = f
	}
	for _, f := range specFields {
		if !f.Required || strings.Contains(f.Path, "[]") {
			continue
		}
		// Only fields whose parents are required too (or top level).
		parts := strings.Split(f.Path, ".")
		ok := true
		for i := 1; i < len(parts); i++ {
			if !byPath[strings.Join(parts[:i], ".")].Required {
				ok = false
			}
		}
		if !ok {
			continue
		}
		node := spec
		for _, p := range parts[:len(parts)-1] {
			next, _ := node[p].(map[string]any)
			if next == nil {
				next = map[string]any{}
				node[p] = next
			}
			node = next
		}
		last := parts[len(parts)-1]
		if _, set := node[last]; !set {
			node[last] = placeholder(f)
		}
	}
	return spec
}

func placeholder(f Field) any {
	switch {
	case len(f.Enum) > 0:
		return f.Enum[0]
	case f.Type == "object":
		return map[string]any{}
	case f.Type == "boolean":
		return false
	case f.Type == "integer" || f.Type == "number":
		return 0
	case strings.HasPrefix(f.Type, "[]"):
		return []any{}
	}
	return "<" + f.Path + ">"
}

var tmplRE = regexp.MustCompile(`\{\{([a-zA-Z_]+)(?::([A-Za-z0-9_.-]+))?\}\}`)

// fill replaces the live tests' {{...}} templates with readable values.
func fill(v any, name string) any {
	switch x := v.(type) {
	case string:
		return fillString(x, name)
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = fill(e, name)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = fill(e, name)
		}
		return out
	}
	return v
}

func fillString(s, name string) string {
	return tmplRE.ReplaceAllStringFunc(s, func(m string) string {
		p := tmplRE.FindStringSubmatch(m)
		switch p[1] {
		case "name", "run":
			return name
		case "name_", "run_":
			return strings.ReplaceAll(name, "-", "_")
		case "NAME", "RUN":
			return strings.ToUpper(name)
		case "NAME_", "RUN_":
			return strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
		case "cred", "env", "var":
			return "<" + p[2] + ">"
		case "date":
			var days int
			if _, err := fmt.Sscanf(p[2], "%d", &days); err == nil {
				return time.Now().UTC().AddDate(0, 0, days).Format("2006-01-02")
			}
		}
		return m
	})
}

func deepCopy(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	return runtime.DeepCopyJSONValue(m).(map[string]any)
}
