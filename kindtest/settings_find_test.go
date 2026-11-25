// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/rest"
)

func TestEveryKindSettingsFind(t *testing.T) {
	typesYAML := []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: ProjectFilter
  group: project.example.hfl.dev
  version: v1alpha1
  plural: projectfilters
  shape: settings
  read:
    method: GET
    path: "/projects/{org}/{project}/filters/"
    find: {list: "", param: filter_id, field: id}
  update:
    method: PUT
    path: "/projects/{org}/{project}/filters/{filter_id}/"
  params:
  - {name: org, field: org}
  - {name: project, field: project}
  - {name: filter_id, field: filterId}
  sample:
    org: acme
    project: web
    filterId: browser-extensions
    mode: enabled
`)
	EveryKind(t, typesYAML, &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}, nil)
}
