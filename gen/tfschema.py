#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Fetch the Terraform provider's own schema for a vendor's gap types.

  tfschema.py <vendor.yaml>
  tfschema.py --strip <terraform-schema.json>   drop any prose from a written file

Runs `terraform providers schema -json` for the provider and version
terraform-gaps.json pins (tf_version), keeps the gap types no kind covers
(every gap type of a vendor with no types.yaml yet; a draft kind covers
none) and vendor.yaml's hardBlocks do not rule out, and writes hack/gen/terraform-schema.json: per type, its attributes (type, required,
optional, computed, sensitive, deprecated) and nested blocks (nesting
mode, min and max items). Facts only: the provider is MPL-2.0, so its
prose (descriptions, from the schema or the registry docs) is never
written. krmgen.py builds a draft kind's spec from it for every gap type
no kind covers (README "Draft kinds").

The terraform CLI is $TERRAFORM, else `terraform` on PATH, else
TERRAFORM_VERSION downloaded from releases.hashicorp.com: its SHA256SUMS
checked against HashiCorp's signing key (gpg, by fingerprint) and the
archive against SHA256SUMS. `terraform init` checks the provider's own
signature.

Run it when the gaps file moves to a new provider version, and commit the
result: the generator reads only the committed file, so regenerating needs
no network.
"""
import hashlib
import io
import json
import os
import shutil
import subprocess
import sys
import tempfile
import urllib.request
import zipfile

import yaml

TERRAFORM_VERSION = "1.17.2"
RELEASES = "https://releases.hashicorp.com/terraform"
# HashiCorp's release signing key (https://www.hashicorp.com/security).
HASHICORP_KEY = "https://www.hashicorp.com/.well-known/pgp-key.txt"
HASHICORP_FPR = "C874011F0AB405110D02105534365D9472D7468F"


def fetch(url):
    req = urllib.request.Request(url, headers={"User-Agent": "krm-rest-runtime-tfschema"})
    with urllib.request.urlopen(req, timeout=300) as r:
        return r.read()


def terraform_cli(work):
    """A terraform binary: $TERRAFORM, on PATH, or the pinned release, verified."""
    if os.environ.get("TERRAFORM"):
        return os.environ["TERRAFORM"]
    if shutil.which("terraform"):
        return shutil.which("terraform")
    v, arch = TERRAFORM_VERSION, {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(os.uname().machine, "amd64")
    plat = f"{sys.platform}_{arch}"  # linux_amd64, darwin_arm64
    sums, sig = fetch(f"{RELEASES}/{v}/terraform_{v}_SHA256SUMS"), fetch(f"{RELEASES}/{v}/terraform_{v}_SHA256SUMS.sig")
    gnupg = os.path.join(work, "gnupg")
    os.makedirs(gnupg, mode=0o700)
    for name, data in (("SHA256SUMS", sums), ("SHA256SUMS.sig", sig), ("hashicorp.asc", fetch(HASHICORP_KEY))):
        open(os.path.join(work, name), "wb").write(data)
    env = dict(os.environ, GNUPGHOME=gnupg)
    subprocess.run(["gpg", "--batch", "--quiet", "--import", os.path.join(work, "hashicorp.asc")], env=env, check=True)
    st = subprocess.run(["gpg", "--batch", "--status-fd", "1", "--verify", os.path.join(work, "SHA256SUMS.sig"), os.path.join(work, "SHA256SUMS")],
                        env=env, capture_output=True, text=True)
    if st.returncode != 0 or not any(l.startswith("[GNUPG:] VALIDSIG ") and HASHICORP_FPR in l for l in st.stdout.splitlines()):
        sys.exit(f"terraform {v}: SHA256SUMS is not signed by HashiCorp's key {HASHICORP_FPR}")
    zname = f"terraform_{v}_{plat}.zip"
    want = next((l.split()[0] for l in sums.decode().splitlines() if l.endswith(" " + zname)), None)
    if not want:
        sys.exit(f"terraform {v}: no {zname} in SHA256SUMS")
    z = fetch(f"{RELEASES}/{v}/{zname}")
    if hashlib.sha256(z).hexdigest() != want:
        sys.exit(f"terraform {v}: {zname} does not match SHA256SUMS")
    zipfile.ZipFile(io.BytesIO(z)).extract("terraform", work)
    path = os.path.join(work, "terraform")
    os.chmod(path, 0o755)
    return path


def trim_attr(a):
    """An attribute as the generator needs it. A computed-only one (an
    output) keeps only that: no spec field is made of it."""
    if a.get("computed") and not a.get("optional") and not a.get("required"):
        return {"computed": True}
    out = {"type": a.get("type", "string")}
    for k in ("required", "optional", "computed", "sensitive", "deprecated"):
        if a.get(k):
            out[k] = True
    if a.get("nested_type"):  # plugin framework's nested attributes
        nt = a["nested_type"]
        out["nested"] = {"nesting_mode": nt.get("nesting_mode", "single"),
                         "attributes": {n: trim_attr(x) for n, x in sorted((nt.get("attributes") or {}).items())}}
        out.pop("type", None)
    return out


def trim_block(b):
    out = {}
    attrs = {n: trim_attr(a) for n, a in sorted((b.get("attributes") or {}).items())}
    if attrs:
        out["attributes"] = attrs
    blocks = {}
    for n, bt in sorted((b.get("block_types") or {}).items()):
        e = {"nesting_mode": bt.get("nesting_mode", "list")}
        for k in ("min_items", "max_items"):
            if bt.get(k):
                e[k] = bt[k]
        e["block"] = trim_block(bt.get("block") or {})
        blocks[n] = e
    if blocks:
        out["block_types"] = blocks
    if b.get("deprecated"):
        out["deprecated"] = True
    return out


def strip_prose(v):
    """v without any description (Terraform's prose is MPL-2.0)."""
    if isinstance(v, dict):
        return {k: strip_prose(x) for k, x in v.items() if k not in ("description", "description_kind")}
    if isinstance(v, list):
        return [strip_prose(x) for x in v]
    return v


def main():
    if sys.argv[1] == "--strip":
        doc = strip_prose(json.load(open(sys.argv[2])))
        doc["source"] = "terraform providers schema -json; facts only (no descriptions: the provider is MPL-2.0)"
        with open(sys.argv[2], "w") as f:
            json.dump(doc, f, indent=1, sort_keys=True)
            f.write("\n")
        return
    cfg_path = sys.argv[1]
    base = os.path.dirname(os.path.abspath(cfg_path))
    cfg = yaml.safe_load(open(cfg_path))
    tf = cfg["terraform"]
    provider, prefix = tf["provider"], tf["prefix"]
    g = json.load(open(os.path.join(base, tf["gaps"])))[provider]
    version, gaps = g["tf_version"], sorted(g["gaps"])
    types_yaml = os.path.join(base, "..", "..", "types.yaml")
    if os.path.exists(types_yaml):
        covered = {t for k in (yaml.safe_load(open(types_yaml)) or {}).get("kinds") or []
                   if k.get("readiness") != "draft" for t in k.get("terraform") or []}
        gaps = [t for t in gaps if t not in covered]
    gaps = [t for t in gaps if t not in (cfg.get("hardBlocks") or {})]  # no draft is made for these
    work = tempfile.mkdtemp(prefix="tfschema-")
    try:
        cli = terraform_cli(work)
        cli_version = json.loads(subprocess.run([cli, "version", "-json"], capture_output=True, text=True, check=True).stdout)["terraform_version"]
        proj = os.path.join(work, "p")
        os.makedirs(proj)
        local = provider.split("/")[1].lower().replace("-", "_")
        open(os.path.join(proj, "main.tf"), "w").write(
            'terraform {\n  required_providers {\n    %s = { source = "%s", version = "%s" }\n  }\n}\n' % (local, provider, version))
        env = dict(os.environ, TF_IN_AUTOMATION="1", CHECKPOINT_DISABLE="1")
        subprocess.run([cli, "init", "-input=false", "-no-color", "-backend=false"], cwd=proj, env=env, check=True, capture_output=True)
        out = subprocess.run([cli, "providers", "schema", "-json"], cwd=proj, env=env, check=True, capture_output=True, text=True).stdout
    finally:
        shutil.rmtree(work, ignore_errors=True)
    schemas = json.loads(out)["provider_schemas"]
    key = next(k for k in schemas if k.lower().endswith("/" + provider.lower()))
    rs = schemas[key].get("resource_schemas") or {}
    resources, missing = {}, []
    for t in gaps:
        if t not in rs:
            missing.append(t)
            continue
        resources[t] = trim_block(rs[t].get("block") or {})
    doc = {"provider": provider, "version": version, "terraform": cli_version,
           "source": f"terraform providers schema -json (terraform {cli_version}); facts only (no descriptions: the provider is MPL-2.0)",
           "resources": strip_prose(resources)}
    with open(os.path.join(base, "terraform-schema.json"), "w") as f:
        json.dump(doc, f, indent=1, sort_keys=True)
        f.write("\n")
    print(f"{provider} {version}: schema of {len(resources)} of {len(gaps)} uncovered gap types (terraform {cli_version})"
          + (f"; not in the provider: {', '.join(missing)}" if missing else ""), file=sys.stderr)


if __name__ == "__main__":
    main()
