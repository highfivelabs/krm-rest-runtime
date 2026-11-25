// SPDX-License-Identifier: Apache-2.0

// Package manager runs a vendor's generic manager: one controller per
// generated kind, all on the same reconciler.
package manager

import (
	"flag"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/reconciler"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// Options for Main.
type Options struct {
	Vendor *rest.Vendor
	Types  []byte // the vendor's embedded types.yaml
	Hooks  map[string][]reconciler.Hook
}

// Main parses flags and runs the manager until it is stopped.
//
// --groups limits the controllers to some API groups (e.g.
// "projects.gitlab.hfl.dev,groups.gitlab.hfl.dev"), so a cluster that
// installed only those groups' CRDs runs only their controllers.
func Main(o Options) {
	var groups, metrics, probes string
	var leader bool
	flag.StringVar(&groups, "groups", "", "comma-separated API groups to run controllers for (default: all)")
	flag.StringVar(&metrics, "metrics-bind-address", ":8080", "metrics address")
	flag.StringVar(&probes, "health-probe-bind-address", ":8081", "health probe address")
	flag.BoolVar(&leader, "leader-elect", true, "enable leader election")
	zopts := zap.Options{}
	zopts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zopts)))

	if err := run(o, groups, metrics, probes, leader); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(o Options, groups, metrics, probes string, leader bool) error {
	f, err := descriptor.Parse(o.Types)
	if err != nil {
		return fmt.Errorf("types.yaml: %w", err)
	}
	want := map[string]bool{}
	for _, g := range strings.Split(groups, ",") {
		if g = strings.TrimSpace(g); g != "" {
			want[g] = true
		}
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metrics},
		HealthProbeBindAddress: probes,
		LeaderElection:         leader,
		LeaderElectionID:       o.Vendor.Name + "-krm-extensions.hfl.dev",
	})
	if err != nil {
		return err
	}
	run, drafts := Controllers(f, want)
	kinds := reconciler.KindsByName(f)
	for _, k := range run {
		r := &reconciler.Reconciler{Client: mgr.GetClient(), Kind: k, Vendor: o.Vendor, Connection: f.ConnectionGVK(), Hooks: o.Hooks[k.Kind], Kinds: kinds}
		if err := r.SetupWithManager(mgr); err != nil {
			return fmt.Errorf("%s: %w", k.GVK(), err)
		}
	}
	ctrl.Log.Info("controllers registered", "vendor", o.Vendor.Name, "kinds", len(run), "draftsNotRun", len(drafts))
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}

// Controllers are the kinds the manager runs a controller for: those in
// the groups wanted (all, if none is named), less the drafts, which are
// never reconciled and are named in the second list.
func Controllers(f *descriptor.File, groups map[string]bool) (run []*descriptor.Kind, drafts []string) {
	for i := range f.Kinds {
		k := &f.Kinds[i]
		if len(groups) > 0 && !groups[k.Group] {
			continue
		}
		if k.IsDraft() {
			// A draft's calls are the generator's guesses: no controller
			// runs for it, and its CRD is not in the install bundle.
			drafts = append(drafts, k.Kind+"."+k.Group)
			continue
		}
		run = append(run, k)
	}
	return run, drafts
}
