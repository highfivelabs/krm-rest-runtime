// SPDX-License-Identifier: Apache-2.0

package kindtest

import "testing"

// One element of a list another resource holds: an ID in a Vault OIDC
// key's allowed_client_ids (a whole write, the read in "data"); a user,
// team or service account in a Grafana role's assignments (three lists,
// all sent back); a GitHub repository's value of one custom property (a
// bare array read, a patch of the one element, removed with value null).
// A kind at the same path (the key itself) is unaffected.
func TestEveryKindElement(t *testing.T) {
	typesYAML := []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: IdentityOidcKey
  group: identity.example.hfl.dev
  version: v1alpha1
  plural: identityoidckeys
  shape: upsert
  read: {method: GET, path: "/identity/oidc/key/{name}"}
  update: {method: POST, path: "/identity/oidc/key/{name}"}
  delete: {method: DELETE, path: "/identity/oidc/key/{name}"}
  envelope: data
  params: [{name: name, field: name}]
  sample: {name: example-name, algorithm: RS256}
- kind: IdentityOidcKeyAllowedClientId
  group: identity.example.hfl.dev
  version: v1alpha1
  plural: identityoidckeyallowedclientids
  shape: upsert
  read: {method: GET, path: "/identity/oidc/key/{name}"}
  update: {method: POST, path: "/identity/oidc/key/{name}"}
  delete: {method: POST, path: "/identity/oidc/key/{name}"}
  envelope: data
  element: {in: {allowed_client_id: allowed_client_ids}}
  params: [{name: name, field: name}]
  sample: {name: example-name-holder, allowed_client_id: client-1}
- kind: RoleAssignmentItem
  group: role.example.hfl.dev
  version: v1alpha1
  plural: roleassignmentitems
  shape: upsert
  read: {method: GET, path: "/access-control/roles/{roleUID}/assignments"}
  update: {method: PUT, path: "/access-control/roles/{roleUID}/assignments"}
  delete: {method: PUT, path: "/access-control/roles/{roleUID}/assignments"}
  element: {in: {userId: users, teamId: teams, serviceAccountId: serviceAccounts}}
  params: [{name: roleUID, field: roleUID}]
  sample: {roleUID: example-roleUID-holder, teamId: 7}
- kind: RepositoryCustomProperty
  group: repository.example.hfl.dev
  version: v1alpha1
  plural: repositorycustomproperties
  shape: upsert
  read: {method: GET, path: "/repos/{owner}/{repo}/properties/values"}
  update: {method: PATCH, path: "/repos/{owner}/{repo}/properties/values"}
  delete: {method: PATCH, path: "/repos/{owner}/{repo}/properties/values"}
  element: {key: property_name, patch: properties, unset: {value: null}}
  params: [{name: owner, field: owner}, {name: repo, field: repo}]
  sample: {owner: example-owner-holder, repo: example-repo-holder, property_name: team, value: platform}
`)
	EveryKind(t, typesYAML, stateVendor, nil)
	t.Run("rehearse", func(t *testing.T) {
		t.Setenv(EnvFakeRecord, "1")
		t.Setenv(EnvLiveCassettes, t.TempDir())
		Rehearse(t, typesYAML, stateVendor, nil, nil)
	})
}

// A field set by a call per value: a feature enabled and disabled (nothing
// else to write; deleting the object disables it), and a default the
// vendor makes, adopted, updated, activated, and deactivated rather than
// deleted (Okta's authenticators).
func TestEveryKindState(t *testing.T) {
	typesYAML := []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: Feature
  group: feature.example.hfl.dev
  version: v1alpha1
  plural: features
  shape: settings
  read: {method: GET, path: "/features/{featureId}"}
  state:
    field: status
    calls:
      ENABLED: {method: POST, path: "/features/{featureId}/enable"}
      DISABLED: {method: POST, path: "/features/{featureId}/disable"}
    delete: DISABLED
  params: [{name: featureId, field: featureId}]
  sample: {featureId: f1, status: ENABLED}
- kind: RoleSubscription
  group: role.example.hfl.dev
  version: v1alpha1
  plural: rolesubscriptions
  shape: settings
  read: {method: GET, path: "/roles/{roleRef}/subscriptions/{notificationType}"}
  state:
    field: status
    calls:
      subscribed: {method: POST, path: "/roles/{roleRef}/subscriptions/{notificationType}/subscribe"}
      unsubscribed: {method: POST, path: "/roles/{roleRef}/subscriptions/{notificationType}/unsubscribe"}
    delete: unsubscribed
  params: [{name: roleRef, field: roleRef}, {name: notificationType, field: notificationType}]
  sample: {roleRef: SUPER_ADMIN, notificationType: AD_AGENT, status: subscribed}
- kind: Authenticator
  group: authenticator.example.hfl.dev
  version: v1alpha1
  plural: authenticators
  shape: crud
  read: {method: GET, path: "/authenticators/{authenticatorId}"}
  update: {method: PUT, path: "/authenticators/{authenticatorId}"}
  adopt: {method: GET, path: /authenticators, match: {key: "{key}"}}
  state:
    field: status
    calls:
      ACTIVE: {method: POST, path: "/authenticators/{authenticatorId}/lifecycle/activate"}
      INACTIVE: {method: POST, path: "/authenticators/{authenticatorId}/lifecycle/deactivate"}
    delete: INACTIVE
  id: {param: authenticatorId, from: id}
  sample: {key: security_question, name: Security Question, status: ACTIVE}
`)
	EveryKind(t, typesYAML, stateVendor, nil)
	t.Run("rehearse", func(t *testing.T) {
		t.Setenv(EnvFakeRecord, "1")
		t.Setenv(EnvLiveCassettes, t.TempDir())
		Rehearse(t, typesYAML, stateVendor, nil, nil)
	})
}

// A schedule read and written at its load test's path and deleted by the
// ID its read answers (Grafana k6: /schedules/{current.id}); a trusted
// server made by naming it in its collection's body and read from the list
// (Okta: {"trusted": ["{associatedServerId}"]}); a CA made on one of two
// create paths, with no update (Vault's generated KMIP CA).
func TestEveryKindCurrentDeleteAndCreateBody(t *testing.T) {
	typesYAML := []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: K6Schedule
  group: k6.example.hfl.dev
  version: v1alpha1
  plural: k6schedules
  shape: upsert
  read: {method: GET, path: "/load_tests/{id}/schedule"}
  update: {method: POST, path: "/load_tests/{id}/schedule"}
  delete: {method: DELETE, path: "/schedules/{current.id}"}
  params: [{name: id, field: loadTestID}]
  sample: {loadTestID: "7", starts: "2030-01-01T00:00:00Z"}
- kind: TrustedServer
  group: trusted.example.hfl.dev
  version: v1alpha1
  plural: trustedservers
  shape: upsert
  create: {method: POST, path: "/authorizationServers/{authServerId}/associatedServers", body: {trusted: ["{associatedServerId}"]}}
  read: {method: GET, path: "/authorizationServers/{authServerId}/associatedServers", query: {trusted: "true"}, find: {field: id, param: associatedServerId}}
  delete: {method: DELETE, path: "/authorizationServers/{authServerId}/associatedServers/{associatedServerId}"}
  params: [{name: authServerId, field: authServerId}, {name: associatedServerId, field: associatedServerId}]
  sample: {authServerId: a1, associatedServerId: s2}
- kind: KmipCaGenerated
  group: kmip.example.hfl.dev
  version: v1alpha1
  plural: kmipcagenerateds
  shape: upsert
  create: {method: POST, path: "/{mount}/ca/{name}/generate"}
  read: {method: GET, path: "/{mount}/ca/{name}"}
  delete: {method: DELETE, path: "/{mount}/ca/{name}"}
  params: [{name: mount, field: mount}, {name: name, field: name}]
  sample: {mount: kmip, name: ca1, key_type: rsa, key_bits: 2048}
`)
	EveryKind(t, typesYAML, stateVendor, nil)
	t.Run("rehearse", func(t *testing.T) {
		t.Setenv(EnvFakeRecord, "1")
		t.Setenv(EnvLiveCassettes, t.TempDir())
		Rehearse(t, typesYAML, stateVendor, nil, nil)
	})
}
