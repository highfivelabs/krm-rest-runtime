# SPDX-License-Identifier: Apache-2.0
"""Draft kinds: a skeleton for every Terraform gap type no kind covers.

krmgen.py calls build() once every other kind is made. For each gap type
that has no kind and that vendor.yaml's hardBlocks does not rule out, it
writes a kind with readiness `draft`:

- its spec from the Terraform provider's own schema for the type
  (hack/gen/terraform-schema.json, written by tfschema.py): attributes,
  nested blocks, required, sensitive, descriptions; each field under the
  API's name where the request body of the closest API path has it,
  camelCased from Terraform's otherwise;
- its calls: the generator's best candidates, the spec's paths whose words
  are the type's (create: POST on the collection, read: GET on the item,
  update: PATCH or PUT on it, delete: DELETE on it);
- `draft.open`: every part still unresolved, by name (`delete: no DELETE on
  the item path ...`, `no API in spec: ...`), the vendor's own note on it
  first (vendor.yaml draftNotes);
- `draft.candidates`: the other paths worth a look.

A draft is never reconciled (the manager runs no controller for it, the
reconciler refuses it, the fake API has no route for it) and its CRD is
written to config/crds/drafts/, outside the install bundle. It goes away
by itself once a real kind covers its Terraform type.
"""
import copy
import re
import sys

SPEC_METHODS = ("get", "post", "put", "patch", "delete")


def check_lists(cfg, gaps, covered):
    """vendor.yaml hardBlocks and draftNotes: {tf_type: reason}, each a gap
    type no kind covers (a stale entry fails generation, so the README's
    "Not covered" stays true)."""
    hard = cfg.get("hardBlocks") or {}
    notes = cfg.get("draftNotes") or {}
    for name, table in (("hardBlocks", hard), ("draftNotes", notes)):
        for t, why in table.items():
            if t not in gaps:
                sys.exit(f"{name}: {t} is not a Terraform gap type (terraform-gaps.json)")
            if t in covered:
                sys.exit(f"{name}: {t} has a kind now; remove it from {name}")
            if not isinstance(why, str) or not why.strip():
                sys.exit(f"{name}: {t} needs a reason")
    both = sorted(set(hard) & set(notes))
    if both:
        sys.exit(f"hardBlocks and draftNotes both name {', '.join(both)}")
    return hard, notes


def tf_schema(t, h):
    """A Terraform type expression ("string", ["list", "string"], ...) as a
    structural schema."""
    if isinstance(t, str):
        return {"string": {"type": "string"}, "number": {"type": "number"}, "bool": {"type": "boolean"}}.get(
            t, {"x-kubernetes-preserve-unknown-fields": True})
    if isinstance(t, list) and len(t) == 2:
        kind, inner = t
        if kind in ("list", "set"):
            return {"type": "array", "items": tf_schema(inner, h)}
        if kind == "map":
            return {"type": "object", "additionalProperties": tf_schema(inner, h)}
        if kind == "object":
            return {"type": "object", "properties": {field_name(n, h): tf_schema(v, h) for n, v in sorted(inner.items())}}
    return {"x-kubernetes-preserve-unknown-fields": True}


def field_name(n, h):
    return h.camel(n.split("_"), upper=False) if "_" in n else n


def is_output(a):
    return a.get("computed") and not a.get("optional") and not a.get("required")


def describe(a, out):
    """A field's description from the schema's facts only: Terraform's own
    descriptions are MPL-2.0 prose and never copied."""
    if a.get("sensitive"):
        out["description"] = "Sensitive in Terraform: set it from a Secret (spec.secretRefs)."
    return out


def attr_schema(a, h):
    if a.get("nested"):
        inner = block_schema(a["nested"], h)
        mode = a["nested"].get("nesting_mode", "single")
        s = {"type": "array", "items": inner} if mode in ("list", "set") else (
            {"type": "object", "additionalProperties": inner} if mode == "map" else inner)
    else:
        s = tf_schema(a.get("type", "string"), h)
    return describe(a, dict(s))


def block_schema(b, h):
    """A Terraform block (its attributes and nested blocks, outputs left out)
    as an object schema with camelCased field names."""
    props, req = {}, []
    for n, a in sorted((b.get("attributes") or {}).items()):
        if is_output(a) or a.get("deprecated"):
            continue
        props[field_name(n, h)] = attr_schema(a, h)
        if a.get("required"):
            req.append(field_name(n, h))
    for n, bt in sorted((b.get("block_types") or {}).items()):
        if bt["block"].get("deprecated"):
            continue
        inner = block_schema(bt["block"], h)
        mode = bt.get("nesting_mode", "list")
        s = inner if mode in ("single", "group") else (
            {"type": "object", "additionalProperties": inner} if mode == "map" else {"type": "array", "items": inner})
        if mode in ("list", "set") and bt.get("max_items"):
            s["maxItems"] = bt["max_items"]
        props[field_name(n, h)] = s
        if bt.get("min_items"):
            req.append(field_name(n, h))
    out = {"type": "object", "properties": props}
    if req:
        out["required"] = sorted(req)
    if not props:
        out = {"type": "object", "x-kubernetes-preserve-unknown-fields": True}
    return out


def sample(s, name, depth=0):
    if s.get("enum"):
        return s["enum"][0]
    t = s.get("type")
    if t in ("integer", "number"):
        return 1
    if t == "boolean":
        return True
    if t == "array":
        return []
    if t == "object" or s.get("properties"):
        out = {}
        if depth < 6:
            for r in s.get("required") or []:
                if r in (s.get("properties") or {}):
                    out[r] = sample(s["properties"][r], r, depth + 1)
        return out
    if t == "string":
        return f"example-{name}".replace("_", "-")
    return "example"


class Paths:
    """Every path of the vendor's specs, with its methods and words."""

    def __init__(self, specs, cfg, h):
        ov = cfg.get("overrides") or {}
        skip = tuple(ov.get("excludePathPrefixes") or ())
        pt = ov.get("placeholderWords")
        self.h = h
        self.by = {}  # path -> (spec, {method: op})
        for sp in specs:
            for p, _, ops in sp.paths():
                if any(p.startswith(x) for x in skip):
                    continue
                ms = {m.lower(): v for m, v in ops.items() if m.lower() in SPEC_METHODS
                      and not any(str((v or {}).get("operationId", "")).startswith(x) for x in h.SKIP_OPS)}
                if ms and p not in self.by:
                    self.by[p] = (sp, ms)
        self.words = {p: h.toks(re.sub(pt, lambda m: m.group(1), p) if pt else p) for p in self.by}
        self.items = {}  # collection -> its item paths
        for p in self.by:
            if re.search(r"/\{[^}]+\}/?$", p):
                self.items.setdefault(h.family(p), []).append(p)

    def methods(self, p):
        return self.by[p][1] if p in self.by else {}

    def family_of(self, p):
        """(collection, item) for a path: an item and its collection, a
        collection and its item, or a path alone (a singleton)."""
        if re.search(r"/\{[^}]+\}/?$", p):
            c = self.h.family(p)
            return (c if c in self.by else None), p
        its = sorted(self.items.get(p.rstrip("/") if self.h.TRAILING_SLASH else p) or [])
        return (p, its[0]) if its else (None, p)

    def crudness(self, p):
        c, i = self.family_of(p)
        return len(set(self.methods(i)) | {"post" for m in self.methods(c) if m == "post"})

    def rank(self, words):
        """Paths sharing a word with the type, best first: fewest of its
        words missing (each counting twice) and other words, then fewest
        missing, most CRUD calls, shortest."""
        out = []
        for p, pw in self.words.items():
            if words & pw:
                out.append(((2 * len(words - pw) + len(pw - words), len(words - pw), -self.crudness(p), len(p), p), p))
        return [p for _, p in sorted(out)]


def unwrap(props):
    """A request body that is one object wrapped in its name ({"service": {...}})."""
    if len(props) == 1:
        (k, v), = props.items()
        if v.get("type") == "object" and v.get("properties"):
            return k, v
    return None, None


def ptoks(name, h):
    """A placeholder's or attribute's words, with the vendor's synonyms for
    the word or its plural (Atlas's {groupId} is a project)."""
    s = re.sub(r"([a-z0-9])([A-Z])", r"\1_\2", name).lower()
    out = set()
    for w in re.split(r"[^a-z0-9]+", s):
        w = h.SYN.get(w, h.SYN.get(w + "s", w))
        for y in w.split("_"):
            y = h.WORD_SYN.get(y, y)
            if y and y not in ("id", "ids"):
                out.add(h.sing(y))
    return out


def build(cfg, base, specs, gaps, prefix, kinds, crds, used, h, tf):
    """The draft kinds and their CRDs, appended to kinds and crds; returns
    {hardBlocks, draftNotes} for the report."""
    covered = {t for k in kinds for t in k.get("terraform") or ()}
    hard, notes = check_lists(cfg, set(gaps), covered)
    schema = (tf or {}).get("resources") or {}
    paths = Paths(specs, cfg, h)
    names = {(k["group"], k["kind"]) for k in kinds}
    taken = {}  # API path -> the real kind that calls it
    for k in kinds:
        for op in (k.get("create"), k.get("read"), k.get("update"), k.get("delete")):
            if op and op.get("path"):
                taken.setdefault(op["path"], k["kind"])
    for t in sorted(set(gaps) - covered - set(hard)):
        words = t[len(prefix) + 1:].split("_") if t.startswith(prefix + "_") else t.split("_")
        kind = h.camel(words)
        area = words[0] if words[0] != "zero" else "zerotrust"
        group = f"{area}.{cfg['groupSuffix']}"
        open_ = []
        if t in notes:
            open_.append("known blocker: " + " ".join(notes[t].split()))
        if (group, kind) in used or (group, kind) in names:
            open_.append(f"kind name: {kind} in {group} is another kind's; this draft is {kind}Draft")
            kind += "Draft"
        used[(group, kind)] = True
        gt = h.toks(t[len(prefix) + 1:])
        ranked = paths.rank(gt) if gt else []
        desc = {"kind": kind, "group": group, "version": "v1alpha1", "plural": h.plural(kind)}
        coll = item = None
        body_props, body_req, body_sp = {}, [], None
        if not ranked:
            open_.append(f"no API in spec: no path has the words {', '.join(sorted(gt)) or t}")
            desc["shape"] = "crud"
            desc["read"] = {}  # none found: the descriptor's read is required, and empty
        else:
            coll, item = paths.family_of(ranked[0])
            im, cm = paths.methods(item), paths.methods(coll)
            missing = sorted(gt - paths.words[ranked[0]])
            if missing:
                open_.append(f"path: the best match {ranked[0]} lacks the words {', '.join(missing)}; confirm it is this resource")
            create = ("POST", coll) if coll and "post" in cm else None
            update = ("PATCH", item) if "patch" in im else (("PUT", item) if "put" in im else None)
            if create:
                shape = "crud"
            elif "put" in im:
                shape = "upsert" if "delete" in im else "settings"
            elif "patch" in im:
                shape = "settings"
            else:
                shape = "crud"
            desc["shape"] = shape
            if create:
                desc["create"] = {"method": create[0], "path": create[1]}
            elif shape == "crud":
                if "post" in im:
                    desc["create"] = {"method": "POST", "path": item}
                    open_.append(f"create: no POST on a collection; POST {item} is the item path's own (an action?)")
                else:
                    open_.append(f"create: no POST on {coll or 'a collection of ' + item} and no PUT on the item path")
            if "get" in im:
                desc["read"] = {"method": "GET", "path": item}
            elif coll and "get" in cm:
                desc["read"] = {"method": "GET", "path": coll}
                open_.append(f"read: no GET on the item path {item}; found only in a list (GET {coll}): overrides.readFrom or listReads")
            else:
                desc["read"] = {}  # none found: the descriptor's read is required, and empty
                open_.append(f"read: no GET on {item}" + (f" or {coll}" if coll else ""))
            if update:
                desc["update"] = {"method": update[0], "path": update[1]}
            elif shape == "crud":
                open_.append(f"update: no PUT or PATCH on {item}: every field would be create-only (confirm, or overrides.updates)")
            if "delete" in im:
                desc["delete"] = {"method": "DELETE", "path": item}
            elif shape in ("crud", "upsert"):
                open_.append(f"delete: no DELETE on the item path {item} (overrides.forget if the API cannot delete it, deleteBody or deletes if another call does)")
            if item in taken:
                open_.append(f"path: {item} is {taken[item]}'s already; a second kind there needs overrides.map or creates")
            body_op, body_path = (cm.get("post"), coll) if create else (
                (im.get("post"), item) if desc.get("create") else ((im.get(update[0].lower()), item) if update else (None, None)))
            body_sp = paths.by[body_path][0] if body_op else None
            if body_op:
                bs = body_sp.body_schema(body_op)
                st = h.structural(body_sp, bs or {"type": "object"})
                body_props, body_req = dict(st.get("properties") or {}), list(st.get("required") or [])
                env, inner = unwrap(body_props)
                if env and h.attr_norm(env) not in {h.attr_norm(n) for n in (schema.get(t) or {}).get("attributes") or {}}:
                    body_props, body_req = dict(inner.get("properties") or {}), list(inner.get("required") or [])
                    desc["envelope"] = env
                    desc["bodyEnvelope"] = env
                    open_.append(f"envelope: the request body is wrapped in {env}; confirm the read answers the same")
            elif shape != "settings" or not update:
                open_.append("body: no request body in the spec for the create or update")
        # Path parameters, each a spec field of its own.
        ops = [desc.get(c) for c in ("create", "read", "update", "delete")]
        ph = []
        for op in ops:
            for p in re.findall(r"\{\+?([^}]+)\}", (op or {}).get("path") or ""):
                if p not in ph:
                    ph.append(p)
        tfb = schema.get(t)
        props, req, unconfirmed = {}, set(), []
        tf_attrs = {}
        if tfb:
            # Not Terraform's own: its resource ID (optional in SDKv2's
            # schemas) and its timeouts block.
            for n, a in (tfb.get("attributes") or {}).items():
                if not is_output(a) and not a.get("deprecated") and n != "id":
                    tf_attrs[n] = ("attr", a)
            for n, bt in (tfb.get("block_types") or {}).items():
                if not bt["block"].get("deprecated") and n != "timeouts":
                    tf_attrs[n] = ("block", bt)
        params = []
        ph_tf = {}  # placeholder -> the Terraform attribute that fills it
        for p in ph:
            pw = ptoks(p, h)
            hit = [n for n in tf_attrs if h.attr_norm(n) == h.attr_norm(p) or (pw and ptoks(n, h) == pw)]
            if len(hit) == 1:
                ph_tf[p] = hit[0]
        tf_ph = {v: k for k, v in ph_tf.items()}
        for p in ph:
            field = p if not p.startswith("current.") else None
            if field is None:
                continue
            params.append({"name": p, "field": field})
            s = {"type": "string"}
            if p in ph_tf:
                s = dict(s, description=f"Fills {{{p}}} in the API path; Terraform's {ph_tf[p]}.")
            else:
                s["description"] = f"Fills {{{p}}} in the API path."
            props[field] = s
            req.add(field)
        api = {h.attr_norm(k): k for k in body_props}
        for n, (kind_, a) in sorted(tf_attrs.items()):
            if n in tf_ph:
                continue
            key = h.attr_norm(n)
            name = api.get(key)
            if name is None:
                hits = [v for k2, v in api.items() if key.startswith(k2) and key[len(k2):] in h.TF_SUFFIXES]
                name = hits[0] if len(hits) == 1 else None
            if kind_ == "attr":
                s = attr_schema(a, h)
                required = bool(a.get("required"))
            else:
                bs = block_schema(a["block"], h)
                mode = a.get("nesting_mode", "list")
                s = bs if mode in ("single", "group") else (
                    {"type": "object", "additionalProperties": bs} if mode == "map" else {"type": "array", "items": bs})
                required = bool(a.get("min_items"))
            if name is not None and name not in props:
                api_s = copy.deepcopy(body_props[name])
                if not api_s.get("description") and s.get("description"):
                    api_s["description"] = s["description"]
                props[name] = api_s
                if required or name in body_req:
                    req.add(name)
                continue
            fname = field_name(n, h)
            if fname in props:
                fname = n
            props[fname] = s
            unconfirmed.append(fname)
            if required:
                req.add(fname)
        for name in body_req:
            # A field the API requires that Terraform names otherwise or not at all.
            if name not in props and name in body_props:
                props[name] = copy.deepcopy(body_props[name])
                req.add(name)
        if not tfb:
            if tf is None:
                open_.append("fields: no Terraform schema (hack/gen/terraform-schema.json): run krm-rest-runtime/gen/tfschema.py")
            else:
                open_.append(f"fields: terraform-schema.json has no {t}: run krm-rest-runtime/gen/tfschema.py again")
            for name, s in body_props.items():
                if name not in props:
                    props[name] = copy.deepcopy(s)
                    if name in body_req:
                        req.add(name)
        if unconfirmed:
            u = sorted(unconfirmed)
            open_.append("fields from Terraform schema; confirm against API: " + ", ".join(u[:12])
                         + (f" and {len(u) - 12} more" if len(u) > 12 else ""))
        outputs = sorted(n for n, a in ((tfb or {}).get("attributes") or {}).items() if is_output(a) and n != "id")
        if params:
            desc["params"] = params
        desc["terraform"] = [t]
        spec_schema = {"type": "object", "properties": props}
        if req:
            spec_schema["required"] = sorted(req)
        smp = sample(spec_schema, "spec")
        if smp:
            desc["sample"] = smp
        desc["readiness"] = "draft"
        alts = [p for p in ranked[1:6] if p not in (item, coll)]
        desc["draft"] = {"open": open_}
        if alts:
            desc["draft"]["candidates"] = [f"{' '.join(m.upper() for m in SPEC_METHODS if m in paths.methods(p))} {p}" for p in alts]
        kinds.append(desc)
        schema_props = dict(props)
        schema_props["connectionRef"] = {"type": "object", "description": f"The {cfg['connectionKind']} to use; default \"default\" in the same namespace.",
                                         "properties": {"name": {"type": "string"}}}
        schema_props["secretRefs"] = {"type": "object", "description": "Fill body fields from Secrets: {<field>: {name, key}}. Use for tokens, passwords and secret values.",
                                      "maxProperties": 64,
                                      "additionalProperties": {"type": "object", "required": ["name", "key"], "properties": {"name": {"type": "string"}, "key": {"type": "string"}}}}
        schema_props["fieldRefs"] = h.FIELD_REFS
        spec_schema["properties"] = schema_props
        c = h.crd(group, kind, desc["plural"], desc["read"].get("path") or item or "(none)", {"tf": [t]}, spec_schema, desc)
        top = c["spec"]["versions"][0]["schema"]["openAPIV3Schema"]
        top["description"] = (f"{kind}: DRAFT for Terraform's {t}, not reconciled (no controller runs for it). "
                              f"Open items: {'; '.join(open_)}")[:1500]
        if outputs:
            top["properties"]["status"]["properties"]["atProvider"]["description"] = (
                "What the API answers. Terraform's computed attributes: " + ", ".join(outputs))[:1000]
        crds.append(c)
    return {"hardBlocks": hard, "draftNotes": notes}
