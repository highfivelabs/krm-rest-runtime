// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	apifake "github.com/highfivelabs/krm-rest-runtime/fake"
	"github.com/highfivelabs/krm-rest-runtime/rest"
	"github.com/highfivelabs/krm-rest-runtime/sqlapi"
)

func monitorKind() *descriptor.Kind {
	return &descriptor.Kind{
		Kind: "ResourceMonitor", Group: "resource.example.hfl.dev", Version: "v1alpha1", Plural: "resourcemonitors",
		Shape: descriptor.SQLShape, Read: descriptor.Op{Method: "POST", Path: "/api/v2/statements"},
		SQL: &descriptor.SQL{
			Mode: descriptor.SQLObject, ID: []string{"name"},
			Fields: map[string]descriptor.SQLField{"name": {Type: "identifier"}, "credit_quota": {Type: "integer"},
				"frequency": {Type: "keyword"}, "comment": {Type: "string"}},
			Create: "CREATE RESOURCE MONITOR {@id}[ WITH[ CREDIT_QUOTA = {credit_quota}][ FREQUENCY = {frequency}]]",
			Read: []descriptor.SQLRead{{Statement: "SHOW RESOURCE MONITORS LIKE {@like}", Match: map[string]string{"name": "{name}"},
				Columns: map[string]string{"credit_quota": "credit_quota", "frequency": "frequency", "comment": "comment"}}},
			Alter:  map[string]string{"credit_quota": "ALTER RESOURCE MONITOR {@id} SET CREDIT_QUOTA = {credit_quota}"},
			Unset:  map[string]string{"credit_quota": "ALTER RESOURCE MONITOR {@id} SET CREDIT_QUOTA = null"},
			Delete: "DROP RESOURCE MONITOR IF EXISTS {@id}",
		},
	}
}

// sqlHarness runs a kind on the fake's SQL API and records each statement.
type sqlHarness struct {
	t     *testing.T
	api   *apifake.Server
	r     *Reconciler
	c     client.Client
	req   ctrl.Request
	stmts []string
}

func newSQLHarness(t *testing.T, k *descriptor.Kind, spec map[string]any) *sqlHarness {
	t.Helper()
	h := &sqlHarness{t: t, api: apifake.New(&descriptor.File{Kinds: []descriptor.Kind{*k}})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "POST" {
			b, _ := io.ReadAll(req.Body)
			var body sqlapi.Request
			_ = json.Unmarshal(b, &body)
			h.stmts = append(h.stmts, body.Statement)
			req.Body = io.NopCloser(bytes.NewReader(b))
		}
		h.api.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	h.r, h.c, h.req = newTestReconciler(t, k, srv.URL, spec)
	h.r.Wait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return h
}

// reconcile runs once and returns the object, its Ready reason and message,
// and the statements sent.
func (h *sqlHarness) reconcile() (*unstructured.Unstructured, string, string, []string) {
	h.t.Helper()
	h.stmts = nil
	_, _ = h.r.Reconcile(context.Background(), h.req)
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(h.r.Kind.GVK())
	if err := h.c.Get(context.Background(), h.req.NamespacedName, obj); err != nil {
		return nil, "", "", h.stmts
	}
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if len(conds) == 0 {
		return obj, "", "", h.stmts
	}
	c := conds[0].(map[string]any)
	return obj, c["reason"].(string), c["message"].(string), h.stmts
}

func (h *sqlHarness) setSpec(obj *unstructured.Unstructured, f string, v any) {
	h.t.Helper()
	if v == nil {
		unstructured.RemoveNestedField(obj.Object, "spec", f)
	} else {
		_ = unstructured.SetNestedField(obj.Object, v, "spec", f)
	}
	if err := h.c.Update(context.Background(), obj); err != nil {
		h.t.Fatal(err)
	}
}

func hasStmt(stmts []string, prefix string) bool {
	for _, s := range stmts {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// An object of a name that is taken is never taken over: CREATE is refused,
// the object gets no status.id, and deleting it drops nothing.
func TestSQLNeverTakesOverAnObject(t *testing.T) {
	k := monitorKind()
	h := newSQLHarness(t, k, map[string]any{"name": "taken", "credit_quota": float64(5)})
	// Someone else's monitor of that name.
	srv := httptest.NewServer(h.api)
	defer srv.Close()
	sk := sqlapi.New(k, rest.New(h.r.Vendor, srv.URL, nil), func(ctx context.Context, _ time.Duration) error { return nil })
	if _, err := sk.Create(context.Background(), map[string]any{"credit_quota": float64(99)}, []string{"TAKEN"}); err != nil {
		t.Fatal(err)
	}
	obj, reason, msg, stmts := h.reconcile()
	if reason != "SyncFailed" || !strings.Contains(msg, "already exists") || hasStmt(stmts, "ALTER") {
		t.Fatalf("reason %s %q, statements %v", reason, msg, stmts)
	}
	if id, _, _ := unstructured.NestedString(obj.Object, "status", "id"); id != "" {
		t.Fatalf("status.id %q: the object is not ours", id)
	}
	if err := h.c.Delete(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	_, _, _, stmts = h.reconcile()
	if len(stmts) != 0 {
		t.Fatalf("deleting sent %v; the monitor is someone else's", stmts)
	}
	if recs := h.api.SQLRecords("ResourceMonitor"); len(recs) != 1 || recs[0]["credit_quota"] != float64(99) {
		t.Fatalf("the other monitor changed: %v", recs)
	}
}

// A changed spec is altered field by field; a field the spec drops is
// unset; a field that has no ALTER is drift that stops; a new name is
// refused rather than making a second object.
func TestSQLUpdates(t *testing.T) {
	k := monitorKind()
	h := newSQLHarness(t, k, map[string]any{"name": "rm", "credit_quota": float64(5), "frequency": "monthly"})
	obj, reason, _, stmts := h.reconcile()
	if reason != "Created" || strings.Join(stmts, "\n") != `CREATE RESOURCE MONITOR "RM" WITH CREDIT_QUOTA = 5 FREQUENCY = MONTHLY`+"\n"+`SHOW RESOURCE MONITORS LIKE 'RM'` {
		t.Fatalf("create: %s %v", reason, stmts)
	}
	if id, _, _ := unstructured.NestedString(obj.Object, "status", "id"); id != `"RM"` {
		t.Fatalf("status.id %q", id)
	}
	if obj, reason, _, stmts = h.reconcile(); reason != "Unchanged" || len(stmts) != 1 {
		t.Fatalf("resync: %s %v (monthly reads back MONTHLY: not drift)", reason, stmts)
	}

	h.setSpec(obj, "credit_quota", float64(7))
	obj, reason, _, stmts = h.reconcile()
	if reason != "Updated" || !hasStmt(stmts, `ALTER RESOURCE MONITOR "RM" SET CREDIT_QUOTA = 7`) {
		t.Fatalf("update: %s %v", reason, stmts)
	}

	h.setSpec(obj, "credit_quota", nil)
	obj, reason, _, stmts = h.reconcile()
	if reason != "Updated" || !hasStmt(stmts, `ALTER RESOURCE MONITOR "RM" SET CREDIT_QUOTA = null`) {
		t.Fatalf("unset: %s %v", reason, stmts)
	}

	h.setSpec(obj, "frequency", "weekly")
	obj, reason, msg, stmts := h.reconcile()
	if reason != "SyncFailed" || !strings.Contains(msg, "spec.frequency") || hasStmt(stmts, "ALTER") || hasStmt(stmts, "CREATE") {
		t.Fatalf("create-only: %s %q %v", reason, msg, stmts)
	}
	h.setSpec(obj, "frequency", "MONTHLY")
	obj, _, _, _ = h.reconcile()
	h.setSpec(obj, "name", "other")
	obj, reason, msg, stmts = h.reconcile()
	if reason != "SyncFailed" || !strings.Contains(msg, "rename") || len(stmts) != 0 {
		t.Fatalf("rename: %s %q %v", reason, msg, stmts)
	}

	// Deleting drops the monitor it made, by the name it made it with.
	if err := h.c.Delete(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	if _, _, _, stmts = h.reconcile(); len(stmts) != 1 || stmts[0] != `DROP RESOURCE MONITOR IF EXISTS "RM"` {
		t.Fatalf("delete: %v", stmts)
	}
}

// A status.id edited to hold SQL is never sent.
func TestSQLStatusIDIsNotTrusted(t *testing.T) {
	k := monitorKind()
	h := newSQLHarness(t, k, map[string]any{"name": "rm"})
	obj, _, _, _ := h.reconcile()
	_ = unstructured.SetNestedField(obj.Object, `"RM"; DROP DATABASE PROD`, "status", "id")
	if err := h.c.Status().Update(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Delete(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	_, reason, msg, stmts := h.reconcile()
	for _, s := range stmts {
		if strings.Contains(s, "DROP DATABASE") {
			t.Fatalf("sent %q", s)
		}
	}
	if reason != "DeleteFailed" || !strings.Contains(msg, "not a quoted object name") {
		t.Fatalf("%s %q %v", reason, msg, stmts)
	}
}

func tokenKind() *descriptor.Kind {
	return &descriptor.Kind{
		Kind: "UserProgrammaticAccessToken", Group: "user.example.hfl.dev", Version: "v1alpha1", Plural: "userprogrammaticaccesstokens",
		Shape: descriptor.SQLShape, Read: descriptor.Op{Method: "POST", Path: "/api/v2/statements"},
		SecretOutputs: []string{"token"},
		SQL: &descriptor.SQL{
			Mode: descriptor.SQLObject, ID: []string{"user_name", "name"},
			Fields: map[string]descriptor.SQLField{"user_name": {Type: "identifier"}, "name": {Type: "identifier"},
				"comment": {Type: "string"}, "token": {Type: "string", Output: true}},
			Create:        "ALTER USER {user_name} ADD PROGRAMMATIC ACCESS TOKEN {name}[ COMMENT = {comment}]",
			CreateColumns: map[string]string{"token": "token_secret"},
			Read: []descriptor.SQLRead{{Statement: "SHOW USER PROGRAMMATIC ACCESS TOKENS FOR USER {user_name}",
				Match: map[string]string{"name": "{name}"}, Columns: map[string]string{"comment": "comment"}}},
			Alter:  map[string]string{"comment": "ALTER USER {user_name} MODIFY PROGRAMMATIC ACCESS TOKEN {name} SET COMMENT = {comment}"},
			Delete: "ALTER USER IF EXISTS {user_name} REMOVE PROGRAMMATIC ACCESS TOKEN {name}",
		},
	}
}

// A secret Snowflake answers once, to the create (a programmatic access
// token's), goes to the Secret spec.writeSecretRef names, never to the
// status; later reads, which lack it, leave the Secret as it is. An object
// named by two fields uses the name it was made with.
func TestSQLCreateSecret(t *testing.T) {
	h := newSQLHarness(t, tokenKind(), map[string]any{"user_name": "svc", "name": "ci", "comment": "c",
		"writeSecretRef": map[string]any{"name": "ci-token"}})
	obj, reason, msg, stmts := h.reconcile()
	if reason != "Created" {
		t.Fatalf("reason %s %q, statements %v", reason, msg, stmts)
	}
	if id, _, _ := unstructured.NestedString(obj.Object, "status", "id"); id != `"SVC"."CI"` {
		t.Errorf("status.id %q", id)
	}
	if _, has, _ := unstructured.NestedFieldNoCopy(obj.Object, "status", "atProvider", "token"); has {
		t.Error("the token is in the status")
	}
	sec := &corev1.Secret{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "ci-token"}, sec); err != nil {
		t.Fatal(err)
	}
	token := string(sec.Data["token"])
	if token == "" || sec.OwnerReferences[0].Name != "item" {
		t.Fatalf("Secret %v", sec)
	}
	h.setSpec(obj, "comment", "d")
	if _, reason, msg, stmts = h.reconcile(); reason != "Updated" || !hasStmt(stmts, `ALTER USER "SVC" MODIFY PROGRAMMATIC ACCESS TOKEN "CI" SET COMMENT = 'd'`) {
		t.Fatalf("update: %s %q %v", reason, msg, stmts)
	}
	_ = h.c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "ci-token"}, sec)
	if string(sec.Data["token"]) != token {
		t.Error("a read without the token changed the Secret")
	}
}

// A list Snowflake has no SET for is changed by adding the elements the
// spec has and removing those it does not; a read gives a row per element.
func TestSQLListAddRemove(t *testing.T) {
	k := &descriptor.Kind{
		Kind: "FailoverGroup", Group: "failover.example.hfl.dev", Version: "v1alpha1", Plural: "failovergroups",
		Shape: descriptor.SQLShape, Read: descriptor.Op{Method: "POST", Path: "/api/v2/statements"},
		SQL: &descriptor.SQL{
			Mode: descriptor.SQLObject, ID: []string{"name"},
			Fields: map[string]descriptor.SQLField{"name": {Type: "identifier"}, "allowed_databases": {Type: "identifiers"}},
			Create: "CREATE FAILOVER GROUP {@id}",
			Read: []descriptor.SQLRead{{Statement: "SHOW FAILOVER GROUPS", Match: map[string]string{"name": "{name}"}},
				{Statement: "SHOW DATABASES IN FAILOVER GROUP {@id}", ListColumns: map[string]string{"allowed_databases": "name"}}},
			Add:    map[string]string{"allowed_databases": "ALTER FAILOVER GROUP {@id} ADD {allowed_databases} TO ALLOWED_DATABASES"},
			Remove: map[string]string{"allowed_databases": "ALTER FAILOVER GROUP {@id} REMOVE {allowed_databases} FROM ALLOWED_DATABASES"},
			Delete: "DROP FAILOVER GROUP IF EXISTS {@id}",
		},
	}
	if _, err := descriptor.Parse(mustJSON(t, map[string]any{"vendor": "x", "connection": map[string]any{"group": "core.example.hfl.dev", "version": "v1alpha1", "kind": "Connection"}, "kinds": []any{k}})); err != nil {
		t.Fatal(err)
	}
	h := newSQLHarness(t, k, map[string]any{"name": "fg", "allowed_databases": []any{"a"}})
	obj, reason, msg, stmts := h.reconcile()
	if reason != "Created" || !hasStmt(stmts, `ALTER FAILOVER GROUP "FG" ADD "A" TO ALLOWED_DATABASES`) {
		t.Fatalf("create: %s %q %v", reason, msg, stmts)
	}
	if obj, reason, _, stmts = h.reconcile(); reason != "Unchanged" {
		t.Fatalf("resync: %s %v", reason, stmts)
	}
	h.setSpec(obj, "allowed_databases", []any{"b", "a", "c"})
	if obj, reason, msg, stmts = h.reconcile(); reason != "Updated" || !hasStmt(stmts, `ALTER FAILOVER GROUP "FG" ADD "B", "C" TO ALLOWED_DATABASES`) || hasStmt(stmts, "ALTER FAILOVER GROUP \"FG\" REMOVE") {
		t.Fatalf("add: %s %q %v", reason, msg, stmts)
	}
	h.setSpec(obj, "allowed_databases", []any{"c"})
	if obj, reason, msg, stmts = h.reconcile(); reason != "Updated" || !hasStmt(stmts, `ALTER FAILOVER GROUP "FG" REMOVE "A", "B" FROM ALLOWED_DATABASES`) {
		t.Fatalf("remove: %s %q %v", reason, msg, stmts)
	}
	h.setSpec(obj, "allowed_databases", nil)
	if _, reason, msg, stmts = h.reconcile(); reason != "Updated" || !hasStmt(stmts, `ALTER FAILOVER GROUP "FG" REMOVE "C" FROM ALLOWED_DATABASES`) {
		t.Fatalf("dropped from the spec: %s %q %v", reason, msg, stmts)
	}
	if recs := h.api.SQLRecords("FailoverGroup"); len(recs) != 1 || len(recs[0]["allowed_databases"].([]any)) != 0 {
		t.Fatalf("records %v", recs)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
