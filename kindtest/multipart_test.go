// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/rest"
)

func TestEveryKindMultipart(t *testing.T) {
	typesYAML := []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: WorkersScript
  group: workers.example.hfl.dev
  version: v1alpha1
  plural: workersscripts
  shape: upsert
  multipart:
    files: [files]
  read: {method: GET, path: "/accounts/{account_id}/workers/scripts/{script_name}"}
  update: {method: PUT, path: "/accounts/{account_id}/workers/scripts/{script_name}"}
  delete: {method: DELETE, path: "/accounts/{account_id}/workers/scripts/{script_name}"}
  params:
  - {name: account_id, field: accountID}
  - {name: script_name, field: scriptName}
  sample:
    accountID: a1
    scriptName: s1
    metadata:
      main_module: index.js
    files:
    - name: index.js
      contentType: application/javascript+module
      content: "export default {};"
- kind: SecureFile
  group: files.example.hfl.dev
  version: v1alpha1
  plural: securefiles
  shape: crud
  multipart:
    files: [file]
  create: {method: POST, path: "/projects/{project_id}/secure_files"}
  read: {method: GET, path: "/projects/{project_id}/secure_files/{id}"}
  delete: {method: DELETE, path: "/projects/{project_id}/secure_files/{id}"}
  params:
  - {name: project_id, field: projectID}
  id: {param: id, from: id}
  sample:
    projectID: p1
    name: cert.pem
    file:
      name: cert.pem
      contentType: application/x-pem-file
      content: "CERT"
- kind: RepositoryFile
  group: repository.example.hfl.dev
  version: v1alpha1
  plural: repositoryfiles
  shape: upsert
  multipart:
    files: [file]
  compareReturnedOnly: true
  writeOnly: [file, branch, commit_message]
  create: {method: POST, path: "/projects/{id}/repository/files/{file_path}"}
  read: {method: GET, path: "/projects/{id}/repository/files/{file_path}", query: {ref: "{branch}"}}
  update: {method: PUT, path: "/projects/{id}/repository/files/{file_path}"}
  delete: {method: DELETE, path: "/projects/{id}/repository/files/{file_path}", query: {branch: "{branch}", commit_message: "{commit_message}"}}
  params:
  - {name: id, field: projectID}
  - {name: file_path, field: filePath}
  sample:
    projectID: p1
    filePath: README.md
    branch: main
    commit_message: add readme
    file:
      name: README.md
      contentType: text/plain
      content: "hello"
`)
	EveryKind(t, typesYAML, &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}, nil)
}
