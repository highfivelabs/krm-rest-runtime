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
        self.assertEqual([(f, t["kind"], how) for f, t, how in refs[("team.x.hfl.dev", "Member")]], [("teamID", "Team", None)])

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


# GitHub's shape: Terraform's repository is our repo, which no name says;
# the path does (/repos/{owner}/{repo}/... runs through /repos/{owner}/{id}).
PATH_TYPES = {"vendor": "x", "kinds": [
    {"kind": "Repository", "group": "repo.x.hfl.dev", "version": "v1alpha1", "shape": "crud",
     "create": {"method": "POST", "path": "/orgs/{owner}/repos"}, "read": {"method": "GET", "path": "/repos/{owner}/{id}"},
     "params": [{"name": "owner", "field": "owner"}], "id": {"param": "id", "from": "name"},
     "sample": {"owner": "the-org", "name": "example-repo"}},
    {"kind": "Secret", "group": "repo.x.hfl.dev", "version": "v1alpha1", "shape": "upsert",
     "read": {"method": "GET", "path": "/repos/{owner}/{repo}/secrets/{secret_name}"},
     "params": [{"name": "owner", "field": "owner"}, {"name": "repo", "field": "repo"}, {"name": "secret_name", "field": "secretName"}],
     "sample": {"owner": "example-owner", "repo": "example-repo", "secretName": "s"}},
    # Under /teams/{team_id}: no path of Repository's; and its body field
    # source names a repository the path doesn't.
    {"kind": "Mirror", "group": "repo.x.hfl.dev", "version": "v1alpha1", "shape": "crud",
     "create": {"method": "POST", "path": "/mirrors"}, "read": {"method": "GET", "path": "/mirrors/{id}"},
     "id": {"param": "id", "from": "id"}, "sample": {"source": "example-repo"}},
    {"kind": "Fork", "group": "repo.x.hfl.dev", "version": "v1alpha1", "shape": "crud",
     "create": {"method": "POST", "path": "/forks"}, "read": {"method": "GET", "path": "/forks/{id}"},
     "id": {"param": "id", "from": "id"}, "sample": {"name": "f"}},
]}
PATH_SLICE = {"entries": [
    {"ours": {"kind": "Secret", "group": "repo.x.hfl.dev"},
     "facts": {"references": [{"field": "repository", "target": "x_repository"}]},
     "targetsOurs": {"x_repository": ["repo.x.hfl.dev/Repository"]}},
    {"ours": {"kind": "Mirror", "group": "repo.x.hfl.dev"},
     "facts": {"references": [{"field": "source", "target": "x_repository"}]},
     "targetsOurs": {"x_repository": ["repo.x.hfl.dev/Repository"]}},
    {"ours": {"kind": "Fork", "group": "repo.x.hfl.dev"},
     "facts": {"references": [{"field": "upstream", "target": "x_repository"}]},     # no field, no path: none
     "targetsOurs": {"x_repository": ["repo.x.hfl.dev/Repository"]}},
]}


def write_repo(types, sl, skip=None):
    d = tempfile.mkdtemp()
    yaml.safe_dump(types, open(os.path.join(d, "types.yaml"), "w"))
    os.makedirs(os.path.join(d, "hack", "upjet"))
    json.dump(sl, open(os.path.join(d, "hack", "upjet", "provider-x.json"), "w"))
    if skip is not None:
        os.makedirs(os.path.join(d, "hack", "gen"))
        yaml.safe_dump({"upjet": skip}, open(os.path.join(d, "hack", "gen", "examples-skip.yaml"), "w"))
    return d


def generate(d):
    subprocess.run([sys.executable, os.path.join(os.path.dirname(__file__), "examples.py"), d], check=True, capture_output=True)


class ByPath(unittest.TestCase):
    def test_refs(self):
        refs = examples.upjet_refs(examples.Vendor(write_repo(PATH_TYPES, PATH_SLICE)))
        self.assertEqual([(f, t["kind"], how) for f, t, how in refs[("repo.x.hfl.dev", "Secret")]],
                         [("owner", "Repository", ("same", "owner")), ("repo", "Repository", None)])
        self.assertEqual([(f, t["kind"], how) for f, t, how in refs[("repo.x.hfl.dev", "Mirror")]], [("source", "Repository", None)])
        self.assertNotIn(("repo.x.hfl.dev", "Fork"), refs)

    def test_example(self):
        d = write_repo(PATH_TYPES, PATH_SLICE)
        generate(d)
        repo, secret = list(yaml.safe_load_all(open(os.path.join(d, "examples", "Secret", "with-Repository.yaml"))))
        # The secret's repo is the repository's ID, and its owner the
        # repository's own (not its sample's example-owner).
        self.assertEqual(secret["spec"]["owner"], "the-org")
        self.assertNotIn("repo", secret["spec"])
        self.assertEqual(secret["spec"]["fieldRefs"], {"repo": {"kind": "Repository", "name": "example-repository"}})
        self.assertFalse(os.path.exists(os.path.join(d, "examples", "Fork")))

    def test_aligned(self):
        v = examples.Vendor(write_repo(PATH_TYPES, PATH_SLICE))
        self.assertEqual(examples.aligned(v.kind("Secret"), v.kind("Repository")), {"owner": "owner", "repo": "id"})
        self.assertIsNone(examples.aligned(v.kind("Mirror"), v.kind("Repository")))
        # A literal segment that differs is another resource.
        other = dict(v.kind("Secret"), read={"method": "GET", "path": "/repositories/{owner}/{repo}/secrets/{secret_name}"})
        self.assertIsNone(examples.aligned(other, v.kind("Repository")))
        # Nor does GraphQL's one endpoint align anything.
        gq = dict(v.kind("Repository"), read={"method": "POST", "path": "/graphql", "graphql": {"operation": "R"}})
        self.assertIsNone(examples.aligned(dict(v.kind("Secret"), read=gq["read"]), gq))


if __name__ == "__main__":
    unittest.main()
