#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Tests for krmgen.py's API coverage (docs/coverage): python3 gen/coverage_test.py."""
import csv, json, os, sys, tempfile, unittest

sys.path.insert(0, os.path.dirname(__file__))
import krmgen  # noqa: E402

FOUND = [  # what find_resources returns: one entry per API resource the spec has
    {"item": "/api/v1/teams/{teamId}", "shape": "crud"},
    {"item": "/api/v1/teams/{teamId}/members/{userId}", "shape": "crud"},
    {"item": "/api/v1/monitors/{id}", "shape": "crud"},
    {"item": "/api/v2/settings/branding", "shape": "settings"},
    {"item": "/api/v1/widgets/{id}", "shape": "crud"},
]
KINDS = [  # ours: Team by its path, Branding by the Terraform type matched to its resource
    {"kind": "Team", "terraform": ["x_team"], "read": {"path": "/api/v1/teams/{id}"}},
    {"kind": "Branding", "terraform": ["x_branding_settings"], "read": {"path": "/api/v2/other"}},
]


def run(covered):
    base = tempfile.mkdtemp()
    gaps = {"x/x": {"gaps": ["x_team", "x_team_member", "x_branding_settings"], **({"covered": covered} if covered else {})}}
    json.dump(gaps, open(os.path.join(base, "gaps.json"), "w"))
    cfg = {"title": "X", "terraform": {"provider": "x/x", "prefix": "x", "gaps": "gaps.json"}}
    found = [dict(r) for r in FOUND]
    krmgen.match(found, gaps["x/x"]["gaps"], "x", {})  # as build() does: every found resource gains its "tf"
    krmgen.coverage(cfg, base, {}, "x", found, KINDS)
    out = tempfile.mkdtemp()
    krmgen.write_coverage(out, cfg, base)
    d = os.path.join(out, "docs", "coverage")
    rows = {r["resource"]: r for r in csv.DictReader(open(os.path.join(d, "resources.csv")))}
    svcs = {r["service"]: r for r in csv.DictReader(open(os.path.join(d, "services.csv")))}
    return rows, svcs, open(os.path.join(d, "README.md")).read()


class Coverage(unittest.TestCase):
    def test_every_found_resource_is_a_row(self):
        rows, _, _ = run(None)
        self.assertEqual(sorted(rows), sorted(r["item"] for r in FOUND))

    def test_ours_by_path_and_by_terraform_type(self):
        rows, _, _ = run(None)
        self.assertEqual(rows["/api/v1/teams/{teamId}"]["ours_kinds"], "Team")              # its path, placeholder names aside
        self.assertEqual(rows["/api/v2/settings/branding"]["ours_kinds"], "Branding")      # the Terraform type matched to it
        self.assertEqual(rows["/api/v1/widgets/{id}"]["ours_kinds"], "")                   # neither

    def test_official_only_from_the_covered_list(self):
        rows, svcs, readme = run(None)
        self.assertFalse(any(r["official_terraform"] for r in rows.values()))
        self.assertIn("has none, so nothing counts as official", readme)
        rows, svcs, readme = run(["x_monitor"])
        self.assertEqual(rows["/api/v1/monitors/{id}"]["official_terraform"], "x_monitor")
        self.assertEqual(svcs["monitors"]["resources_official"], "1")

    def test_services_and_totals(self):
        _, svcs, readme = run(["x_monitor"])
        self.assertEqual(svcs["teams"]["resources_total"], "2")          # the version segment is not the service
        self.assertEqual(svcs["teams"]["resources_ours_beyond"], "1")
        self.assertEqual(svcs["settings"]["resources_ours_beyond"], "1")
        self.assertIn("**3 of 5** API resources have a kind (60%): 1 by X's own operators, 2 more", readme)


if __name__ == "__main__":
    unittest.main()
