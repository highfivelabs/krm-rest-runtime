#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Tests for examples.py's examples from Upjet references (python3 gen/examples_test.py)."""
import json, os, subprocess, sys, tempfile, unittest

import yaml

sys.path.insert(0, os.path.dirname(__file__))
import examples  # noqa: E402

TYPES = {"vendor": "x", "kinds": [
    {"kind": "Team", "group": "team.x.hfl.dev", "version": "v1alpha1", "shape": "crud", "id": {"param": "id", "from": "id"},
     "sample": {"name": "example-name"}},
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


class Extractors(unittest.TestCase):
    T = {"kind": "Group", "id": {"param": "id", "from": "id"}}

    def ref(self, x, **kw):
        return {"field": "group_id", "target": "okta_group", "targetKind": "group.okta.upbound.io/Group", "extractor": x, **kw}

    def test_takes_id(self):
        yes = [None, "github.com/crossplane/upjet/v2/pkg/resource.ExtractResourceID()",
               "github.com/healthcarecom/provider-okta/apis/group/v1alpha1.GroupID()"]
        for x in yes:
            self.assertTrue(examples.takes_id(self.ref(x), self.T), x)
        # The Terraform type's words name the kind too (okta_auth_server, kind Server).
        self.assertTrue(examples.takes_id(
            {"target": "okta_auth_server", "targetKind": "auth.okta.upbound.io/Server",
             "extractor": "github.com/healthcarecom/provider-okta/apis/auth/v1alpha1.AuthServerID()"}, self.T))
        no = ["github.com/healthcarecom/provider-okta/apis/app/v1alpha1.OauthClientID()",   # a client ID, not the kind's ID
              "github.com/healthcarecom/provider-okta/apis/user/v1alpha1.GroupID()",        # not the target's package
              "computedFieldExtractor(\"id\")",                                             # code we can't see
              "refs.ExtractParamPath(\"id\", true)"]                                        # not Upjet's
        for x in no:
            self.assertFalse(examples.takes_id(self.ref(x), self.T), x)
        # <Kind>ID() is our ID only where ours is the API's id.
        self.assertFalse(examples.takes_id(self.ref(yes[2]), {"id": {"param": "id", "from": "name"}}))
        # No status.id at all: an upsert.
        self.assertFalse(examples.takes_id(self.ref(None), {"kind": "Settings"}))

    def test_param_path(self):
        team = {"id": {"param": "id", "from": "slug"}}
        slug = 'github.com/crossplane/upjet/v2/pkg/resource.ExtractParamPath("slug",true)'
        self.assertTrue(examples.takes_id(self.ref(slug), team))
        self.assertFalse(examples.takes_id(self.ref(slug.replace("slug", "name")), team))

    def test_example(self):
        types = {"vendor": "x", "kinds": [
            {"kind": "Group", "group": "group.x.hfl.dev", "version": "v1alpha1", "shape": "crud",
             "id": {"param": "id", "from": "id"}, "sample": {"name": "g"}},
            {"kind": "Rule", "group": "group.x.hfl.dev", "version": "v1alpha1", "shape": "crud",
             "id": {"param": "id", "from": "id"}, "sample": {"groupId": "x", "ownerId": "y"}},
        ]}
        sl = {"entries": [{"ours": {"kind": "Rule", "group": "group.x.hfl.dev"}, "facts": {"references": [
            {"field": "group_id", "target": "x_group", "targetKind": "group.x.upbound.io/Group",
             "extractor": "example.com/provider-x/apis/group/v1alpha1.GroupID()"},
            {"field": "owner_id", "target": "x_group", "targetKind": "group.x.upbound.io/Group",
             "extractor": "example.com/provider-x/apis/group/v1alpha1.OwnerID()"},
        ]}, "targetsOurs": {"x_group": ["group.x.hfl.dev/Group"]}}]}
        d = write_repo(types, sl)
        generate(d)
        _, rule = list(yaml.safe_load_all(open(os.path.join(d, "examples", "Rule", "with-Group.yaml"))))
        self.assertEqual(rule["spec"]["fieldRefs"], {"groupId": {"kind": "Group", "name": "example-group"}})
        self.assertEqual(rule["spec"]["ownerId"], "y")


# Values other than status.id: an upsert's name, and a value the live test
# reads from the target (GitHub's node_id, which GraphQL kinds take).
VALUE_TYPES = {"vendor": "x", "kinds": [
    {"kind": "Repository", "group": "repo.x.hfl.dev", "version": "v1alpha1", "shape": "crud",
     "create": {"method": "POST", "path": "/orgs/{owner}/repos"}, "read": {"method": "GET", "path": "/repos/{owner}/{id}"},
     "params": [{"name": "owner", "field": "owner"}], "id": {"param": "id", "from": "name"},
     "sample": {"owner": "the-org", "name": "example-repo"}, "serverFields": {"node_id": "R_x"}},
    {"kind": "Protection", "group": "repo.x.hfl.dev", "version": "v1alpha1", "shape": "crud",
     "create": {"method": "POST", "path": "/graphql", "graphql": {"operation": "C"}},
     "read": {"method": "POST", "path": "/graphql", "graphql": {"operation": "R"}},
     "id": {"param": "id", "from": "id"}, "sample": {"repositoryId": "R_example", "pattern": "main"}},
    {"kind": "OrgSecret", "group": "org.x.hfl.dev", "version": "v1alpha1", "shape": "upsert",
     "read": {"method": "GET", "path": "/orgs/{org}/secrets/{secret_name}"},
     "params": [{"name": "org", "field": "org"}, {"name": "secret_name", "field": "secretName"}],
     "sample": {"org": "the-org", "secretName": "S"}},
    {"kind": "OrgSecretRepo", "group": "org.x.hfl.dev", "version": "v1alpha1", "shape": "upsert",
     "read": {"method": "GET", "path": "/orgs/{org}/secrets/{secret_name}/repositories/{repository_id}"},
     "params": [{"name": "org", "field": "org"}, {"name": "secret_name", "field": "secretName"}, {"name": "repository_id", "field": "repositoryID"}],
     "sample": {"org": "example-org", "secretName": "example-secret-name", "repositoryID": "1"}},
    {"kind": "Plugin", "group": "sys.x.hfl.dev", "version": "v1alpha1", "shape": "upsert",
     "read": {"method": "GET", "path": "/sys/plugins/catalog/{type}/{name}"},
     "params": [{"name": "type", "field": "type"}, {"name": "name", "field": "name"}], "sample": {"type": "secret", "name": "p"}},
    {"kind": "Pin", "group": "sys.x.hfl.dev", "version": "v1alpha1", "shape": "upsert",
     "read": {"method": "GET", "path": "/sys/plugins/pins/{type}/{name}"},
     "params": [{"name": "type", "field": "type"}, {"name": "name", "field": "name"}], "sample": {"type": "x", "name": "y"}},
]}
VALUE_SLICE = {"entries": [
    {"ours": {"kind": "Protection", "group": "repo.x.hfl.dev"},
     "facts": {"references": [{"field": "repository_id", "target": "x_repository"}]},
     "targetsOurs": {"x_repository": ["repo.x.hfl.dev/Repository"]}},
    {"ours": {"kind": "OrgSecretRepo", "group": "org.x.hfl.dev"},
     "facts": {"references": [{"field": "secret_name", "target": "x_org_secret"}]},
     "targetsOurs": {"x_org_secret": ["org.x.hfl.dev/OrgSecret"]}},
    {"ours": {"kind": "Pin", "group": "sys.x.hfl.dev"},
     "facts": {"references": [
         {"field": "name", "target": "x_plugin", "extractor": 'github.com/crossplane/upjet/v2/pkg/resource.ExtractParamPath("name",false)'},
         {"field": "type", "target": "x_plugin", "extractor": 'github.com/crossplane/upjet/v2/pkg/resource.ExtractParamPath("type",true)'},
     ]},
     "targetsOurs": {"x_plugin": ["sys.x.hfl.dev/Plugin"]}},
]}
VALUE_LIVE = {
    "before": [{"method": "POST", "path": "/orgs/{{cred:org}}/repos", "body": {"name": "{{run}}"}}],
    "kinds": [{"kind": "Protection", "replaceSample": True,
               "before": [{"method": "GET", "path": "/repos/{{cred:org}}/{{run}}", "save": {"repoNode": "node_id"}}],
               "spec": {"repositoryId": "{{var:repoNode}}", "pattern": "{{name}}/*"}}],
}


class Values(unittest.TestCase):
    def repo(self, live=True):
        d = write_repo(VALUE_TYPES, VALUE_SLICE)
        if live:
            os.makedirs(os.path.join(d, "hack", "live"))
            yaml.safe_dump(VALUE_LIVE, open(os.path.join(d, "hack", "live", "live.yaml"), "w"))
        return d

    def refs(self, d, kind):
        refs = examples.upjet_refs(examples.Vendor(d))
        return [(f, t["kind"], how) for (g, k), rs in refs.items() if k == kind for f, t, how in rs]

    def test_upsert_name(self):
        d = self.repo()
        self.assertEqual(self.refs(d, "OrgSecretRepo"), [("org", "OrgSecret", ("same", "org")), ("secretName", "OrgSecret", ("spec", "secret_name"))])
        generate(d)
        _, sr = list(yaml.safe_load_all(open(os.path.join(d, "examples", "OrgSecretRepo", "with-OrgSecret.yaml"))))
        self.assertEqual(sr["spec"]["org"], "the-org")
        self.assertEqual(sr["spec"]["fieldRefs"], {"secretName": {"kind": "OrgSecret", "name": "example-orgsecret", "fieldPath": "spec.secretName"}})

    def test_param_path_spec(self):
        # name: the plugin's spec.name. type: ExtractParamPath(..., true)
        # is the plugin's status, which an upsert's isn't known to hold.
        self.assertEqual(self.refs(self.repo(), "Pin"), [("name", "Plugin", ("spec", "name"))])

    def test_live_value(self):
        d = self.repo()
        self.assertEqual(self.refs(d, "Protection"), [("repositoryId", "Repository", ("path", "status.atProvider.node_id"))])
        generate(d)
        _, pr = list(yaml.safe_load_all(open(os.path.join(d, "examples", "Protection", "with-Repository.yaml"))))
        self.assertEqual(pr["spec"]["fieldRefs"], {"repositoryId": {"kind": "Repository", "name": "example-repository",
                                                                    "fieldPath": "status.atProvider.node_id"}})
        # Without the live test, Upjet's default is status.id: the name.
        self.assertEqual(self.refs(self.repo(live=False), "Protection"), [("repositoryId", "Repository", None)])


# Cloudflare's shape: a consumer of a queue, both in one account.
def acct_kind(kind, path, params, sample, idp):
    return {"kind": kind, "group": "q.x.hfl.dev", "version": "v1alpha1", "shape": "crud",
            "create": {"method": "POST", "path": path.rsplit("/", 1)[0]}, "read": {"method": "GET", "path": path},
            "params": [{"name": n, "field": f} for n, f in params], "id": {"param": idp, "from": "id"}, "sample": sample}


SHARED_TYPES = {"vendor": "x", "kinds": [
    acct_kind("Account", "/accounts/{account_id}", [], {"name": "a"}, "account_id"),
    acct_kind("Queue", "/accounts/{account_id}/queues/{queue_id}", [("account_id", "accountID")],
              {"accountID": "example-account-id", "queue_name": "q"}, "queue_id"),
    acct_kind("Consumer", "/accounts/{account_id}/queues/{queue_id}/consumers/{consumer_id}",
              [("account_id", "accountID"), ("queue_id", "queueID")], {"accountID": "example-account-id", "queueID": "x", "type": "worker"}, "consumer_id"),
]}


def shared_slice(queue_refs=True):
    acct = {"x_account": ["q.x.hfl.dev/Account"]}
    entries = [{"ours": {"kind": "Consumer", "group": "q.x.hfl.dev"},
                "facts": {"references": [{"field": "account_id", "target": "x_account"}, {"field": "queue_id", "target": "x_queue"}]},
                "targetsOurs": {**acct, "x_queue": ["q.x.hfl.dev/Queue"]}}]
    if queue_refs:
        entries.append({"ours": {"kind": "Queue", "group": "q.x.hfl.dev"},
                        "facts": {"references": [{"field": "account_id", "target": "x_account"}]}, "targetsOurs": acct})
    return {"entries": entries}


class SharedParents(unittest.TestCase):
    def test_one_account(self):
        d = write_repo(SHARED_TYPES, shared_slice())
        generate(d)
        acct, queue, consumer = list(yaml.safe_load_all(open(os.path.join(d, "examples", "Consumer", "with-Account-and-Queue.yaml"))))
        a = {"kind": "Account", "name": "example-account"}
        self.assertEqual(queue["spec"]["fieldRefs"], {"accountID": a})
        self.assertEqual(consumer["spec"]["fieldRefs"], {"accountID": a, "queueID": {"kind": "Queue", "name": "example-queue"}})

    def test_parents_that_dont_meet(self):
        # The queue is in its sample's account, the consumer in the new
        # Account: Cloudflare would find no such queue. No example.
        d = write_repo(SHARED_TYPES, shared_slice(queue_refs=False))
        p = subprocess.run([sys.executable, os.path.join(os.path.dirname(__file__), "examples.py"), d], capture_output=True, text=True)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn("Consumer: no example from Upjet references (accountID would be both", p.stderr)
        self.assertFalse(os.path.exists(os.path.join(d, "examples", "Consumer")))


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
        self.assertEqual(list(secret["spec"]), ["owner", "secretName", "fieldRefs"])  # in its place
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
