// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/rest"
	"github.com/highfivelabs/krm-rest-runtime/sqlapi"
)

// Status fields an sql object keeps beside appliedFields.
const (
	// CreatedWith: a hash of each field the object was created with that
	// Snowflake neither reads back nor changes (a view's query, a policy's
	// signature): a spec that changes one is reported, not ignored.
	CreatedWith = "createdWith"
	// Settling: the wait for an asynchronous object's status (SQL.Settle):
	// since when, the Ready reason once it settles, the spec it is for and
	// the write-only fields already written for it.
	Settling = "settling"
	// SettlingReason is the Ready=False reason while it waits.
	SettlingReason = "Settling"
)

const sqlCreatedWithKey = "@createdWith"

// SettleRetry is how soon an object waiting for its status looks again.
var SettleRetry = 15 * time.Second

// sqlStatus is what an sql object's status says of its last sync.
type sqlStatus struct {
	applied     []string // status.appliedFields
	hash        string   // the spec's hash now
	appliedHash string   // status.appliedHash: the spec last applied in full
	createdWith map[string]string
	settling    *settleState
}

type settleState struct {
	Since   string   `json:"since"`
	Action  string   `json:"action"`
	Hash    string   `json:"hash,omitempty"`
	Written []string `json:"written,omitempty"`
}

func sqlStatusOf(obj *unstructured.Unstructured, hash string) sqlStatus {
	st := sqlStatus{applied: appliedFields(obj), hash: hash}
	st.appliedHash, _, _ = unstructured.NestedString(obj.Object, "status", "appliedHash")
	if m, ok, _ := unstructured.NestedStringMap(obj.Object, "status", CreatedWith); ok {
		st.createdWith = m
	}
	if m, ok, _ := unstructured.NestedMap(obj.Object, "status", Settling); ok {
		b, _ := json.Marshal(m)
		var s settleState
		if json.Unmarshal(b, &s) == nil {
			st.settling = &s
		}
	}
	return st
}

// settlingErr: the object is not done yet. The reconciler waits for it by
// requeueing, never by blocking.
type settlingErr struct {
	action  string         // the Ready reason once it settles
	msg     string         // what it waits for
	obs     map[string]any // what the last read answered
	written []string       // write-only fields written for this spec
	delete  bool
}

func (e *settlingErr) Error() string { return e.msg }

// fieldHash fingerprints one field's value.
func fieldHash(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// createOnly: a field Snowflake takes only when it makes the object (its
// CREATE writes it) and never reads back: a change to it would otherwise
// go unseen. Not one only the delete names (whether a DROP cascades).
func createOnly(q *descriptor.SQL, f string) bool {
	fd, ok := q.Fields[f]
	if !ok || fd.Output || f == q.Warehouse || q.Readable(f) || q.Settable(f) {
		return false
	}
	for _, idf := range q.ID {
		if idf == f {
			return false
		}
	}
	t, err := descriptor.ParseSQLTemplate(q.Create)
	if err != nil {
		return false
	}
	for _, p := range t.Fields() {
		if p == f || strings.HasPrefix(p, f+".") {
			return true
		}
	}
	return false
}

// createdWithOf hashes the spec's create-only fields.
func createdWithOf(q *descriptor.SQL, body map[string]any) map[string]string {
	out := map[string]string{}
	for f, v := range body {
		if createOnly(q, f) {
			out[f] = fieldHash(v)
		}
	}
	return out
}

func setCreatedWith(params map[string]string, cw map[string]string) {
	b, _ := json.Marshal(cw)
	params[sqlCreatedWithKey] = string(b)
}

// createdWithParam is the createdWith status a sync left in params.
func createdWithParam(params map[string]string) map[string]any {
	s := params[sqlCreatedWithKey]
	if s == "" {
		return nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(s), &m) != nil || len(m) == 0 {
		return nil
	}
	return m
}

// checkCreatedWith: the spec's create-only fields are what the object was
// created with. One the status has no hash of yet (an object made before
// the hashes were kept) is taken as it is now.
func checkCreatedWith(q *descriptor.SQL, params map[string]string, body map[string]any, was map[string]string) error {
	now := createdWithOf(q, body)
	keep := map[string]string{}
	for f, h := range was {
		if _, still := q.Fields[f]; still && createOnly(q, f) {
			keep[f] = h
		}
	}
	for _, f := range sortedKeys(keep) {
		if now[f] != keep[f] {
			setCreatedWith(params, keep)
			if _, set := body[f]; !set {
				return fmt.Errorf("spec.%s was dropped, and Snowflake can only set it when the object is created; delete and recreate the object, or put it back", f)
			}
			return fmt.Errorf("spec.%s changed, and Snowflake can only set it when the object is created; delete and recreate the object, or put it back", f)
		}
	}
	for f, h := range now {
		keep[f] = h
	}
	setCreatedWith(params, keep)
	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// settleStatus is the status the object's read answered.
func settleStatus(q *descriptor.SQL, obs map[string]any) string {
	if v, ok := obs[q.Settle.Field]; ok && v != nil {
		return descriptor.Text(v)
	}
	return ""
}

// syncSQLSettle is syncSQL for an object whose statements are
// asynchronous (SQL.Settle): Snowflake refuses a statement while the work
// of the last is in flight, so each is sent only once the status says it
// is done, and the reconciler requeues while it is not. The fields a
// change writes are sent one statement at a time, each read back.
func (r *Reconciler) syncSQLSettle(ctx context.Context, sk *sqlapi.Kind, params map[string]string, body map[string]any, st sqlStatus) (map[string]any, string, error) {
	q := r.Kind.SQL
	set := q.Settle
	id, err := sqlapi.IDOf(q, body)
	if err != nil {
		return nil, "", err
	}
	was := params[sqlIDKey]
	if was != "" && was != sqlapi.FormatID(id) {
		return nil, "", fmt.Errorf("the object is %s and the spec now names %s: Snowflake would make a second object; delete and recreate the Kubernetes object to rename it", was, sqlapi.FormatID(id))
	}
	// A wait for this spec goes on where it was: what it wrote already, and
	// the reason to report once it settles.
	action := ""
	written := map[string]bool{}
	if s := st.settling; s != nil {
		if s.Action == "created" || s.Hash == st.hash {
			action = s.Action
		}
		if s.Hash == st.hash {
			for _, f := range s.Written {
				written[f] = true
			}
		}
	}
	// Fields dropped from the spec are unset; until they are, the status
	// keeps them among the fields applied.
	pendingApplied := map[string]bool{}
	for _, f := range st.applied {
		pendingApplied[f] = true
	}
	for f := range body {
		pendingApplied[f] = true
	}
	finish := func(done bool) {
		fields := sortedFields(body)
		if !done {
			fields = fields[:0]
			for f := range pendingApplied {
				fields = append(fields, f)
			}
			sort.Strings(fields)
		}
		params[sqlAppliedKey] = strings.Join(fields, ",")
	}
	wait := func(obs map[string]any, msg string) (map[string]any, string, error) {
		finish(false)
		if action == "" {
			action = "unchanged" // nothing sent yet: it only waits
		}
		var wl []string
		for f := range written {
			wl = append(wl, f)
		}
		sort.Strings(wl)
		return nil, "", &settlingErr{action: action, msg: msg, obs: obs, written: wl}
	}

	var obs map[string]any
	if was != "" {
		obs, err = sk.Read(ctx, body, id)
		switch {
		case errors.Is(err, rest.ErrNotFound):
			was = "" // dropped outside Kubernetes: make it again
		case err != nil:
			return nil, "", fmt.Errorf("read: %w", err)
		default:
			if err := checkCreatedWith(q, params, body, st.createdWith); err != nil {
				return obs, "drifted", err
			}
		}
	}
	if was == "" {
		// CREATE refuses a name that is taken: an object this one did not
		// make is never taken over.
		if _, err := sk.Create(ctx, body, id); err != nil {
			return nil, "", fmt.Errorf("create: %w", err)
		}
		params[sqlIDKey] = sqlapi.FormatID(id)
		setCreatedWith(params, createdWithOf(q, body))
		action = "created"
		written = map[string]bool{}
		t, _ := descriptor.ParseSQLTemplate(q.Create)
		for _, f := range t.Fields() {
			if _, ok := body[f]; ok && !q.Readable(f) {
				written[f] = true
			}
		}
		if obs, err = sk.Read(ctx, body, id); err != nil {
			return nil, "", fmt.Errorf("read after create: %w", err)
		}
	}

	unset := map[string]bool{}
	for {
		status := settleStatus(q, obs)
		switch {
		case descriptor.SettleIn(set.Failed, status):
			finish(false)
			return obs, "failed", fmt.Errorf("Snowflake reports status %s; delete and recreate the object", status)
		case descriptor.SettleIn(set.Busy, status):
			return wait(obs, fmt.Sprintf("Snowflake reports status %s; waiting for %s", status, strings.Join(set.Ready, " or ")))
		}
		// The next statement: a field that differs (or, never read back,
		// was not written for this spec yet), else one the spec dropped.
		norm := sqlapi.Normalize(q, body, obs)
		next, drop := "", false
		for _, f := range sortedFields(body) {
			if !q.Readable(f) {
				if st.hash != st.appliedHash && !written[f] && q.Settable(f) {
					next = f
					break
				}
				continue
			}
			if Drift(map[string]any{f: body[f]}, norm, nil) == "" {
				continue
			}
			if !q.Settable(f) {
				finish(false)
				return obs, "drifted", fmt.Errorf("spec.%s differs from Snowflake, and it can only be set when the object is created; delete and recreate the object", f)
			}
			next = f
			break
		}
		if next == "" {
			for _, f := range st.applied {
				if _, still := body[f]; !still && !unset[f] && (q.Unset[f] != "" || q.Remove[f] != "") {
					next, drop = f, true
					break
				}
			}
		}
		if next == "" {
			if !descriptor.SettleIn(set.Ready, status) {
				return wait(obs, fmt.Sprintf("Snowflake reports status %s; waiting for %s", status, strings.Join(set.Ready, " or ")))
			}
			finish(true)
			if action == "" {
				action = "unchanged"
			}
			return obs, action, nil
		}
		var err error
		switch {
		case drop && q.Unset[next] == "":
			have, _ := norm[next].([]any)
			err = sk.Delta(ctx, next, body, id, nil, have)
		case drop:
			err = sk.Alter(ctx, next, body, id, true)
		case q.Alter[next] == "": // a list changed by adding and removing elements
			want, _ := body[next].([]any)
			have, _ := norm[next].([]any)
			err = sk.Delta(ctx, next, body, id, want, have)
		default:
			err = sk.Alter(ctx, next, body, id, false)
		}
		if err != nil {
			finish(false)
			if drop {
				return nil, "", fmt.Errorf("unset %s: %w", next, err)
			}
			return nil, "", fmt.Errorf("update %s: %w", next, err)
		}
		if drop {
			unset[next] = true
			delete(pendingApplied, next)
		} else if !q.Readable(next) {
			written[next] = true
		}
		if action != "created" {
			action = "updated"
		}
		if obs, err = sk.Read(ctx, body, id); err != nil {
			finish(false)
			return nil, "", fmt.Errorf("read after update: %w", err)
		}
	}
}

// deleteSQLSettle tears an asynchronous object down: in a status its DROP
// takes (Gone) it is dropped; else the teardown step for its status runs
// (TERMINATE), or it is busy, and the reconciler waits.
func (r *Reconciler) deleteSQLSettle(ctx context.Context, sk *sqlapi.Kind, spec map[string]any, id []string) error {
	set := r.Kind.SQL.Settle
	ran := map[int]bool{}
	for {
		obs, err := sk.Read(ctx, spec, id)
		if errors.Is(err, rest.ErrNotFound) {
			return nil // gone already
		}
		if err != nil {
			return err
		}
		status := settleStatus(r.Kind.SQL, obs)
		wait := func(msg string) error {
			return &settlingErr{action: "deleting", msg: msg, obs: obs, delete: true}
		}
		busy := descriptor.SettleIn(set.Busy, status)
		if descriptor.SettleIn(set.Gone, status) || (len(set.Gone) == 0 && !busy) {
			if err := sk.Delete(ctx, spec, id); err != nil && !errors.Is(err, rest.ErrNotFound) {
				return err
			}
			return nil
		}
		if busy {
			return wait(fmt.Sprintf("Snowflake reports status %s; waiting for it to settle before it is torn down", status))
		}
		step := -1
		for i, s := range set.Teardown {
			if descriptor.SettleIn(s.When, status) || (len(s.When) == 0 && step < 0) {
				step = i
				if len(s.When) > 0 {
					break
				}
			}
		}
		if step < 0 || set.Teardown[step].Run == "" || ran[step] {
			return wait(fmt.Sprintf("Snowflake reports status %s; waiting for %s to drop it", status, strings.Join(set.Gone, " or ")))
		}
		if _, err := sk.Run(ctx, set.Teardown[step].Run, spec, id); err != nil && !errors.Is(err, rest.ErrNotFound) {
			return fmt.Errorf("teardown in status %s: %w", status, err)
		}
		ran[step] = true
	}
}

// settling reports an object that waits for its status, and looks again
// soon. A wait longer than the settle's timeout is an error, retried with
// backoff.
func (r *Reconciler) settling(ctx context.Context, obj *unstructured.Unstructured, params map[string]string, s *settlingErr, hash string) (ctrl.Result, error) {
	status, _, _ := unstructured.NestedMap(obj.Object, "status")
	if status == nil {
		status = map[string]any{}
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	since := now
	if old := sqlStatusOf(obj, hash).settling; old != nil && (old.Action == "deleting") == s.delete {
		if t, err := time.Parse(time.RFC3339, old.Since); err == nil {
			since = t
		}
	}
	if !s.delete {
		if params[sqlIDKey] != "" {
			status["id"] = params[sqlIDKey]
		}
		if v, ok := params[sqlAppliedKey]; ok {
			applied := []any{}
			for _, f := range strings.Split(v, ",") {
				if f != "" {
					applied = append(applied, f)
				}
			}
			status[AppliedFields] = applied
		}
		if cw := createdWithParam(params); cw != nil {
			status[CreatedWith] = cw
		}
	}
	if s.obs != nil {
		obs := map[string]any{}
		for k, v := range s.obs {
			if !r.Kind.IsWriteOnly(k) {
				obs[k] = v
			}
		}
		status["atProvider"] = descriptor.WithoutFields(obs, r.Kind.SecretOutputs)
	}
	written := make([]any, 0, len(s.written))
	for _, f := range s.written {
		written = append(written, f)
	}
	status[Settling] = map[string]any{"since": since.Format(time.RFC3339), "action": s.action, "hash": hash, "written": written}
	status["observedGeneration"] = obj.GetGeneration()
	timeout := r.Kind.SQL.Settle.TimeoutOf()
	if now.Sub(since) > timeout {
		reason := "SyncFailed"
		if s.delete {
			reason = "DeleteFailed"
		}
		cause := fmt.Errorf("%s, for longer than %s", s.msg, timeout)
		status["conditions"] = []any{condition("Ready", metav1.ConditionFalse, reason, cause.Error())}
		if err := r.writeStatus(ctx, obj, status); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, cause
	}
	status["conditions"] = []any{condition("Ready", metav1.ConditionFalse, SettlingReason, s.msg)}
	if err := r.writeStatus(ctx, obj, status); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: SettleRetry}, nil
}
