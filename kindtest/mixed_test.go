// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// Kinds whose calls no one REST item describes: made over GraphQL and read
// over REST (an organization; a board read by the number inside its global
// ID), a ref whose name keeps its slashes, an identity written by its
// user's update and deleted on a path of its own, a scope there or not,
// and whole sets of labels and of repository IDs.
var mixedTypes = []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: Organization
  group: things.example.hfl.dev
  version: v1alpha1
  plural: organizations
  shape: crud
  create:
    method: POST
    path: /graphql
    graphql:
      operation: CreateEnterpriseOrganization
      query: "mutation CreateEnterpriseOrganization($input: CreateEnterpriseOrganizationInput!) { createEnterpriseOrganization(input: $input) { organization { login } } }"
      variables: {input: {enterpriseId: "{enterprise_id}", login: "{login}", profileName: "{login}", billingEmail: "{billing_email}"}}
      result: createEnterpriseOrganization/organization
  read: {method: GET, path: "/orgs/{id}"}
  update: {method: PATCH, path: "/orgs/{id}"}
  delete: {method: DELETE, path: "/orgs/{id}"}
  id: {param: id, from: login}
  createOnly: [enterprise_id, login]
  writeOnly: [enterprise_id]
  sample: {enterprise_id: E_1, login: example-org, billing_email: billing@example.com}
- kind: EpicBoard
  group: things.example.hfl.dev
  version: v1alpha1
  plural: epicboards
  shape: crud
  create:
    method: POST
    path: /graphql
    graphql:
      operation: EpicBoardCreate
      query: "mutation EpicBoardCreate($groupPath: ID!, $name: String) { epicBoardCreate(input: {groupPath: $groupPath, name: $name}) { epicBoard { id name } errors } }"
      variables: {groupPath: "{groupPath}", name: "{name}"}
      result: epicBoardCreate/epicBoard
  read: {method: GET, path: "/groups/{groupPath}/epic_boards/{id}"}
  update:
    method: POST
    path: /graphql
    graphql:
      operation: EpicBoardUpdate
      query: "mutation EpicBoardUpdate($id: BoardsEpicBoardID!, $name: String) { epicBoardUpdate(input: {id: $id, name: $name}) { epicBoard { id name } errors } }"
      variables: {id: "gid://gitlab/Boards::EpicBoard/{id}", name: "{name}"}
      result: epicBoardUpdate/epicBoard
  delete:
    method: POST
    path: /graphql
    graphql:
      operation: DestroyEpicBoard
      query: "mutation DestroyEpicBoard($id: BoardsEpicBoardID!) { destroyEpicBoard(input: {id: $id}) { errors } }"
      variables: {id: "gid://gitlab/Boards::EpicBoard/{id}"}
  params:
  - {name: groupPath, field: groupPath}
  id: {param: id, from: id, match: "gid://gitlab/Boards::EpicBoard/{id}"}
  sample: {groupPath: example-group, name: example-name}
- kind: Branch
  group: things.example.hfl.dev
  version: v1alpha1
  plural: branches
  shape: upsert
  create: {method: POST, path: "/repos/{owner}/{repo}/git/refs", body: {ref: "refs/heads/{branch}"}}
  read: {method: GET, path: "/repos/{owner}/{repo}/git/ref/heads/{+branch}"}
  update: {method: PATCH, path: "/repos/{owner}/{repo}/git/refs/heads/{+branch}"}
  delete: {method: DELETE, path: "/repos/{owner}/{repo}/git/refs/heads/{+branch}"}
  params:
  - {name: owner, field: owner}
  - {name: repo, field: repo}
  - {name: branch, field: branch}
  writeOnly: [sha]
  sample: {owner: o, repo: r, branch: feature/x, sha: aa218f56b14c9653891f9e74264a383fa43fefbd}
- kind: UserIdentity
  group: things.example.hfl.dev
  version: v1alpha1
  plural: useridentities
  shape: upsert
  read: {method: GET, path: "/users/{id}", find: {list: identities, field: provider, param: provider}}
  update: {method: PUT, path: "/users/{id}"}
  delete: {method: DELETE, path: "/users/{id}/identities/{provider}"}
  params:
  - {name: id, field: user_id}
  - {name: provider, field: provider, inBody: true}
  sample: {user_id: 7, provider: ldapmain, extern_uid: example-uid}
- kind: InstanceScope
  group: things.example.hfl.dev
  version: v1alpha1
  plural: instancescopes
  shape: upsert
  create: {method: POST, path: "/runner_controllers/{id}/scopes/instance"}
  read: {method: GET, path: "/runner_controllers/{id}/scopes", find: {list: instance_level_scopings}}
  delete: {method: DELETE, path: "/runner_controllers/{id}/scopes/instance"}
  params:
  - {name: id, field: runner_controller_id}
  sample: {runner_controller_id: 7}
- kind: RunnerScope
  group: things.example.hfl.dev
  version: v1alpha1
  plural: runnerscopes
  shape: upsert
  create: {method: POST, path: "/runner_controllers/{id}/scopes/runners/{runner_id}"}
  read: {method: GET, path: "/runner_controllers/{id}/scopes", find: {list: runner_level_scopings, field: runner_id, param: runner_id}}
  delete: {method: DELETE, path: "/runner_controllers/{id}/scopes/runners/{runner_id}"}
  params:
  - {name: id, field: runner_controller_id}
  - {name: runner_id, field: runner_id}
  sample: {runner_controller_id: 7, runner_id: 8}
- kind: LabelSet
  group: things.example.hfl.dev
  version: v1alpha1
  plural: labelsets
  shape: set
  set: {field: labels, key: name, param: name}
  read: {method: GET, path: "/repos/{owner}/{repo}/labels", query: {per_page: "100"}}
  create: {method: POST, path: "/repos/{owner}/{repo}/labels"}
  update: {method: PATCH, path: "/repos/{owner}/{repo}/labels/{name}"}
  delete: {method: DELETE, path: "/repos/{owner}/{repo}/labels/{name}"}
  params:
  - {name: owner, field: owner}
  - {name: repo, field: repo}
  sample: {owner: o, repo: r, labels: [{name: bug, color: d73a4a}, {name: docs, color: 0075ca}]}
- kind: RepositorySet
  group: things.example.hfl.dev
  version: v1alpha1
  plural: repositorysets
  shape: set
  set: {field: repository_ids, key: id, param: repository_id, list: repositories}
  read: {method: GET, path: "/user/installations/{installation_id}/repositories"}
  create: {method: PUT, path: "/user/installations/{installation_id}/repositories/{repository_id}"}
  delete: {method: DELETE, path: "/user/installations/{installation_id}/repositories/{repository_id}", ok: [422]}
  params:
  - {name: installation_id, field: installation_id}
  sample: {installation_id: 7, repository_ids: [1296269, 1296270]}
# Named by two fields in the delete's query, beside a second list added to
# in one call and removed only all together (Atlas's global cluster).
- kind: NamespaceSet
  group: things.example.hfl.dev
  version: v1alpha1
  plural: namespacesets
  shape: set
  set:
    field: namespaces
    keys: [db, collection]
    list: namespaces
    batch:
      field: zone_mappings
      key: location
      at: zone_map
      value: zone
      add: {method: POST, path: "/clusters/{cluster}/sharding/zones"}
      clear: {method: DELETE, path: "/clusters/{cluster}/sharding/zones"}
  read: {method: GET, path: "/clusters/{cluster}/sharding"}
  create: {method: POST, path: "/clusters/{cluster}/sharding/namespaces"}
  delete: {method: DELETE, path: "/clusters/{cluster}/sharding/namespaces", query: {db: "{db}", collection: "{collection}"}}
  params:
  - {name: cluster, field: cluster}
  sample:
    cluster: c1
    namespaces: [{db: app, collection: users, shard_key: user_id}, {db: app, collection: orders, shard_key: order_id}]
    zone_mappings: [{location: US-VA, zone: Zone 1}, {location: DE, zone: Zone 2}]
# Never read (Okta's resource owners, which only a filter by the parent's
# ORN lists): written when the spec changes, and only then.
- kind: ResourceOwners
  group: things.example.hfl.dev
  version: v1alpha1
  plural: resourceowners
  shape: settings
  unread: the API lists owners only by the resource's parent, which the spec does not name
  update: {method: POST, path: "/governance/api/v1/resource-owners"}
  sample: {principalOrns: ["orn:okta:directory:o1:users:u1"], resourceOrns: ["orn:okta:idp:o1:apps:app:a1"]}
`)

func mixedVendor() *rest.Vendor {
	return &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}
}

func TestEveryKindMixed(t *testing.T) {
	EveryKind(t, mixedTypes, mixedVendor(), nil)
}

// A rehearsal records each kind's calls: the GraphQL create and the REST
// read back of a mixed kind, a branch's slashes kept in its paths, a set's
// one call per element.
func TestRehearseMixed(t *testing.T) {
	t.Setenv(EnvLiveRecords, filepath.Join(t.TempDir(), "records.json"))
	dir := filepath.Join(t.TempDir(), "cassettes")
	t.Setenv(EnvLiveCassettes, dir)
	t.Setenv(EnvFakeRecord, "1")
	t.Run("record", func(t *testing.T) { Rehearse(t, mixedTypes, mixedVendor(), nil, nil) })
	if t.Failed() {
		t.FailNow()
	}
	for file, wants := range map[string][]string{
		"Organization.things.example.hfl.dev.json":   {`\"operationName\":\"CreateEnterpriseOrganization\"`, `"path": "/orgs/example-org"`, `"method": "PATCH"`},
		"EpicBoard.things.example.hfl.dev.json":      {`EpicBoardCreate`, `"path": "/groups/example-group/epic_boards/`, `gid://gitlab/Boards::EpicBoard/`},
		"Branch.things.example.hfl.dev.json":         {`"path": "/repos/o/r/git/ref/heads/feature/x"`, `refs/heads/feature/x`, `"path": "/repos/o/r/git/refs/heads/feature/x"`},
		"LabelSet.things.example.hfl.dev.json":       {`"path": "/repos/o/r/labels?per_page=100"`, `"path": "/repos/o/r/labels/docs"`},
		"RepositorySet.things.example.hfl.dev.json":  {`"path": "/user/installations/7/repositories/1296270"`},
		"ResourceOwners.things.example.hfl.dev.json": {`"path": "/governance/api/v1/resource-owners"`, `orn:okta:idp:o1:apps:app:a1`},
		"NamespaceSet.things.example.hfl.dev.json":   {`"path": "/clusters/c1/sharding/namespaces?collection=orders\u0026db=app"`, `"path": "/clusters/c1/sharding/zones"`, `{\"zone_mappings\":[{\"location\":\"US-VA\",\"zone\":\"Zone 1\"}`},
	} {
		b := string(mustRead(t, filepath.Join(dir, file)))
		for _, want := range wants {
			if !strings.Contains(b, want) {
				t.Errorf("%s lacks %s:\n%s", file, want, b)
			}
		}
	}
	t.Setenv(EnvFakeRecord, "")
	t.Run("replay", func(t *testing.T) { Rehearse(t, mixedTypes, mixedVendor(), nil, nil) })
}
