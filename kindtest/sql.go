// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	apifake "github.com/highfivelabs/krm-rest-runtime/fake"
	"github.com/highfivelabs/krm-rest-runtime/reconciler"
	"github.com/highfivelabs/krm-rest-runtime/rest"
	"github.com/highfivelabs/krm-rest-runtime/sqlapi"
)

// sqlSteps: an sql kind through create, read back, update, drift repair
// and delete on the fake's SQL API.
func sqlSteps(t *testing.T, api *apifake.Server, k *descriptor.Kind, spec map[string]any, c client.Client,
	r *reconciler.Reconciler, req ctrl.Request, reconcile func(string) *unstructured.Unstructured) {
	t.Helper()
	ctx := context.Background()
	q := k.SQL
	// An asynchronous kind (sql.settle) waits for its status: the fake
	// keeps each statement's work in flight for a read, so a create, an
	// update and a delete each wait at least once, and are done the next
	// time.
	waits := 0
	settle := func(step string) *unstructured.Unstructured {
		t.Helper()
		if q.Settle == nil {
			return reconcile(step)
		}
		for i := 0; ; i++ {
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatalf("%s: reconcile: %v", step, err)
			}
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(k.GVK())
			if err := c.Get(ctx, req.NamespacedName, got); err != nil {
				t.Fatalf("%s: get: %v", step, err)
			}
			conds, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
			var cond map[string]any
			if len(conds) > 0 {
				cond, _ = conds[0].(map[string]any)
			}
			if cond["status"] == "True" {
				return got
			}
			if cond["reason"] != reconciler.SettlingReason || i >= 10 {
				t.Fatalf("%s: not Ready: %v", step, conds)
			}
			waits++
		}
	}
	waited := func(step string) {
		t.Helper()
		if q.Settle != nil && waits == 0 {
			t.Fatalf("%s: never waited for the status, though the fake keeps the work in flight", step)
		}
		waits = 0
	}
	only := func(step string) map[string]any {
		t.Helper()
		recs := api.SQLRecords(k.Kind)
		if len(recs) != 1 {
			t.Fatalf("%s: the fake holds %d %s, want 1: %v", step, len(recs), k.Kind, recs)
		}
		return recs[0]
	}

	// 1. Create, and read back: nothing more is sent.
	got := settle("create")
	waited("create")
	rec := only("create")
	written := map[string]bool{q.Warehouse: q.Warehouse != ""} // the fake keeps the warehouse a statement ran in
	for n, tmpl := range q.Templates() {
		if t, err := descriptor.ParseSQLTemplate(tmpl); err == nil && n != "delete" {
			for _, f := range t.Fields() {
				written[f] = true
			}
		}
	}
	for f, v := range bodyOf(k, spec) {
		if !written[f] {
			continue // only the delete names it (who ownership goes back to)
		}
		if !sqlapi.SameField(q.Fields[f], v, rec[f]) && fmt.Sprint(v) != fmt.Sprint(rec[f]) {
			t.Fatalf("create: %s = %v in Snowflake, want %v", f, rec[f], v)
		}
	}
	if q.Mode == descriptor.SQLObject {
		if id, _, _ := unstructured.NestedString(got.Object, "status", "id"); id == "" {
			t.Fatalf("create: no status.id")
		}
	}
	got = reconcile("resync")
	if reason := readyReason(got); reason != "Unchanged" {
		t.Fatalf("resync: Ready reason %s, want Unchanged (the read back differs from the spec)", reason)
	}

	// 2. Change a field (a grant: revoke and grant again), 3. change it
	// behind the controller's back, and it is put back.
	if field := changeable(k, spec); field != "" {
		_ = unstructured.SetNestedField(got.Object, "changed-"+field, "spec", field)
		if err := c.Update(ctx, got); err != nil {
			t.Fatal(err)
		}
		settle("update")
		waited("update")
		rec = only("update")
		if rec[field] != "changed-"+field {
			t.Fatalf("update: %s = %v in Snowflake, want the new value", field, rec[field])
		}
		if q.Mode != descriptor.SQLGrant && q.Readable(field) {
			rec[field] = "edited-in-the-console"
			settle("drift")
			if rec = only("drift"); rec[field] != "changed-"+field {
				t.Fatalf("drift: %s = %v, want it restored", field, rec[field])
			}
		}
	}

	// 4. Delete.
	got = &unstructured.Unstructured{}
	got.SetGroupVersionKind(k.GVK())
	_ = c.Get(ctx, req.NamespacedName, got)
	if err := c.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("delete: reconcile: %v", err)
		}
		if err := c.Get(ctx, req.NamespacedName, got); err != nil || q.Settle == nil || i >= 10 {
			break
		}
		waits++
	}
	waited("delete")
	if q.PerField() {
		// What the settings are on stays (the account): each field set is
		// unset.
		rec := only("delete")
		for f := range bodyOf(k, spec) {
			if _, set := rec[f]; set && q.Settable(f) {
				t.Fatalf("delete: %s still set in Snowflake: %v", f, rec[f])
			}
		}
	} else if recs := api.SQLRecords(k.Kind); len(recs) != 0 {
		t.Fatalf("delete: still in Snowflake: %v", recs)
	}
	if err := c.Get(ctx, req.NamespacedName, got); err == nil {
		t.Fatalf("delete: the object still exists (finalizer not removed)")
	}
}

// sqlChangeable picks a string field an sql kind can change in place and
// read back (a grant: any string field), not one that names the object.
func sqlChangeable(k *descriptor.Kind, spec map[string]any) string {
	q := k.SQL
	naming := map[string]bool{}
	for _, f := range q.ID {
		naming[f] = true
	}
	if q.Mode == descriptor.SQLSet {
		t, _ := descriptor.ParseSQLTemplate(q.Delete)
		for _, f := range t.Fields() {
			naming[f] = true // what the setting is on
		}
	}
	var cands []string
	for f, v := range spec {
		if _, ok := v.(string); !ok || naming[f] || q.Fields[f].Type != "string" {
			continue
		}
		if q.Mode == descriptor.SQLGrant || (q.Settable(f) && q.Readable(f)) {
			cands = append(cands, f)
		}
	}
	sort.Strings(cands)
	if len(cands) == 0 {
		return ""
	}
	return cands[0]
}

// sqlUpdatable: the live steps change a field of the kind in place.
func sqlUpdatable(k *descriptor.Kind) bool {
	return k.SQL != nil && k.SQL.Mode != descriptor.SQLGrant
}

// sqlID is the object's name parts from params (status.id), if it has one.
func sqlID(k *descriptor.Kind, params map[string]string) ([]string, error) {
	if k.SQL.Mode != descriptor.SQLObject {
		return nil, nil
	}
	if params["@id"] == "" {
		return nil, rest.ErrNotFound
	}
	return sqlapi.ParseID(params["@id"], len(k.SQL.ID))
}

// sqlRead reads an sql kind's object as the reconciler does.
func (r *liveRun) sqlRead(ctx context.Context, k *descriptor.Kind, params map[string]string, spec map[string]any) (map[string]any, error) {
	id, err := sqlID(k, params)
	if err != nil {
		return nil, err
	}
	return sqlapi.New(k, r.api, r.noWait()).Read(ctx, bodyOf(k, spec), id)
}

// sqlOutOfBand changes field to value with the kind's own statement, as a
// person in a worksheet would.
func (r *liveRun) sqlOutOfBand(ctx context.Context, k *descriptor.Kind, params map[string]string, spec map[string]any, field string, value any) error {
	id, err := sqlID(k, params)
	if err != nil {
		return err
	}
	vals := bodyOf(k, spec)
	vals[field] = value
	sk := sqlapi.New(k, r.api, r.noWait())
	if k.SQL.Mode == descriptor.SQLSet && !k.SQL.PerField() {
		_, err := sk.Create(ctx, vals, id)
		return err
	}
	return sk.Alter(ctx, field, vals, id, false)
}

// sqlForceDelete drops what a failed test left.
func (r *liveRun) sqlForceDelete(ctx context.Context, k *descriptor.Kind, params map[string]string, spec map[string]any) error {
	id, err := sqlID(k, params)
	if errors.Is(err, rest.ErrNotFound) {
		return nil // never made
	}
	if err != nil {
		return err
	}
	if err := sqlapi.New(k, r.api, r.noWait()).Delete(ctx, bodyOf(k, spec), id); err != nil && !errors.Is(err, rest.ErrNotFound) {
		return err
	}
	return nil
}

// noWait: a replay or rehearsal polls without waiting.
func (r *liveRun) noWait() func(context.Context, time.Duration) error {
	if r.vclock.IsZero() {
		return nil
	}
	return func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
}
