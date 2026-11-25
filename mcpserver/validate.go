// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/highfivelabs/krm-rest-runtime/crdcheck"
)

// Problem is one thing wrong with a manifest.
type Problem struct {
	// Doc is the document's index in the manifest, from 0.
	Doc  int    `json:"doc"`
	Kind string `json:"kind,omitempty"`
	Name string `json:"name,omitempty"`
	// Field is the path, e.g. spec.configuration.target.
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
	// Severity: "error" (the API server or the controller refuses it) or
	// "warning" (accepted, but not what was meant: an unknown field is
	// dropped).
	Severity string `json:"severity"`
}

// Validation is the result of validate.
type Validation struct {
	Valid    bool      `json:"valid"`
	Objects  int       `json:"objects"`
	Problems []Problem `json:"problems,omitempty"`
	// Unchecked says what this check does not cover.
	Unchecked string `json:"unchecked"`
}

const unchecked = "Nothing is sent to the vendor's API, so what only the API checks is not checked; plan_on_fake runs the controller against the fake API."

// decodeDocs splits a YAML or JSON stream into objects; empty documents
// are skipped.
func decodeDocs(manifest string) ([]map[string]any, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(manifest)), 4096)
	var docs []map[string]any
	for {
		var m map[string]any
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", len(docs), err)
		}
		if len(m) > 0 {
			docs = append(docs, m)
		}
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("the manifest holds no objects")
	}
	return docs, nil
}

func (c *catalog) validate(manifest string) (*Validation, error) {
	docs, err := decodeDocs(manifest)
	if err != nil {
		return nil, err
	}
	v := &Validation{Objects: len(docs), Unchecked: unchecked}
	add := func(i int, kind, name, fld, sev, msg string) {
		v.Problems = append(v.Problems, Problem{Doc: i, Kind: kind, Name: name, Field: fld, Message: msg, Severity: sev})
	}
	conn := c.file.Connection.Group + "/" + c.file.Connection.Version
	for i, d := range docs {
		apiVersion, _ := d["apiVersion"].(string)
		kind, _ := d["kind"].(string)
		meta, _ := d["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if apiVersion == "" || kind == "" {
			add(i, kind, name, "", "error", "apiVersion and kind are required")
			continue
		}
		if name == "" {
			add(i, kind, name, "metadata.name", "error", "required")
		}
		if (apiVersion == "v1" && kind == "Secret") || (apiVersion == conn && kind == c.file.Connection.Kind) {
			continue // not this vendor's resource kinds; the API server checks them
		}
		k := c.byGVK(apiVersion, kind)
		if k == nil {
			add(i, kind, name, "", "error", fmt.Sprintf("%s %s is not one of %s's kinds; list_kinds lists them", apiVersion, kind, c.file.Vendor))
			continue
		}
		spec, _ := d["spec"].(map[string]any)
		fieldRefs, _ := spec["fieldRefs"].(map[string]any)
		for _, p := range k.Params {
			if _, ref := fieldRefs[p.Field]; ref {
				continue
			}
			if s := p.In(spec); s == nil || s == "" {
				add(i, kind, name, "spec."+p.Field, "error", fmt.Sprintf("required: fills {%s} in %s", p.Name, k.Read.Path))
			}
		}
		if refs, ok := spec["secretRefs"].(map[string]any); ok {
			for f := range refs {
				if _, both := spec[f]; both {
					add(i, kind, name, "spec."+f, "warning", "set both here and in spec.secretRefs; the Secret's value is sent")
				}
			}
		}
		if spec["replaceOnChange"] == true && k.Replace == nil {
			add(i, kind, name, "spec.replaceOnChange", "error", "this kind cannot be re-created in place; the controller refuses it")
		}
		s := c.schema(k)
		if s == nil {
			add(i, kind, name, "", "warning", "no CRD loaded for this kind; its schema was not checked")
			continue
		}
		problems, err := crdcheck.Validate(s, d)
		if err != nil {
			return nil, fmt.Errorf("%s: schema: %w", kind, err)
		}
		for _, p := range problems {
			sev := "warning"
			if p.Error {
				sev = "error"
			}
			add(i, kind, name, p.Field, sev, p.Message)
		}
	}
	// One problem per field: the URL-parameter message says more than the
	// schema's "Required value" for the same field.
	seen := map[string]bool{}
	kept := v.Problems[:0]
	for _, p := range v.Problems {
		k := fmt.Sprint(p.Doc, "|", p.Field, "|", p.Severity)
		if p.Field != "" && seen[k] {
			continue
		}
		seen[k] = true
		kept = append(kept, p)
	}
	v.Problems = kept
	sort.SliceStable(v.Problems, func(a, b int) bool { return v.Problems[a].Doc < v.Problems[b].Doc })
	v.Valid = true
	for _, p := range v.Problems {
		if p.Severity == "error" {
			v.Valid = false
		}
	}
	return v, nil
}
