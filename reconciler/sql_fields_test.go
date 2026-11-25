// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	apifake "github.com/highfivelabs/krm-rest-runtime/fake"
)

// accountKind: the account's settings, each set by an ALTER ACCOUNT of its
// own (Terraform's snowflake_current_account).
func accountKind() *descriptor.Kind {
	return &descriptor.Kind{
		Kind: "CurrentAccount", Group: "current.example.hfl.dev", Version: "v1alpha1", Plural: "currentaccounts",
		Shape: descriptor.SQLShape, Read: descriptor.Op{Method: "POST", Path: "/api/v2/statements"},
		SQL: &descriptor.SQL{
			Mode: descriptor.SQLSet,
			Fields: map[string]descriptor.SQLField{"timezone": {Type: "string"}, "statement_timeout_in_seconds": {Type: "integer"},
				"password_policy": {Type: "name"}, "resource_monitor": {Type: "identifier"}},
			Read: []descriptor.SQLRead{
				{Statement: "SHOW PARAMETERS IN ACCOUNT", Properties: &descriptor.SQLProperties{Name: "key", Value: "value"},
					Columns: map[string]string{"timezone": "TIMEZONE", "statement_timeout_in_seconds": "STATEMENT_TIMEOUT_IN_SECONDS"}},
				{Statement: "SELECT * FROM TABLE (SNOWFLAKE.INFORMATION_SCHEMA.POLICY_REFERENCES (REF_ENTITY_NAME => CURRENT_ACCOUNT(), REF_ENTITY_DOMAIN => 'ACCOUNT'))",
					Properties: &descriptor.SQLProperties{Name: "POLICY_KIND", Value: "POLICY_DB.POLICY_SCHEMA.POLICY_NAME"},
					Columns:    map[string]string{"password_policy": "PASSWORD_POLICY"}},
				{Statement: "SHOW RESOURCE MONITORS", Properties: &descriptor.SQLProperties{Name: "level", Value: "name"},
					Columns: map[string]string{"resource_monitor": "ACCOUNT"}},
			},
			Alter: map[string]string{
				"timezone":                     "ALTER ACCOUNT SET TIMEZONE = {timezone}",
				"statement_timeout_in_seconds": "ALTER ACCOUNT SET STATEMENT_TIMEOUT_IN_SECONDS = {statement_timeout_in_seconds}",
				"password_policy":              "ALTER ACCOUNT SET PASSWORD POLICY {password_policy}",
				"resource_monitor":             "ALTER ACCOUNT SET RESOURCE_MONITOR = {resource_monitor}",
			},
			Unset: map[string]string{
				"timezone":                     "ALTER ACCOUNT UNSET TIMEZONE",
				"statement_timeout_in_seconds": "ALTER ACCOUNT UNSET STATEMENT_TIMEOUT_IN_SECONDS",
				"password_policy":              "ALTER ACCOUNT UNSET PASSWORD POLICY",
				"resource_monitor":             "ALTER ACCOUNT UNSET RESOURCE_MONITOR",
			},
			UnsetFirst: []string{"password_policy"},
		},
	}
}

func parseKind(t *testing.T, k *descriptor.Kind) error {
	t.Helper()
	_, err := descriptor.Parse(mustJSON(t, map[string]any{"vendor": "x", "connection": map[string]any{"group": "core.example.hfl.dev", "version": "v1alpha1", "kind": "Connection"}, "kinds": []any{k}}))
	return err
}

func checkKind(t *testing.T, k *descriptor.Kind) {
	t.Helper()
	if err := parseKind(t, k); err != nil {
		t.Fatal(err)
	}
}

// Settings set field by field: a statement per field that differs, none for
// one that does not; a policy Snowflake will not replace is unset first; a
// field the spec drops is unset, one Snowflake has not set is left; deleting
// unsets what was applied, and only that.
func TestSQLSettingsFieldByField(t *testing.T) {
	k := accountKind()
	checkKind(t, k)
	h := newSQLHarness(t, k, map[string]any{"timezone": "Europe/London", "password_policy": "db.sc.p1"})
	obj, reason, msg, stmts := h.reconcile()
	if reason != "Created" || !hasStmt(stmts, `ALTER ACCOUNT SET TIMEZONE = 'Europe/London'`) || !hasStmt(stmts, `ALTER ACCOUNT SET PASSWORD POLICY "DB"."SC"."P1"`) || hasStmt(stmts, "ALTER ACCOUNT UNSET") {
		t.Fatalf("create: %s %q %v", reason, msg, stmts)
	}
	if l, _, _ := unstructured.NestedStringSlice(obj.Object, "status", AppliedFields); strings.Join(l, ",") != "password_policy,timezone" {
		t.Fatalf("appliedFields %v", l)
	}
	if obj, reason, _, stmts = h.reconcile(); reason != "Unchanged" || hasStmt(stmts, "ALTER") {
		t.Fatalf("resync: %s %v", reason, stmts)
	}

	// One field changes: one statement; the policy is unset first.
	h.setSpec(obj, "password_policy", "db.sc.p2")
	obj, reason, msg, stmts = h.reconcile()
	var alters []string
	for _, s := range stmts {
		if strings.HasPrefix(s, "ALTER") {
			alters = append(alters, s)
		}
	}
	if reason != "Updated" || strings.Join(alters, "\n") != "ALTER ACCOUNT UNSET PASSWORD POLICY\n"+`ALTER ACCOUNT SET PASSWORD POLICY "DB"."SC"."P2"` {
		t.Fatalf("update: %s %q %v", reason, msg, stmts)
	}

	// Changed in Snowflake: put back.
	h.api.SQLRecords("CurrentAccount")[0]["timezone"] = "UTC"
	if obj, reason, _, stmts = h.reconcile(); reason != "Updated" || !hasStmt(stmts, `ALTER ACCOUNT SET TIMEZONE = 'Europe/London'`) || hasStmt(stmts, "ALTER ACCOUNT SET PASSWORD") {
		t.Fatalf("drift: %s %v", reason, stmts)
	}

	// Dropped from the spec: unset; one Snowflake no longer has is left.
	h.setSpec(obj, "timezone", nil)
	delete(h.api.SQLRecords("CurrentAccount")[0], "password_policy")
	h.setSpec(obj, "password_policy", nil)
	obj, reason, msg, stmts = h.reconcile()
	if reason != "Updated" || !hasStmt(stmts, "ALTER ACCOUNT UNSET TIMEZONE") || hasStmt(stmts, "ALTER ACCOUNT UNSET PASSWORD POLICY") {
		t.Fatalf("unset: %s %q %v", reason, msg, stmts)
	}

	h.setSpec(obj, "statement_timeout_in_seconds", float64(60))
	obj, reason, msg, stmts = h.reconcile()
	if reason != "Updated" || !hasStmt(stmts, "ALTER ACCOUNT SET STATEMENT_TIMEOUT_IN_SECONDS = 60") || hasStmt(stmts, "UNSET") {
		t.Fatalf("set: %s %q %v", reason, msg, stmts)
	}
	if err := h.c.Delete(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	_, _, _, stmts = h.reconcile()
	var unsets []string
	for _, s := range stmts {
		if strings.HasPrefix(s, "ALTER") {
			unsets = append(unsets, s)
		}
	}
	if strings.Join(unsets, "\n") != "ALTER ACCOUNT UNSET STATEMENT_TIMEOUT_IN_SECONDS" {
		t.Fatalf("delete: %v", stmts)
	}
	if recs := h.api.SQLRecords("CurrentAccount"); len(recs) != 1 || len(recs[0]) != 0 {
		t.Fatalf("the account: %v", recs)
	}
}

// Settings on something named (the organization account): a read that
// selects by the name and finds nothing is an error, and nothing is set.
func TestSQLSettingsOnSomethingNamed(t *testing.T) {
	k := &descriptor.Kind{
		Kind: "CurrentOrganizationAccount", Group: "current.example.hfl.dev", Version: "v1alpha1", Plural: "currentorganizationaccounts",
		Shape: descriptor.SQLShape, Read: descriptor.Op{Method: "POST", Path: "/api/v2/statements"},
		Sample: map[string]any{"name": "org_acct", "comment": "c"},
		SQL: &descriptor.SQL{
			Mode:   descriptor.SQLSet,
			Fields: map[string]descriptor.SQLField{"name": {Type: "identifier"}, "comment": {Type: "string"}},
			Read: []descriptor.SQLRead{{Statement: "SHOW ORGANIZATION ACCOUNTS",
				Match: map[string]string{"account_name": "{name}", "is_organization_account": "true"}, Columns: map[string]string{"comment": "comment"}}},
			Alter: map[string]string{"comment": "ALTER ORGANIZATION ACCOUNT SET COMMENT = {comment}"},
			Unset: map[string]string{"comment": "ALTER ORGANIZATION ACCOUNT UNSET COMMENT"},
		},
	}
	checkKind(t, k)
	h := newSQLHarness(t, k, map[string]any{"name": "other", "comment": "c"})
	obj, reason, msg, stmts := h.reconcile()
	if reason != "SyncFailed" || !strings.Contains(msg, "exists already") || hasStmt(stmts, "ALTER") {
		t.Fatalf("another name: %s %q %v", reason, msg, stmts)
	}
	h.setSpec(obj, "name", "ORG_ACCT")
	if _, reason, msg, stmts = h.reconcile(); reason != "Created" || !hasStmt(stmts, "ALTER ORGANIZATION ACCOUNT SET COMMENT = 'c'") {
		t.Fatalf("its name: %s %q %v", reason, msg, stmts)
	}
	if _, reason, _, stmts = h.reconcile(); reason != "Unchanged" {
		t.Fatalf("resync: %s %v", reason, stmts)
	}
}

// A set kind set field by field takes no create or delete, and alters
// fields; unsetFirst is for its fields that have an unset and are read.
func TestSQLSettingsFieldByFieldChecks(t *testing.T) {
	k := accountKind()
	k.SQL.Delete = "ALTER ACCOUNT UNSET TIMEZONE"
	if err := parseKind(t, k); err == nil || !strings.Contains(err.Error(), "delete is not taken") {
		t.Fatalf("a delete: %v", err)
	}
	k = accountKind()
	k.SQL.UnsetFirst = []string{"nope"}
	if err := parseKind(t, k); err == nil || !strings.Contains(err.Error(), "unsetFirst") {
		t.Fatalf("unsetFirst: %v", err)
	}
	k = accountKind()
	k.SQL.Alter = nil
	k.SQL.UnsetFirst = nil
	if err := parseKind(t, k); err == nil {
		t.Fatal("no alter: no error")
	}
}

// A statement the account's settings share with a kind of one setting (the
// attachment's ALTER ACCOUNT SET PASSWORD POLICY) sets both on the fake, as
// in Snowflake they are one setting; the attachment still makes its own.
func TestSQLSettingsShareAStatement(t *testing.T) {
	acct := accountKind()
	att := &descriptor.Kind{
		Kind: "AccountPasswordPolicyAttachment", Group: "account.example.hfl.dev", Version: "v1alpha1", Plural: "accountpasswordpolicyattachments",
		Shape: descriptor.SQLShape, Read: descriptor.Op{Method: "POST", Path: "/api/v2/statements"},
		SQL: &descriptor.SQL{
			Mode: descriptor.SQLSet, Fields: map[string]descriptor.SQLField{"password_policy": {Type: "name"}},
			Create: "ALTER ACCOUNT SET PASSWORD POLICY {password_policy}",
			Read: []descriptor.SQLRead{{Statement: "SHOW PASSWORD POLICIES ON ACCOUNT",
				Columns: map[string]string{"password_policy": "database_name.schema_name.name"}}},
			Delete: "ALTER ACCOUNT UNSET PASSWORD POLICY",
		},
	}
	h := newSQLHarness(t, att, map[string]any{"password_policy": "db.sc.p"})
	h.api = apifake.New(&descriptor.File{Kinds: []descriptor.Kind{*att, *acct}})
	obj, reason, msg, stmts := h.reconcile()
	if reason != "Created" {
		t.Fatalf("attach: %s %q %v", reason, msg, stmts)
	}
	if a, c := h.api.SQLRecords("AccountPasswordPolicyAttachment"), h.api.SQLRecords("CurrentAccount"); len(a) != 1 || len(c) != 1 || c[0]["password_policy"] == nil {
		t.Fatalf("attachment %v, account %v", a, c)
	}
	if err := h.c.Delete(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	if a, c := h.api.SQLRecords("AccountPasswordPolicyAttachment"), h.api.SQLRecords("CurrentAccount"); len(a) != 0 || len(c) != 1 || c[0]["password_policy"] != nil {
		t.Fatalf("detached: attachment %v, account %v", a, c)
	}
}
