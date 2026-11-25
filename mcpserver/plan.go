// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"

	"github.com/highfivelabs/krm-rest-runtime/kindtest"
)

// The plan's shape is kindtest's: plan_on_fake is kindtest.RunManifest.
type (
	Call       = kindtest.Call
	ObjectPlan = kindtest.ObjectRun
	Plan       = kindtest.ManifestRun
)

// plan runs the manifest's objects through the controllers on the fake API
// (kindtest.RunManifest).
func (c *catalog) plan(ctx context.Context, manifest string, del bool, o Options) (*Plan, error) {
	docs, err := decodeDocs(manifest)
	if err != nil {
		return nil, err
	}
	return kindtest.RunManifest(ctx, c.file, o.Vendor, o.Hooks, docs, del)
}
