// SPDX-License-Identifier: Apache-2.0

package descriptor

import (
	"fmt"
	"sort"
	"strings"
)

// Element makes a kind one element of a list another resource holds, which
// the API reads and writes only as part of that resource: an ID in a Vault
// OIDC key's allowed_client_ids, a user in a Grafana role's assignments, a
// GitHub repository's value of one custom property. The kind reads the
// resource at Read.Path and writes it with Update; it has no other calls of
// its own (Delete is the same write, with the element left out).
type Element struct {
	// In, for a list of bare values (IDs): the spec field whose value the
	// element is, and the list it is in (a slash path inside the resource
	// as read). With more than one, the spec sets exactly one of the fields:
	// a Grafana role's users, teams and serviceAccounts.
	In map[string]string `json:"in,omitempty"`
	// List and Key, for a list of objects: where the list is ("" the read
	// answers the list itself, GitHub's [{"property_name", "value"}]) and the
	// element field that names one. The element is the kind's body.
	List string `json:"list,omitempty"`
	Key  string `json:"key,omitempty"`
	// Patch: the write sends this element alone, in a list at this body
	// field (GitHub's PATCH .../properties/values {"properties": [...]}),
	// and removes it by sending it with Unset's fields. Without Patch the
	// write sends whole lists: every list In names, as read just before,
	// with this element added or removed (a read-modify-write, as
	// Terraform's provider does).
	Patch string `json:"patch,omitempty"`
	// Unset: the fields an element is sent with to remove it, beside its
	// Key (GitHub: {"value": null}).
	Unset map[string]any `json:"unset,omitempty"`
	// Whole: the write sends the whole resource as read just before, with
	// the lists changed, for an API whose write replaces the resource
	// (Okta's PUT /api/v1/apps/{appId}, its redirect URIs in
	// settings.oauthClient.redirect_uris), as Terraform's provider does.
	Whole bool `json:"whole,omitempty"`
}

// IsElement reports whether the kind is one element of another resource's
// list (Kind.Element).
func (k *Kind) IsElement() bool { return k.Element != nil }

// Lists are the lists an Element kind's whole write sends (In's), sorted.
func (e *Element) Lists() []string {
	out := make([]string, 0, len(e.In))
	for _, l := range e.In {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// Of is the element body names: for bare values, the one In field body
// sets, its list and its value; for objects, Key's value and List.
func (e *Element) Of(body map[string]any) (field, list string, value any, err error) {
	if len(e.In) == 0 {
		v := body[e.Key]
		if v == nil || Text(v) == "" {
			return "", "", nil, fmt.Errorf("spec.%s is required", e.Key)
		}
		return e.Key, e.List, v, nil
	}
	var set []string
	for f := range e.In {
		if v, ok := body[f]; ok && v != nil && Text(v) != "" {
			set = append(set, f)
		}
	}
	sort.Strings(set)
	if len(set) != 1 {
		names := make([]string, 0, len(e.In))
		for f := range e.In {
			names = append(names, "spec."+f)
		}
		sort.Strings(names)
		return "", "", nil, fmt.Errorf("set exactly one of %s (set: %d)", strings.Join(names, ", "), len(set))
	}
	return set[0], e.In[set[0]], body[set[0]], nil
}

// Find is the index of the element named value in list: a bare value equal
// to it, or an object whose Key is (both as text), or -1.
func (e *Element) Find(list []any, value any) int {
	for i, x := range list {
		if len(e.In) == 0 {
			m, _ := x.(map[string]any)
			if m != nil && m[e.Key] != nil && Text(m[e.Key]) == Text(value) {
				return i
			}
			continue
		}
		if _, obj := x.(map[string]any); !obj && x != nil && Text(x) == Text(value) {
			return i
		}
	}
	return -1
}

func (e *Element) validate(k *Kind) error {
	if k.Shape != Upsert || k.Update == nil || k.Delete == nil {
		return fmt.Errorf("element is for upsert kinds with an update (the write) and a delete")
	}
	if (len(e.In) == 0) == (e.Key == "") {
		return fmt.Errorf("element needs in (bare values) or key (objects), not both")
	}
	if len(e.In) > 0 && e.List != "" {
		return fmt.Errorf("element: list is for objects (key); in names each list")
	}
	if len(e.In) > 0 && e.Patch != "" {
		return fmt.Errorf("element: patch is for objects (key)")
	}
	if e.Whole && e.Patch != "" {
		return fmt.Errorf("element: whole is for a write of whole lists, not a patch")
	}
	if len(e.Unset) > 0 && e.Patch == "" {
		return fmt.Errorf("element: unset is for a patch")
	}
	if k.Read.Find != nil || k.Create != nil {
		return fmt.Errorf("element: the kind reads its resource whole and has no create")
	}
	return nil
}

// State is a field the API sets by a call per value rather than in a body:
// a lifecycle (Okta's org factor: POST .../lifecycle/activate or
// .../deactivate), a subscription (.../subscribe, .../unsubscribe), a
// feature (.../enable, .../disable). The spec field of the same name asks
// for a value; one the read answers otherwise is set by its call.
type State struct {
	// Field is the top-level field the read answers the state in, and the
	// spec field that asks for one; it is never sent in a body.
	Field string `json:"field"`
	// Calls: by value, the call that puts the resource in it. A value
	// matches the read's ignoring case.
	Calls map[string]Op `json:"calls"`
	// Delete: the value deleting the object puts the resource in (a factor
	// is deactivated, a subscription unsubscribed), in place of a delete
	// call: the resource stays, in that state.
	Delete string `json:"delete,omitempty"`
	// Before: by value, calls made in order before the value's call
	// (Grafana Asserts configures its stack's datasets, POST
	// /v2/stack/datasets/auto-setup, before POST /v2/stack/enable).
	Before map[string][]Op `json:"before,omitempty"`
}

// Call is the call that puts the resource in state value, matching ignoring
// case; nil if there is none.
func (s *State) Call(value string) (*Op, string) {
	if op, ok := s.Calls[value]; ok {
		return &op, value
	}
	for v, op := range s.Calls {
		if strings.EqualFold(v, value) {
			op := op
			return &op, v
		}
	}
	return nil, ""
}

// Values are the states the field may ask for, sorted.
func (s *State) Values() []string {
	out := make([]string, 0, len(s.Calls))
	for v := range s.Calls {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func (s *State) validate(k *Kind) error {
	if s.Field == "" || strings.ContainsAny(s.Field, "./") || len(s.Calls) == 0 {
		return fmt.Errorf("state needs a top-level field and a call for each value (one or more: Okta's domain verification has only VERIFIED)")
	}
	if s.Delete != "" {
		if _, ok := s.Calls[s.Delete]; !ok {
			return fmt.Errorf("state.delete %q has no call", s.Delete)
		}
		if k.Delete != nil {
			return fmt.Errorf("state.delete stands in for a delete call: the kind has one")
		}
	}
	for v, ops := range s.Before {
		if _, ok := s.Calls[v]; !ok {
			return fmt.Errorf("state.before.%s: no call sets %q", v, v)
		}
		for _, op := range ops {
			if op.Method == "" || !strings.HasPrefix(op.Path, "/") || op.GraphQL != nil {
				return fmt.Errorf("state.before.%s: a REST call (method and /path)", v)
			}
		}
	}
	if k.Shape != CRUD && k.Shape != Settings && k.Shape != Upsert {
		return fmt.Errorf("state is for crud, upsert and settings kinds")
	}
	if k.IsGraphQL() || k.Element != nil {
		return fmt.Errorf("state is for REST kinds of their own")
	}
	return nil
}

// stateOps are the state calls, by value in order, each after the calls
// made before it.
func (k *Kind) stateOps() []*Op {
	if k.State == nil {
		return nil
	}
	out := []*Op{}
	for _, v := range k.State.Values() {
		for i := range k.State.Before[v] {
			out = append(out, &k.State.Before[v][i])
		}
		op := k.State.Calls[v]
		out = append(out, &op)
	}
	return out
}

// IsStateField reports whether a spec field is the kind's State field,
// which is set by calls and never sent in a body.
func (k *Kind) IsStateField(field string) bool { return k.State != nil && k.State.Field == field }

// StateDeletes reports whether deleting the object sets the resource's state
// (State.Delete) rather than deleting it.
func (k *Kind) StateDeletes() bool { return k.State != nil && k.State.Delete != "" }
