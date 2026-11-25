// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	apifake "github.com/highfivelabs/krm-rest-runtime/fake"
	"github.com/highfivelabs/krm-rest-runtime/reconciler"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// Call is one request the controller made to the (fake) API.
type Call struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Query  string `json:"query,omitempty"`
	Body   any    `json:"body,omitempty"`
	Status int    `json:"status"`
}

// ObjectRun is what the controller did with one object.
type ObjectRun struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
	// Reason and Message are the Ready condition's.
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// Create holds the calls of the first reconcile; Delete those of
	// deleting the object afterwards.
	Create []Call         `json:"create"`
	Delete []Call         `json:"delete,omitempty"`
	Status map[string]any `json:"status,omitempty"`
	Error  string         `json:"error,omitempty"`
}

// ManifestRun is what RunManifest did.
type ManifestRun struct {
	Objects []ObjectRun `json:"objects"`
	Notes   []string    `json:"notes,omitempty"`
}

// recorder wraps the fake API and keeps every request.
type recorder struct {
	h     http.Handler
	mu    sync.Mutex
	calls []Call
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	data, _ := io.ReadAll(req.Body)
	req.Body = io.NopCloser(bytes.NewReader(data))
	rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	r.h.ServeHTTP(rw, req)
	c := Call{Method: req.Method, Path: req.URL.Path, Query: req.URL.RawQuery, Status: rw.status}
	if len(data) > 0 {
		var v any
		if json.Unmarshal(data, &v) == nil {
			c.Body = v
		} else {
			c.Body = fmt.Sprintf("<%d bytes of %s>", len(data), req.Header.Get("Content-Type"))
		}
	}
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
}

func (r *recorder) take() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.calls
	r.calls = nil
	return out
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(s int) { w.status = s; w.ResponseWriter.WriteHeader(s) }

// RunManifest applies every object of f's kinds in docs to an in-memory
// cluster whose Connection points at the fake API, and runs the
// controllers on them as the manager would: in passes, so an object that
// refers to another (spec.fieldRefs) waits until that one is Ready. It
// records each object's calls. With del, it then deletes them in reverse
// order and records those calls too. A Connection, its Secret and Secrets
// that secretRefs name are made up where docs leave them out. Nothing real
// is reached: the fake answers as the descriptors say the vendor's API does.
func RunManifest(ctx context.Context, f *descriptor.File, vendor *rest.Vendor, hooks map[string][]reconciler.Hook, docs []map[string]any, del bool) (*ManifestRun, error) {
	api := apifake.New(f)
	rec := &recorder{h: api}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(f.ConnectionGVK(), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)

	p := &ManifestRun{}
	type item struct {
		k   *descriptor.Kind
		obj *unstructured.Unstructured
	}
	var items []item
	var objs []client.Object
	secrets := map[string]*corev1.Secret{}
	conns := map[string]bool{}
	for i, d := range docs {
		u := &unstructured.Unstructured{Object: d}
		if u.GetNamespace() == "" {
			u.SetNamespace("default")
		}
		apiVersion, kind := u.GetAPIVersion(), u.GetKind()
		switch {
		case apiVersion == "v1" && kind == "Secret":
			s := &corev1.Secret{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, s); err != nil {
				return nil, fmt.Errorf("document %d: %w", i, err)
			}
			if s.Data == nil {
				s.Data = map[string][]byte{}
			}
			for k, v := range s.StringData {
				s.Data[k] = []byte(v)
			}
			s.StringData = nil
			secrets[s.Namespace+"/"+s.Name] = s
		case u.GroupVersionKind() == f.ConnectionGVK():
			conns[u.GetNamespace()+"/"+u.GetName()] = true
			// Point it at the fake, whatever it says.
			_ = unstructured.SetNestedField(u.Object, srv.URL, "spec", "baseURL")
			objs = append(objs, u)
		default:
			k := kindByGVK(f, apiVersion, kind)
			if k == nil {
				p.Notes = append(p.Notes, fmt.Sprintf("document %d (%s %s) is not a %s kind; skipped", i, apiVersion, kind, f.Vendor))
				continue
			}
			if k.IsDraft() {
				p.Notes = append(p.Notes, fmt.Sprintf("document %d: %s; skipped", i, k.DraftReason()))
				continue
			}
			mapper.Add(k.GVK(), meta.RESTScopeNamespace)
			items = append(items, item{k: k, obj: u})
			objs = append(objs, u)
		}
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("the manifest holds no %s objects", f.Vendor)
	}
	// Whatever the manifest leaves out, made up so the controller can run:
	// the Connection and its Secret, and Secrets that secretRefs name.
	for _, it := range items {
		ns := it.obj.GetNamespace()
		spec, _, _ := unstructured.NestedMap(it.obj.Object, "spec")
		connName := "default"
		if ref, ok := spec["connectionRef"].(map[string]any); ok {
			if n, _ := ref["name"].(string); n != "" {
				connName = n
			}
		}
		if !conns[ns+"/"+connName] {
			conn := &unstructured.Unstructured{}
			conn.SetGroupVersionKind(f.ConnectionGVK())
			conn.SetNamespace(ns)
			conn.SetName(connName)
			_ = unstructured.SetNestedField(conn.Object, srv.URL, "spec", "baseURL")
			_ = unstructured.SetNestedField(conn.Object, "plan-credentials", "spec", "secretRef", "name")
			objs = append(objs, conn)
			conns[ns+"/"+connName] = true
			creds := map[string][]byte{}
			if vendor != nil {
				for _, key := range vendor.CredentialKeys {
					creds[key] = []byte("plan-" + key)
				}
			}
			secrets[ns+"/plan-credentials"] = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "plan-credentials"}, Data: creds}
			p.Notes = append(p.Notes, fmt.Sprintf("no Connection %s/%s in the manifest; used one with made-up credentials", ns, connName))
		}
		refs, _, _ := unstructured.NestedMap(spec, "secretRefs")
		for field, ref := range refs {
			m, _ := ref.(map[string]any)
			name, _ := m["name"].(string)
			key, _ := m["key"].(string)
			s := secrets[ns+"/"+name]
			if s == nil {
				s = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Data: map[string][]byte{}}
				secrets[ns+"/"+name] = s
			}
			if _, ok := s.Data[key]; !ok {
				s.Data[key] = []byte("<secret " + name + "/" + key + ">")
				p.Notes = append(p.Notes, fmt.Sprintf("secretRefs.%s: no Secret %s with key %s in the manifest; used a placeholder value", field, name, key))
			}
		}
	}
	for _, s := range secrets {
		objs = append(objs, s)
	}
	b := clientfake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(objs...)
	for _, it := range items {
		b = b.WithStatusSubresource(it.obj)
	}
	cl := b.Build()

	recs := make([]*reconciler.Reconciler, len(items))
	plans := make([]ObjectRun, len(items))
	reqs := make([]ctrl.Request, len(items))
	for i, it := range items {
		recs[i] = &reconciler.Reconciler{Client: cl, Kind: it.k, Vendor: vendor, Connection: f.ConnectionGVK(), Hooks: hooks[it.k.Kind], Kinds: reconciler.KindsByName(f)}
		plans[i] = ObjectRun{Kind: it.k.Kind + "." + it.k.Group, Name: it.obj.GetName()}
		reqs[i] = ctrl.Request{NamespacedName: types.NamespacedName{Namespace: it.obj.GetNamespace(), Name: it.obj.GetName()}}
	}
	// Objects that refer to others (spec.fieldRefs) wait until those are
	// Ready: reconcile in passes, as the controllers would, until none is
	// left waiting or a pass changes nothing.
	waiting := make([]bool, len(items))
	for i := range waiting {
		waiting[i] = true
	}
	for pass := 0; pass <= len(items); pass++ {
		progressed := false
		for i, it := range items {
			if !waiting[i] {
				continue
			}
			op := &plans[i]
			op.Error = ""
			if _, err := recs[i].Reconcile(ctx, reqs[i]); err != nil {
				op.Error = err.Error()
			}
			op.Create = append(op.Create, rec.take()...)
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(it.k.GVK())
			if err := cl.Get(ctx, reqs[i].NamespacedName, got); err == nil {
				status, _, _ := unstructured.NestedMap(got.Object, "status")
				op.Status = status
				conds, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
				for _, cnd := range conds {
					m, _ := cnd.(map[string]any)
					if m["type"] == "Ready" {
						op.Ready = m["status"] == "True"
						op.Reason, _ = m["reason"].(string)
						op.Message, _ = m["message"].(string)
					}
				}
			}
			if op.Reason != reconciler.WaitingForReference {
				waiting[i] = false
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
	if del {
		for i := len(items) - 1; i >= 0; i-- {
			it := items[i]
			req := reqs[i]
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(it.k.GVK())
			if err := cl.Get(ctx, req.NamespacedName, got); err != nil {
				continue
			}
			if err := cl.Delete(ctx, got); err != nil {
				plans[i].Error = strings.TrimSpace(plans[i].Error + "; delete: " + err.Error())
				continue
			}
			if _, err := recs[i].Reconcile(ctx, req); err != nil {
				plans[i].Error = strings.TrimSpace(plans[i].Error + "; delete: " + err.Error())
			}
			plans[i].Delete = rec.take()
		}
	}
	p.Objects = plans
	return p, nil
}

func kindByGVK(f *descriptor.File, apiVersion, kind string) *descriptor.Kind {
	for i := range f.Kinds {
		k := &f.Kinds[i]
		if k.Group+"/"+k.Version == apiVersion && k.Kind == kind {
			return k
		}
	}
	return nil
}
