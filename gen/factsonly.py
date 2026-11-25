#!/usr/bin/env python3
# Copyright 2026 High Five Labs
# SPDX-License-Identifier: Apache-2.0
"""purge.py: make a vendor repo's hand-written generator inputs facts only.

  purge.py CHECKOUT [--spec DIR] [--check]

--check changes nothing: it lists what a purge would remove and exits 1 if
anything (CI runs it as krm-rest-runtime's gen/factsonly.py).

In CHECKOUT (a working tree, edited in place):
  - hack/gen/terraform-docs.json is deleted;
  - hack/gen/terraform-schema.json loses every "description",
    "description_kind" and "deprecation_message";
  - in the hand-written inputs (hack/gen/kinds, hack/gen/supplement,
    hack/gen/sql, hack/handwritten, hack/gen/vendor.yaml), a `description:`
    stays only when every word 5-gram of it is in the vendor's own spec
    files under --spec. Any other description may come from the Terraform
    provider's docs, and a paraphrase is still derived from them, so it goes;
  - in the same inputs, a comment block that names the Terraform provider's
    source (terraform-provider-..., pkg/sdk, pkg/resources) goes.
Each edited YAML file must load to the same data as before minus the
removed descriptions; otherwise the file is left as it was and reported.
Python and other inputs are only reported. Prints a summary; exit 1 if any
file needs a hand edit.
"""
import json
import os
import textwrap
import re
import sys

import yaml

WORD = re.compile(r"[a-z0-9]+")
INPUT_DIRS = ("hack/gen/kinds", "hack/gen/supplement", "hack/gen/sql", "hack/handwritten")
INPUT_FILES = ("hack/gen/vendor.yaml",)
# Non-YAML inputs read by hand: their descriptions are our own words.
REVIEWED = {"hack/handwritten/merge.py", "hack/gen/supplement/apps.py"}
CITES = re.compile(r"terraform-provider-|pkg/sdk/|pkg/resources/|registry\.terraform\.io")


def grams(s, n=5):
    w = WORD.findall(s.lower())
    if len(w) < n:
        return {" ".join(w)} if w else set()
    return {" ".join(w[i:i + n]) for i in range(len(w) - n + 1)}


def spec_grams(d):
    out = set()
    if d and os.path.isdir(d):
        for root, _, files in os.walk(d):
            for f in files:
                with open(os.path.join(root, f), errors="replace") as fh:
                    out |= grams(fh.read())
    return out


CHECK = False


def strip_schema(path):
    with open(path) as fh:
        doc = json.load(fh)
    n = 0

    def walk(x):
        nonlocal n
        if isinstance(x, dict):
            for k in ("description", "description_kind", "deprecation_message"):
                if k in x:
                    del x[k]
                    n += 1
            for v in x.values():
                walk(v)
        elif isinstance(x, list):
            for v in x:
                walk(v)

    walk(doc)
    if n and not CHECK:
        with open(path, "w") as fh:
            json.dump(doc, fh, indent=1, sort_keys=True)
            fh.write("\n")
    return n


def norm(x):
    """None and {} alike: dropping a mapping's only key leaves null."""
    if isinstance(x, dict):
        return {k: norm(v) for k, v in x.items()} or None
    if isinstance(x, list):
        return [norm(v) for v in x]
    return x


def count_desc(x):
    if isinstance(x, dict):
        return sum(1 for k, v in x.items() if k == "description" and isinstance(v, str)) + sum(count_desc(v) for v in x.values())
    if isinstance(x, list):
        return sum(count_desc(v) for v in x)
    return 0


# A description is documentation only beside one of these keys (a schema, a
# field, a kind, an operation or a parameter). Below a data key it is a value
# the reconciler sends or expects (a sample object's own description field),
# and stays.
SCHEMA_KEYS = {"type", "properties", "items", "enum", "format", "$ref", "allOf", "oneOf", "anyOf",
               "required", "readOnly", "writeOnly", "nullable", "additionalProperties", "pattern",
               "minimum", "maximum", "minLength", "maxLength", "operationId", "summary", "parameters",
               "responses", "requestBody", "in", "kind", "shape", "fields", "plural", "content",
               "schema", "x-kubernetes-preserve-unknown-fields"}
DATA_KEYS = {"sample", "samples", "example", "examples", "x-example", "default", "value", "values",
             "body", "match", "spec", "const"}


# Keys whose values map names to field schemas: what sits under one of them is
# a field's definition, whatever the field is called (a field named "value"
# or "default" is still a field). extraFields nests one level more (path, then
# field name).
NAME_KEYS = {"fields", "properties", "params", "schemas", "definitions", "patternProperties", "extraFields"}


def _walk(data, keep, doc, schema, names, drop):
    """Yield (dict, description) for each documentation description, and
    rebuild data without them when drop. doc: not inside data; schema: this
    dict is a field definition; names: this dict maps names to definitions."""
    if isinstance(data, list):
        out, found = [], set()
        for v in data:
            nv, f = _walk(v, keep, doc, schema, False, drop)
            out.append(nv)
            found |= f
        return out, found
    if not isinstance(data, dict):
        return data, set()
    found = set()
    is_doc = (doc and not names and isinstance(data.get("description"), str)
              and (schema or bool(SCHEMA_KEYS.intersection(data))) and not keep(data["description"]))
    if is_doc:
        found.add(data["description"])
    out = {}
    for k, v in data.items():
        if k == "description" and is_doc:
            continue
        if names:
            nv, f = _walk(v, keep, doc, True, k == "extraFields", drop)
        elif k in NAME_KEYS:
            nv, f = _walk(v, keep, doc, False, True, drop)
            if k == "extraFields" and isinstance(v, dict):
                nv, f = {}, set()
                for path, flds in v.items():
                    nv[path], ff = _walk(flds, keep, doc, False, True, drop)
                    f |= ff
        else:
            nv, f = _walk(v, keep, doc and k not in DATA_KEYS, False, False, drop)
        out[k] = nv
        found |= f
    return out, found


def drop_desc(data, keep):
    """data minus the documentation descriptions that keep() rejects."""
    return _walk(data, keep, True, False, False, True)[0]


def removable(data, keep):
    """The description strings drop_desc removes."""
    return _walk(data, keep, True, False, False, False)[1]


FLOW = re.compile(r''',\s*description:\s*("(?:[^"\\]|\\.)*"|'(?:[^']|'')*'|[^,}\n]*)''')
FLOW_FIRST = re.compile(r'''\{\s*description:\s*("(?:[^"\\]|\\.)*"|'(?:[^']|'')*'|[^,}\n]*)\s*,\s*''')
BLOCK = re.compile(r"^(\s*)description:(.*)$")


def scalar(s):
    try:
        v = yaml.safe_load("x: " + s)
        return v["x"] if isinstance(v, dict) else None
    except yaml.YAMLError:
        return None


def edit_yaml(text, keep):
    lines = text.split("\n")
    out = []
    i = 0
    removed = 0
    while i < len(lines):
        ln = lines[i]
        m = BLOCK.match(ln)
        if m and not ln.lstrip().startswith("- "):
            ind = len(m.group(1))
            j = i + 1
            # The value continues on lines indented deeper (folded, literal or plain multi-line).
            while j < len(lines) and (lines[j].strip() == "" or len(lines[j]) - len(lines[j].lstrip()) > ind):
                if lines[j].strip() == "" and (j + 1 >= len(lines) or len(lines[j + 1]) - len(lines[j + 1].lstrip()) <= ind):
                    break
                j += 1
            block = "\n".join(lines[i:j])
            try:
                v = yaml.safe_load(textwrap.dedent(block))
            except yaml.YAMLError:
                v = None
            val = v.get("description") if isinstance(v, dict) else None
            if isinstance(val, str) and not keep(val):
                removed += 1
                i = j
                continue
        if "description:" in ln and "{" in ln:
            def sub(mm, first=False):
                nonlocal removed
                val = scalar(mm.group(1))
                if isinstance(val, str) and not keep(val):
                    removed += 1
                    return "{" if first else ""
                return mm.group(0)
            ln = FLOW.sub(sub, ln)
            ln = FLOW_FIRST.sub(lambda mm: sub(mm, True), ln)
        out.append(ln)
        i += 1
    return "\n".join(out), removed


def edit_comments(text):
    lines = text.split("\n")
    out, i, removed = [], 0, 0
    while i < len(lines):
        if lines[i].lstrip().startswith("#"):
            j = i
            while j < len(lines) and lines[j].lstrip().startswith("#"):
                j += 1
            block = lines[i:j]
            if any(CITES.search(b) for b in block):
                keep = [b for b in block if re.search(r"Copyright|SPDX-License-Identifier", b)]
                out.extend(keep)
                removed += 1
            else:
                out.extend(block)
            i = j
            continue
        out.append(lines[i])
        i += 1
    return "\n".join(out), removed


def main():
    args = sys.argv[1:]
    spec_dir = None
    if "--spec" in args:
        k = args.index("--spec")
        spec_dir = args[k + 1]
        del args[k:k + 2]
    global CHECK
    if "--check" in args:
        CHECK = True
        args.remove("--check")
    root = args[0]
    spec = spec_grams(spec_dir)
    keep = lambda s: len(WORD.findall(s)) < 4 or (bool(spec) and grams(s) <= spec)
    manual, total_desc, total_cites = [], 0, 0

    docs = os.path.join(root, "hack/gen/terraform-docs.json")
    found = 0
    if os.path.exists(docs):
        found += 1
        if CHECK:
            print("FACTS hack/gen/terraform-docs.json holds the Terraform provider's prose (MPL-2.0): delete it")
        else:
            os.remove(docs)
            print("purge: deleted hack/gen/terraform-docs.json")
    schema = os.path.join(root, "hack/gen/terraform-schema.json")
    if os.path.exists(schema):
        n = strip_schema(schema)
        found += n
        if n and CHECK:
            print(f"FACTS hack/gen/terraform-schema.json: {n} descriptions (run tfschema.py --strip)")
        elif n:
            print(f"purge: hack/gen/terraform-schema.json: {n} descriptions removed")

    paths = [p for p in INPUT_FILES if os.path.exists(os.path.join(root, p))]
    for d in INPUT_DIRS:
        for r, _, files in os.walk(os.path.join(root, d)):
            paths += [os.path.relpath(os.path.join(r, f), root) for f in files]
    for rel in sorted(paths):
        p = os.path.join(root, rel)
        try:
            with open(p) as fh:
                text = fh.read()
        except UnicodeDecodeError:
            continue
        if "DO NOT EDIT" in "\n".join(text.split("\n")[:3]) or rel in REVIEWED:
            continue  # generated by a reviewed script, or read by hand
        if rel.endswith((".yaml", ".yml")):
            before = yaml.safe_load(text)
            gone = removable(before, keep)
            new, nd = edit_yaml(text, lambda v: v not in gone)
            new, nc = edit_comments(new)
            try:
                ok = norm(yaml.safe_load(new)) == norm(drop_desc(before, keep))
            except yaml.YAMLError:
                ok = False
            if not ok:
                # Fall back to writing the data back out: the license header
                # stays, other comments go.
                head = []
                for ln in text.split("\n"):
                    if not ln.startswith("#"):
                        break
                    if not CITES.search(ln):
                        head.append(ln)
                data = drop_desc(before, keep)
                new = "\n".join(head + [""]) if head else ""
                new += yaml.safe_dump(data, sort_keys=False, allow_unicode=True, width=1 << 16)
                nd = count_desc(before) - count_desc(data)
                nc = 1
                if norm(yaml.safe_load(new)) != norm(data):
                    manual.append(f"{rel}: could not rewrite; edit by hand")
                    continue
                print(f"purge: {rel}: rewritten from its data (comments other than the header dropped)")
            if (nd or nc) and CHECK:
                found += nd + nc
                print(f"FACTS {rel}: {nd} descriptions not in the vendor's spec, {nc} comments citing Terraform provider source")
                continue
            if nd or nc:
                with open(p, "w") as fh:
                    fh.write(new)
                total_desc += nd
                total_cites += nc
                print(f"purge: {rel}: {nd} descriptions, {nc} citation comments removed")
        else:
            if re.search(r"description", text) or CITES.search(text):
                manual.append(f"{rel}: not YAML; check its descriptions and citations by hand")
    print(f"purge: {total_desc} descriptions and {total_cites} citation comments removed")
    for m in manual:
        print(f"MANUAL {m}")
    if CHECK:
        if found or manual:
            print("Hand-written generator inputs must be facts only: describe a field only in the vendor's "
                  "own specification's words, and never from the Terraform provider's docs or source (MPL-2.0).")
        sys.exit(1 if found or manual else 0)
    sys.exit(1 if manual else 0)


if __name__ == "__main__":
    main()
