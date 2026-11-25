// SPDX-License-Identifier: Apache-2.0

package descriptor

import "testing"

// An Element kind is an upsert that reads its resource whole, names bare
// values (in) or keyed objects (key), and patches only objects; a State
// kind needs two values' calls, and its delete value stands in for a
// delete call (a crud kind with one needs neither delete nor forget); a
// settings kind may have only its state; an upsert with a create may have
// no update; a delete path may name {current.field}.
func TestElementAndStateRules(t *testing.T) {
	head := "vendor: x\nconnection: {group: core.x.hfl.dev, version: v1alpha1, kind: Connection}\nkinds:\n"
	el := "- {kind: E, group: e.x.hfl.dev, version: v1alpha1, plural: es, shape: upsert, read: {method: GET, path: \"/k/{name}\"}, update: {method: POST, path: \"/k/{name}\"}, delete: {method: POST, path: \"/k/{name}\"}, params: [{name: name, field: name}]"
	calls := "calls: {ACTIVE: {method: POST, path: \"/f/{id}/activate\"}, INACTIVE: {method: POST, path: \"/f/{id}/deactivate\"}}"
	st := "- {kind: S, group: s.x.hfl.dev, version: v1alpha1, plural: ss, shape: settings, read: {method: GET, path: \"/f/{id}\"}, params: [{name: id, field: id}]"
	for doc, ok := range map[string]bool{
		el + ", element: {in: {client_id: allowed_client_ids}}}\n":                                 true,
		el + ", element: {key: property_name, patch: properties, unset: {value: null}}}\n":         true,
		el + ", element: {in: {a: as}, key: b}}\n":                                                 false, // both
		el + ", element: {in: {a: as}, patch: as}}\n":                                              false, // patch is for objects
		el + ", element: {key: b, unset: {value: null}}}\n":                                        false, // unset needs a patch
		st + ", state: {field: status, " + calls + ", delete: INACTIVE}}\n":                        true,
		st + ", state: {field: status, " + calls + ", delete: GONE}}\n":                            false, // no call for it
		st + ", state: {field: status, calls: {VERIFIED: {method: POST, path: \"/f/{id}/a\"}}}}\n": true,  // one value (a verification)
		st + "}\n": false, // settings with neither update nor state
		"- {kind: A, group: a.x.hfl.dev, version: v1alpha1, plural: as, shape: crud, read: {method: GET, path: \"/f/{id}\"}, update: {method: PUT, path: \"/f/{id}\"}, adopt: {method: GET, path: /f, match: {key: \"{key}\"}}, id: {param: id, from: id}, state: {field: status, " + calls + ", delete: INACTIVE}}\n": true,
		"- {kind: G, group: g.x.hfl.dev, version: v1alpha1, plural: gs, shape: upsert, create: {method: POST, path: \"/ca/{name}/generate\"}, read: {method: GET, path: \"/ca/{name}\"}, delete: {method: DELETE, path: \"/ca/{name}\"}, params: [{name: name, field: name}]}\n":                                       true,
		"- {kind: K, group: k.x.hfl.dev, version: v1alpha1, plural: ks, shape: upsert, read: {method: GET, path: \"/t/{id}/schedule\"}, update: {method: POST, path: \"/t/{id}/schedule\"}, delete: {method: DELETE, path: \"/schedules/{current.id}\"}, params: [{name: id, field: id}]}\n":                           true,
		"- {kind: K, group: k.x.hfl.dev, version: v1alpha1, plural: ks, shape: upsert, read: {method: GET, path: \"/t/{id}/schedule\"}, update: {method: POST, path: \"/schedules/{current.id}\"}, delete: {method: DELETE, path: \"/t/{id}/schedule\"}, params: [{name: id, field: id}]}\n":                           false, // only a delete path
	} {
		if _, err := Parse([]byte(head + doc)); (err == nil) != ok {
			t.Errorf("%s: err %v, want ok=%v", doc, err, ok)
		}
	}
}
