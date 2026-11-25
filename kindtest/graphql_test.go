// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// GraphQL kinds beside a REST one, through one Connection: a CRUD kind
// named by a global ID, one read from a list, an upsert and settings.
var graphqlTypes = []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: Label
  group: things.example.hfl.dev
  version: v1alpha1
  plural: labels
  shape: crud
  create: {method: POST, path: "/projects/{id}/labels"}
  read: {method: GET, path: "/projects/{id}/labels/{label_id}"}
  update: {method: PUT, path: "/projects/{id}/labels/{label_id}"}
  delete: {method: DELETE, path: "/projects/{id}/labels/{label_id}"}
  params:
  - {name: id, field: project}
  id: {param: label_id, from: id}
  sample: {project: "7", name: example-name}
- kind: GroupSavedReply
  group: group.example.hfl.dev
  version: v1alpha1
  plural: groupsavedreplies
  shape: crud
  create:
    method: POST
    path: /api/graphql
    graphql:
      operation: GroupSavedReplyCreate
      query: "mutation GroupSavedReplyCreate($groupId: GroupID!, $name: String!, $content: String!) { groupSavedReplyCreate(input: {groupId: $groupId, name: $name, content: $content}) { savedReply { id name content } errors } }"
      variables: {groupId: "gid://gitlab/Group/{groupId}", name: "{name}", content: "{content}"}
      result: groupSavedReplyCreate/savedReply
      errors: groupSavedReplyCreate/errors
  read:
    method: POST
    path: /api/graphql
    graphql:
      operation: GroupSavedReply
      query: "query GroupSavedReply($fullPath: ID!, $id: GroupsSavedReplyID!) { group(fullPath: $fullPath) { savedReply(id: $id) { id name content } } }"
      variables: {fullPath: "{groupPath}", id: "{id}"}
      result: group/savedReply
  update:
    method: POST
    path: /api/graphql
    graphql:
      operation: GroupSavedReplyUpdate
      query: "mutation GroupSavedReplyUpdate($id: GroupsSavedReplyID!, $name: String!, $content: String!) { groupSavedReplyUpdate(input: {id: $id, name: $name, content: $content}) { savedReply { id name content } errors } }"
      variables: {id: "{id}", name: "{name}", content: "{content}"}
      result: groupSavedReplyUpdate/savedReply
      errors: groupSavedReplyUpdate/errors
  delete:
    method: POST
    path: /api/graphql
    graphql:
      operation: GroupSavedReplyDestroy
      query: "mutation GroupSavedReplyDestroy($id: GroupsSavedReplyID!) { groupSavedReplyDestroy(input: {id: $id}) { errors } }"
      variables: {id: "{id}"}
      errors: groupSavedReplyDestroy/errors
  params:
  - {name: groupId, field: groupId}
  - {name: groupPath, field: groupPath}
  id: {param: id, from: id}
  sample: {groupId: "7", groupPath: example-group, name: example-name, content: example-content}
- kind: TargetBranchRule
  group: project.example.hfl.dev
  version: v1alpha1
  plural: targetbranchrules
  shape: crud
  create:
    method: POST
    path: /api/graphql
    graphql:
      operation: TargetBranchRuleCreate
      query: "mutation TargetBranchRuleCreate($input: ProjectTargetBranchRuleCreateInput!) { projectTargetBranchRuleCreate(input: $input) { errors targetBranchRule { id name } } }"
      variables: {input: {projectId: "gid://gitlab/Project/{projectId}", name: "{name}", targetBranch: "{targetBranch}"}}
      result: projectTargetBranchRuleCreate/targetBranchRule
      errors: projectTargetBranchRuleCreate/errors
  read:
    method: POST
    path: /api/graphql
    find: {field: id, param: id}
    graphql:
      operation: TargetBranchRules
      query: "query TargetBranchRules($fullPath: ID!) { project(fullPath: $fullPath) { targetBranchRules { nodes { id name targetBranch } } } }"
      variables: {fullPath: "{projectPath}"}
      result: project/targetBranchRules/nodes
  delete:
    method: POST
    path: /api/graphql
    graphql:
      operation: TargetBranchRuleDestroy
      query: "mutation TargetBranchRuleDestroy($id: ProjectsTargetBranchRuleID!) { projectTargetBranchRuleDestroy(input: {id: $id}) { errors } }"
      variables: {id: "{id}"}
      errors: projectTargetBranchRuleDestroy/errors
  params:
  - {name: projectId, field: projectId}
  - {name: projectPath, field: projectPath}
  id: {param: id, from: id}
  sample: {projectId: "7", projectPath: example/project, name: "feature/*", targetBranch: develop}
- kind: ProjectSecurityPolicyAttachment
  group: project.example.hfl.dev
  version: v1alpha1
  plural: projectsecuritypolicyattachments
  shape: upsert
  compare: {securityPolicyProjectId: slugObject}
  read:
    method: POST
    path: /api/graphql
    graphql:
      operation: ProjectSecurityPolicyProject
      query: "query ProjectSecurityPolicyProject($fullPath: ID!) { project(fullPath: $fullPath) { securityPolicyProjectId: securityPolicyProject { id } } }"
      variables: {fullPath: "{fullPath}"}
      result: project
  update:
    method: POST
    path: /api/graphql
    graphql:
      operation: SecurityPolicyProjectAssign
      query: "mutation SecurityPolicyProjectAssign($fullPath: String, $securityPolicyProjectId: ProjectID!) { securityPolicyProjectAssign(input: {fullPath: $fullPath, securityPolicyProjectId: $securityPolicyProjectId}) { errors } }"
      variables: {fullPath: "{fullPath}", securityPolicyProjectId: "{securityPolicyProjectId}"}
      errors: securityPolicyProjectAssign/errors
  delete:
    method: POST
    path: /api/graphql
    graphql:
      operation: SecurityPolicyProjectUnassign
      query: "mutation SecurityPolicyProjectUnassign($fullPath: String) { securityPolicyProjectUnassign(input: {fullPath: $fullPath}) { errors } }"
      variables: {fullPath: "{fullPath}"}
      errors: securityPolicyProjectUnassign/errors
  params:
  - {name: fullPath, field: fullPath}
  sample: {fullPath: example/project, securityPolicyProjectId: "gid://gitlab/Project/9"}
- kind: ProjectCicdCatalog
  group: project.example.hfl.dev
  version: v1alpha1
  plural: projectcicdcatalogs
  shape: upsert
  read:
    method: POST
    path: /api/graphql
    graphql:
      operation: ProjectCatalogResource
      query: "query ProjectCatalogResource($fullPath: ID!) { project(fullPath: $fullPath) { id isCatalogResource } }"
      variables: {fullPath: "{fullPath}"}
      result: project
      exists: isCatalogResource
  update:
    method: POST
    path: /api/graphql
    graphql:
      operation: CatalogResourcesCreate
      query: "mutation CatalogResourcesCreate($projectPath: ID!) { catalogResourcesCreate(input: {projectPath: $projectPath}) { errors } }"
      variables: {projectPath: "{fullPath}"}
      errors: catalogResourcesCreate/errors
  delete:
    method: POST
    path: /api/graphql
    graphql:
      operation: CatalogResourcesDestroy
      query: "mutation CatalogResourcesDestroy($projectPath: ID!) { catalogResourcesDestroy(input: {projectPath: $projectPath}) { errors } }"
      variables: {projectPath: "{fullPath}"}
      errors: catalogResourcesDestroy/errors
  params:
  - {name: fullPath, field: fullPath}
  sample: {fullPath: example/project}
- kind: GroupDependencyProxyImageTtlPolicy
  group: group.example.hfl.dev
  version: v1alpha1
  plural: groupdependencyproxyimagettlpolicies
  shape: settings
  read:
    method: POST
    path: /api/graphql
    graphql:
      operation: GroupDependencyProxyImageTtlPolicy
      query: "query GroupDependencyProxyImageTtlPolicy($fullPath: ID!) { group(fullPath: $fullPath) { dependencyProxyImageTtlPolicy { enabled ttl } } }"
      variables: {fullPath: "{groupPath}"}
      result: group/dependencyProxyImageTtlPolicy
  update:
    method: POST
    path: /api/graphql
    graphql:
      operation: UpdateDependencyProxyImageTtlGroupPolicy
      query: "mutation UpdateDependencyProxyImageTtlGroupPolicy($input: UpdateDependencyProxyImageTtlGroupPolicyInput!) { updateDependencyProxyImageTtlGroupPolicy(input: $input) { errors dependencyProxyImageTtlPolicy { enabled ttl } } }"
      variables: {input: {groupPath: "{groupPath}", enabled: "{enabled}", ttl: "{ttl}"}}
      result: updateDependencyProxyImageTtlGroupPolicy/dependencyProxyImageTtlPolicy
      errors: updateDependencyProxyImageTtlGroupPolicy/errors
  params:
  - {name: groupPath, field: groupPath}
  sample: {groupPath: example-group, enabled: true, ttl: 30}
`)

func graphqlVendor() *rest.Vendor {
	return &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}, RESTPath: "/api/v4"}
}

func TestEveryKindGraphQL(t *testing.T) {
	EveryKind(t, graphqlTypes, graphqlVendor(), nil)
}

// A rehearsal of GraphQL kinds records each POSTed document and its
// variables, at the endpoint's path, and replays them.
func TestRehearseGraphQL(t *testing.T) {
	t.Setenv(EnvLiveRecords, filepath.Join(t.TempDir(), "records.json"))
	dir := filepath.Join(t.TempDir(), "cassettes")
	t.Setenv(EnvLiveCassettes, dir)
	t.Setenv(EnvFakeRecord, "1")
	t.Run("record", func(t *testing.T) { Rehearse(t, graphqlTypes, graphqlVendor(), nil, nil) })
	if t.Failed() {
		t.FailNow()
	}
	b := string(mustRead(t, filepath.Join(dir, "GroupSavedReply.group.example.hfl.dev.json")))
	for _, want := range []string{`"path": "/api/graphql"`, `GroupSavedReplyCreate`, `gid://gitlab/Group/7`, `\"operationName\":\"GroupSavedReplyDestroy\"`} {
		if !strings.Contains(b, want) {
			t.Errorf("cassette lacks %s:\n%s", want, b)
		}
	}
	t.Setenv(EnvFakeRecord, "")
	t.Run("replay", func(t *testing.T) { Rehearse(t, graphqlTypes, graphqlVendor(), nil, nil) })
}
