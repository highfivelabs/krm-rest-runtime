// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// A draft is never reconciled: Reconcile refuses it before reading the
// object (no client, no vendor needed), with an error that is not retried.
func TestReconcileRefusesDraft(t *testing.T) {
	k := &descriptor.Kind{Kind: "Trigger", Group: "event.example.hfl.dev", Version: "v1alpha1", Shape: descriptor.CRUD,
		Readiness: descriptor.ReadinessDraft, Draft: &descriptor.Draft{Open: []string{"no API in spec"}}}
	r := &Reconciler{Kind: k}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "a", Name: "b"}})
	if err == nil || !strings.Contains(err.Error(), "draft") || !errors.Is(err, reconcile.TerminalError(nil)) {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := r.SetupWithManager(nil); err == nil || !strings.Contains(err.Error(), "no API in spec") {
		t.Fatalf("SetupWithManager: %v", err)
	}
}
