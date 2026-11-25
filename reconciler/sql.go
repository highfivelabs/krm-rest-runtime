// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/rest"
	"github.com/highfivelabs/krm-rest-runtime/sqlapi"
)

// Params keys an sql kind keeps beside the path parameters: the object's
// name (status.id) and the fields last applied (status.appliedFields).
const (
	sqlIDKey      = "@id"
	sqlAppliedKey = "@applied"
)

// AppliedFields is the status field listing the spec fields an sql object
// was last made or changed with: one the spec drops is unset.
const AppliedFields = "appliedFields"

func (r *Reconciler) sqlKind(api *rest.Client) *sqlapi.Kind {
	return sqlapi.New(r.Kind, api, r.Wait)
}

// syncSQL makes the object, setting or grant the spec describes, through
// the SQL API: as sync does for a REST kind.
func (r *Reconciler) syncSQL(ctx context.Context, api *rest.Client, params map[string]string, body, prev map[string]any, st sqlStatus) (map[string]any, string, error) {
	q := r.Kind.SQL
	sk := r.sqlKind(api)
	applied, sameAsApplied := st.applied, st.hash == st.appliedHash
	switch q.Mode {
	case descriptor.SQLGrant:
		return r.syncSQLGrant(ctx, sk, body, prev)
	case descriptor.SQLSet:
		if q.PerField() {
			return r.syncSQLFields(ctx, sk, params, body, applied, sameAsApplied, st.appliedHash == "")
		}
		return r.syncSQLSet(ctx, sk, body, prev, sameAsApplied)
	}
	if q.Settle != nil {
		return r.syncSQLSettle(ctx, sk, params, body, st)
	}
	id, err := sqlapi.IDOf(q, body)
	if err != nil {
		return nil, "", err
	}
	was := params[sqlIDKey]
	if was != "" && was != sqlapi.FormatID(id) {
		return nil, "", fmt.Errorf("the object is %s and the spec now names %s: Snowflake would make a second object; delete and recreate the Kubernetes object to rename it", was, sqlapi.FormatID(id))
	}
	params[sqlAppliedKey] = strings.Join(sortedFields(body), ",")
	create := func() (map[string]any, string, error) {
		made, err := sk.Create(ctx, body, id)
		if err != nil {
			return nil, "", fmt.Errorf("create: %w", err)
		}
		params[sqlIDKey] = sqlapi.FormatID(id) // made: from now on it is this object's
		setCreatedWith(params, createdWithOf(q, body))
		// Fields CREATE does not take are set once it exists (a share's
		// accounts: ALTER SHARE ... SET ACCOUNTS).
		t, _ := descriptor.ParseSQLTemplate(q.Create)
		inCreate := map[string]bool{}
		for _, f := range t.Fields() {
			inCreate[f] = true
		}
		for _, f := range sortedFields(body) {
			if inCreate[f] {
				continue
			}
			var err error
			switch {
			case q.Alter[f] != "":
				err = sk.Alter(ctx, f, body, id, false)
			case q.Add[f] != "": // ALTER FAILOVER GROUP ... ADD {allowed_databases} TO ...
				list, _ := body[f].([]any)
				err = sk.Delta(ctx, f, body, id, list, nil)
			}
			if err != nil {
				return nil, "", fmt.Errorf("create, then set %s: %w", f, err)
			}
		}
		obs, err := sk.Read(ctx, body, id)
		if err != nil {
			return nil, "", fmt.Errorf("read after create: %w", err)
		}
		for f, v := range made { // answered only now (a token's secret)
			obs[f] = v
		}
		return obs, "created", nil
	}
	if was == "" {
		// Never made: make it. CREATE refuses a name that is taken, so an
		// object this one did not make is never taken over.
		return create()
	}
	obs, err := sk.Read(ctx, body, id)
	switch {
	case errors.Is(err, rest.ErrNotFound):
		return create() // dropped outside Kubernetes: make it again
	case err != nil:
		return nil, "", fmt.Errorf("read: %w", err)
	}
	if err := checkCreatedWith(q, params, body, st.createdWith); err != nil {
		return obs, "drifted", err
	}
	norm := sqlapi.Normalize(q, body, obs)
	var changes []string
	for _, f := range sortedFields(body) {
		if !q.Readable(f) {
			if !sameAsApplied && q.Settable(f) {
				changes = append(changes, f) // never read back: written again when the spec changes
			}
			continue
		}
		if Drift(map[string]any{f: body[f]}, norm, nil) == "" {
			continue
		}
		if !q.Settable(f) {
			return obs, "drifted", fmt.Errorf("spec.%s differs from Snowflake, and it can only be set when the object is created; delete and recreate the object", f)
		}
		changes = append(changes, f)
	}
	var unsets []string
	for _, f := range applied {
		if _, still := body[f]; !still && (q.Unset[f] != "" || q.Remove[f] != "") {
			unsets = append(unsets, f)
		}
	}
	if len(changes) == 0 && len(unsets) == 0 {
		return obs, "unchanged", nil
	}
	for _, f := range changes {
		var err error
		if q.Alter[f] == "" { // a list changed by adding and removing elements
			want, _ := body[f].([]any)
			have, _ := norm[f].([]any)
			err = sk.Delta(ctx, f, body, id, want, have)
		} else {
			err = sk.Alter(ctx, f, body, id, false)
		}
		if err != nil {
			return nil, "", fmt.Errorf("update %s: %w", f, err)
		}
	}
	for _, f := range unsets {
		var err error
		if q.Unset[f] == "" {
			have, _ := norm[f].([]any)
			err = sk.Delta(ctx, f, body, id, nil, have)
		} else {
			err = sk.Alter(ctx, f, body, id, true)
		}
		if err != nil {
			return nil, "", fmt.Errorf("unset %s: %w", f, err)
		}
	}
	if obs, err = sk.Read(ctx, body, id); err != nil {
		return nil, "", fmt.Errorf("read after update: %w", err)
	}
	return obs, "updated", nil
}

// syncSQLSet: a setting on another object. Not set, or set otherwise: set
// it. When what it is set on changed, the old one is unset first.
func (r *Reconciler) syncSQLSet(ctx context.Context, sk *sqlapi.Kind, body, prev map[string]any, sameAsApplied bool) (map[string]any, string, error) {
	q := r.Kind.SQL
	if prev != nil && movedSetting(q, prev, body) {
		if err := sk.Delete(ctx, prev, nil); err != nil && !errors.Is(err, rest.ErrNotFound) {
			return nil, "", fmt.Errorf("unsetting the previous setting: %w", err)
		}
		prev = nil
	}
	obs, err := sk.Read(ctx, body, nil)
	action := "updated"
	switch {
	case errors.Is(err, rest.ErrNotFound):
		action = "created"
	case err != nil:
		return nil, "", fmt.Errorf("read: %w", err)
	default:
		norm := sqlapi.Normalize(q, body, obs)
		drift := ""
		for _, f := range sortedFields(body) {
			if q.Readable(f) && Drift(map[string]any{f: body[f]}, norm, nil) != "" {
				drift = f
				break
			}
			if !q.Readable(f) && !sameAsApplied {
				drift = f
			}
		}
		if drift == "" {
			return body, "unchanged", nil
		}
	}
	if _, err := sk.Create(ctx, body, nil); err != nil {
		return nil, "", fmt.Errorf("set: %w", err)
	}
	return body, action, nil
}

// syncSQLFields: settings set field by field (the account's parameters,
// resource monitor and policies). Each field the spec sets and Snowflake
// holds otherwise is set by its own alter (a field a read never maps: when
// the spec changed); one Snowflake will not replace in place (unsetFirst) is
// unset first; each field last applied that the spec drops is unset, unless
// a read maps it and finds it not set. A field with no alter (the
// organization account's name) only selects: one that differs is drift.
func (r *Reconciler) syncSQLFields(ctx context.Context, sk *sqlapi.Kind, params map[string]string, body map[string]any, applied []string, sameAsApplied, first bool) (map[string]any, string, error) {
	q := r.Kind.SQL
	obs, err := sk.Read(ctx, body, nil)
	if err != nil {
		if errors.Is(err, rest.ErrNotFound) {
			return nil, "", fmt.Errorf("read: Snowflake has none the spec names (%w): these settings are on something that exists already", err)
		}
		return nil, "", fmt.Errorf("read: %w", err)
	}
	norm := sqlapi.Normalize(q, body, obs)
	var changes []string
	for _, f := range sortedFields(body) {
		if _, isField := q.Fields[f]; !isField {
			continue
		}
		if !q.Readable(f) {
			if !sameAsApplied && q.Settable(f) {
				changes = append(changes, f)
			}
			continue
		}
		if Drift(map[string]any{f: body[f]}, norm, nil) == "" {
			continue
		}
		if !q.Settable(f) {
			return obs, "drifted", fmt.Errorf("spec.%s differs from Snowflake, and no statement changes it", f)
		}
		changes = append(changes, f)
	}
	unsetFirst := map[string]bool{}
	for _, f := range q.UnsetFirst {
		unsetFirst[f] = true
	}
	for _, f := range changes {
		if unsetFirst[f] && obs[f] != nil {
			if err := sk.Alter(ctx, f, body, nil, true); err != nil {
				return nil, "", fmt.Errorf("unset %s before setting it: %w", f, err)
			}
		}
		if err := sk.Alter(ctx, f, body, nil, false); err != nil {
			return nil, "", fmt.Errorf("set %s: %w", f, err)
		}
	}
	var unsets []string
	for _, f := range applied {
		if _, still := body[f]; still || q.Unset[f] == "" {
			continue
		}
		if q.Readable(f) && obs[f] == nil {
			continue // not set: Snowflake refuses to unset a policy that is not
		}
		unsets = append(unsets, f)
		if err := sk.Alter(ctx, f, body, nil, true); err != nil {
			return nil, "", fmt.Errorf("unset %s: %w", f, err)
		}
	}
	params[sqlAppliedKey] = strings.Join(settableFields(q, body), ",")
	switch {
	case first:
		return body, "created", nil
	case len(changes) == 0 && len(unsets) == 0:
		return body, "unchanged", nil
	}
	return body, "updated", nil
}

// settableFields are the spec's fields a statement sets: those a delete
// unsets.
func settableFields(q *descriptor.SQL, body map[string]any) []string {
	var out []string
	for _, f := range sortedFields(body) {
		if q.Settable(f) {
			out = append(out, f)
		}
	}
	return out
}

// deleteSQLFields unsets each field last applied, unless a read maps it and
// finds it not set; the rest stays as it is.
func (r *Reconciler) deleteSQLFields(ctx context.Context, sk *sqlapi.Kind, spec map[string]any, applied []string) error {
	q := r.Kind.SQL
	obs, err := sk.Read(ctx, spec, nil)
	if errors.Is(err, rest.ErrNotFound) {
		return nil // what they were on is gone
	}
	if err != nil {
		return err
	}
	for _, f := range applied {
		if q.Unset[f] == "" || (q.Readable(f) && obs[f] == nil) {
			continue
		}
		if err := sk.Alter(ctx, f, spec, nil, true); err != nil {
			return fmt.Errorf("unset %s: %w", f, err)
		}
	}
	return nil
}

// movedSetting: what the setting is on (the fields its delete and create
// both name) differs between what was applied and the spec. A field only
// the delete names (who ownership goes back to) moves nothing.
func movedSetting(q *descriptor.SQL, prev, body map[string]any) bool {
	t, err := descriptor.ParseSQLTemplate(q.Delete)
	if err != nil {
		return false
	}
	c, err := descriptor.ParseSQLTemplate(q.Create)
	if err != nil {
		return false
	}
	inCreate := map[string]bool{}
	for _, f := range c.Fields() {
		inCreate[f] = true
	}
	for _, f := range t.Fields() {
		if !inCreate[f] {
			continue
		}
		if !sqlapi.SameField(q.Fields[f], prev[f], body[f]) && fmt.Sprint(prev[f]) != fmt.Sprint(body[f]) {
			return true
		}
	}
	return false
}

// syncSQLGrant: as syncAssoc. The grant is read back; when the spec changed,
// what was granted before is revoked first. A grant found whose mapped
// columns differ from the spec (the columns a storage lifecycle policy is
// on) is undone and made again.
func (r *Reconciler) syncSQLGrant(ctx context.Context, sk *sqlapi.Kind, body, prev map[string]any) (map[string]any, string, error) {
	q := r.Kind.SQL
	obs, err := sk.Read(ctx, body, nil)
	stale := false
	switch {
	case err == nil:
		norm := sqlapi.Normalize(q, body, obs)
		for _, rd := range q.Read {
			for f := range rd.Columns {
				if body[f] != nil && Drift(map[string]any{f: body[f]}, norm, nil) != "" {
					stale = true
				}
			}
		}
		if !stale {
			return body, "unchanged", nil
		}
	case !errors.Is(err, rest.ErrNotFound):
		return nil, "", fmt.Errorf("read: %w", err)
	}
	action := "created"
	var undo map[string]any
	switch {
	case prev != nil && Drift(prev, body, nil) != "":
		undo = prev
	case stale:
		undo = body
	}
	if undo != nil {
		if err := sk.Delete(ctx, undo, nil); err != nil && !errors.Is(err, rest.ErrNotFound) {
			return nil, "", fmt.Errorf("revoking the previous grant: %w", err)
		}
		action = "updated"
	}
	if _, err := sk.Create(ctx, body, nil); err != nil {
		return nil, "", fmt.Errorf("grant: %w", err)
	}
	return body, action, nil
}

// deleteSQL drops the object, unsets the setting or revokes the grant last
// applied. Nothing made, nothing to delete.
func (r *Reconciler) deleteSQL(ctx context.Context, api *rest.Client, params map[string]string, spec, prev map[string]any) error {
	q := r.Kind.SQL
	sk := r.sqlKind(api)
	if q.PerField() {
		var applied []string
		for _, f := range strings.Split(params[sqlAppliedKey], ",") {
			if f != "" {
				applied = append(applied, f)
			}
		}
		return r.deleteSQLFields(ctx, sk, spec, applied)
	}
	if q.Mode != descriptor.SQLObject {
		if prev == nil {
			return nil
		}
		// What was applied, and where the status does not keep a field
		// (write-only: the role ownership goes back to), the spec's.
		vals := make(map[string]any, len(prev))
		for f, v := range spec {
			if _, isField := q.Fields[f]; isField {
				vals[f] = v
			}
		}
		for f, v := range prev {
			vals[f] = v
		}
		if err := sk.Delete(ctx, vals, nil); err != nil && !errors.Is(err, rest.ErrNotFound) {
			return err
		}
		return nil
	}
	if params[sqlIDKey] == "" {
		return nil
	}
	id, err := sqlapi.ParseID(params[sqlIDKey], len(q.ID))
	if err != nil {
		return err
	}
	if q.Settle != nil {
		return r.deleteSQLSettle(ctx, sk, spec, id)
	}
	if err := sk.Delete(ctx, spec, id); err != nil && !errors.Is(err, rest.ErrNotFound) {
		return err
	}
	return nil
}

// appliedFields is status.appliedFields.
func appliedFields(obj *unstructured.Unstructured) []string {
	l, _, _ := unstructured.NestedStringSlice(obj.Object, "status", AppliedFields)
	return l
}

func sortedFields(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for f := range m {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}
