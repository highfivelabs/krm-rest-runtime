// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/highfivelabs/krm-rest-runtime/crdcheck"
	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/reconciler"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

var templateRE = regexp.MustCompile(`\{\{([a-zA-Z_]+)(?::([A-Za-z0-9_.-]+))?\}\}`)

// ValidExamples checks every kind's sample, and its spec in live.yaml (if
// given), against the kind's CRD in dir as the API server would on create.
// The every-kind and live tests run on an in-memory Kubernetes that checks
// no schema, so without this a spec the vendor's API takes could still be
// one a real cluster refuses. Live templates become placeholders; one in a
// number field becomes 0.
func ValidExamples(t *testing.T, typesYAML, liveYAML []byte, dir string) {
	t.Helper()
	problems, err := ExampleProblems(typesYAML, liveYAML, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// ExampleProblems is ValidExamples' findings, one line each.
func ExampleProblems(typesYAML, liveYAML []byte, dir string) ([]string, error) {
	f, err := descriptor.Parse(typesYAML)
	if err != nil {
		return nil, err
	}
	crds, err := crdcheck.Load(os.DirFS(dir))
	if err != nil {
		return nil, err
	}
	var live *LiveConfig
	if len(liveYAML) > 0 {
		if live, err = ParseLiveConfig(liveYAML); err != nil {
			return nil, err
		}
	}
	var out []string
	for i := range f.Kinds {
		k := &f.Kinds[i]
		s := crdcheck.Schema(crds[k.Kind+"."+k.Group], k.Version)
		if s == nil {
			out = append(out, fmt.Sprintf("%s.%s: no CRD in %s", k.Kind, k.Group, dir))
			continue
		}
		var cerr error
		check := func(what string, spec map[string]any) {
			obj := map[string]any{
				"apiVersion": k.Group + "/" + k.Version, "kind": k.Kind,
				"metadata": map[string]any{"name": "example", "namespace": ns},
				"spec":     spec,
			}
			if sp, ok := s.Properties["spec"]; ok {
				crdcheck.NumberPlaceholders(&sp, spec, "")
			}
			problems, err := crdcheck.Validate(s, obj)
			if err != nil {
				cerr = fmt.Errorf("%s.%s: %w", k.Kind, k.Group, err)
				return
			}
			for _, p := range problems {
				if p.Error {
					out = append(out, fmt.Sprintf("%s.%s %s: %s: %s", k.Kind, k.Group, what, p.Field, p.Message))
				}
			}
		}
		if sample := copySpec(k.Sample); sample != nil {
			check("sample", sample)
		}
		if cerr != nil {
			return nil, cerr
		}
		if live == nil {
			continue
		}
		lk := findLiveKind(live, k)
		if lk == nil || lk.Skip != "" {
			continue
		}
		spec := map[string]any{}
		if !lk.ReplaceSample {
			if spec = copySpec(k.Sample); spec == nil {
				spec = map[string]any{}
			}
		}
		for field, v := range lk.Spec {
			if v == nil {
				delete(spec, field)
				continue
			}
			spec[field] = placeholders(v)
		}
		check("live spec", spec)
		if cerr != nil {
			return nil, cerr
		}
	}
	return out, nil
}

func findLiveKind(c *LiveConfig, k *descriptor.Kind) *LiveKind {
	for i := range c.Kinds {
		lk := &c.Kinds[i]
		if lk.Kind == k.Kind && (lk.Group == "" || lk.Group == k.Group) {
			return lk
		}
	}
	return nil
}

func copySpec(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	return runtime.DeepCopyJSONValue(m).(map[string]any)
}

// placeholders turns live templates into "<name>" placeholders.
func placeholders(v any) any {
	switch x := v.(type) {
	case string:
		return templateRE.ReplaceAllStringFunc(x, func(m string) string {
			p := templateRE.FindStringSubmatch(m)
			if p[2] != "" {
				return "<" + p[2] + ">"
			}
			return "<" + strings.ToLower(p[1]) + ">"
		})
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = placeholders(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = placeholders(e)
		}
		return out
	}
	return v
}

// ValidExampleFiles checks every example file, dir/<Kind>/*.yaml (the
// vendor repository's examples/, written by gen/examples.py): each object
// passes its CRD in crdDir, and the controllers make them all Ready on the
// fake API, a parent before the kinds that refer to it, and delete them
// again (RunManifest).
func ValidExampleFiles(t *testing.T, typesYAML []byte, vendor *rest.Vendor, hooks map[string][]reconciler.Hook, dir, crdDir string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		t.Run(strings.TrimPrefix(file, dir+string(filepath.Separator)), func(t *testing.T) {
			problems, err := ExampleFileProblems(typesYAML, vendor, hooks, file, crdDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range problems {
				t.Error(p)
			}
		})
	}
}

// ExampleFileProblems is ValidExampleFiles' findings for one file.
func ExampleFileProblems(typesYAML []byte, vendor *rest.Vendor, hooks map[string][]reconciler.Hook, file, crdDir string) ([]string, error) {
	f, err := descriptor.Parse(typesYAML)
	if err != nil {
		return nil, err
	}
	crds, err := crdcheck.Load(os.DirFS(crdDir))
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var docs []map[string]any
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var m map[string]any
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		if len(m) > 0 {
			docs = append(docs, m)
		}
	}
	var out []string
	for i, d := range docs {
		apiVersion, _ := d["apiVersion"].(string)
		kind, _ := d["kind"].(string)
		k := kindByGVK(f, apiVersion, kind)
		if k == nil {
			continue
		}
		s := crdcheck.Schema(crds[k.Kind+"."+k.Group], k.Version)
		if s == nil {
			out = append(out, fmt.Sprintf("document %d: %s: no CRD in %s", i, kind, crdDir))
			continue
		}
		obj := runtime.DeepCopyJSONValue(d).(map[string]any)
		if spec, ok := obj["spec"].(map[string]any); ok {
			if sp, ok := s.Properties["spec"]; ok {
				crdcheck.NumberPlaceholders(&sp, spec, "")
			}
		}
		problems, err := crdcheck.Validate(s, obj)
		if err != nil {
			return nil, err
		}
		for _, p := range problems {
			if p.Error {
				out = append(out, fmt.Sprintf("document %d (%s): %s: %s", i, kind, p.Field, p.Message))
			}
		}
	}
	run, err := RunManifest(context.Background(), f, vendor, hooks, docs, true)
	if err != nil {
		return nil, err
	}
	for _, o := range run.Objects {
		if !o.Ready || o.Error != "" {
			out = append(out, fmt.Sprintf("%s %s: not Ready on the fake API: %s %s %s", o.Kind, o.Name, o.Reason, o.Message, o.Error))
		}
	}
	return out, nil
}
