#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Tests for examples.py's examples from Upjet references (python3 gen/examples_test.py)."""
import json, os, subprocess, sys, tempfile, unittest

import yaml

sys.path.insert(0, os.path.dirname(__file__))
import examples  # noqa: E402

TYPES = {"vendor": "x", "kinds": [
    {"kind": "Team", "group": "team.x.hfl.dev", "version": "v1alpha1", "shape": "crud", "sample": {"name": "example-name"}},
    {"kind": "Member", "group": "team.x.hfl.dev", "version": "v1alpha1", "shape": "crud",
     "sample": {"teamID": "example-teamID", "owner": {"id": "x", "type": "user_reference"}, "role": "example-role", "tags": ["a"]}},
]}

SLICE = {"entries": [{
    "ours": {"kind": "Member", "group": "team.x.hfl.dev"},
    "facts": {"terraform": "x_member", "references": [
        {"field": "team_id", "target": "x_team"},                        # string field, served: an example
        {"field": "owner", "target": "x_team"},                          # a reference object: out of fieldRefs' reach
        {"field": "role", "target": "x_team", "extractor": "Name()"},    # not the target's ID
        {"field": "tags.team_id", "target": "x_team"},                   # nested
        {"field": "unknown_id", "target": "x_unserved"},                 # no field, not served
    ]},
    "targetsOurs": {"x_team": ["team.x.hfl.dev/Team"]},
}]}


def repo(skip=None):
    d = tempfile.mkdtemp()
    yaml.safe_dump(TYPES, open(os.path.join(d, "types.yaml"), "w"))
    os.makedirs(os.path.join(d, "hack", "upjet"))
    json.dump(SLICE, open(os.path.join(d, "hack", "upjet", "provider-x.json"), "w"))
    if skip is not None:
        os.makedirs(os.path.join(d, "hack", "gen"))
        yaml.safe_dump({"upjet": skip}, open(os.path.join(d, "hack", "gen", "examples-skip.yaml"), "w"))
    return d


class UpjetExamples(unittest.TestCase):
    def test_refs(self):
        v = examples.Vendor(repo())
        refs = examples.upjet_refs(v)
        self.assertEqual([(f, t["kind"]) for f, t in refs[("team.x.hfl.dev", "Member")]], [("teamID", "Team")])

    def test_example(self):
        d = repo()
        subprocess.run([sys.executable, os.path.join(os.path.dirname(__file__), "examples.py"), d], check=True, capture_output=True)
        text = open(os.path.join(d, "examples", "Member", "with-Team.yaml")).read()
        self.assertTrue(text.startswith(examples.UPJET_HEAD))
        team, member = list(yaml.safe_load_all(text))
        self.assertEqual(team["kind"], "Team")
        self.assertNotIn("teamID", member["spec"])
        self.assertEqual(member["spec"]["fieldRefs"], {"teamID": {"kind": "Team", "name": "example-team"}})
        # A second run replaces it, and nothing else.
        subprocess.run([sys.executable, os.path.join(os.path.dirname(__file__), "examples.py"), d], check=True, capture_output=True)
        self.assertEqual(open(os.path.join(d, "examples", "Member", "with-Team.yaml")).read(), text)

    def test_skip(self):
        d = repo({"Member": "does not hold on the fake"})
        p = subprocess.run([sys.executable, os.path.join(os.path.dirname(__file__), "examples.py"), d], capture_output=True, text=True)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn("does not hold on the fake", p.stderr)
        self.assertFalse(os.path.exists(os.path.join(d, "examples", "Member")))

    def test_stale_skip_fails(self):
        d = repo({"Nothing": "stale"})
        p = subprocess.run([sys.executable, os.path.join(os.path.dirname(__file__), "examples.py"), d], capture_output=True, text=True)
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("Nothing", p.stderr)


if __name__ == "__main__":
    unittest.main()
