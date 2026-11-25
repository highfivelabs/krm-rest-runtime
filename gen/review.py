# Copyright 2026 High Five Labs
# SPDX-License-Identifier: Apache-2.0
"""Review a vendor repo's generation: per kind, what could go wrong against
the real API. review.py <repo-dir>  -> JSON on stdout.

Flags: empty-body (nothing to set), any-body (the body is any JSON object,
set as spec fields the CRD does not list), no-response-schema (reads undescribed;
compareReturnedOnly), immutable (no update: a change needs recreating),
replaceable (no update, but objects may opt in to re-creating in place),
all-write-only (a read shares no field with the write), write-only:N,
free-form:N (object fields with no schema), tf-many:N (one kind for N
Terraform types), caller-named (the caller picks the ID), live-failed (its
last live run failed; hack/live/records.json says why).

Each kind's readiness is "fake" or "live" (passed against the real API);
"readiness" counts them, the progress metric from fake to live. Draft kinds
(never reconciled) are listed apart, under "drafts", with their open items."""
import json, os, re, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import yaml
import krmgen as K

d = sys.argv[1]
cfgp = f"{d}/hack/gen/vendor.yaml"
cfg = yaml.load(open(cfgp), Loader=K.L)
f, crds, conn = K.build(cfg, os.path.dirname(os.path.abspath(cfgp)))
seen = {a["kind"]: a for a in K.AUDIT}
spec = cfg["specs"][0]
out = {"vendor": cfg["vendor"], "title": cfg["title"], "spec": spec.get("url"), "pinned": "sha256" in spec or bool(re.search(r"/[0-9a-f]{40}/", spec.get("url") or "")),
       "kinds": []}
gaps = json.load(open(os.path.join(os.path.dirname(cfgp), cfg["terraform"]["gaps"])))[cfg["terraform"]["provider"]]
out["tf"] = {"total": gaps["total"], "covered_by_operators": gaps.get("covered_by_operators", len(gaps.get("covered", []))), "gaps": len(gaps["gaps"]),
             "matched": len({t for k in f["kinds"] if k["readiness"] != "draft" for t in k["terraform"]})}
# Drafts (drafts.py): not kinds yet; each with what is open.
out["drafts"] = [{"kind": k["kind"], "tf": k["terraform"], "open": k["draft"]["open"]} for k in f["kinds"] if k["readiness"] == "draft"]
live = K.live_records(cfg, os.path.dirname(os.path.abspath(cfgp)))
for k in f["kinds"]:
    if k["readiness"] == "draft":
        continue
    a = seen.get(k["kind"], {})
    body, resp = set(a.get("body", [])), set(a.get("resp", []))
    flags = []
    if not body and a.get("anyBody"):
        flags.append("any-body")              # the body is any JSON object: the caller's keys, unchecked
    elif not body:
        flags.append("empty-body")            # nothing to set: the spec has no request schema
    if body and not resp and k["shape"] != "assoc":
        flags.append("no-response-schema")    # every field compared: drift on each read
    if k["shape"] == "crud" and not k.get("update"):
        # any spec change is an error until recreated, unless the object opts in to replacing
        flags.append("replaceable" if k.get("replace") else "immutable")
    wo = k.get("writeOnly") or []
    if wo and body and len(wo) == len(body):
        flags.append("all-write-only")        # read shares no field with write: drift invisible
    elif wo:
        flags.append(f"write-only:{len(wo)}")
    if a.get("free"):
        flags.append(f"free-form:{len(a['free'])}")
    if len(k["terraform"]) > 1:
        flags.append(f"tf-many:{len(k['terraform'])}")
    if k.get("id", {}).get("from") not in (None, "id") and k["shape"] == "crud":
        flags.append("caller-named")
    rec = live.get(f"{k['kind']}.{k['group']}") or {}
    if rec.get("result") == "fail":
        flags.append("live-failed")
    out["kinds"].append({"kind": k["kind"], "shape": k["shape"], "path": k["read"]["path"], "tf": k["terraform"],
                         "nbody": len(body), "readiness": k["readiness"], "flags": flags})
out["readiness"] = {r: sum(k["readiness"] == r for k in out["kinds"]) for r in ("fake", "live")}
print(json.dumps(out))
