// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/rest"
)

func TestEveryKindArrayBodies(t *testing.T) {
	typesYAML := []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: AccessListApiKey
  group: access.example.hfl.dev
  version: v1alpha1
  plural: accesslistapikeys
  shape: crud
  bodyArray: true
  create: {method: POST, path: "/apiKeys/{apiUserId}/accessList"}
  read: {method: GET, path: "/apiKeys/{apiUserId}/accessList/{ipAddress}"}
  delete: {method: DELETE, path: "/apiKeys/{apiUserId}/accessList/{ipAddress}"}
  params:
  - {name: apiUserId, field: apiuserid}
  id: {param: ipAddress, from: ipAddress}
  sample:
    apiuserid: k1
    ipAddress: 192.0.2.1
- kind: CloudConnectorRules
  group: cloud.example.hfl.dev
  version: v1alpha1
  plural: cloudconnectorruleses
  shape: settings
  envelope: result
  bodyField: rules
  read: {method: GET, path: "/zones/{zone_id}/cloud_connector/rules"}
  update: {method: PUT, path: "/zones/{zone_id}/cloud_connector/rules"}
  params:
  - {name: zone_id, field: zoneID}
  sample:
    zoneID: z1
    rules:
    - description: example
      enabled: true
`)
	EveryKind(t, typesYAML, &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}, nil)
}
