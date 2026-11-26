// SPDX-License-Identifier: Apache-2.0

// Package reconciler is the one controller every generated kind runs on. It
// works on unstructured objects, so a vendor repository ships no Go types:
// types.yaml says how each kind maps onto the API, and the CRD says what its
// spec may hold.
//
// Spec fields keep the vendor API's own names, so the vendor's API reference
// applies field for field. Path parameters are top-level spec fields too.
// spec.secretRefs fills body fields from Secrets: {"<field>": {name, key}};
// spec.fieldRefs fills spec fields from other objects (refs.go).
//
// The object is namespaced; it reaches the vendor through the Connection
// named in spec.connectionRef.name (default "default") in its namespace.
package reconciler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// Resync is how often a Ready object is read again to catch drift.
var Resync = 10 * time.Minute

// NotFoundRecheck is the total time the reconciler polls with backoff to
// read a CRUD object again when a read finds nothing, before treating it as
// deleted outside Kubernetes. APIs that answer reads from a replica can
// answer 404 for several seconds right after a write (HFL-619, HFL-653);
// 0 turns the recheck off.
var NotFoundRecheck = 8 * time.Second

// Hook changes a request body before it is sent (GitHub's secrets are
// encrypted with the repository's public key, for example).
type Hook func(ctx context.Context, c *rest.Client, k *descriptor.Kind, params map[string]string, body map[string]any) error

// Reconciler reconciles every object of one kind.
type Reconciler struct {
	Client     client.Client
	Kind       *descriptor.Kind
	Vendor     *rest.Vendor
	Connection schema.GroupVersionKind
	Hooks      []Hook
	// Kinds maps the vendor's kind names to their GVKs, for spec.fieldRefs
	// that name a kind without its apiVersion (KindsByName).
	Kinds map[string]schema.GroupVersionKind
	// NewClient lets tests point the reconciler at a fake API.
	NewClient func(v *rest.Vendor, baseURL string, creds map[string]string) *rest.Client
	// Wait replaces the wait between NotFoundRecheck's reads (nil: wait d).
	// A replay of recorded calls does not wait: the recording already has
	// the answers the waits were for.
	Wait func(ctx context.Context, d time.Duration) error
	// Now replaces the clock a wait for an object's status is timed by
	// (nil: time.Now).
	Now func() time.Time
}

func (r *Reconciler) finalizer() string { return r.Kind.Group + "/finalizer" }

// Annotations an object may carry, the same for every kind. They let an
// object take over a resource that exists already, as a Terraform
// offramp does, without making or deleting anything.
const (
	// AnnAdoptID names the existing resource a CRUD kind's object stands
	// for, by the ID status.id would hold (the item path's ID parameter).
	// Read while status.id is empty: the first reconcile reads the resource
	// by it, rather than creating one, and records it in status.id. A kind
	// whose path is all spec fields (an upsert, a settings kind) needs
	// none: it is read before it is written.
	AnnAdoptID = "krm.hfl.dev/adopt-id"
	// AnnDeletionPolicy "abandon" leaves the resource when the object is
	// deleted: the finalizer goes, no delete is sent. Anything else, or
	// none, deletes it (the default).
	AnnDeletionPolicy = "krm.hfl.dev/deletion-policy"
)

// SetupWithManager registers the controller.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Kind.IsDraft() {
		return errors.New(r.Kind.DraftReason())
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(r.Kind.GVK())
	return ctrl.NewControllerManagedBy(mgr).For(u).Named(strings.ToLower(r.Kind.Kind + "." + r.Kind.Group)).Complete(r)
}

// Reconcile drives one object toward its spec.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.Kind.IsDraft() {
		// Its calls are guesses: nothing is sent, ever.
		return ctrl.Result{}, reconcile.TerminalError(errors.New(r.Kind.DraftReason()))
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(r.Kind.GVK())
	if err := r.Client.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
	if spec == nil {
		spec = map[string]any{}
	}
	deleting := !obj.GetDeletionTimestamp().IsZero()
	resolved, err := r.resolveRefs(ctx, obj, spec, deleting)
	if err != nil {
		var w *waitErr
		if !errors.As(err, &w) {
			return r.fail(ctx, obj, "InvalidSpec", err, nil)
		}
		if deleting && neverSynced(obj) {
			// Nothing was made, so nothing is deleted.
			if controllerutil.RemoveFinalizer(obj, r.finalizer()) {
				return ctrl.Result{}, r.Client.Update(ctx, obj)
			}
			return ctrl.Result{}, nil
		}
		return r.waitForRef(ctx, obj, w)
	}
	api, err := r.apiClient(ctx, obj, spec)
	if err != nil {
		return r.fail(ctx, obj, "ConnectionError", err, nil)
	}
	params, err := r.params(obj, spec)
	if err != nil {
		return r.fail(ctx, obj, "InvalidSpec", err, nil)
	}

	if deleting {
		if !controllerutil.ContainsFinalizer(obj, r.finalizer()) {
			return ctrl.Result{}, nil
		}
		if obj.GetAnnotations()[AnnDeletionPolicy] == "abandon" {
			controllerutil.RemoveFinalizer(obj, r.finalizer())
			return ctrl.Result{}, r.Client.Update(ctx, obj)
		}
		specBody := func() (map[string]any, error) { return r.body(ctx, obj, spec) }
		del := r.delete
		if r.Kind.Shape == descriptor.SetShape {
			del = func(ctx context.Context, api *rest.Client, params map[string]string, prev map[string]any, spec func() (map[string]any, error)) error {
				if neverSynced(obj) {
					return nil // nothing was added
				}
				if r.Kind.Set.Holder != nil {
					return DeleteHolder(ctx, api, r.Kind, params) // the elements go with it
				}
				if _, ok := prev[r.Kind.Set.Field]; ok {
					return DeleteSet(ctx, api, r.Kind, params, prev) // the elements last applied
				}
				body, err := spec()
				if err != nil {
					return err
				}
				return DeleteSet(ctx, api, r.Kind, params, body)
			}
		}
		if r.Kind.SQL != nil {
			del = func(ctx context.Context, api *rest.Client, params map[string]string, prev map[string]any, _ func() (map[string]any, error)) error {
				if r.Kind.SQL.PerField() {
					params[sqlAppliedKey] = strings.Join(appliedFields(obj), ",")
				}
				return r.deleteSQL(ctx, api, params, spec, prev)
			}
		}
		if r.Kind.Rotate != nil {
			// The tokens it replaced go; the last one only where the spec
			// says so, else it is left to expire (delete_on_destroy).
			del = func(ctx context.Context, api *rest.Client, params map[string]string, prev map[string]any, specBody func() (map[string]any, error)) error {
				if err := r.retire(ctx, api, obj, params); err != nil {
					return err
				}
				if !r.deleteOnDelete(spec) {
					return nil
				}
				return r.delete(ctx, api, params, prev, specBody)
			}
		}
		if err := del(ctx, api, params, applied(obj), specBody); err != nil {
			if s := (*settlingErr)(nil); errors.As(err, &s) {
				return r.settling(ctx, obj, params, s, "")
			}
			return r.fail(ctx, obj, "DeleteFailed", err, nil)
		}
		controllerutil.RemoveFinalizer(obj, r.finalizer())
		return ctrl.Result{}, r.Client.Update(ctx, obj)
	}
	if controllerutil.AddFinalizer(obj, r.finalizer()) {
		if err := r.Client.Update(ctx, obj); err != nil {
			return ctrl.Result{}, err
		}
	}

	replaceOK, err := r.replaceOnChange(spec)
	if err != nil {
		return r.fail(ctx, obj, "InvalidSpec", err, nil)
	}
	body, err := r.body(ctx, obj, spec)
	if err != nil {
		return r.fail(ctx, obj, "InvalidSpec", err, nil)
	}
	// The hash is of the body as the spec gives it, before the hooks: a hook
	// may send something different each time for the same spec (GitHub's
	// secrets are sealed with a fresh nonce), which is not a change.
	hash := bodyHash(body)
	for _, h := range r.Hooks {
		if err := h(ctx, api, r.Kind, params, body); err != nil {
			return r.fail(ctx, obj, "HookFailed", err, nil)
		}
	}

	var rot *rotation
	if r.Kind.Rotate != nil {
		// A token replaced before it expires: whether this sync makes a new one.
		if rot, err = r.planRotation(obj, spec, params); err != nil {
			return r.fail(ctx, obj, "InvalidSpec", err, nil)
		}
		ctx = withRotation(ctx, rot)
	}

	prev, _, _ := unstructured.NestedString(obj.Object, "status", "appliedHash")
	var want any // the state the spec asks for (Kind.State), set by its call, never sent in a body
	if st := r.Kind.State; st != nil {
		want = body[st.Field]
		body = descriptor.WithoutFields(body, []string{st.Field})
	}
	var observed map[string]any
	var action string
	switch {
	case r.Kind.Element != nil:
		observed, action, err = r.syncElement(ctx, api, params, body, applied(obj))
	case r.Kind.Shape == descriptor.Assoc:
		observed, action, err = r.syncAssoc(ctx, api, params, body, applied(obj))
	case r.Kind.Shape == descriptor.SetShape:
		observed, action, err = r.syncSet(ctx, api, params, body, applied(obj), prev == "")
	case r.Kind.Unread != "":
		observed, action, err = r.syncUnread(ctx, api, params, body, hash == prev, prev == "", applied(obj))
	case r.Kind.SQL != nil:
		observed, action, err = r.syncSQL(ctx, api, params, body, applied(obj), sqlStatusOf(obj, hash))
	default:
		observed, action, err = r.sync(ctx, api, params, body, hash == prev, replaceOK)
		if err == nil && r.Kind.State != nil {
			observed, action, err = r.syncState(ctx, api, params, want, body, observed, action)
		}
	}
	if s := (*settlingErr)(nil); errors.As(err, &s) {
		return r.settling(ctx, obj, params, s, hash)
	}
	if err != nil {
		return r.fail(ctx, obj, "SyncFailed", err, params)
	}
	log.FromContext(ctx).V(1).Info("synced", "action", action)
	if resolved != nil {
		// Kept so the object can still be read and deleted once the
		// objects it refers to are gone (refs.go). A new object may have
		// no status, or a null one, yet.
		if st, _ := obj.Object["status"].(map[string]any); st == nil {
			obj.Object["status"] = map[string]any{}
		}
		if err := unstructured.SetNestedMap(obj.Object, resolved, "status", "resolvedRefs"); err != nil {
			return ctrl.Result{}, err
		}
	}
	if len(r.Kind.SecretOutputs) > 0 {
		if err := r.writeSecretOutputs(ctx, obj, spec, observed); err != nil {
			return r.fail(ctx, obj, "SecretWriteFailed", err, params)
		}
	}
	if rot != nil {
		// The new token is in the Secret: the old one goes once the status
		// names the new one.
		action = recordRotation(obj, rot, action)
		if err := r.ready(ctx, obj, params, observed, action, hash); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.retire(ctx, api, obj, params); err != nil {
			log.FromContext(ctx).Error(err, "retiring replaced tokens; tried again at the next sync")
			return ctrl.Result{RequeueAfter: min(RefRetry, rotationRequeue(obj, r.clock()))}, nil
		}
		return ctrl.Result{RequeueAfter: rotationRequeue(obj, r.clock())}, nil
	}
	return ctrl.Result{RequeueAfter: Resync}, r.ready(ctx, obj, params, observed, action, hash)
}

// sync creates, updates or leaves the resource, and returns what the API
// reports afterwards.
// sameAsApplied says the body is what was last sent: write-only fields, which
// the API never returns, are then taken to be unchanged.
// replaceOK says the object opted in to re-creating a kind with no update.
func (r *Reconciler) sync(ctx context.Context, api *rest.Client, params map[string]string, body map[string]any, sameAsApplied, replaceOK bool) (map[string]any, string, error) {
	k := r.Kind
	var err error
	adopted := false
	if k.Shape == descriptor.CRUD && params[k.ID.Param] == "" && k.Adopt != nil {
		// It exists already (a default policy): find it, then update it.
		id, aerr := r.adopt(ctx, api, params, body)
		if aerr != nil {
			return nil, "", aerr
		}
		params[k.ID.Param] = id
		adopted = true
	} else if k.Shape == descriptor.CRUD && params[k.ID.Param] == "" && k.ID.Find != nil && r.foundFirst(ctx, api, params, body, &err) {
		// Made already, by a create that answered no ID: found, not made again.
	} else if err != nil {
		return nil, "", err
	} else if k.Shape == descriptor.CRUD && params[k.ID.Param] == "" {
		var resp any
		if k.Create.GraphQL != nil {
			resp, err = GraphQLCall(ctx, api, k.Create, params, body)
		} else {
			path, perr := rest.Expand(k.Create.Path, params)
			if perr != nil {
				return nil, "", perr
			}
			send := body
			if rot := rotationFrom(ctx); rot != nil && k.Rotate != nil {
				// A token: named after the spec's prefix and its expiry.
				send = k.Rotate.CreateBody(body, rot.spec, rot.now, rot.expires)
				rot.made = true
			}
			resp, err = api.DoWith(ctx, k.Create.Method, rest.WithQuery(path, k.Create.QueryOf(params)), Request(k, k.Create, OpBody(k.Create, send, params, nil)), k.Create.HeadersOf(params))
		}
		if err != nil {
			return nil, "", fmt.Errorf("create: %w", err)
		}
		resp = Answer(k, k.Create, resp)
		obs := r.unwrap(resp)
		if k.ID.IsTemplate() {
			// Named by more than one spec field (Snowflake's NAME(TYPE, ...)).
			id, ok := k.ID.Compose(body)
			if !ok {
				return nil, "", fmt.Errorf("create: spec lacks a field of the ID %q", k.ID.From)
			}
			params[k.ID.Param] = id
			return obs, "created", nil
		}
		id := descriptor.Dig(obs, k.ID.From)
		if id == nil && k.Envelope != "" {
			// Beside the envelope (Confluent: {"id": ..., "spec": {...}}).
			id = descriptor.Dig(resp, k.ID.From)
		}
		if id == nil && k.ID.Param != k.ID.From {
			// Named after the path parameter, where the spec does not
			// describe the response (Vault's {"data": {"method_id": ...}}).
			id = descriptor.Dig(obs, k.ID.Param)
		}
		if id == nil {
			// A key the caller chose: the body has it even when the response
			// names it differently.
			for _, f := range []string{k.ID.From, k.ID.Param} {
				if v, ok := body[f]; ok && fmt.Sprint(v) != "" {
					id = v
					break
				}
			}
		}
		if id == nil && k.ID.Find != nil {
			// The create answered no ID (an asynchronous bulk add): find it.
			got, err := r.found(ctx, api, params, body)
			if err != nil {
				return nil, "", err
			}
			if got == "" {
				return nil, "", fmt.Errorf("create: made, but not listed at %s yet: looked for again at the next sync", k.ID.Find.Path)
			}
			id = got
		}
		if id == nil {
			return nil, "", fmt.Errorf("create: response has no %q", k.ID.From)
		}
		part, ok := k.ID.Part(IDString(id))
		if !ok {
			return nil, "", fmt.Errorf("create: %q is %q, which is not %s", k.ID.From, IDString(id), k.ID.Match)
		}
		params[k.ID.Param] = part
		if k.Create.GraphQL != nil && k.Read.GraphQL == nil {
			// Made over GraphQL, read over REST (a mixed kind): read it back
			// at once, in the form drift is compared in, and update what
			// the create could not set (GitHub's organization description,
			// which only the REST update takes).
			// Not found is an error here, never a reason to create again.
			readPath, err := rest.Expand(k.Read.Path, params)
			if err != nil {
				return nil, "", err
			}
			resp, err := r.readRecheck(ctx, api, readPath, params)
			if err != nil {
				return nil, "", fmt.Errorf("read after create: %w", err)
			}
			obs = r.unwrap(resp)
			if k.Update != nil && Drift(body, r.normalize(body, obs), r.skip(obs)) != "" {
				k.FillCurrent(obs, params)
				return r.put(ctx, api, params, body, "created", obs)
			}
			return obs, "created", nil
		}
		return obs, "created", nil
	}

	readPath, err := rest.Expand(k.Read.Path, params)
	if err != nil {
		return nil, "", err
	}
	var resp any
	if k.Shape == descriptor.CRUD {
		resp, err = r.readRecheck(ctx, api, readPath, params)
	} else {
		resp, err = r.read(ctx, api, readPath, params)
	}
	switch {
	case errors.Is(err, rest.ErrNotFound) && k.Adopt != nil:
		if adopted {
			return nil, "", fmt.Errorf("read: the %s found at %s is gone: %w", k.Kind, readPath, err)
		}
		// Gone: find it again.
		delete(params, k.ID.Param)
		return r.sync(ctx, api, params, body, false, replaceOK)
	case errors.Is(err, rest.ErrNotFound) && k.Shape == descriptor.CRUD:
		// Deleted outside Kubernetes: create it again.
		delete(params, k.ID.Param)
		return r.sync(ctx, api, params, body, false, replaceOK)
	case errors.Is(err, rest.ErrNotFound) && k.Update == nil && k.Create == nil:
		return nil, "", fmt.Errorf("read: %w", err) // nothing makes it (a feature set only by its state calls)
	case errors.Is(err, rest.ErrNotFound) && (k.Shape == descriptor.Upsert || k.Shape == descriptor.Settings):
		if k.Create != nil {
			return r.write(ctx, api, k.Create, params, body, "created", nil)
		}
		return r.put(ctx, api, params, body, "created", nil)
	case err != nil:
		return nil, "", fmt.Errorf("read: %w", err)
	}
	obs := r.unwrap(resp)
	cur := obs
	if k.Order != nil {
		cur, _ = resp.(map[string]any) // the API's own answer: {current.data.id}, and a partial order's other IDs
	}
	field := Drift(body, r.normalize(body, obs), r.skip(obs))
	if field == "" && (sameAsApplied || !r.hasUnseen(body, obs)) {
		return obs, "unchanged", nil
	}
	if k.Update == nil && k.Replace != nil {
		if replaceOK {
			return r.replace(ctx, api, params, body, obs)
		}
		return obs, "drifted", fmt.Errorf("spec.%s differs from the API, and this kind cannot be updated in place; delete and recreate the object, or set spec.replaceOnChange: true to re-create it in place", field)
	}
	if k.Update == nil {
		return obs, "drifted", fmt.Errorf("spec.%s differs from the API, and this kind cannot be updated in place; delete and recreate the object", field)
	}
	for _, f := range k.CreateOnly {
		if sub, ok := descriptor.PickField(body, f); ok && Drift(sub, r.normalize(body, obs), r.skip(obs)) != "" {
			return obs, "drifted", fmt.Errorf("spec.%s differs from the API, and it can only be set when the object is created; delete and recreate the object", f)
		}
	}
	upd := body
	if len(k.OmitUnchanged) > 0 {
		norm := r.normalize(body, obs)
		skip := r.skip(obs)
		upd = make(map[string]any, len(body))
		for f, v := range body {
			if k.IsOmitUnchanged(f) && Drift(map[string]any{f: v}, norm, skip) == "" {
				continue
			}
			upd[f] = v
		}
	}
	// Create-only fields stay out of an update; what it carries goes, as
	// the API answered it (a statement's name, which its PUT requires).
	upd = descriptor.WithoutFields(upd, k.CreateOnly)
	k.FillCurrent(cur, params) // {current.scope}: what the API holds now, not what the spec asks for
	return r.put(ctx, api, params, Carried(k.Update, upd, cur, nil), "updated", cur)
}

// Carried is body with each of op's Carry fields (dotted) set as obs, what
// a read just answered, has it, else as from (the spec) has it; body itself
// is not changed. A field neither has is left out.
func Carried(op *descriptor.Op, body, obs, from map[string]any) map[string]any {
	if op == nil || len(op.Carry) == 0 {
		return body
	}
	out := body
	for _, f := range op.Carry {
		at := strings.ReplaceAll(f, ".", "/")
		v := descriptor.Dig(obs, at)
		if v == nil {
			v = descriptor.Dig(from, at)
		}
		if v == nil {
			continue
		}
		out = withDotted(out, strings.Split(f, "."), v)
	}
	return out
}

// withDotted is a copy of m with the field at parts set to v, copying each
// object on the way rather than changing it.
func withDotted(m map[string]any, parts []string, v any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, x := range m {
		out[k] = x
	}
	if len(parts) == 1 {
		out[parts[0]] = v
		return out
	}
	sub, _ := out[parts[0]].(map[string]any)
	out[parts[0]] = withDotted(sub, parts[1:], v)
	return out
}

// adopt finds the resource an Adopt kind takes over: the element of its
// list (every page) that Adopt.Match describes, filled from the spec; its
// ID.
func (r *Reconciler) adopt(ctx context.Context, api *rest.Client, params map[string]string, body map[string]any) (string, error) {
	k := r.Kind
	id, path, err := r.listed(ctx, api, k.Adopt, params, body, k.Adopt.Matches)
	if err != nil {
		return "", fmt.Errorf("finding the %s to adopt: %w", k.Kind, err)
	}
	if id == "" {
		return "", fmt.Errorf("finding the %s to adopt: none at %s has %s", k.Kind, path, matchText(k.Adopt.Match))
	}
	return id, nil
}

// listed is the ID of the element of a's list (every page) that match
// takes, filled from the spec's body and path parameters; "" for none. It
// answers the list's path too.
func (r *Reconciler) listed(ctx context.Context, api *rest.Client, a *descriptor.Adopt, params map[string]string, body map[string]any,
	match func(e, vals map[string]any) bool) (string, string, error) {
	k := r.Kind
	vals := make(map[string]any, len(body)+len(k.Params))
	for f, v := range body {
		vals[f] = v
	}
	for _, p := range k.Params {
		vals[p.Field] = params[p.Name]
	}
	path, err := rest.Expand(a.Path, params)
	if err != nil {
		return "", "", err
	}
	at := ""
	if a.Find != nil {
		at = a.Find.List
	}
	url := rest.WithQuery(path, a.QueryFrom(vals, params))
	for page := 0; url != "" && page < MaxPages; page++ {
		resp, next, err := api.DoPaged(ctx, a.Method, url, nil, a.Headers)
		if err != nil {
			return "", path, err
		}
		list, _ := descriptor.Dig(resp, at).([]any)
		for _, e := range list {
			m, _ := e.(map[string]any)
			if m != nil && match(m, vals) {
				id := descriptor.Dig(m, k.ID.From)
				if id == nil || IDString(id) == "" {
					return "", path, fmt.Errorf("the match has no %q", k.ID.From)
				}
				return IDString(id), path, nil
			}
		}
		url = next
	}
	return "", path, nil
}

// found is the ID of a resource a create made without answering it
// (ID.Find): the element of its list that holds what the spec sets; "" for
// none yet.
func (r *Reconciler) found(ctx context.Context, api *rest.Client, params map[string]string, body map[string]any) (string, error) {
	id, _, err := r.listed(ctx, api, r.Kind.ID.Find, params, body, r.Kind.ID.Find.MatchesSpec)
	if err != nil {
		return "", fmt.Errorf("finding the %s made: %w", r.Kind.Kind, err)
	}
	return id, nil
}

// foundFirst looks, before a create, for the resource a create made
// earlier without answering its ID (ID.Find), and takes its ID; false when
// there is none (or err is set).
func (r *Reconciler) foundFirst(ctx context.Context, api *rest.Client, params map[string]string, body map[string]any, err *error) bool {
	id, ferr := r.found(ctx, api, params, body)
	if ferr != nil {
		*err = ferr
		return false
	}
	if id == "" {
		return false
	}
	params[r.Kind.ID.Param] = id
	return true
}

func matchText(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for f, v := range m {
		parts = append(parts, f+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// put sends the update; cur is the resource as the API answered it just
// before, if it was read (for the update body's {current.field} values).
func (r *Reconciler) put(ctx context.Context, api *rest.Client, params map[string]string, body map[string]any, action string, cur map[string]any) (map[string]any, string, error) {
	return r.write(ctx, api, r.Kind.Update, params, body, action, cur)
}

func (r *Reconciler) write(ctx context.Context, api *rest.Client, op *descriptor.Op, params map[string]string, body map[string]any, action string, cur map[string]any) (map[string]any, string, error) {
	path, err := rest.Expand(op.Path, params)
	if err != nil {
		return nil, "", err
	}
	if o := r.Kind.Order; o != nil {
		// A partial order: the IDs the spec does not list keep their place after it.
		ids, _ := body[o.Field].([]any)
		body = descriptor.MergeAt(body, o.Field, o.Wanted(ids, cur))
	}
	var resp any
	if op.GraphQL != nil {
		resp, err = GraphQLCall(ctx, api, op, params, body)
	} else if op.Whole {
		// The resource as it is now, with the body in place.
		readPath, rerr := rest.Expand(r.Kind.Read.Path, params)
		if rerr != nil {
			return nil, "", rerr
		}
		raw, rerr := api.DoWith(ctx, r.Kind.Read.Method, rest.WithQuery(readPath, r.Kind.Read.QueryOf(params)), nil, r.Kind.Read.HeadersOf(params))
		if rerr != nil {
			return nil, "", fmt.Errorf("read before %s: %w", strings.TrimSuffix(action, "d"), rerr)
		}
		resp, err = api.DoWith(ctx, op.Method, rest.WithQuery(path, op.QueryOf(params)), WholeBody(r.Kind, op, raw, OpBody(op, WithUpdateID(r.Kind, body, params), params, cur)), op.HeadersOf(params))
	} else {
		resp, err = api.DoWith(ctx, op.Method, rest.WithQuery(path, op.QueryOf(params)), Request(r.Kind, op, OpBody(op, WithUpdateID(r.Kind, body, params), params, cur)), op.HeadersOf(params))
	}
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", strings.TrimSuffix(action, "d"), err)
	}
	// A reorder answers what it likes (nothing, or part of the list), and a
	// write of what is read as raw bytes answers a status (Workers KV's
	// {"success": true}): read the object instead. So does a mixed kind's
	// GraphQL write whose read is REST.
	if obs := r.unwrap(Answer(r.Kind, op, resp)); len(obs) > 0 && r.Kind.Order == nil && r.Kind.Read.Raw == "" && (op.GraphQL == nil || r.Kind.Read.GraphQL != nil) {
		return obs, action, nil
	}
	// No answer, or a GraphQL one of a kind read over REST (a mixed
	// kind): read it back, in the form drift is compared in.
	readPath, _ := rest.Expand(r.Kind.Read.Path, params)
	resp, err = r.readRecheck(ctx, api, readPath, params)
	if err != nil {
		return nil, "", fmt.Errorf("read after %s: %w", action, err)
	}
	return r.unwrap(resp), action, nil
}

// WholeBody is a whole write's body (Op.Whole): raw, the resource as a read
// answered it, with each of body's fields in place of raw's at the call's
// (else the kind's) BodyEnvelope; raw itself is not changed.
func WholeBody(k *descriptor.Kind, op *descriptor.Op, raw any, body map[string]any) map[string]any {
	env := k.BodyEnvelope
	if op != nil && op.BodyEnvelope != "" {
		env = op.BodyEnvelope
	}
	var parts []string
	if env != "" {
		parts = strings.Split(env, "/")
	}
	var put func(m map[string]any, parts []string) map[string]any
	put = func(m map[string]any, parts []string) map[string]any {
		out := make(map[string]any, len(m)+len(body))
		for f, v := range m {
			out[f] = v
		}
		if len(parts) == 0 {
			for f, v := range body {
				out[f] = v
			}
			return out
		}
		sub, _ := out[parts[0]].(map[string]any)
		out[parts[0]] = put(sub, parts[1:])
		return out
	}
	m, _ := raw.(map[string]any)
	return put(m, parts)
}

// WithUpdateID is an update's body: with the kind's UpdateID field set to
// the resource's ID (JSON:API's data.id), when it has one and the spec does
// not set the field itself.
func WithUpdateID(k *descriptor.Kind, body map[string]any, params map[string]string) map[string]any {
	if k.UpdateID == "" || k.ID == nil || params[k.ID.Param] == "" {
		return body
	}
	if _, ok := body[k.UpdateID]; ok {
		return body
	}
	out := make(map[string]any, len(body)+1)
	for f, v := range body {
		out[f] = v
	}
	out[k.UpdateID] = params[k.ID.Param]
	return out
}

// WithOpBody merges op.Body into body (without mutating body) when op carries
// per-operation fields (e.g. JSON:API type on update).
func WithOpBody(op *descriptor.Op, body map[string]any) map[string]any {
	return OpBody(op, body, nil, nil)
}

// OpBody is WithOpBody with op.Body's templates filled: a {param} from
// params, a {current.field} from cur, the resource as the API answered it
// last (Grafana Synthetic Monitoring's check update names the tenantId the
// API gave it). A value that is one template takes the value's own type.
func OpBody(op *descriptor.Op, body map[string]any, params map[string]string, cur map[string]any) map[string]any {
	if op != nil && op.Request != nil {
		return RequestBody(op, body, params, cur)
	}
	if op == nil || len(op.Body) == 0 {
		return body
	}
	fixed, _ := fillTemplates(op.Body, templateVals(params, cur)).(map[string]any)
	return mergeMaps(body, fixed)
}

// RequestBody is a call's whole body where the call builds it from the
// spec (Op.Request): its template filled from the spec's body fields, the
// path parameters and the ID, and cur, the resource as the API answered it
// last ({current.field}).
func RequestBody(op *descriptor.Op, body map[string]any, params map[string]string, cur map[string]any) map[string]any {
	vals := templateVals(params, cur)
	for f, v := range body {
		if _, ok := vals[f]; !ok {
			vals[f] = v
		}
	}
	out, _ := descriptor.FillRequest(op.Request, vals)
	m, _ := out.(map[string]any)
	return m
}

// templateVals are the values body templates are filled from: path
// parameters (and the ID) by name, and the current resource as "current".
func templateVals(params map[string]string, cur map[string]any) map[string]any {
	vals := make(map[string]any, len(params)+1)
	for p, v := range params {
		vals[p] = v
	}
	if cur != nil {
		vals["current"] = cur
	}
	return vals
}

func mergeMaps(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		if om, ok := v.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = mergeMaps(bm, om)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// hasWriteOnly: the body carries fields the API never returns (a write-only
// field may be nested: settings.clientSecret).
func (r *Reconciler) hasWriteOnly(body map[string]any) bool {
	for _, w := range r.Kind.WriteOnly {
		if descriptor.Dig(body, strings.ReplaceAll(w, ".", "/")) != nil {
			return true
		}
	}
	return false
}

func (r *Reconciler) delete(ctx context.Context, api *rest.Client, params map[string]string, prev map[string]any, spec func() (map[string]any, error)) error {
	k := r.Kind
	if k.Shape == descriptor.Assoc {
		if prev == nil {
			return nil // never applied
		}
		return r.undo(ctx, api, params, prev)
	}
	if k.Element != nil {
		return r.deleteElement(ctx, api, params, prev, spec)
	}
	if k.StateDeletes() {
		return r.deleteState(ctx, api, params)
	}
	if k.Delete == nil || (k.Shape == descriptor.CRUD && params[k.ID.Param] == "") {
		return nil // settings stay; a CRUD resource never created has nothing to delete
	}
	if err := BeforeDelete(ctx, api, k, params); err != nil {
		return err
	}
	if k.Delete.GraphQL != nil {
		if _, err := GraphQLCall(ctx, api, k.Delete, params, nil); err != nil && !errors.Is(err, rest.ErrNotFound) {
			return err
		}
		return nil
	}
	k.FillCurrent(prev, params) // {current.scope}: as the API last answered it
	path, err := rest.Expand(k.Delete.Path, params)
	if err != nil && strings.Contains(k.Delete.Path, "{current.") {
		// Named by what the API answered (a k6 schedule by its own ID),
		// which the status lacks: read it now.
		readPath, rerr := rest.Expand(k.Read.Path, params)
		if rerr != nil {
			return rerr
		}
		resp, rerr := r.read(ctx, api, readPath, params)
		if errors.Is(rerr, rest.ErrNotFound) {
			return nil // gone already, or never made
		}
		if rerr != nil {
			return fmt.Errorf("read before delete: %w", rerr)
		}
		k.FillCurrent(r.unwrap(resp), params)
		path, err = rest.Expand(k.Delete.Path, params)
	}
	if err != nil {
		return err
	}
	var body map[string]any
	if k.Delete.SendBody || k.Delete.Request != nil {
		if body, err = spec(); err != nil {
			return err
		}
	}
	send := DeleteBody(k, params, body)
	if len(k.Delete.Carry) > 0 {
		// The delete names what it deletes as the API has it now (a GitHub
		// file's blob sha): read it first; the rest from the spec.
		readPath, err := rest.Expand(k.Read.Path, params)
		if err != nil {
			return err
		}
		resp, err := r.read(ctx, api, readPath, params)
		if errors.Is(err, rest.ErrNotFound) {
			return nil // gone already
		}
		if err != nil {
			return fmt.Errorf("read before delete: %w", err)
		}
		from := body
		if from == nil {
			from, _ = spec() // a Secret gone by now leaves only what the read has
		}
		send = Request(k, k.Delete, Carried(k.Delete, DeleteFields(k, params, body), r.unwrap(resp), from))
	}
	if _, err := api.DoWith(ctx, k.Delete.Method, rest.WithQuery(path, k.Delete.QueryOf(params)), send, k.Delete.HeadersOf(params)); err != nil && !errors.Is(err, rest.ErrNotFound) && !allowed(k.Delete, err) {
		return err
	}
	return nil
}

// DeleteBody is what a (non-assoc) kind's delete sends: the spec's body
// where the delete sends it (Delete.SendBody: Grafana Asserts' thresholds),
// else the delete's own fixed body (a delete that is a write: GitHub closes
// an issue), its {param} templates filled from params; with Delete.Outer
// beside it. Nil: no body.
func DeleteBody(k *descriptor.Kind, params map[string]string, specBody map[string]any) any {
	return Request(k, k.Delete, DeleteFields(k, params, specBody))
}

// DeleteFields is the delete's body before Request shapes it (DeleteBody);
// nil for none.
func DeleteFields(k *descriptor.Kind, params map[string]string, specBody map[string]any) map[string]any {
	op := k.Delete
	var body map[string]any
	switch {
	case op.SendBody:
		body = specBody
		if body == nil {
			body = map[string]any{}
		}
	case op.Body != nil:
		body, _ = fillTemplates(op.Body, templateVals(params, nil)).(map[string]any)
	case op.Request != nil:
		body = RequestBody(op, specBody, params, nil)
	}
	return body
}

// fillTemplates copies v with each string that holds templates filled
// from vals (dotted for a nested value: {current.tenantId}); a string that
// is one template whole takes the value as it is (a number stays one). A
// template vals cannot fill leaves its string as it is.
func fillTemplates(v any, vals map[string]any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for f, e := range t {
			out[f] = fillTemplates(e, vals)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = fillTemplates(e, vals)
		}
		return out
	case string:
		if !descriptor.IsTemplate(t) {
			return t
		}
		if m := wholeTemplate.FindStringSubmatch(t); m != nil {
			if got := descriptor.Dig(vals, strings.ReplaceAll(m[1], ".", "/")); got != nil {
				return got
			}
			return t
		}
		if got, ok := (&descriptor.ID{From: t}).Compose(vals); ok {
			return got
		}
	}
	return v
}

var wholeTemplate = regexp.MustCompile(`^\{([^{}]+)\}$`)

// Request is the request body of one of the kind's calls: body as the
// kind's API takes it (WrapBody), in op's own BodyEnvelope where it has one
// (an assoc's undo: PagerDuty's {"remove": [...]}), with op.Outer's fields
// beside it (Datadog's {"meta": {}}). A nil body and no Outer: no body.
func Request(k *descriptor.Kind, op *descriptor.Op, body map[string]any) any {
	var out any
	if body != nil && op != nil && op.Request != nil {
		out = body // built whole by the call (RequestBody): sent as it is
	} else if body != nil {
		if op != nil && op.BareBody && k.BodyEnvelope != "" {
			kk := *k
			kk.BodyEnvelope = "" // sent as it is (an RPC delete's {"id": ...})
			k = &kk
		}
		if op != nil && op.BodyEnvelope != "" && op.BodyEnvelope != k.BodyEnvelope {
			kk := *k
			kk.BodyEnvelope = op.BodyEnvelope
			k = &kk
		}
		if op != nil && (op.BodyArray || k.Multipart != nil && !k.MultipartOn(op)) {
			// This call alone takes a one-element array (Cloudflare's bulk
			// create), or JSON where the kind's others are multipart.
			kk := *k
			kk.BodyArray = kk.BodyArray || op.BodyArray
			if !k.MultipartOn(op) {
				kk.Multipart = nil
			}
			k = &kk
		}
		out = WrapBody(k, body)
	}
	if op == nil || len(op.Outer) == 0 {
		return out
	}
	m, ok := out.(map[string]any)
	if out != nil && !ok {
		return out // a bare array or a form: nothing can be beside it
	}
	merged := make(map[string]any, len(m)+len(op.Outer))
	for f, v := range m {
		merged[f] = v
	}
	for f, v := range op.Outer {
		merged[f] = v
	}
	return merged
}

// allowed reports whether err is an API answer whose status op counts as
// done (descriptor.Op.OK): a delete the API leaves to the parent resource.
func allowed(op *descriptor.Op, err error) bool {
	var apiErr *rest.APIError
	return errors.As(err, &apiErr) && op.Allows(apiErr.Status)
}

// BeforeDelete makes a kind's beforeDelete calls, in order (GitLab's stop
// before an environment can be deleted). A 404 is not an error: the delete
// that follows finds the resource gone too.
func BeforeDelete(ctx context.Context, api *rest.Client, k *descriptor.Kind, params map[string]string) error {
	for _, op := range k.BeforeDelete {
		path, err := rest.Expand(op.Path, params)
		if err != nil {
			return err
		}
		if _, err := api.DoWith(ctx, op.Method, rest.WithQuery(path, op.QueryOf(params)), nil, op.HeadersOf(params)); err != nil && !errors.Is(err, rest.ErrNotFound) {
			return fmt.Errorf("before delete, %s %s: %w", op.Method, op.Path, err)
		}
	}
	return nil
}

func (r *Reconciler) unwrap(resp any) map[string]any {
	return UnwrapResponse(r.Kind, resp)
}

// WrapBody shapes body as the API expects it: returning a MultipartBody for
// multipart kinds, extracting BodyField for a bare array body, wrapping in a
// one-element array for BodyArray, and wrapping in BodyEnvelope if set.
func WrapBody(k *descriptor.Kind, body map[string]any) any {
	if k.Multipart != nil {
		return &rest.MultipartBody{Fields: body, Files: k.Multipart.Files}
	}
	if k.Order != nil {
		return k.Order.Request(body)
	}
	var out any = body
	if k.BodyField != "" {
		arr, _ := body[k.BodyField].([]any)
		if arr == nil {
			arr = []any{}
		}
		out = arr
	} else if k.BodyArray {
		out = []any{body}
	}
	if k.BodyEnvelope == "" {
		return out
	}
	parts := strings.Split(k.BodyEnvelope, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		out = map[string]any{parts[i]: out}
	}
	return out
}

// UnwrapResponse digs Envelope out of resp and, for BodyField kinds, wraps
// a JSON array response back into {BodyField: array}.
func UnwrapResponse(k *descriptor.Kind, resp any) map[string]any {
	if k.Order != nil {
		return k.Order.Observed(resp)
	}
	v := descriptor.Dig(resp, k.Envelope)
	if k.BodyField != "" {
		if arr, ok := v.([]any); ok {
			return map[string]any{k.BodyField: arr}
		}
		return nil
	}
	m, _ := v.(map[string]any)
	return m
}

// Answer is resp as the kind's other answers hold the object: where op
// answers it elsewhere (Op.Envelope: Cloudflare's bulk create, {"result":
// [{...}]} at result/0), moved into the kind's Envelope; else resp.
func Answer(k *descriptor.Kind, op *descriptor.Op, resp any) any {
	if op == nil || op.Envelope == "" {
		return resp
	}
	m, _ := descriptor.Dig(resp, op.Envelope).(map[string]any)
	if m == nil {
		return nil
	}
	return WrapEnvelope(k, m)
}

// params collects path parameter values from the spec and, for a created
// CRUD resource, its ID from the status.
func (r *Reconciler) params(obj *unstructured.Unstructured, spec map[string]any) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range r.Kind.Params {
		v := p.In(spec)
		if v == nil || fmt.Sprint(v) == "" {
			return nil, fmt.Errorf("spec.%s is required", p.Field)
		}
		out[p.Name] = IDString(v)
	}
	if err := r.Kind.FillQuery(spec, out); err != nil {
		return nil, err
	}
	if r.Kind.ID != nil {
		if id, _, _ := unstructured.NestedString(obj.Object, "status", "id"); id != "" {
			out[r.Kind.ID.Param] = id
		} else if id := obj.GetAnnotations()[AnnAdoptID]; id != "" {
			out[r.Kind.ID.Param] = id
		}
	}
	if r.Kind.SQL != nil {
		if id, _, _ := unstructured.NestedString(obj.Object, "status", "id"); id != "" {
			out[sqlIDKey] = id
		}
	}
	return out, nil
}

// body is the spec without path parameters and our own fields, with
// spec.secretRefs resolved.
func (r *Reconciler) body(ctx context.Context, obj *unstructured.Unstructured, spec map[string]any) (map[string]any, error) {
	isParam := map[string]bool{"connectionRef": true, "secretRefs": true, FieldRefs: true, WriteSecretRef: true}
	if r.Kind.Replace != nil {
		isParam[ReplaceOnChange] = true
	}
	for _, p := range r.Kind.Params {
		if !p.InBody {
			isParam[p.Field] = true
		}
	}
	for _, f := range r.Kind.QueryFields {
		isParam[f] = true
	}
	out := map[string]any{}
	if len(r.Kind.Body) > 0 {
		for _, f := range r.Kind.Body {
			if v, ok := spec[f]; ok {
				out[f] = v
			}
		}
	} else {
		for f, v := range spec {
			if !isParam[f] {
				out[f] = v
			}
		}
	}
	refs, _, _ := unstructured.NestedMap(spec, "secretRefs")
	for field, ref := range refs {
		m, _ := ref.(map[string]any)
		name, _ := m["name"].(string)
		key, _ := m["key"].(string)
		sec := &corev1.Secret{}
		if err := r.Client.Get(ctx, types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}, sec); err != nil {
			return nil, fmt.Errorf("secretRefs.%s: %w", field, err)
		}
		v, ok := sec.Data[key]
		if !ok {
			return nil, fmt.Errorf("secretRefs.%s: secret %s has no key %q", field, name, key)
		}
		if strings.Contains(field, ".") {
			// Inside an object (Confluent's Flink connection auth_data.data,
			// a JSON string of the connection's secret keys).
			out = withDotted(out, strings.Split(field, "."), string(v))
			continue
		}
		out[field] = string(v)
	}
	return out, nil
}

func (r *Reconciler) apiClient(ctx context.Context, obj *unstructured.Unstructured, spec map[string]any) (*rest.Client, error) {
	name := "default"
	if ref, ok := spec["connectionRef"].(map[string]any); ok {
		if n, _ := ref["name"].(string); n != "" {
			name = n
		}
	}
	conn := &unstructured.Unstructured{}
	conn.SetGroupVersionKind(r.Connection)
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}, conn); err != nil {
		return nil, fmt.Errorf("%s %s/%s: %w", r.Connection.Kind, obj.GetNamespace(), name, err)
	}
	baseURL, _, _ := unstructured.NestedString(conn.Object, "spec", "baseURL")
	secretName, _, _ := unstructured.NestedString(conn.Object, "spec", "secretRef", "name")
	sec := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: obj.GetNamespace(), Name: secretName}, sec); err != nil {
		return nil, fmt.Errorf("%s %s: secret %q: %w", r.Connection.Kind, name, secretName, err)
	}
	creds := map[string]string{}
	if missing := r.Vendor.MissingKeys(sec.Data); missing != "" {
		return nil, fmt.Errorf("secret %s has no key %s", secretName, missing)
	}
	for k, v := range sec.Data { // optional extras (e.g. Datadog's site)
		if _, ok := creds[k]; !ok {
			creds[k] = string(v)
		}
	}
	newClient := r.NewClient
	if newClient == nil {
		newClient = rest.New
	}
	return newClient(r.Vendor, baseURL, creds), nil
}

func (r *Reconciler) ready(ctx context.Context, obj *unstructured.Unstructured, params map[string]string, observed map[string]any, action, hash string) error {
	status := map[string]any{"appliedHash": hash}
	if r.Kind.ID != nil {
		status["id"] = params[r.Kind.ID.Param]
	}
	if r.Kind.SQL.PerField() {
		applied := []any{}
		for _, f := range strings.Split(params[sqlAppliedKey], ",") {
			if f != "" {
				applied = append(applied, f)
			}
		}
		status[AppliedFields] = applied
	}
	if r.Kind.SQL != nil && r.Kind.SQL.Mode == descriptor.SQLObject {
		status["id"] = params[sqlIDKey]
		applied := []any{}
		for _, f := range strings.Split(params[sqlAppliedKey], ",") {
			if f != "" {
				applied = append(applied, f)
			}
		}
		status[AppliedFields] = applied
		if cw := createdWithParam(params); cw != nil {
			status[CreatedWith] = cw
		}
	}
	obs := map[string]any{}
	for k, v := range observed {
		// An assoc's status is the link it made, which its undo sends
		// whole: a field its list does not answer is kept too.
		if !r.Kind.IsWriteOnly(k) || r.Kind.Shape == descriptor.Assoc {
			obs[k] = v
		}
	}
	status["atProvider"] = descriptor.WithoutFields(obs, r.Kind.SecretOutputs) // a secret the API made is never status
	if rr, ok, _ := unstructured.NestedMap(obj.Object, "status", "resolvedRefs"); ok {
		status["resolvedRefs"] = rr
	}
	if rot, ok, _ := unstructured.NestedMap(obj.Object, "status", RotationStatus); ok && r.Kind.Rotate != nil {
		status[RotationStatus] = rot // a rotating token's expiry and the tokens to retire
	}
	status["observedGeneration"] = obj.GetGeneration()
	status["conditions"] = []any{condition("Ready", metav1.ConditionTrue, strings.ToUpper(action[:1])+action[1:], "")}
	return r.writeStatus(ctx, obj, status)
}

func (r *Reconciler) fail(ctx context.Context, obj *unstructured.Unstructured, reason string, cause error, params map[string]string) (ctrl.Result, error) {
	status, _, _ := unstructured.NestedMap(obj.Object, "status")
	if status == nil {
		status = map[string]any{}
	}
	if params != nil && r.Kind.ID != nil && params[r.Kind.ID.Param] != "" {
		status["id"] = params[r.Kind.ID.Param]
	}
	if params != nil && r.Kind.SQL != nil && params[sqlIDKey] != "" {
		status["id"] = params[sqlIDKey]
	}
	status["observedGeneration"] = obj.GetGeneration()
	status["conditions"] = []any{condition("Ready", metav1.ConditionFalse, reason, cause.Error())}
	if err := r.writeStatus(ctx, obj, status); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	return r.failResult(cause)
}

// failResult is when to try again after cause: an error the API calls the
// caller's (a 4xx but 429, or a GraphQL answer's errors) at the next
// resync, as retrying will not help until the spec changes; any other now,
// with backoff.
func (r *Reconciler) failResult(cause error) (ctrl.Result, error) {
	var apiErr *rest.APIError
	var gqlErr *rest.GraphQLError
	if errors.As(cause, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != 429 || errors.As(cause, &gqlErr) {
		return ctrl.Result{RequeueAfter: Resync}, nil
	}
	return ctrl.Result{}, cause
}

// RefRetry is how soon an object waiting for a fieldRef looks again.
var RefRetry = 10 * time.Second

// waitForRef reports a reference that does not resolve yet; nothing is sent.
func (r *Reconciler) waitForRef(ctx context.Context, obj *unstructured.Unstructured, w *waitErr) (ctrl.Result, error) {
	status, _, _ := unstructured.NestedMap(obj.Object, "status")
	if status == nil {
		status = map[string]any{}
	}
	status["observedGeneration"] = obj.GetGeneration()
	status["conditions"] = []any{condition("Ready", metav1.ConditionFalse, WaitingForReference, w.msg)}
	if err := r.writeStatus(ctx, obj, status); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: RefRetry}, nil
}

// neverSynced: nothing was ever sent for the object.
func neverSynced(obj *unstructured.Unstructured) bool {
	id, _, _ := unstructured.NestedString(obj.Object, "status", "id")
	hash, _, _ := unstructured.NestedString(obj.Object, "status", "appliedHash")
	return id == "" && hash == ""
}

func (r *Reconciler) writeStatus(ctx context.Context, obj *unstructured.Unstructured, status map[string]any) error {
	if err := unstructured.SetNestedField(obj.Object, status, "status"); err != nil {
		return err
	}
	return r.Client.Status().Update(ctx, obj)
}

func condition(t string, s metav1.ConditionStatus, reason, msg string) map[string]any {
	return map[string]any{
		"type": t, "status": string(s), "reason": reason, "message": msg,
		"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
	}
}

// IDString formats an ID or path parameter. A JSON number decodes as a
// float64, which fmt.Sprint writes in exponent form once it is large
// (GitHub's release 270123456 would be read at /releases/2.70123456e+08,
// a 404, and made again); a whole number is written as an integer.
func IDString(v any) string { return descriptor.Text(v) }

// bodyHash fingerprints what is sent, secrets included, without storing them.
func bodyHash(body map[string]any) string {
	b, _ := json.Marshal(body) // map keys marshal sorted
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// readRecheck calls read and, on ErrNotFound when NotFoundRecheck > 0, polls
// with exponential backoff up to NotFoundRecheck before returning ErrNotFound.
func (r *Reconciler) readRecheck(ctx context.Context, api *rest.Client, path string, params map[string]string) (any, error) {
	resp, err := r.read(ctx, api, path, params)
	if !errors.Is(err, rest.ErrNotFound) || NotFoundRecheck <= 0 {
		return resp, err
	}
	step := NotFoundRecheck / 15
	if step < time.Millisecond {
		step = NotFoundRecheck
	}
	for remaining := NotFoundRecheck; remaining > 0; {
		wait := min(step, remaining)
		if err := r.wait(ctx, wait); err != nil {
			return nil, err
		}
		resp, err = r.read(ctx, api, path, params)
		if !errors.Is(err, rest.ErrNotFound) {
			return resp, err
		}
		remaining -= wait
		step *= 2
	}
	return nil, err
}

// read GETs a resource; with Read.Find it lists and picks the element.
func (r *Reconciler) read(ctx context.Context, api *rest.Client, path string, params map[string]string) (any, error) {
	op := r.Kind.Read
	if op.GraphQL != nil {
		return GraphQLRead(ctx, api, r.Kind, params)
	}
	if op.Raw != "" {
		// Not JSON: the field's value itself (Workers KV's value).
		data, err := api.DoRaw(ctx, op.Method, rest.WithQuery(path, op.QueryOf(params)), op.HeadersOf(params))
		if err != nil {
			return nil, err
		}
		return WrapEnvelope(r.Kind, map[string]any{op.Raw: string(data)}), nil
	}
	if op.Find == nil {
		resp, err := api.DoWith(ctx, op.Method, rest.WithQuery(path, op.QueryOf(params)), ReadBody(r.Kind, params), op.HeadersOf(params))
		if err != nil {
			return nil, err
		}
		if op.Envelope != "" {
			if resp = Answer(r.Kind, &op, resp); resp == nil {
				return nil, rest.ErrNotFound // an empty list: there is none
			}
		}
		if op.IsAbsent(r.unwrap(resp)) {
			// 200, saying there is none (Atlas's state: UNCONFIGURED).
			return nil, rest.ErrNotFound
		}
		return resp, nil
	}
	want := params[op.Find.Param]
	if op.Find.Key != "" {
		var ok bool
		if want, ok = op.Find.KeyIn(params); !ok {
			return nil, fmt.Errorf("read: no value for a parameter of the key %s", op.Find.Key)
		}
	}
	m, err := FindPagedWith(ctx, api, r.Kind, rest.WithQuery(path, op.QueryOf(params)), want, params)
	if err != nil {
		return nil, err
	}
	if op.Envelope != "" {
		// Where in the element the object is (Datadog's managed
		// organizations: {"id", "type", "attributes": {...}}).
		if m, _ = descriptor.Dig(m, op.Envelope).(map[string]any); m == nil {
			return nil, rest.ErrNotFound
		}
	}
	return WrapEnvelope(r.Kind, m), nil // the element is the object: unwrap finds it where a single answer has it
}

// ReadBody is what the kind's read sends: nil for most, or its Body with
// the path parameters and the ID filled in (an RPC API's Get call names the
// object in it: {"id": "{id}"}).
func ReadBody(k *descriptor.Kind, params map[string]string) any {
	if k.Read.Body == nil {
		return nil
	}
	return fillTemplates(k.Read.Body, templateVals(params, nil))
}

// MaxPages is how many pages of a list a find read follows (Link rel="next")
// before it calls the item missing.
const MaxPages = 50

// FindPaged reads a find kind's list from url, page by page while the API
// says there is a next one (GitHub's, GitLab's Link header), and returns
// the element whose find field is want.
func FindPaged(ctx context.Context, api *rest.Client, k *descriptor.Kind, url, want string) (map[string]any, error) {
	return FindPagedWith(ctx, api, k, url, want, nil)
}

// FindPagedWith is FindPaged with params filling the read's header templates.
func FindPagedWith(ctx context.Context, api *rest.Client, k *descriptor.Kind, url, want string, params map[string]string) (map[string]any, error) {
	send := ReadBody(k, params) // a POST listing (Grafana IRM's GetRoles {})
	for page := 0; url != "" && page < MaxPages; page++ {
		resp, next, err := api.DoPaged(ctx, k.Read.Method, url, send, k.Read.HeadersOf(params))
		if err != nil {
			return nil, err
		}
		if m, ok := k.FindIn(resp, want); ok {
			return m, nil
		}
		url = next
	}
	return nil, rest.ErrNotFound
}

// WrapEnvelope is obj as a single answer of the kind holds it: inside its
// Envelope, if it has one.
func WrapEnvelope(k *descriptor.Kind, obj map[string]any) any {
	if k.Envelope == "" {
		return obj
	}
	var out any = obj
	parts := strings.Split(k.Envelope, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		out = map[string]any{parts[i]: out}
	}
	return out
}

// normalize copies observed, taking the desired form of fields the API reads
// back differently but equivalently (Normalize).
func (r *Reconciler) normalize(desired, observed map[string]any) map[string]any {
	return Normalize(r.Kind, desired, observed)
}

// applied is what an assoc object last sent, kept in status.atProvider.
func applied(obj *unstructured.Unstructured) map[string]any {
	m, ok, _ := unstructured.NestedMap(obj.Object, "status", "atProvider")
	if !ok || len(m) == 0 {
		return nil
	}
	return m
}

// syncAssoc makes sure the link the spec describes exists; when the spec
// changed, it first undoes the link last applied.
func (r *Reconciler) syncAssoc(ctx context.Context, api *rest.Client, params map[string]string, body, prev map[string]any) (map[string]any, string, error) {
	k := r.Kind
	readPath, err := rest.Expand(k.Read.Path, params)
	if err != nil {
		return nil, "", err
	}
	var list []any // every page's, until the link is on one (Link rel="next")
	for url, page := rest.WithQuery(readPath, k.Read.QueryOf(params)), 0; url != "" && page < MaxPages; page++ {
		resp, next, err := api.DoPaged(ctx, k.Read.Method, url, nil, k.Read.HeadersOf(params))
		if errors.Is(err, rest.ErrNotFound) {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("read: %w", err)
		}
		var l []any
		if k.Read.Find != nil {
			l, _ = descriptor.Dig(resp, k.ListAt()).([]any)
		} else {
			l, _ = resp.([]any)
		}
		list = append(list, l...)
		if Holds(l, body, k.IsWriteOnly) {
			break
		}
		url = next
	}
	if Holds(list, body, k.IsWriteOnly) {
		return body, "unchanged", nil
	}
	action := "created"
	if prev != nil && Drift(prev, body, nil) != "" {
		if err := r.undo(ctx, api, params, prev); err != nil {
			return nil, "", fmt.Errorf("undoing the previous link: %w", err)
		}
		action = "updated"
	}
	path, err := rest.Expand(k.Create.Path, params)
	if err != nil {
		return nil, "", err
	}
	if _, err := api.DoWith(ctx, k.Create.Method, rest.WithQuery(path, k.Create.QueryOf(params)), Request(k, k.Create, body), k.Create.HeadersOf(params)); err != nil {
		return nil, "", fmt.Errorf("create: %w", err)
	}
	return body, action, nil
}

func (r *Reconciler) undo(ctx context.Context, api *rest.Client, params map[string]string, body map[string]any) error {
	op := r.Kind.Delete
	path, err := rest.Expand(op.Path, params)
	if err != nil {
		return err
	}
	var send map[string]any
	if op.SendBody {
		send = body
	}
	// A query template is the undone link's field (Kafka's DELETE .../acls?principal=...).
	if _, err := api.DoWith(ctx, op.Method, rest.WithQuery(path, op.QueryFrom(body, params)), Request(r.Kind, op, send), op.HeadersOf(params)); err != nil && !errors.Is(err, rest.ErrNotFound) {
		return err
	}
	return nil
}

// skip says which top-level fields drift checks leave out: write-only ones,
// and, for kinds whose reads are undescribed, those the API did not return.
func (r *Reconciler) skip(obs map[string]any) func(string) bool {
	k := r.Kind
	if !k.CompareReturnedOnly {
		return k.IsWriteOnly
	}
	return func(f string) bool {
		if k.IsWriteOnly(f) {
			return true
		}
		if strings.Contains(f, ".") {
			return false // only top-level fields can be missing from an undescribed read
		}
		_, ok := obs[f]
		return !ok
	}
}

// hasUnseen: the body sets a field a read cannot confirm (write-only, or not
// returned by a kind whose reads are undescribed). Such a spec is written
// once each time it changes, rather than on every resync.
func (r *Reconciler) hasUnseen(body, obs map[string]any) bool {
	if r.hasWriteOnly(body) {
		return true
	}
	if r.Kind.CompareReturnedOnly {
		for f := range body {
			if _, ok := obs[f]; !ok {
				return true
			}
		}
	}
	return false
}

// wait waits d, or until ctx is done (its error then); Wait replaces it.
func (r *Reconciler) wait(ctx context.Context, d time.Duration) error {
	if r.Wait != nil {
		return r.Wait(ctx, d)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// syncUnread writes a kind that is never read (Kind.Unread): its update,
// sent when the spec changed since it was last applied, and only then; the
// status keeps what the last write answered.
func (r *Reconciler) syncUnread(ctx context.Context, api *rest.Client, params map[string]string, body map[string]any, same, first bool, prev map[string]any) (map[string]any, string, error) {
	k := r.Kind
	if same {
		if prev == nil {
			prev = map[string]any{}
		}
		return prev, "unchanged", nil
	}
	action := "updated"
	if first {
		action = "created"
	}
	op := k.Update
	path, err := rest.Expand(op.Path, params)
	if err != nil {
		return nil, "", err
	}
	resp, err := api.DoWith(ctx, op.Method, rest.WithQuery(path, op.QueryOf(params)), Request(k, op, OpBody(op, WithUpdateID(k, body, params), params, nil)), op.HeadersOf(params))
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", strings.TrimSuffix(action, "d"), err)
	}
	obs := r.unwrap(Answer(k, op, resp))
	if obs == nil {
		obs = map[string]any{}
	}
	return obs, action, nil
}
