// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// TestEveryKindListsAndEncodings runs, on the fake, the kinds whose calls
// differ from one another: an order of other objects (read by position from
// a list another kind's items are found in), a bulk create answered as a
// list, a read answered as raw bytes, a multipart create with a JSON update,
// an item named by a nested body field, written on another path or made on a
// collection that does not name it.
func TestEveryKindListsAndEncodings(t *testing.T) {
	typesYAML := []byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: Group
  group: scanner.example.hfl.dev
  version: v1alpha1
  plural: groups
  shape: crud
  envelope: data
  bodyEnvelope: data
  create: {method: POST, path: /scanner/config/groups}
  read: {method: GET, path: /scanner/config, find: {list: included, field: id, param: group_id}}
  update: {method: PATCH, path: "/scanner/config/groups/{group_id}"}
  delete: {method: DELETE, path: "/scanner/config/groups/{group_id}"}
  id: {param: group_id, from: id}
  sample: {name: g}
- kind: GroupOrder
  group: scanner.example.hfl.dev
  version: v1alpha1
  plural: grouporders
  shape: settings
  read: {method: GET, path: /scanner/config}
  update:
    method: PATCH
    path: /scanner/config
    body: {data: {type: configuration, id: "{current.data.id}"}}
    outer: {meta: {}}
  order: {field: group_ids, list: data/relationships/groups/data, at: data/relationships/groups/data, item: {type: group}}
  serverFields: {data: {id: config-1}}
  sample: {group_ids: [g1, g2]}
- kind: RuleOrder
  group: rule.example.hfl.dev
  version: v1alpha1
  plural: ruleorders
  shape: settings
  read: {method: GET, path: "/apps/{app_id}/rules"}
  update: {method: POST, path: "/apps/{app_id}/rules/order"}
  params: [{name: app_id, field: appID}]
  order: {field: rule_ids, list: data, sortBy: attributes.position, at: data/attributes/rule_ids, partial: true}
  sample: {appID: a1, rule_ids: [r1, r2, r3]}
- kind: Secret
  group: secrets.example.hfl.dev
  version: v1alpha1
  plural: secrets
  shape: crud
  envelope: result
  create: {method: POST, path: "/stores/{store_id}/secrets", bodyArray: true, envelope: result/0}
  read: {method: GET, path: "/stores/{store_id}/secrets/{secret_id}"}
  update: {method: PATCH, path: "/stores/{store_id}/secrets/{secret_id}"}
  delete: {method: DELETE, path: "/stores/{store_id}/secrets/{secret_id}"}
  params: [{name: store_id, field: storeID}]
  id: {param: secret_id, from: id}
  sample: {storeID: s1, name: KEY}
- kind: Asset
  group: assets.example.hfl.dev
  version: v1alpha1
  plural: assets
  shape: crud
  envelope: result
  create: {method: POST, path: "/requests/{request_id}/asset/new"}
  read: {method: GET, path: "/requests/{request_id}/asset/{asset_id}", envelope: result/0}
  update: {method: PUT, path: "/requests/{request_id}/asset/{asset_id}"}
  delete: {method: DELETE, path: "/requests/{request_id}/asset/{asset_id}"}
  params: [{name: request_id, field: requestID}]
  id: {param: asset_id, from: id}
  multipart: {files: [file], calls: [create]}
  writeOnly: [file]
  createOnly: [file]
  sample: {requestID: q1, source: s, file: {name: a.txt, content: hello}}
- kind: Value
  group: kv.example.hfl.dev
  version: v1alpha1
  plural: values
  shape: upsert
  read: {method: GET, path: "/values/{key_name}", raw: value}
  update: {method: PUT, path: "/values/{key_name}"}
  delete: {method: DELETE, path: "/values/{key_name}"}
  params: [{name: key_name, field: keyName}]
  multipart: {}
  compareReturnedOnly: true
  sample: {keyName: k, value: some text}
- kind: Forecast
  group: cost.example.hfl.dev
  version: v1alpha1
  plural: forecasts
  shape: upsert
  envelope: data
  bodyEnvelope: data
  read: {method: GET, path: "/budget/{budget_id}/forecast"}
  update: {method: PUT, path: /budget/forecast}
  delete: {method: DELETE, path: "/budget/{budget_id}/forecast"}
  params: [{name: budget_id, field: attributes.budget_uid, inBody: true}]
  sample: {type: forecast, attributes: {budget_uid: b1}}
- kind: Framework
  group: compliance.example.hfl.dev
  version: v1alpha1
  plural: frameworks
  shape: crud
  envelope: data
  bodyEnvelope: data
  create: {method: POST, path: /frameworks}
  read: {method: GET, path: "/frameworks/{handle}/{version}"}
  update: {method: PUT, path: "/frameworks/{handle}/{version}"}
  delete: {method: DELETE, path: "/frameworks/{handle}/{version}"}
  params: [{name: handle, field: attributes.handle, inBody: true}]
  id: {param: version, from: attributes/version}
  sample: {type: framework, attributes: {handle: h, version: "1", name: n}}
`)
	EveryKind(t, typesYAML, &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}, nil)
}
