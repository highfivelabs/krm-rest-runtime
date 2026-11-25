// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/highfivelabs/krm-rest-runtime/crdcheck"
)

// ValidCRDs runs the API server's own CRD validation (structural schemas
// included) over every CRD in dir, as `kubectl apply` would meet it, and
// over the draft kinds' CRDs in dir/drafts.
func ValidCRDs(t *testing.T, dir string) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = apiextensions.AddToScheme(scheme)
	_ = apiextensionsv1.AddToScheme(scheme)
	files, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
	drafts, _ := filepath.Glob(filepath.Join(dir, crdcheck.DraftsDir, "*.yaml"))
	files = append(files, drafts...)
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, "kustomization.yaml") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		v1 := &apiextensionsv1.CustomResourceDefinition{}
		if err := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096).Decode(v1); err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
			continue
		}
		apiextensionsv1.SetObjectDefaults_CustomResourceDefinition(v1)
		internal := &apiextensions.CustomResourceDefinition{}
		if err := scheme.Convert(v1, internal, nil); err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
			continue
		}
		if errs := validation.ValidateCustomResourceDefinition(context.Background(), internal); len(errs) > 0 {
			msg := errs.ToAggregate().Error()
			if len(msg) > 600 {
				msg = msg[:600] + "…"
			}
			t.Errorf("%s: %s", filepath.Base(f), msg)
		}
		n++
	}
	if n == 0 {
		t.Fatalf("no CRDs in %s", dir)
	}
}
