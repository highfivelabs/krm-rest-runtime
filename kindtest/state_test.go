// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/rest"
)

var stateVendor = &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}

// Fields carried from the last read into an update and a delete: a
// Kubernetes-style API's resourceVersion and a name the server chose
// (Grafana's time interval), and a GitHub file's blob sha. The fake refuses
// a write without the current value (409) and moves the version on after
// each update, so only a fresh read gets it through.
func TestEveryKindCarry(t *testing.T) {
	EveryKind(t, []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: TimeInterval
  group: apps.example.hfl.dev
  version: v1alpha1
  plural: timeintervals
  shape: crud
  create: {method: POST, path: "/apis/{namespace}/timeintervals"}
  read: {method: GET, path: "/apis/{namespace}/timeintervals/{name}"}
  update: {method: PUT, path: "/apis/{namespace}/timeintervals/{name}", carry: [metadata.name, metadata.resourceVersion]}
  delete: {method: DELETE, path: "/apis/{namespace}/timeintervals/{name}"}
  params: [{name: namespace, field: namespace}]
  id: {param: name, from: metadata/name}
  sample: {namespace: default, kind: TimeInterval, metadata: {}, title: weekdays}
- kind: RepositoryFile
  group: repository.example.hfl.dev
  version: v1alpha1
  plural: repositoryfiles
  shape: upsert
  read: {method: GET, path: "/repos/{repo}/contents/{path}", query: {ref: "{branch}"}}
  update: {method: PUT, path: "/repos/{repo}/contents/{path}", carry: [sha]}
  delete: {method: DELETE, path: "/repos/{repo}/contents/{path}", carry: [sha, message, branch]}
  params: [{name: repo, field: repo}, {name: path, field: path}]
  writeOnly: [content]
  sample: {repo: web, path: README.md, branch: main, message: add a readme, content: aGk=}
`), stateVendor, nil)
}

// One object read on another path than it is written at: Grafana's public
// dashboard (read at .../public-dashboards, updated and deleted at
// .../public-dashboards/{uid}) and Vault's tokenization key configuration
// (written at .../keys/{name}/config, read at .../keys/{name}).
func TestEveryKindReadsElsewhere(t *testing.T) {
	EveryKind(t, []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: DashboardPublic
  group: dashboard.example.hfl.dev
  version: v1alpha1
  plural: dashboardpublics
  shape: crud
  create: {method: POST, path: "/dashboards/uid/{dashboardUid}/public-dashboards"}
  read: {method: GET, path: "/dashboards/uid/{dashboardUid}/public-dashboards"}
  update: {method: PATCH, path: "/dashboards/uid/{dashboardUid}/public-dashboards/{uid}"}
  delete: {method: DELETE, path: "/dashboards/uid/{dashboardUid}/public-dashboards/{uid}"}
  params: [{name: dashboardUid, field: dashboardUid}]
  id: {param: uid, from: uid}
  sample: {dashboardUid: d1, share: public}
- kind: TokenizationKeyConfig
  group: transform.example.hfl.dev
  version: v1alpha1
  plural: tokenizationkeyconfigs
  shape: settings
  read: {method: GET, path: "/{mount}/tokenization/keys/{name}"}
  update: {method: POST, path: "/{mount}/tokenization/keys/{name}/config"}
  params: [{name: mount, field: mount}, {name: name, field: name}]
  envelope: data
  sample: {mount: transform, name: ssn, auto_rotate_period: 4320h}
`), stateVendor, nil)
}

// A list that names its items only (Vault's LIST answers
// {"data": {"keys": ["a", "b"]}}): find field "" is the element itself.
func TestEveryKindBareNames(t *testing.T) {
	EveryKind(t, []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: KmipScope
  group: kmip.example.hfl.dev
  version: v1alpha1
  plural: kmipscopes
  shape: upsert
  read: {method: GET, path: "/{mount}/scope", query: {list: "true"}, find: {list: data/keys, param: scope}}
  update: {method: POST, path: "/{mount}/scope/{scope}"}
  delete: {method: DELETE, path: "/{mount}/scope/{scope}"}
  params: [{name: mount, field: mount}, {name: scope, field: scope}]
  envelope: data
  compareReturnedOnly: true
  sample: {mount: kmip, scope: payments}
`), stateVendor, nil)
}

// A read that answers 200, not 404, when there is none: Atlas's push-based
// log export (state: UNCONFIGURED) and search deployment (no id). The first
// write is then the create, and a delete leaves that answer.
func TestEveryKindAbsent(t *testing.T) {
	EveryKind(t, []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: PushBasedLogExport
  group: push.example.hfl.dev
  version: v1alpha1
  plural: pushbasedlogexports
  shape: upsert
  create: {method: POST, path: "/groups/{groupId}/pushBasedLogExport"}
  read: {method: GET, path: "/groups/{groupId}/pushBasedLogExport", absent: {state: UNCONFIGURED}}
  update: {method: PATCH, path: "/groups/{groupId}/pushBasedLogExport"}
  delete: {method: DELETE, path: "/groups/{groupId}/pushBasedLogExport"}
  params: [{name: groupId, field: groupId}]
  sample: {groupId: g1, bucketName: logs, prefixPath: atlas}
  serverFields: {state: ACTIVE}
- kind: SearchDeployment
  group: search.example.hfl.dev
  version: v1alpha1
  plural: searchdeployments
  shape: upsert
  create: {method: POST, path: "/groups/{groupId}/clusters/{clusterName}/search/deployment"}
  read: {method: GET, path: "/groups/{groupId}/clusters/{clusterName}/search/deployment", absent: {id: null}}
  update: {method: PATCH, path: "/groups/{groupId}/clusters/{clusterName}/search/deployment"}
  delete: {method: DELETE, path: "/groups/{groupId}/clusters/{clusterName}/search/deployment"}
  params: [{name: groupId, field: groupId}, {name: clusterName, field: clusterName}]
  sample: {groupId: g1, clusterName: c1, specs: [], name: s1}
  serverFields: {id: "650000000000000000000001", stateName: IDLE}
`), stateVendor, nil)
}

// Objects the vendor makes itself, taken over rather than created (Okta's
// default policies): found in a list by their fields, updated, and left
// when the object is deleted. Two kinds adopt from one list.
func TestEveryKindAdopt(t *testing.T) {
	EveryKind(t, []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: PolicyMfaDefault
  group: policy.example.hfl.dev
  version: v1alpha1
  plural: policymfadefaults
  shape: crud
  read: {method: GET, path: "/policies/{policyId}"}
  update: {method: PUT, path: "/policies/{policyId}"}
  adopt: {method: GET, path: /policies, query: {type: MFA_ENROLL}, match: {system: "true"}}
  forget: Okta makes it.
  id: {param: policyId, from: id}
  sample: {name: Default Policy, type: MFA_ENROLL}
- kind: PostAuthRule
  group: post.example.hfl.dev
  version: v1alpha1
  plural: postauthrules
  shape: crud
  read: {method: GET, path: "/policies/{policyId}/rules/{ruleId}"}
  update: {method: PUT, path: "/policies/{policyId}/rules/{ruleId}"}
  adopt: {method: GET, path: "/policies/{policyId}/rules", match: {name: "{name}"}}
  forget: It cannot be created or deleted.
  params: [{name: policyId, field: policyId}]
  id: {param: ruleId, from: id}
  sample: {policyId: p1, name: Default Rule}
- kind: SessionViolationRule
  group: session.example.hfl.dev
  version: v1alpha1
  plural: sessionviolationrules
  shape: crud
  read: {method: GET, path: "/policies/{policyId}/rules/{ruleId}"}
  update: {method: PUT, path: "/policies/{policyId}/rules/{ruleId}"}
  adopt: {method: GET, path: "/policies/{policyId}/rules", match: {name: "{name}"}}
  forget: It cannot be created or deleted.
  params: [{name: policyId, field: policyId}]
  id: {param: ruleId, from: id}
  sample: {policyId: p1, name: Default Rule}
`), stateVendor, nil)
}

// An upsert whose one write creates and updates (Vault's GCP KMS key):
// create-only fields go on the first write and are left out after it,
// which the fake refuses them on.
func TestEveryKindUpsertCreateOnly(t *testing.T) {
	EveryKind(t, []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: GcpkmsKey
  group: gcpkms.example.hfl.dev
  version: v1alpha1
  plural: gcpkmskeys
  shape: upsert
  read: {method: GET, path: "/{mount}/keys/{key}"}
  update: {method: POST, path: "/{mount}/keys/{key}"}
  delete: {method: DELETE, path: "/{mount}/keys/{key}"}
  params: [{name: mount, field: mount}, {name: key, field: key}]
  createOnly: [purpose]
  sample: {mount: gcpkms, key: k1, key_ring: ring, purpose: encrypt_decrypt}
- kind: AcmeAccount
  group: pki.example.hfl.dev
  version: v1alpha1
  plural: acmeaccounts
  shape: upsert
  create: {method: POST, path: "/{mount}/config/acme-account/{name}"}
  read: {method: GET, path: "/{mount}/config/acme-account/{name}"}
  update: {method: PUT, path: "/{mount}/config/acme-account/{name}"}
  delete: {method: DELETE, path: "/{mount}/config/acme-account/{name}"}
  params: [{name: mount, field: mount}, {name: name, field: name}]
  createOnly: [directory_url]
  sample: {mount: pki, name: le, directory_url: "https://acme.example/directory", default_nameserver: ns1}
`), stateVendor, nil)
}

// A caller-named item made on another path that names it too (Vault's
// imported KMIP CA: POST .../ca/{name}/import, read and updated at
// .../ca/{name}).
func TestEveryKindUpsertCreatedElsewhere(t *testing.T) {
	EveryKind(t, []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: KmipCaImported
  group: kmip.example.hfl.dev
  version: v1alpha1
  plural: kmipcaimporteds
  shape: upsert
  create: {method: POST, path: "/{mount}/ca/{name}/import"}
  read: {method: GET, path: "/{mount}/ca/{name}"}
  update: {method: POST, path: "/{mount}/ca/{name}"}
  delete: {method: DELETE, path: "/{mount}/ca/{name}"}
  params: [{name: mount, field: mount}, {name: name, field: name}]
  createOnly: [ca_pem]
  sample: {mount: kmip, name: ca1, ca_pem: pem, scope_field: OU}
`), stateVendor, nil)
}

// A create-only field the update must still send as the API has it
// (Confluent's Flink statement: name is immutable, and its PUT requires it
// with metadata.resource_version): carried, it goes; changed in the spec,
// it is drift.
func TestEveryKindCarryCreateOnly(t *testing.T) {
	EveryKind(t, []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: FlinkStatement
  group: flink.example.hfl.dev
  version: v1alpha1
  plural: flinkstatements
  shape: crud
  create: {method: POST, path: "/environments/{environment_id}/statements"}
  read: {method: GET, path: "/environments/{environment_id}/statements/{statement_name}"}
  update: {method: PUT, path: "/environments/{environment_id}/statements/{statement_name}", carry: [name, metadata.resource_version]}
  delete: {method: DELETE, path: "/environments/{environment_id}/statements/{statement_name}"}
  params: [{name: environment_id, field: environment_id}]
  id: {param: statement_name, from: name}
  createOnly: [name]
  sample: {environment_id: env-1, name: s1, principal: sa-1, spec: {statement: "SELECT 1;"}}
`), stateVendor, nil)
}
