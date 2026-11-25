// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// runtimeKind is an asynchronous object, as an Openflow runtime: each
// statement leaves it busy until it is read again (the fake's model).
func runtimeKind() *descriptor.Kind {
	return &descriptor.Kind{
		Kind: "OpenflowRuntime", Group: "openflow.example.hfl.dev", Version: "v1alpha1", Plural: "openflowruntimes",
		Shape: descriptor.SQLShape, Read: descriptor.Op{Method: "POST", Path: "/api/v2/statements"},
		WriteOnly: []string{"display_name", "node_type"},
		SQL: &descriptor.SQL{
			Mode: descriptor.SQLObject, ID: []string{"name"},
			Fields: map[string]descriptor.SQLField{"name": {Type: "identifier"}, "node_type": {Type: "string"},
				"min_nodes": {Type: "integer"}, "display_name": {Type: "string"}, "comment": {Type: "string"},
				"status": {Type: "string", Output: true}},
			Create: "CREATE OPENFLOW RUNTIME {@id} NODE_TYPE = {node_type} MIN_NODES = {min_nodes}[ DISPLAY_NAME = {display_name}][ COMMENT = {comment}]",
			Read: []descriptor.SQLRead{{Statement: "SHOW OPENFLOW RUNTIMES LIKE {@like}", Match: map[string]string{"name": "{name}"},
				Columns: map[string]string{"status": "status", "min_nodes": "min_nodes", "comment": "comment"}}},
			Alter: map[string]string{
				"min_nodes":    "ALTER OPENFLOW RUNTIME {@id} SET MIN_NODES = {min_nodes}",
				"display_name": "ALTER OPENFLOW RUNTIME {@id} SET DISPLAY_NAME = {display_name}",
				"comment":      "ALTER OPENFLOW RUNTIME {@id} SET COMMENT = {comment}",
			},
			Unset:  map[string]string{"comment": "ALTER OPENFLOW RUNTIME {@id} UNSET COMMENT"},
			Delete: "DROP OPENFLOW RUNTIME IF EXISTS {@id}",
			Settle: &descriptor.SQLSettle{
				Field: "status", Ready: []string{"ACTIVE", "SUSPENDED"}, Failed: []string{"CREATE_FAILED"},
				Busy: []string{"CREATING", "UPDATING"}, Gone: []string{"DELETED"},
				Teardown: []descriptor.SQLTeardownStep{{When: []string{"DELETING"}}, {Run: "ALTER OPENFLOW RUNTIME IF EXISTS {@id} TERMINATE"}},
			},
		},
	}
}

func countStmt(stmts []string, prefix string) int {
	n := 0
	for _, s := range stmts {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

// An asynchronous object is waited for, never blocked on: each statement
// is read back and, while the status is busy, the reconciler requeues
// (Ready=False, Settling); the next statement goes once it is done, and
// the Ready reason is the one the wait was for. Nothing is sent twice.
func TestSQLSettleCreateAndUpdate(t *testing.T) {
	h := newSQLHarness(t, runtimeKind(), map[string]any{"name": "rt", "node_type": "SMALL", "min_nodes": float64(1), "display_name": "d1"})
	obj, reason, msg, stmts := h.reconcile()
	if reason != SettlingReason || !strings.Contains(msg, "CREATING") || countStmt(stmts, "CREATE OPENFLOW RUNTIME") != 1 {
		t.Fatalf("create: %s %q %v", reason, msg, stmts)
	}
	if id, _, _ := unstructured.NestedString(obj.Object, "status", "id"); id != `"RT"` {
		t.Fatalf("status.id %q while it waits: a delete must still drop it", id)
	}
	if _, reason, msg, stmts = h.reconcile(); reason != "Created" || countStmt(stmts, "ALTER") != 0 {
		t.Fatalf("created: %s %q %v (display_name was written by the CREATE)", reason, msg, stmts)
	}
	obj, reason, _, stmts = h.reconcile()
	if reason != "Unchanged" || len(stmts) != 1 {
		t.Fatalf("resync: %s %v", reason, stmts)
	}

	// Three changes: one statement a reconcile, each waited for.
	h.setSpec(obj, "comment", "c")
	h.setSpec(obj, "display_name", "d2")
	h.setSpec(obj, "min_nodes", float64(2))
	var all []string
	for i := 0; ; i++ {
		obj, reason, msg, stmts = h.reconcile()
		all = append(all, stmts...)
		if reason != SettlingReason {
			break
		}
		if countStmt(stmts, "ALTER") > 1 || i > 6 {
			t.Fatalf("update: %d ALTERs while busy: %v", countStmt(stmts, "ALTER"), stmts)
		}
	}
	if reason != "Updated" || countStmt(all, "ALTER") != 3 || !hasStmt(all, `ALTER OPENFLOW RUNTIME "RT" SET DISPLAY_NAME = 'd2'`) {
		t.Fatalf("update: %s %q %v", reason, msg, all)
	}
	if obj, reason, _, stmts = h.reconcile(); reason != "Unchanged" || countStmt(stmts, "ALTER") != 0 {
		t.Fatalf("after update: %s %v", reason, stmts)
	}

	// Busy behind the controller's back: nothing is sent until it settles.
	rec := h.api.SQLRecords("OpenflowRuntime")[0]
	rec["status"] = "UPDATING"
	h.setSpec(obj, "comment", "c2")
	if obj, reason, _, stmts = h.reconcile(); reason != SettlingReason || countStmt(stmts, "ALTER") != 0 {
		t.Fatalf("busy: %s %v", reason, stmts)
	}
	// A wait longer than the timeout is an error, retried with backoff.
	h.r.Now = func() time.Time { return time.Now().Add(31 * time.Minute) }
	if obj, reason, msg, _ = h.reconcile(); reason != "SyncFailed" || !strings.Contains(msg, "longer than 30m0s") {
		t.Fatalf("timeout: %s %q", reason, msg)
	}
	h.r.Now = nil
	rec["status"] = "ACTIVE"
	for i := 0; i < 4 && reason != "Updated"; i++ {
		obj, reason, _, _ = h.reconcile()
	}
	if reason != "Updated" || rec["comment"] != "c2" {
		t.Fatalf("after the wait: %s, comment %v", reason, rec["comment"])
	}

	// A failure status is an error, not a wait.
	rec["status"] = "CREATE_FAILED"
	if obj, reason, msg, _ = h.reconcile(); reason != "SyncFailed" || !strings.Contains(msg, "CREATE_FAILED") {
		t.Fatalf("failed: %s %q", reason, msg)
	}
	rec["status"] = "ACTIVE"
	_, _, _, _ = h.reconcile()
}

// Deleting tears the object down first: TERMINATE once it is settled,
// read back at once, then a wait while it is DELETING, then DROP once it
// is DELETED.
func TestSQLSettleDelete(t *testing.T) {
	h := newSQLHarness(t, runtimeKind(), map[string]any{"name": "rt", "node_type": "SMALL", "min_nodes": float64(1)})
	_, _, _, _ = h.reconcile()
	obj, reason, _, _ := h.reconcile()
	if reason != "Created" {
		t.Fatalf("create: %s", reason)
	}
	if err := h.c.Delete(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	var all []string
	for i := 0; i < 6; i++ {
		got, reason, msg, stmts := h.reconcile()
		all = append(all, stmts...)
		if got == nil {
			break
		}
		if reason != SettlingReason {
			t.Fatalf("delete: %s %q %v", reason, msg, stmts)
		}
		if hasStmt(stmts, "DROP") {
			t.Fatalf("dropped while %q", msg)
		}
	}
	want := []string{
		`SHOW OPENFLOW RUNTIMES LIKE 'RT'`, `ALTER OPENFLOW RUNTIME IF EXISTS "RT" TERMINATE`, `SHOW OPENFLOW RUNTIMES LIKE 'RT'`,
		`SHOW OPENFLOW RUNTIMES LIKE 'RT'`, `DROP OPENFLOW RUNTIME IF EXISTS "RT"`,
	}
	if strings.Join(all, "\n") != strings.Join(want, "\n") {
		t.Fatalf("delete sent\n%s\nwant\n%s", strings.Join(all, "\n"), strings.Join(want, "\n"))
	}
	if recs := h.api.SQLRecords("OpenflowRuntime"); len(recs) != 0 {
		t.Fatalf("still there: %v", recs)
	}
}

// A field Snowflake takes only when it makes the object, and never reads
// back, is not ignored when the spec changes it: the change is reported.
// An object made before the hashes were kept takes its spec as it is.
func TestSQLCreatedWith(t *testing.T) {
	k := monitorKind()
	k.SQL.Fields["start"] = descriptor.SQLField{Type: "string"}
	k.SQL.Create = "CREATE RESOURCE MONITOR {@id}[ WITH[ CREDIT_QUOTA = {credit_quota}][ FREQUENCY = {frequency}][ START_TIMESTAMP = {start}]]"
	k.WriteOnly = []string{"start"}
	h := newSQLHarness(t, k, map[string]any{"name": "rm", "credit_quota": float64(5), "start": "IMMEDIATELY"})
	obj, reason, _, _ := h.reconcile()
	if reason != "Created" {
		t.Fatalf("create: %s", reason)
	}
	if cw, _, _ := unstructured.NestedStringMap(obj.Object, "status", CreatedWith); len(cw) != 1 || cw["start"] == "" {
		t.Fatalf("status.createdWith %v", cw)
	}
	h.setSpec(obj, "start", "2030-01-01 00:00")
	obj, reason, msg, stmts := h.reconcile()
	if reason != "SyncFailed" || !strings.Contains(msg, "spec.start changed") || hasStmt(stmts, "ALTER") || hasStmt(stmts, "CREATE") {
		t.Fatalf("changed: %s %q %v", reason, msg, stmts)
	}
	h.setSpec(obj, "start", nil)
	if obj, reason, msg, _ = h.reconcile(); reason != "SyncFailed" || !strings.Contains(msg, "spec.start was dropped") {
		t.Fatalf("dropped: %s %q", reason, msg)
	}
	h.setSpec(obj, "start", "IMMEDIATELY")
	if obj, reason, _, _ = h.reconcile(); reason != "Unchanged" {
		t.Fatalf("put back: %s", reason)
	}
	// Made before: no hashes; the spec now is taken as what it was made with.
	unstructured.RemoveNestedField(obj.Object, "status", CreatedWith)
	if err := h.c.Status().Update(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	h.setSpec(obj, "start", "2031-01-01 00:00")
	if obj, reason, _, _ = h.reconcile(); reason != "Unchanged" {
		t.Fatalf("no hashes yet: %s", reason)
	}
	if cw, _, _ := unstructured.NestedStringMap(obj.Object, "status", CreatedWith); cw["start"] != fieldHash("2031-01-01 00:00") {
		t.Fatalf("status.createdWith %v", cw)
	}
}
