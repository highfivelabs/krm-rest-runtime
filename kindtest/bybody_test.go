// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// An RPC API's object: made with {"collector": {...}}, read, changed and
// deleted by calls whose bodies name it (Grafana Fleet Management), the
// read's and delete's bodies outside the kind's bodyEnvelope.
func TestEveryKindReadsByBody(t *testing.T) {
	typesYAML := []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: Collector
  group: fleet.example.hfl.dev
  version: v1alpha1
  plural: collectors
  shape: crud
  bodyEnvelope: collector
  id: {param: id, from: id}
  create: {method: POST, path: /collector.v1.CollectorService/CreateCollector}
  read: {method: POST, path: /collector.v1.CollectorService/GetCollector, body: {id: "{id}"}}
  update: {method: POST, path: /collector.v1.CollectorService/UpdateCollector, body: {id: "{id}"}}
  delete: {method: POST, path: /collector.v1.CollectorService/DeleteCollector, body: {id: "{id}"}, bareBody: true}
  sample: {name: example-collector}
- kind: SecretId
  group: approle.example.hfl.dev
  version: v1alpha1
  plural: secretids
  shape: crud
  envelope: data
  id: {param: id, from: secret_id_accessor}
  params: [{name: role_name, field: role_name}]
  create: {method: POST, path: "/auth/approle/role/{role_name}/secret-id"}
  read: {method: POST, path: "/auth/approle/role/{role_name}/secret-id-accessor/lookup", body: {secret_id_accessor: "{id}"}}
  delete: {method: POST, path: "/auth/approle/role/{role_name}/secret-id-accessor/destroy", body: {secret_id_accessor: "{id}"}}
  sample: {role_name: example, metadata: "{}"}
- kind: CustomSecretId
  group: approle.example.hfl.dev
  version: v1alpha1
  plural: customsecretids
  shape: crud
  envelope: data
  id: {param: id, from: secret_id_accessor}
  params: [{name: role_name, field: role_name}]
  create: {method: POST, path: "/auth/approle/role/{role_name}/custom-secret-id"}
  read: {method: POST, path: "/auth/approle/role/{role_name}/secret-id-accessor/lookup", body: {secret_id_accessor: "{id}"}}
  delete: {method: POST, path: "/auth/approle/role/{role_name}/secret-id-accessor/destroy", body: {secret_id_accessor: "{id}"}}
  sample: {role_name: example, secret_id: example-secret}
`)
	EveryKind(t, typesYAML, &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}, nil)
}

// A link found in an object keyed by its path parameters rather than in a
// list (Vault's secrets sync association: {"associated_destinations":
// {"gh/my-dest": {...}}}), removed by a POST naming it in its body.
func TestEveryKindKeyedFind(t *testing.T) {
	typesYAML := []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: Association
  group: sync.example.hfl.dev
  version: v1alpha1
  plural: associations
  shape: upsert
  envelope: data
  compareReturnedOnly: true
  params:
  - {name: type, field: type}
  - {name: name, field: name}
  - {name: mount, field: mount, inBody: true}
  - {name: secret_name, field: secret_name, inBody: true}
  create: {method: POST, path: "/sys/sync/destinations/{type}/{name}/associations/set"}
  read:
    method: GET
    path: /sys/sync/associations/destinations
    query: {mount: "{mount}", secret_name: "{secret_name}"}
    find: {list: data/associated_destinations, key: "{type}/{name}"}
  delete:
    method: POST
    path: "/sys/sync/destinations/{type}/{name}/associations/remove"
    body: {mount: "{mount}", secret_name: "{secret_name}"}
  sample: {type: gh, name: dest, mount: kv, secret_name: app}
`)
	EveryKind(t, typesYAML, &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}, nil)
}

// A catch-all kind at /{+path} (Vault's generic endpoint) answers only what
// no other kind's route does.
func TestEveryKindCatchAll(t *testing.T) {
	typesYAML := []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: Endpoint
  group: generic.example.hfl.dev
  version: v1alpha1
  plural: endpoints
  shape: upsert
  envelope: data
  compareReturnedOnly: true
  params: [{name: path, field: path}]
  read: {method: GET, path: "/{+path}"}
  update: {method: POST, path: "/{+path}"}
  delete: {method: DELETE, path: "/{+path}"}
  sample: {path: auth/userpass/users/example, token_ttl: 1h}
- kind: Policy
  group: sys.example.hfl.dev
  version: v1alpha1
  plural: policies
  shape: upsert
  envelope: data
  params: [{name: name, field: name}]
  read: {method: GET, path: "/sys/policy/{name}"}
  update: {method: POST, path: "/sys/policy/{name}"}
  delete: {method: DELETE, path: "/sys/policy/{name}"}
  sample: {name: example, policy: example-policy}
`)
	EveryKind(t, typesYAML, &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}, nil)
}

// A set whose list holds keys only, each element read on its own, in a
// holder the kind makes first and deletes, with the elements, last (a
// Vault database secrets mount and its connections).
func TestEveryKindSetHolder(t *testing.T) {
	typesYAML := []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: DatabaseMount
  group: database.example.hfl.dev
  version: v1alpha1
  plural: databasemounts
  shape: set
  compareReturnedOnly: true
  params: [{name: path, field: path}]
  set:
    field: connections
    key: name
    param: name
    list: data/keys
    readEach: {method: GET, path: "/{path}/config/{name}", envelope: data}
    holder:
      create: {method: POST, path: "/sys/mounts/{path}", body: {type: database}}
      read: {method: GET, path: "/sys/mounts/{path}"}
      delete: {method: DELETE, path: "/sys/mounts/{path}"}
      fields: [description]
  read: {method: GET, path: "/{path}/config", query: {list: "true"}}
  create: {method: POST, path: "/{path}/config/{name}"}
  update: {method: POST, path: "/{path}/config/{name}"}
  delete: {method: DELETE, path: "/{path}/config/{name}"}
  sample:
    path: database
    description: example
    connections:
    - {name: a, plugin_name: postgresql-database-plugin, username: vault}
    - {name: b, plugin_name: mysql-database-plugin, username: vault}
`)
	EveryKind(t, typesYAML, &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}, nil)
}

// A POST listing all of them that the read finds the object in (Grafana
// IRM's RolesService.GetRoles {}), and an upsert an RPC API names by a body
// field (Fleet Management's collector id); an update that carries what a
// read answered, sent outside the bodyEnvelope.
func TestEveryKindReadsByBodyFindAndUpsert(t *testing.T) {
	typesYAML := []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: Role
  group: incident.example.hfl.dev
  version: v1alpha1
  plural: roles
  shape: crud
  envelope: role
  bodyEnvelope: role
  id: {param: id, from: roleID}
  create: {method: POST, path: /RolesService.CreateRole}
  read: {method: POST, path: /RolesService.GetRoles, body: {}, find: {list: roles, field: roleID, param: id}}
  update: {method: POST, path: /RolesService.UpdateRole, body: {roleID: "{id}"}}
  delete: {method: POST, path: /RolesService.DeleteRole, body: {roleID: "{id}"}, bareBody: true}
  sample: {name: Commander}
- kind: Field
  group: incident.example.hfl.dev
  version: v1alpha1
  plural: fields
  shape: crud
  envelope: field
  bodyEnvelope: field
  id: {param: id, from: uuid}
  create: {method: POST, path: /FieldsService.AddField}
  read: {method: POST, path: /FieldsService.GetField, body: {fieldUUID: "{id}", includeArchived: true}}
  update: {method: POST, path: /FieldsService.UpdateField, body: {fieldUUID: "{id}"}, bareBody: true, carry: [version]}
  delete: {method: POST, path: /FieldsService.DeleteField, body: {fieldUUID: "{id}"}, bareBody: true}
  sample: {name: Team, slug: team}
- kind: Collector
  group: fleet.example.hfl.dev
  version: v1alpha1
  plural: collectors
  shape: upsert
  bodyEnvelope: collector
  params: [{name: id, field: id, inBody: true}]
  create: {method: POST, path: /collector.v1.CollectorService/CreateCollector}
  read: {method: POST, path: /collector.v1.CollectorService/GetCollector, body: {id: "{id}"}}
  update: {method: POST, path: /collector.v1.CollectorService/UpdateCollector}
  delete: {method: POST, path: /collector.v1.CollectorService/DeleteCollector, body: {id: "{id}"}, bareBody: true}
  sample: {id: example, name: example-collector}
`)
	EveryKind(t, typesYAML, &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}, nil)
}

// Settings kinds that act on another kind's resource (Okta's domain
// verification: a one-value state; its certificate: a write on its own
// path), and one that only revokes on delete (an app token).
func TestEveryKindRiders(t *testing.T) {
	typesYAML := []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: Domain
  group: domain.example.hfl.dev
  version: v1alpha1
  plural: domains
  shape: crud
  id: {param: domainId, from: id}
  create: {method: POST, path: /api/v1/domains}
  read: {method: GET, path: "/api/v1/domains/{domainId}"}
  update: {method: PUT, path: "/api/v1/domains/{domainId}"}
  delete: {method: DELETE, path: "/api/v1/domains/{domainId}"}
  serverFields: {validationStatus: VERIFIED}
  sample: {domain: example.com}
- kind: DomainCertificate
  group: domain.example.hfl.dev
  version: v1alpha1
  plural: domaincertificates
  shape: settings
  params: [{name: domainId, field: domainId}]
  writeOnly: [certificate]
  read: {method: GET, path: "/api/v1/domains/{domainId}"}
  update: {method: PUT, path: "/api/v1/domains/{domainId}/certificate"}
  sample: {domainId: example-domain-id, certificate: x}
- kind: DomainVerification
  group: domain.example.hfl.dev
  version: v1alpha1
  plural: domainverifications
  shape: settings
  params: [{name: domainId, field: domainId}]
  state: {field: validationStatus, calls: {VERIFIED: {method: POST, path: "/api/v1/domains/{domainId}/verify"}}}
  read: {method: GET, path: "/api/v1/domains/{domainId}"}
  sample: {domainId: example-domain-id, validationStatus: VERIFIED}
- kind: Token
  group: app.example.hfl.dev
  version: v1alpha1
  plural: tokens
  shape: settings
  params: [{name: appId, field: appId}, {name: tokenId, field: tokenId}]
  state: {field: status, calls: {REVOKED: {method: DELETE, path: "/api/v1/apps/{appId}/tokens/{tokenId}"}}, delete: REVOKED}
  read: {method: GET, path: "/api/v1/apps/{appId}/tokens/{tokenId}"}
  sample: {appId: app, tokenId: token}
- kind: App
  group: app.example.hfl.dev
  version: v1alpha1
  plural: apps
  shape: crud
  id: {param: appId, from: id}
  create: {method: POST, path: /api/v1/apps}
  read: {method: GET, path: "/api/v1/apps/{appId}"}
  update: {method: PUT, path: "/api/v1/apps/{appId}"}
  delete: {method: DELETE, path: "/api/v1/apps/{appId}"}
  sample: {label: example}
- kind: RedirectUri
  group: app.example.hfl.dev
  version: v1alpha1
  plural: redirecturis
  shape: upsert
  params: [{name: appId, field: appId}]
  element: {in: {uri: settings/oauthClient/redirect_uris}, whole: true}
  read: {method: GET, path: "/api/v1/apps/{appId}"}
  update: {method: PUT, path: "/api/v1/apps/{appId}"}
  delete: {method: PUT, path: "/api/v1/apps/{appId}"}
  sample: {appId: example-app, uri: "https://example.com/callback"}
`)
	EveryKind(t, typesYAML, &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}, nil)
}

// One call that both makes and replaces (Atlas's
// :grantMongoDBEmployeeAccess), read inside another resource (op envelope),
// revoked by another call; and a CRUD kind found in a list and deleted on
// a path its create does not name (Atlas's project API key, deleted as the
// organization's).
func TestEveryKindGrantAndOrgDelete(t *testing.T) {
	typesYAML := []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: Grant
  group: cluster.example.hfl.dev
  version: v1alpha1
  plural: grants
  shape: upsert
  params: [{name: groupId, field: groupId}, {name: clusterName, field: clusterName}]
  create: {method: POST, path: "/groups/{groupId}/clusters/{clusterName}:grant"}
  read: {method: GET, path: "/groups/{groupId}/clusters/{clusterName}", envelope: grant}
  update: {method: POST, path: "/groups/{groupId}/clusters/{clusterName}:grant"}
  delete: {method: POST, path: "/groups/{groupId}/clusters/{clusterName}:revoke"}
  sample: {groupId: g, clusterName: c, grantType: LOGS, expirationTime: "2027-01-01T00:00:00Z"}
- kind: OrgKey
  group: key.example.hfl.dev
  version: v1alpha1
  plural: orgkeys
  shape: crud
  id: {param: id, from: id}
  params: [{name: orgId, field: orgId}]
  create: {method: POST, path: "/orgs/{orgId}/apiKeys"}
  read: {method: GET, path: "/orgs/{orgId}/apiKeys/{id}"}
  update: {method: PATCH, path: "/orgs/{orgId}/apiKeys/{id}"}
  delete: {method: DELETE, path: "/orgs/{orgId}/apiKeys/{id}"}
  sample: {orgId: o, desc: org}
- kind: ProjectKey
  group: key.example.hfl.dev
  version: v1alpha1
  plural: projectkeys
  shape: crud
  id: {param: id, from: id}
  params: [{name: groupId, field: groupId}, {name: orgId, field: orgId}]
  writeOnly: [roles]
  create: {method: POST, path: "/groups/{groupId}/apiKeys"}
  read: {method: GET, path: "/groups/{groupId}/apiKeys", find: {list: results, field: id, param: id}}
  update: {method: PATCH, path: "/groups/{groupId}/apiKeys/{id}"}
  delete: {method: DELETE, path: "/orgs/{orgId}/apiKeys/{id}"}
  sample: {groupId: g, orgId: o, desc: key, roles: [GROUP_READ_ONLY]}
`)
	EveryKind(t, typesYAML, &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}, nil)
}

// Two kinds made by one call (PagerDuty's business and technical service
// dependencies, POST /service_dependencies/associate) and undone by its
// body to another, each read from a list a body field names.
func TestEveryKindSameCreate(t *testing.T) {
	typesYAML := []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: BusinessDependency
  group: service.example.hfl.dev
  version: v1alpha1
  plural: businessdependencies
  shape: crud
  id: {param: id, from: id}
  params: [{name: dependentId, field: dependent_service.id, inBody: true}]
  bodyEnvelope: relationships
  bodyArray: true
  create: {method: POST, path: /service_dependencies/associate, envelope: relationships/0}
  read: {method: GET, path: "/service_dependencies/business_services/{dependentId}", find: {list: relationships, field: id, param: id}}
  delete: {method: POST, path: /service_dependencies/disassociate, sendBody: true}
  sample: {supporting_service: {id: S, type: service}, dependent_service: {id: B, type: business_service}}
- kind: TechnicalDependency
  group: service.example.hfl.dev
  version: v1alpha1
  plural: technicaldependencies
  shape: crud
  id: {param: id, from: id}
  params: [{name: dependentId, field: dependent_service.id, inBody: true}]
  bodyEnvelope: relationships
  bodyArray: true
  create: {method: POST, path: /service_dependencies/associate, envelope: relationships/0}
  read: {method: GET, path: "/service_dependencies/technical_services/{dependentId}", find: {list: relationships, field: id, param: id}}
  delete: {method: POST, path: /service_dependencies/disassociate, sendBody: true}
  sample: {supporting_service: {id: S, type: service}, dependent_service: {id: T, type: service}}
`)
	EveryKind(t, typesYAML, &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}, nil)
}
