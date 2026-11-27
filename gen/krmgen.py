#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Generate a vendor's kinds from its OpenAPI spec and Terraform's resource list.

  krmgen.py <vendor.yaml> <out-dir>

vendor.yaml names the spec files, the Terraform provider, how the API wraps
responses, and hand corrections. The generator keeps only API resources that
stand in for a Terraform resource the vendor's own operator lacks (the gap
list), so every kind it writes fills a known gap.

Writes into <out-dir>: types.yaml (the descriptors the manager embeds),
config/crds/<group>_<plural>.yaml (one CRD per kind, plus the Connection CRD),
and zz_generated.report.md (every kind, its API paths, its Terraform types).
"""
import copy, json, os, re, sys

import yaml

import drafts
import graphqlgen

class L(getattr(yaml, "CSafeLoader", yaml.SafeLoader)):
    """Leaves dates as strings: specs carry example dates that are not valid ones."""


L.yaml_implicit_resolvers = {k: [(t, r) for t, r in v if t != "tag:yaml.org,2002:timestamp"]
                             for k, v in L.yaml_implicit_resolvers.items()}


class Dumper(yaml.SafeDumper):
    pass


Dumper.add_representer(str, lambda d, s: d.represent_scalar("tag:yaml.org,2002:str", s, style='"' if "\n" not in s and (s == "" or s[0] in "{[*&!|>'\"%@`#" or ": " in s or s in ("y", "n", "yes", "no", "on", "off", "true", "false", "null")) else None))


def dump(obj):
    return yaml.dump(obj, Dumper=Dumper, sort_keys=False, width=1000, allow_unicode=True)


# ---------------------------------------------------------------- spec


class Spec:
    def __init__(self, path, strip="", form_bodies=False):
        self.doc = json.load(open(path)) if path.endswith(".json") else yaml.load(open(path), Loader=L)
        self.strip = strip
        self.form_bodies = form_bodies  # vendor.yaml formBodiesAsJSON
        self.name = os.path.basename(path)

    def ref(self, s, seen=()):
        while isinstance(s, dict) and "$ref" in s:
            r = s["$ref"]
            if r in seen:
                return {"x-cycle": True}
            seen = seen + (r,)
            node = self.doc
            for part in r.lstrip("#/").split("/"):
                part = part.replace("~1", "/").replace("~0", "~")
                if isinstance(node, list):
                    node = node[int(part)] if part.isdigit() and int(part) < len(node) else {}
                else:
                    node = node.get(part, {})
            s = node
        return s

    def paths(self):
        for p, ops in self.doc.get("paths", {}).items():
            yield (p[len(self.strip):] if self.strip and p.startswith(self.strip) else p), p, ops

    def body_schema(self, op):
        rb = self.ref((op or {}).get("requestBody") or {})
        content = rb.get("content") or {}
        for ct in ("application/json", "application/json; charset=utf-8", "application/vnd.api+json"):
            if ct in content:
                return self.flatten(content[ct].get("schema") or {})
        for ct, v in content.items():
            if "json" in ct:
                return self.flatten(v.get("schema") or {})
        if self.form_bodies:
            # The API takes JSON for operations the spec describes only as a
            # form (GitLab's Grape endpoints that also accept an upload): the
            # form's fields are the body, less the file uploads.
            for ct in ("multipart/form-data", "application/x-www-form-urlencoded"):
                if ct in content:
                    sch = self.flatten(content[ct].get("schema") or {})
                    props = {k: v for k, v in (sch.get("properties") or {}).items()
                             if self.ref(v).get("format") != "binary"}
                    if props:
                        return dict(sch, properties=props,
                                    required=[r for r in sch.get("required") or [] if r in props])
        for prm in (op or {}).get("parameters", []):  # Swagger 2 body parameter
            prm = self.ref(prm)
            if prm.get("in") == "body":
                return self.flatten(prm.get("schema") or {})
        if (op or {}).get("parameters"):  # form-style params (GitLab): treat as body fields
            props, req = {}, []
            for prm in op["parameters"]:
                prm = self.ref(prm)
                if prm.get("in") in ("formData", "query") and prm.get("name"):
                    sch = prm.get("schema") or {k: v for k, v in prm.items() if k in ("type", "items", "enum", "format", "default")}
                    props[prm["name"].replace("[]", "")] = dict(self.flatten(sch), description=prm.get("description", ""))
                    if prm.get("required"):
                        req.append(prm["name"].replace("[]", ""))
            if props:
                return {"type": "object", "properties": props, "required": req}
        return None

    def multipart_schema(self, op, files=()):
        """Return (schema, file_fields) for a multipart/form-data requestBody,
        replacing binary upload fields, and those named in files (a file the
        spec types as a plain string), with {name, content, contentBase64,
        contentType} objects."""
        rb = self.ref((op or {}).get("requestBody") or {})
        mp = (rb.get("content") or {}).get("multipart/form-data")
        if not mp:
            return None, []
        sch = self.flatten(mp.get("schema") or {})
        props = dict(sch.get("properties") or {})
        req = list(sch.get("required") or [])
        file_obj = {
            "type": "object",
            "properties": {
                "name": {"type": "string", "description": "Filename for the multipart part."},
                "content": {"type": "string", "description": "UTF-8 file content."},
                "contentBase64": {"type": "string", "description": "Base64-encoded binary file content."},
                "contentType": {"type": "string", "description": "MIME type for the multipart part."},
            },
        }
        file_fields = []
        for k, v in list(props.items()):
            fv = self.flatten(v)
            desc = fv.get("description") or (v.get("description") if isinstance(v, dict) else None)
            if fv.get("format") == "binary" or k in files:
                f = copy.deepcopy(file_obj)
                if desc:
                    f["description"] = desc
                props[k] = f
                file_fields.append(k)
            elif fv.get("type") == "array" and self.flatten(fv.get("items") or {}).get("format") == "binary":
                f = {"type": "array", "items": copy.deepcopy(file_obj)}
                if desc:
                    f["description"] = desc
                props[k] = f
                file_fields.append(k)
        ap = self.flatten(sch.get("additionalProperties") or {})
        ap_is_bin = ap.get("format") == "binary" or (
            ap.get("type") == "array" and self.flatten(ap.get("items") or {}).get("format") == "binary"
        )
        out = {k: v for k, v in sch.items() if k != "additionalProperties"} if ap_is_bin else dict(sch)
        if ap_is_bin:
            for r in req:
                if r not in props:
                    props[r] = {"type": "array", "items": copy.deepcopy(file_obj)} if ap.get("type") == "array" else copy.deepcopy(file_obj)
                    file_fields.append(r)
        out["properties"] = props
        if req:
            out["required"] = req
        return out, sorted(file_fields)

    def media_types(self, op):
        """(accept, content_type) when the operation uses a vendor media type
        rather than plain JSON (MongoDB Atlas versions each endpoint this way)."""
        def pick(content):
            cts = [c for c in (content or {}) if "json" in c]
            return next((c for c in cts if c not in ("application/json", "application/json; charset=utf-8")), None)
        acc = None
        for code in ("200", "201", "202", "204", 200, 201):
            r = self.ref(((op or {}).get("responses") or {}).get(code) or {})
            acc = pick(r.get("content"))
            if acc:
                break
        ct = pick(self.ref((op or {}).get("requestBody") or {}).get("content"))
        return acc, ct

    def response_schema(self, op):
        for code in ("200", "201", "202", 200, 201):
            r = self.ref(((op or {}).get("responses") or {}).get(code) or {})
            content = r.get("content") or {}
            for ct, v in content.items():
                if "json" in ct:
                    return self.flatten(v.get("schema") or {})
            if "schema" in r:
                return self.flatten(r["schema"])
        return None

    def flatten(self, s, seen=()):
        """Resolve refs; merge allOf, and a oneOf/anyOf with no properties of
        its own, at the top level only."""
        while isinstance(s, dict) and "$ref" in s:
            # Keep the refs resolved here, so a schema that reaches itself
            # (Cloudflare's) ends in x-cycle when flattened again below.
            if s["$ref"] in seen:
                return {"x-cycle": True}
            seen = seen + (s["$ref"],)
            s = self.ref({"$ref": s["$ref"]})
        if not isinstance(s, dict):
            return {}
        if "allOf" in s:
            out = {k: v for k, v in s.items() if k != "allOf"}
            props, req = dict(out.get("properties") or {}), list(out.get("required") or [])
            writable = set()
            for part in s["allOf"]:
                p = self.flatten(part, seen)
                for k, v in (p.get("properties") or {}).items():
                    # Parts may each describe the same property (Confluent's
                    # spec: one gives its fields, the next what is required).
                    props[k] = self.merge(props[k], v) if k in props else v
                    if k in (p.get("required") or []) and not self.ref(v).get("readOnly"):
                        writable.add(k)
                req += p.get("required") or []
            for k in writable:
                # A field one part inherits read-only from a shared base and
                # another part requires on write (PagerDuty's `type`: "tag",
                # which POST /tags refuses to go without) is writable; a
                # constant is defaulted, so a spec need not name it.
                v = props[k] = {kk: vv for kk, vv in self.flatten(props[k], seen).items() if kk != "readOnly"}
                if len(v.get("enum") or []) == 1 and "x-hfl-default" not in v:
                    props[k] = dict(v, **{"x-hfl-default": v["enum"][0]})
            if not props:
                # allOf of a plain value (a $ref'd string with a description
                # beside it: Cloudflare's access rule mode, Snowflake's
                # identifiers) stays that value, not an object.
                for part in s["allOf"]:
                    for k, v in self.flatten(part, seen).items():
                        out.setdefault(k, v)
                if out.get("type") not in (None, "object") or "enum" in out:
                    return out
            out.update(type="object", properties=props, required=sorted(set(req)))
            return out
        alts = s.get("oneOf") or s.get("anyOf")
        if alts and not s.get("properties"):
            # One schema per variant (Okta's apps by signOnMode): offer the
            # union of their fields; required only if every variant requires it.
            parts = [self.flatten(a, seen) for a in alts]
            if all(p.get("properties") for p in parts):
                props = {}
                for p in parts:
                    for k, v in p["properties"].items():
                        props[k] = self.union(props[k], v) if k in props else v
                req = set.intersection(*(set(p.get("required") or []) for p in parts))
                out = {k: v for k, v in s.items() if k not in ("oneOf", "anyOf", "discriminator")}
                out.update(type="object", properties=props, required=sorted(req))
                return out
        if alts and s.get("properties"):
            # A schema that already declares all its properties alongside
            # per-variant oneOf/anyOf specializations (Sentry's Detector), or
            # shared base properties + discriminator (Atlas's LogIntegration).
            parts = [self.flatten(a, seen) for a in alts]
            if all(p.get("properties") and (s.get("discriminator") or set(p["properties"]) <= set(s["properties"])) for p in parts):
                props = dict(s["properties"])
                for p in parts:
                    for k, v in p["properties"].items():
                        props[k] = self.union(props[k], v) if k in props else v
                req = set(s.get("required") or []) | set.intersection(*(set(p.get("required") or []) for p in parts))
                out = {k: v for k, v in s.items() if k not in ("oneOf", "anyOf", "discriminator")}
                out.update(type="object", properties=props, required=sorted(req))
                return out
        return s

    def union(self, a, b):
        """One property as two variants of a union describe it: a value of
        either shape is valid. Objects take both variants' fields, required
        only where both require them; enums join; two different types
        accept either (no type)."""
        a, b = self.flatten(a), self.flatten(b)
        if a == b:
            return a
        if a.get("properties") and b.get("properties"):
            props = dict(a["properties"])
            for k, v in b["properties"].items():
                props[k] = self.union(props[k], v) if k in props else v
            out = dict(a, properties=props)
            req = sorted(set(a.get("required") or []) & set(b.get("required") or []))
            out.pop("required", None)
            if req:
                out["required"] = req
            return out
        ta, tb = a.get("type"), b.get("type")
        if ta and tb and ta != tb:
            out = {"oneOf": [a, b]}
            if a.get("description") or b.get("description"):
                out["description"] = a.get("description") or b.get("description")
            return out
        if "enum" in a or "enum" in b:
            if "enum" in a and "enum" in b:
                out = {k: v for k, v in a.items() if k != "x-hfl-default"}
                out["enum"] = list(a["enum"]) + [e for e in b["enum"] if e not in a["enum"]]
                return out
            # One variant takes any value: so does the union.
            return {k: v for k, v in (b if "enum" in a else a).items() if k != "x-hfl-default"}
        return a

    def merge(self, a, b):
        """Two schemas for one property, merged (object fields unioned)."""
        a, b = self.flatten(a), self.flatten(b)
        out = dict(a)
        for k, v in b.items():
            if k == "properties":
                props = dict(a.get("properties") or {})
                for pk, pv in v.items():
                    props[pk] = self.merge(props[pk], pv) if pk in props else pv
                out["properties"] = props
            elif k == "required":
                out["required"] = sorted(set(a.get("required") or []) | set(v))
            else:
                out.setdefault(k, v)
        return out

    def props(self, s):
        s = self.flatten(s or {})
        return {k: self.ref(v) for k, v in (s.get("properties") or {}).items()}


# ---------------------------------------------------------------- shapes

# vendor.yaml `trailingSlash: true`: every path ends in a slash (Sentry), so a collection and
# its item are told apart without it. Off by default: Vault has both /x and /x/ (a list).
TRAILING_SLASH = False


def norm(p):
    n = re.sub(r"\{[^}]+\}", "{}", p)
    return n.rstrip("/") if TRAILING_SLASH else n


def family(p):
    """A path less its last placeholder: an item's collection, or the path itself."""
    q = p.rstrip("/") if TRAILING_SLASH else p
    return re.sub(r"/\{[^}]+\}$", "", q)


def semi(p):
    """A path keyed with its parents' placeholder names, its own last one as {}."""
    q = p.rstrip("/") if TRAILING_SLASH else p
    return re.sub(r"\{[^}]+\}$", "{}", q)


# operationId prefixes to ignore (vendor.yaml overrides.excludeOperationIdPrefixes),
# e.g. Vault's enterprise-stub-* paths, which describe no body.
SKIP_OPS = ()


def find_resources(specs, post_updates=False, skip_prefixes=(), list_reads=None, assocs=(), creates=None, deletes=None, forget=(), post_update_paths=(),
                   read_from=None, patch_upserts=(), updates=None, keyed_by=None, create_body=None):
    res = []
    # postUpdates: every path is written by a POST to it (Vault); or only the
    # listed ones (overrides.postUpdates: Okta's threat insight settings).
    pu = lambda path: post_updates or path in post_update_paths
    # overrides.creates: a path, or {path, method} for a create that is not a
    # POST (Datadog's PUT /cost/budget, the collection, makes a budget).
    # A list: one kind per create path, each for the Terraform type it
    # names (Vault's KMIP CA: .../ca/{name}/generate and .../ca/{name}/import,
    # both read at .../ca/{name}), with `update: false` for one the item's
    # update is not for, and `createOnly` fields of its own.
    creates = creates or {}
    create_list = [(i, e if isinstance(e, dict) else {"path": e}) for i, c in creates.items() for e in (c if isinstance(c, list) else [c])]
    # An item all of whose creates name their Terraform type keeps its own
    # kind too (Vault's PKI issuer, beside the root CA generated as one).
    creates = {i: (c[0] if isinstance(c, list) else c) for i, c in creates.items()
               if not (isinstance(c, list) and all(isinstance(e, dict) and e.get("terraform") for e in c))}
    create_method = {i: (c.get("method") or "post").lower() if isinstance(c, dict) else "post" for i, c in creates.items()}
    creates = {i: c["path"] if isinstance(c, dict) else c for i, c in creates.items()}
    deletes = deletes or {}
    updates = updates or {}
    for sp in specs:
        by = {}
        kept = [(p, raw, ops) for p, raw, ops in sp.paths() if not any(p.startswith(x) for x in skip_prefixes)]
        # Paths are keyed by their shape, placeholder names aside, so that a
        # collection pairs with its item whatever each calls its parent's ID.
        # Where two paths of one family differ only in a placeholder's name
        # (Vault's /{aws_mount}/role and /{ldap_mount}/role), the shape alone
        # would keep only one of them: that family keeps its parents' names.
        prefixes = {}
        for p, _, _ in kept:
            pre = family(p)
            prefixes.setdefault(norm(pre), set()).add(pre.rstrip("/") if TRAILING_SLASH else pre)
        clash = {f for f, pres in prefixes.items() if len(pres) > 1}
        for p, raw, ops in kept:
            ms = {m.lower(): v for m, v in ops.items() if m.lower() in ("get", "post", "put", "patch", "delete")
                  and not any(str((v or {}).get("operationId", "")).startswith(x) for x in SKIP_OPS)}
            by[semi(p) if norm(family(p)) in clash else norm(p)] = (p, raw, ms)
        for np_, (p, raw, ms) in by.items():
            if not np_.endswith("{}"):
                it = by.get(np_ + "/{}")
                if list_reads and p in list_reads and "post" in ms and "get" in ms and it and "delete" in it[2] and "get" not in it[2]:
                    # Listed, not got (Snowflake roles): read finds it in the list.
                    res.append(dict(shape="crud", spec=sp, coll=p, item=it[0], create=ms["post"], read=ms["get"], update=None,
                                    delete=it[2]["delete"], find=list_reads[p]))
                elif "post" in ms and it and "get" in it[2] and ("delete" in it[2] or it[0] in forget):
                    upd = "patch" if "patch" in it[2] else ("put" if "put" in it[2] else None)
                    if not upd and pu(it[0]) and "post" in it[2]:
                        upd = "post"  # Vault: an item is updated by a POST to its own path (MFA methods)
                    res.append(dict(shape="crud", spec=sp, coll=p, item=it[0], create=ms["post"], read=it[2]["get"],
                                    update=(upd, it[2][upd]) if upd else None, delete=it[2].get("delete")))
                elif {"get", "put", "delete"} <= set(ms) and "post" not in ms:
                    # A singleton the caller turns on and off (branch protection).
                    res.append(dict(shape="upsert", spec=sp, item=p, read=ms["get"], update=("put", ms["put"]), delete=ms["delete"]))
                elif "get" in ms and ("put" in ms or "patch" in ms) and "post" not in ms and "delete" not in ms:
                    upd = "patch" if "patch" in ms else "put"
                    res.append(dict(shape="settings", spec=sp, item=p, read=ms["get"], update=(upd, ms[upd])))
                elif p in patch_upserts and {"get", "patch", "delete"} <= set(ms) and "put" not in ms:
                    # A singleton a PATCH turns on and a DELETE resets (Cloudflare's
                    # DNSSEC): overrides.patchUpserts.
                    res.append(dict(shape="upsert", spec=sp, item=p, read=ms["get"], update=("patch", ms["patch"]), delete=ms["delete"]))
                elif pu(p) and {"get", "post"} <= set(ms) and not {"put", "patch"} & set(ms) and not it:
                    # Vault: a write is a POST to the path it is read from.
                    shape = "upsert" if "delete" in ms else "settings"
                    res.append(dict(shape=shape, spec=sp, item=p, read=ms["get"], update=("post", ms["post"]), delete=ms.get("delete")))
            else:
                parent = by.get(np_[:-3])
                if p in patch_upserts and {"get", "patch", "delete"} <= set(ms) and "put" not in ms:
                    res.append(dict(shape="upsert", spec=sp, item=p, read=ms["get"], update=("patch", ms["patch"]), delete=ms["delete"]))
                elif {"put", "get", "delete"} <= set(ms) and not (parent and "post" in parent[2]) and p not in creates:
                    res.append(dict(shape="upsert", spec=sp, item=p, read=ms["get"], update=("put", ms["put"]), delete=ms["delete"]))
                elif "get" in ms and ("patch" in ms or "put" in ms) and "delete" not in ms and not (parent and "post" in parent[2]) and p not in creates:
                    # One of a fixed set the caller names (a zone setting by id).
                    upd = "patch" if "patch" in ms else "put"
                    res.append(dict(shape="settings", spec=sp, item=p, read=ms["get"], update=(upd, ms[upd])))
                elif pu(p) and {"get", "post", "delete"} <= set(ms) and not {"put", "patch"} & set(ms) and p not in creates and not (
                        parent and "post" in parent[2] and not np_[:-3].endswith("{}")):
                    # A parent that is itself an item (Vault's deprecated
                    # /sys/plugins/catalog/{name} above /sys/plugins/catalog/{type}/{name})
                    # writes to its own path; it does not create this one.
                    res.append(dict(shape="upsert", spec=sp, item=p, read=ms["get"], update=("post", ms["post"]), delete=ms["delete"]))
                elif p in post_update_paths and {"get", "post"} <= set(ms) and not {"put", "patch", "delete"} & set(ms) and p not in creates:
                    # Settings named by an ID, written by a POST to the path they
                    # are read from (Okta's profile mapping): overrides.postUpdates.
                    res.append(dict(shape="settings", spec=sp, item=p, read=ms["get"], update=("post", ms["post"])))
        raw = {p: ops for p, _, ops in sp.paths()}
        for item_p, ce in create_list:
            # The item is made by a POST elsewhere (Sentry: POST /organizations/{org}/teams/ makes
            # /teams/{org}/{slug}/), so the path pairing above cannot see it.
            coll_p = ce["path"]
            fixed = {k: v for k, v in (("tfFixed", [ce["terraform"]] if ce.get("terraform") else None),
                                       ("createOnlyOwn", ce.get("createOnly")),
                                       ("secretOutputsOwn", ce.get("secretOutputs")),
                                       ("idFromOwn", ce.get("idFrom")),
                                       ("forgetOwn", ce.get("forget")),
                                       ("writeOnlyOwn", ce.get("writeOnly"))) if v is not None}
            it_, co_ = raw.get(item_p) or {}, raw.get(coll_p) or {}
            it_ = {m.lower(): v for m, v in it_.items()}
            co_ = {m.lower(): v for m, v in co_.items()}
            del_p = deletes.get(item_p)
            if del_p:
                # Deleted on another path too (Grafana: POST /admin/users makes
                # /users/{user_id}, and DELETE /admin/users/{user_id} removes it),
                # which may name it by a field its read answers (Confluent's
                # mirror topic, deleted as the topic {current.mirror_topic_name}).
                spec_del = del_p
                if "{current." in del_p:
                    shape_of = {norm(p): p for p in raw}
                    spec_del = shape_of.get(norm(re.sub(r"\{current\.[^}]+\}", "{x}", del_p)), del_p)
                d_ = {m.lower(): v for m, v in (raw.get(spec_del) or {}).items()}
                if "delete" in d_:
                    it_ = dict(it_, delete=d_["delete"])
            cm = (ce.get("method") or "post").lower()
            if ce.get("forget"):
                # This kind's own: deleting the object leaves what its create
                # made (an imported CA), though the item path has a delete
                # another kind there uses.
                it_ = {m: v for m, v in it_.items() if m != "delete"}
            if cm in co_ and "get" in it_ and ("delete" in it_ or item_p in forget or ce.get("forget")):
                upd = "patch" if "patch" in it_ else ("put" if "put" in it_ else None)
                if not upd and pu(item_p) and "post" in it_ and item_p != coll_p:
                    upd = "post"  # updated by a POST to its own path (Vault's TPM group): postUpdates
                if ce.get("update") is False:
                    upd = None  # the item's update is another kind's (an imported CA's, not a generated one's)
                named = re.findall(r"\{([^}]+)\}", item_p)
                if item_p != coll_p and named and named[-1] in re.findall(r"\{([^}]+)\}", coll_p):
                    # Caller-named, made on another path that names it too (Vault's
                    # KMIP CA: POST .../ca/{name}/generate, read at .../ca/{name}).
                    if upd or ce.get("update") is False:
                        res.append(dict(shape="upsert", spec=sp, item=item_p, create=co_["post"], createPath=coll_p, read=it_["get"],
                                        update=(upd, it_[upd]) if upd else None, delete=it_.get("delete"), deletePath=del_p or item_p, **fixed))
                    continue
                if item_p == coll_p:
                    # Caller-named item where POST on the item path creates and PUT/PATCH updates.
                    if upd:
                        res.append(dict(shape="upsert", spec=sp, item=item_p, create=co_[cm], read=it_["get"],
                                        update=(upd, it_[upd]), delete=it_.get("delete"), deletePath=del_p or item_p, createMethod=cm, **fixed))
                else:
                    res.append(dict(shape="crud", spec=sp, coll=coll_p, item=item_p, create=co_[cm], read=it_["get"],
                                    update=(upd, it_[upd]) if upd else None, delete=it_.get("delete"), deletePath=del_p or item_p,
                                    createMethod=cm, **fixed))
        for item_p, rf in (read_from or {}).items():
            # Read from a list elsewhere (overrides.readFrom): an item the API
            # writes and deletes but never gets one of (GitHub's role teams,
            # Okta's group members, PagerDuty's team members).
            lst = {m.lower(): v for m, v in (raw.get(rf["path"]) or {}).items()}
            if item_p not in raw or "get" not in lst:
                continue
            if "field" in rf:
                # Found in a list by a field; "" the element itself (Vault's {"keys": [...]}).
                find = {"list": rf.get("list", ""), "field": rf["field"]}
                extra = dict(find=find, readPath=rf["path"], findParam=rf.get("param"))
            else:
                # One object read on another path (Grafana's public dashboard).
                extra = dict(readPath=rf["path"])
            have = [r for r in res if r["spec"] is sp and r["item"] == item_p]
            if have:
                for r in have:
                    r.update(extra, read=lst["get"])
                continue
            it_ = {m.lower(): v for m, v in raw[item_p].items()}
            coll_p = (creates or {}).get(item_p) or family(item_p)
            co_ = {m.lower(): v for m, v in (raw.get(coll_p) or {}).items()} if coll_p != item_p else {}
            upd = "patch" if "patch" in it_ else ("put" if "put" in it_ else None)
            if "post" in co_ and "delete" in it_ and item_p in (create_body or {}):
                # Named by the caller in the create's body (overrides.createBody:
                # Okta's trusted server, {"trusted": ["{associatedServerId}"]}):
                # an upsert, made on the collection, with no update.
                res.append(dict(shape="upsert", spec=sp, item=item_p, create=co_["post"], createPath=coll_p, read=lst["get"],
                                update=None, delete=it_["delete"], createBody=create_body[item_p], **extra))
            elif "post" in co_ and ("delete" in it_ or item_p in forget):
                res.append(dict(shape="crud", spec=sp, coll=coll_p, item=item_p, create=co_["post"], read=lst["get"],
                                update=(upd, it_[upd]) if upd else None, delete=it_.get("delete"), **extra))

            elif "put" in it_ and "delete" in it_:
                res.append(dict(shape="upsert", spec=sp, item=item_p, read=lst["get"], update=("put", it_["put"]),
                                delete=it_["delete"], **extra))
            elif pu(item_p) and "post" in it_ and "delete" in it_:
                # Written by a POST to its own path (Vault's KMIP scope).
                res.append(dict(shape="upsert", spec=sp, item=item_p, read=lst["get"], update=("post", it_["post"]),
                                delete=it_["delete"], **extra))
            elif not upd and pu(item_p) and "post" in it_ and "delete" not in it_ and item_p in forget:
                # Written by a POST to its own path, and removed by the same
                # POST of a fixed body (overrides.deleteBody): Grafana's
                # resource permission for one user, {"permission": ""}.
                res.append(dict(shape="upsert", spec=sp, item=item_p, read=lst["get"], update=("post", it_["post"]),
                                delete=None, **extra))
            elif pu(item_p) and "post" in it_:
                res.append(dict(shape="settings", spec=sp, item=item_p, read=lst["get"], update=("post", it_["post"]), **extra))
            elif upd and "delete" not in it_:
                res.append(dict(shape="settings", spec=sp, item=item_p, read=lst["get"], update=(upd, it_[upd]), **extra))
        for coll_p, field in (keyed_by or {}).items():
            # Objects named by a body field on their collection, with no item
            # path: POST makes one, GET lists them all, PUT changes the one
            # its body names, DELETE with that body removes it (Datadog's v1
            # Azure and GCP integrations): overrides.keyedBy.
            if coll_p not in raw:
                continue  # another spec's
            co_ = {m.lower(): v for m, v in raw[coll_p].items()}
            if not {"get", "post", "put", "delete"} <= set(co_):
                sys.exit(f"overrides.keyedBy: {coll_p} needs GET, POST, PUT and DELETE")
            res[:] = [r for r in res if not (r["spec"] is sp and r["item"] == coll_p)]
            res.append(dict(shape="upsert", spec=sp, item=coll_p, coll=coll_p, create=co_["post"], read=co_["get"], update=("put", co_["put"]),
                            delete=co_["delete"], find={"list": "", "field": field}, findParam=field, keyedBy=field))
        for item_p in updates:
            it_ = {m.lower(): v for m, v in (raw.get(item_p) or {}).items()}
            if {"get", "delete"} <= set(it_) and not any(r["spec"] is sp and r["item"] == item_p for r in res):
                # Read and deleted on its own path, written (made and
                # replaced) only on another, which names it in its body: an
                # upsert (Datadog's PUT /cost/budget/custom-forecast, read at
                # /cost/budget/{budget_id}/custom-forecast).
                res.append(dict(shape="upsert", spec=sp, item=item_p, read=it_["get"], update=None, delete=it_["delete"]))
        for item_p, del_p in deletes.items():
            if item_p in creates:
                continue
            # Deleted on another path, which may name it by a field its read
            # answers (Grafana k6's schedule: read and written at
            # .../load_tests/{id}/schedule, deleted at /schedules/{current.id}):
            # overrides.deletes without creates.
            shape_of = {norm(p): p for p in raw}
            sp_del = shape_of.get(norm(re.sub(r"\{current\.[^}]+\}", "{x}", del_p)))
            dop = {m.lower(): v for m, v in (raw.get(sp_del) or {}).items()}.get("delete")
            for r in res:
                if r["spec"] is sp and r["item"] == item_p and r["shape"] in ("settings", "upsert") and dop:
                    r.update(shape="upsert", delete=dop, deletePath=del_p)
        for r in res:
            u = updates.get(r["item"]) if r["spec"] is sp else None
            if u:
                # Updated on another path than the item's, its ID in the body
                # (Grafana Synthetic Monitoring's POST /check/update).
                m = (u.get("method") or "put").lower()
                uop = (raw.get(u["path"]) or {}).get(m) or (raw.get(u["path"]) or {}).get(m.upper())
                if uop is None:
                    sys.exit(f"overrides.updates: {r['item']}: no {m.upper()} {u['path']} in the spec")
                r["update"] = (m, uop)
                r["updatePath"] = u["path"]
        for a in assocs:
            if a["create"] in raw:
                # A link, not the resource the paths alone suggest (Grafana's
                # GET, POST and DELETE /teams/{teamId}/groups, which postUpdates
                # would read as one upsert).
                res[:] = [r for r in res if not (r["spec"] is sp and r["item"] == a["read"] and r["shape"] != "assoc")]
                g = lambda path, m: (raw.get(path) or {}).get(m)
                res.append(dict(shape="assoc", spec=sp, coll=a["create"], item=a["read"], create=g(a["create"], "post"),
                                read=g(a["read"], "get"), update=None, delete=g(a["delete"], a.get("deleteMethod", "post")),
                                assoc=a))
    return res


# ---------------------------------------------------------------- Terraform matching

SYN = {"orgs": "organization", "org": "organization", "repos": "repository", "repo": "repository",
       "envs": "environment", "vars": "variable", "datasources": "data_source", "webhooks": "webhook",
       "protected_branches": "branch_protection", "dns_records": "dns_record"}
STOP = {"api", "v1", "v2", "v3", "v4", "id", "owner", "configuration", "config", "client"}


def sing(x):
    if x.endswith("ies") and len(x) > 4:
        return x[:-3] + "y"
    if re.search(r"(ch|sh|ss|x)es$", x):
        return x[:-2]
    if x.endswith("s") and not x.endswith("ss") and len(x) > 3:
        return x[:-1]
    return x


def toks(s):
    s = re.sub(r"\{[^}]+\}", " ", s)
    s = re.sub(r"([a-z0-9])([A-Z])", r"\1_\2", s)  # camelCase path segments (Atlas, Okta)
    out = set()
    for x in re.split(r"[^a-z0-9_]+", s.lower()):
        for y in SYN.get(x, x).replace("-", "_").split("_"):
            y = WORD_SYN.get(y, y)
            if y and y not in STOP:
                out.add(sing(y))
    return out


# Per-vendor single-word synonyms (vendor.yaml overrides.wordSynonyms); "" drops the word.
WORD_SYN = {}


def match(res, gaps, prefix, overrides):
    SYN.update(overrides.get("synonyms") or {})
    WORD_SYN.clear()
    WORD_SYN.update(overrides.get("wordSynonyms") or {})
    forced = {}
    for tf, path in (overrides.get("map") or {}).items():
        # A list: the Terraform type is split across kinds (Grafana's
        # permission items: one kind per user, team or role path).
        for p in path if isinstance(path, list) else [path]:
            forced.setdefault(p, []).append(tf)
    excluded = (set(overrides.get("exclude") or []) | set(overrides.get("adopt") or {}) | set(overrides.get("elements") or {})
                | set(overrides.get("_handwritten") or ())
                | {st["terraform"] for st in (overrides.get("states") or {}).values() if st.get("terraform")})
    # A placeholder may name what the path is about (Vault: {aws_mount_path}).
    pt = overrides.get("placeholderWords")
    for r in res:
        item = re.sub(pt, lambda m: m.group(1), r["item"]) if pt else r["item"]
        r["toks"] = toks(item)
        r["tf"] = list(r.get("tfFixed") or forced.get(r["item"], []))
    claimed = {tf for tfs in forced.values() for tf in tfs} | {tf for r in res for tf in r.get("tfFixed") or ()}
    for g in gaps:
        if g in claimed or g in excluded:
            continue
        gt = toks(g[len(prefix) + 1:])
        cands = [r for r in res if gt and gt <= r["toks"] and not r.get("tfFixed")]
        if not cands:
            continue
        want_settings = bool(gt & {"setting", "permission"})
        best = min(cands, key=lambda r: (len(r["toks"] - gt), (r["shape"] == "settings") != want_settings, len(r["item"])))
        best["tf"].append(g)
    for r in res:
        # Keep a Terraform type only where its words are exactly the path's, or the single
        # most specific one: "actions_secret" does not stand in for an org-level secret.
        exact = [g for g in r["tf"] if toks(g[len(prefix) + 1:]) == r["toks"] or g in forced.get(r["item"], []) or g in (r.get("tfFixed") or ())]
        r["tf"] = sorted(exact) or sorted(r["tf"], key=lambda g: (-len(toks(g)), g))[:1]
    return [r for r in res if r["tf"] and r["item"] not in set(overrides.get("excludePaths") or [])]


# ---------------------------------------------------------------- schema → CRD

DROP = {"readOnly", "xml", "externalDocs", "example", "examples", "discriminator", "deprecated", "x-stainless-terraform-configurability", "nullable", "title", "writeOnly", "const"}


def structural(sp, s, depth=0, seen=()):
    s = sp.flatten(s, seen)
    if not isinstance(s, dict) or s.get("x-cycle") or depth > 6:
        return {"x-kubernetes-preserve-unknown-fields": True}
    if any(k in s for k in ("oneOf", "anyOf", "not")):
        out = {"x-kubernetes-preserve-unknown-fields": True}
        if s.get("description"):
            out["description"] = s["description"][:400]
        return out
    t = s.get("type")
    if not t and s.get("enum") and all(isinstance(e, str) for e in s["enum"]):
        t = "string"  # an enum of strings that names no type (Cloudflare's list kind)
    nullable = bool(s.get("nullable"))
    if isinstance(t, list):
        nullable = nullable or "null" in t
        t = next((x for x in t if x != "null"), None)
    out = {}
    if s.get("description"):
        out["description"] = s["description"][:400]
    if nullable and t:
        # The API takes null here; the API server keeps a null only where
        # the schema says nullable (else it drops it, and a required field
        # is then missing: Sentry's Detector data source environment).
        out["nullable"] = True
    if t in ("string", "integer", "number", "boolean"):
        out["type"] = t
        if t == "string" and s.get("enum") and all(isinstance(e, str) for e in s["enum"]):
            out["enum"] = [e for e in s["enum"]]
            if "x-hfl-default" in s:
                out["default"] = s["x-hfl-default"]
        return out
    if t == "array":
        items = s.get("items") or {}
        out.update(type="array", items=structural(sp, items, depth + 1, seen))
        if "type" not in out["items"] and not out["items"].get("x-kubernetes-preserve-unknown-fields"):
            out["items"] = {"x-kubernetes-preserve-unknown-fields": True}
        return out
    props = s.get("properties")
    if props:
        out["type"] = "object"
        out["properties"] = {}
        for k, v in props.items():
            v2 = sp.ref(v)
            if isinstance(v2, dict) and v2.get("readOnly"):
                continue
            out["properties"][k] = structural(sp, v, depth + 1, seen)
        response_model = any(sp.ref(props.get(r) or {}).get("readOnly") for r in (s.get("required") or []))

        def optional(r):
            # A required field the API defaults is not the caller's to send
            # (Cloudflare's Worker: tags, logpush). A body that is the
            # response's model (it requires read-only fields too) also
            # requires objects the API fills in: Worker's observability.
            v = sp.flatten(props.get(r) or {})
            if "default" in v:
                return True
            return response_model and v.get("type") == "object" and not v.get("required")
        req = [r for r in (s.get("required") or []) if r in out["properties"] and not optional(r)]
        for r in s.get("required") or []:
            # Made optional for its default: the CRD carries the default, so
            # the API server fills it in and the controller still sends it.
            p, v = out["properties"].get(r), sp.flatten(props.get(r) or {})
            if p is None or r in req or "default" not in v or "default" in p:
                continue
            d, pt = v["default"], p.get("type")
            ok = {"string": isinstance(d, str), "boolean": isinstance(d, bool),
                  "integer": isinstance(d, int) and not isinstance(d, bool),
                  "number": isinstance(d, (int, float)) and not isinstance(d, bool),
                  "array": isinstance(d, list) and not d}.get(pt, False)
            if ok and (not p.get("enum") or d in p["enum"]):
                p["default"] = d
        if req:
            out["required"] = sorted(set(req))
        return out
    ap = s.get("additionalProperties")
    if isinstance(ap, dict) and ap:
        out.update(type="object", additionalProperties=structural(sp, ap, depth + 1, seen))
        if "type" not in out["additionalProperties"] and not out["additionalProperties"].get("x-kubernetes-preserve-unknown-fields"):
            out["additionalProperties"] = {"x-kubernetes-preserve-unknown-fields": True}
        return out
    if not t:
        # No type and no fields: any value (Sentry's cron schedule, "a
        # crontab string, or a 2 element tuple"). type: object would refuse
        # the string.
        out.pop("nullable", None)
        out["x-kubernetes-preserve-unknown-fields"] = True
        return out
    out.update(type="object")
    out["x-kubernetes-preserve-unknown-fields"] = True
    return out


# How deep a sample fills required fields (PagerDuty's Jira mapping rule
# requires config.jira.status_mapping.triggered.id, five levels down).
SAMPLE_DEPTH = 6


def typed(s, v):
    """v as schema s's scalar type, or None when it is not one: a spec's
    example "false" for a boolean (Cloudflare's), "10" for an integer."""
    t = s.get("type")
    if isinstance(t, list):
        t = next((x for x in t if x != "null"), None)
    if t == "boolean":
        if isinstance(v, bool):
            return v
        return {"true": True, "false": False}.get(str(v).lower())
    if t in ("integer", "number"):
        if isinstance(v, bool):
            return None
        if isinstance(v, (int, float)):
            return v
        try:
            f = float(str(v))
        except ValueError:
            return None
        return int(f) if t == "integer" and f == int(f) else (f if t == "number" else None)
    if t == "string" and not isinstance(v, str):
        return None if isinstance(v, bool) else str(v)  # GitHub's display_name example 20.04: "20.04"
    return v


def sample_value(sp, name, s, depth=0):
    s = sp.flatten(s)
    for key in ("example", "default"):
        v = s.get(key)
        if v not in (None, "") and not isinstance(v, (dict, list)):
            tv = typed(s, v)
            if tv is not None:
                return tv
    if s.get("enum"):
        return s["enum"][0]
    t = s.get("type")
    if isinstance(t, list):
        t = next((x for x in t if x != "null"), None)
    if t == "integer" or t == "number":
        return 1
    if t == "boolean":
        return True
    if t == "array":
        return []
    if t == "object" or s.get("properties"):
        out = {}
        if depth < SAMPLE_DEPTH:
            for r in s.get("required") or []:
                if r in (s.get("properties") or {}):
                    out[r] = sample_value(sp, r, s["properties"][r], depth + 1)
        return out
    return f"example-{name}".replace("_", "-")


# Response-only fields whose name says they hold a secret the API made: the
# name, less case, "_" and "-", ends so (privateKey, access_token,
# client_secret); a name that only mentions one (tokenType, secretId,
# token_expires_at) does not, nor one shown masked (Atlas's maskedSecret).
SECRET_ENDS = ("secret", "password", "token", "privatekey", "apikey", "accesskey", "secretkey", "signingkey", "sharedkey")


def secret_outputs(sp, rprops, bprops):
    out = []
    for f, v in rprops.items():
        n = f.lower().replace("_", "").replace("-", "")
        t = sp.flatten(v).get("type")
        if f not in bprops and t in (None, "string") and n.endswith(SECRET_ENDS) and not n.startswith(("masked", "redacted", "hashed")):
            out.append(f)
    return out


# Formats a server-set field's sample takes, rather than example-<name>.
FORMAT_SAMPLES = {"date-time": "2000-01-01T00:00:00Z", "date": "2000-01-01", "uri": "https://example.com/",
                  "url": "https://example.com/", "email": "user@example.com", "uuid": "00000000-0000-0000-0000-000000000000",
                  "ipv4": "192.0.2.1", "ipv6": "2001:db8::1", "hostname": "example.com"}
MAX_SERVER_FIELDS = 100


def server_fields(sp, rprops, bprops, desc):
    """The read response's top-level fields no request body has, each with a
    sample value; the ID and path fields are the fake's own. A response's
    "id" is the fake's own only when it is the ID: one named by another
    field (GitHub's team, by its slug) keeps its numeric id as a server
    field, as other kinds' references read it."""
    skip = {(desc.get("id") or {}).get("from") or "id"} | {p["field"] for p in desc.get("params") or []}
    out = {}
    for f in sorted(rprops):
        if f in bprops or f in skip or len(out) >= MAX_SERVER_FIELDS:
            continue
        s = sp.flatten(rprops[f])
        if s.get("writeOnly") or s.get("x-cycle"):
            continue
        t = s.get("type")
        if isinstance(t, list):
            t = next((x for x in t if x != "null"), None)
        if t in (None, "string") and s.get("format") in FORMAT_SAMPLES and s.get("example") in (None, "") and not s.get("enum"):
            out[f] = FORMAT_SAMPLES[s["format"]]
        else:
            out[f] = sample_value(sp, f, s)
    return out


# ---------------------------------------------------------------- naming

def camel(words, upper=True):
    out = "".join(w[:1].upper() + w[1:] for w in words if w)
    return out if upper else out[:1].lower() + out[1:]


def param_field(path, name):
    if name == "id":  # GitLab's /projects/{id}: name it after the segment before it
        segs = path.split("/")
        i = segs.index("{id}")
        base = sing(segs[i - 1].replace("-", "_")) if i > 0 else "parent"
        name = base + "_id"
    w = re.split(r"[^A-Za-z0-9]+", name)
    out = camel([x.lower() for x in w], upper=False)
    return re.sub(r"Id$", "ID", out)


def plural(kind):
    k = kind.lower()
    if k.endswith("y") and not k.endswith(("ay", "ey", "oy", "uy")):
        return k[:-1] + "ies"
    if re.search(r"(s|x|ch|sh)$", k):
        return k + "es"
    return k + "s"


# ---------------------------------------------------------------- build

# What build saw for each kind, for tools that review a generation (not written anywhere).
AUDIT = []


def live_records(cfg, base):
    """hack/live/records.json (or vendor.yaml's liveRecords, relative to it):
    the last live result per kind, keyed Kind.group, written by kindtest.Live."""
    p = os.path.join(base, cfg.get("liveRecords", "../live/records.json"))
    return json.load(open(p)) if os.path.exists(p) else {}


def fake_only(cfg, base):
    """The kinds hack/live/live.yaml marks fakeOnly (next to the records):
    sold only by contract, so never run live. {Kind.group: reason}."""
    p = os.path.join(base, os.path.dirname(cfg.get("liveRecords", "../live/records.json")), "live.yaml")
    if not os.path.exists(p):
        return {}
    live = yaml.safe_load(open(p)) or {}
    return {f"{k['kind']}.{k.get('group', '')}": k["fakeOnly"] for k in live.get("kinds") or [] if k.get("fakeOnly")}


def query_from_spec(names):
    """vendor.yaml queryFromSpec: required query parameters filled from the spec.
    Confluent Cloud reads and deletes a cluster only with ?environment=<id>,
    the id of the spec's environment reference."""
    names = set(names or ())

    def fill(sp, path, op, bprops):
        if not names:
            return {}
        item = sp.doc.get("paths", {}).get(sp.strip + path) or sp.doc.get("paths", {}).get(path) or {}
        out = {}
        for prm in (item.get("parameters") or []) + ((op or {}).get("parameters") or []):
            prm = sp.ref(prm)
            name = prm.get("name") or ""
            field = name[len("spec."):] if name.startswith("spec.") else name
            if prm.get("in") != "query" or not prm.get("required") or field not in names or field not in bprops:
                continue
            v = sp.flatten(bprops[field])
            out[name] = "{%s.id}" % field if "id" in (v.get("properties") or {}) else "{%s}" % field
        return out
    return fill


def adopt_resources(specs, adopt, gaps, post_update_paths):
    """overrides.adopt: {<Terraform type>: {item, list, at, query, match,
    reason}}: a resource that exists already (Okta's default authorization
    server), taken over rather than made. It is found in the list at list
    (its array at at) by the fields match gives (a value, or a {field}
    template filled from the spec), read and updated at item, and never
    deleted. One kind per Terraform type, beside any kind at the same path."""
    out = []
    for tf, a in adopt.items():
        if tf not in gaps:
            sys.exit(f"overrides.adopt: {tf} is not a Terraform gap type")
        for k in ("item", "list", "match"):
            if not a.get(k):
                sys.exit(f"overrides.adopt: {tf} needs {k}")
        found = False
        for sp in specs:
            raw = {p: ops for p, _, ops in sp.paths()}
            it_ = {m.lower(): v for m, v in (raw.get(a["item"]) or {}).items()}
            ls_ = {m.lower(): v for m, v in (raw.get(a["list"]) or {}).items()}
            if "get" not in it_ or "get" not in ls_:
                continue
            upd = "put" if "put" in it_ else ("patch" if "patch" in it_ else ("post" if "post" in it_ and a["item"] in post_update_paths else None))
            if not upd:
                sys.exit(f"overrides.adopt: {tf}: {a['item']} has no update (PUT or PATCH)")
            out.append(dict(shape="crud", spec=sp, item=a["item"], create=None, read=it_["get"], update=(upd, it_[upd]),
                            delete=None, adopt=a, listOp=ls_["get"], tf=[tf]))
            found = True
            break
        if not found:
            sys.exit(f"overrides.adopt: {tf}: no spec has GET {a['item']} and GET {a['list']}")
    return out


def state_resources(specs, states, gaps, res):
    """overrides.states: {<item path>: {field, calls: {<value>: {method,
    path}}, delete, terraform}}: a field the API sets by a call per value
    (Okta's org factor, POST .../lifecycle/activate or .../deactivate),
    added to the kind at the item path; where none is (the item has only a
    GET: Okta's feature), a settings kind of its own for the Terraform type
    `terraform` names."""
    out = []
    have = {r["item"] for r in res}
    for item, st in states.items():
        for k in ("field", "calls"):
            if not st.get(k):
                sys.exit(f"overrides.states: {item} needs {k}")
        if item in have:
            continue
        tf = st.get("terraform")
        if not tf:
            sys.exit(f"overrides.states: no kind at {item}: name its terraform type")
        if tf not in gaps:
            sys.exit(f"overrides.states: {tf} is not a Terraform gap type")
        for sp in specs:
            raw = {p: ops for p, _, ops in sp.paths()}
            it_ = {m.lower(): v for m, v in (raw.get(item) or {}).items()}
            if "get" in it_:
                out.append(dict(shape="settings", spec=sp, item=item, read=it_["get"], update=None, tf=[tf]))
                break
        else:
            sys.exit(f"overrides.states: no spec has GET {item}")
    return out


def element_resources(specs, elements, gaps, post_update_paths, post_updates):
    """overrides.elements: {<Terraform type>: {item, in | key + list, patch,
    unset, method}}: one element of a list the resource at item holds,
    which the API reads and writes only whole (Vault's OIDC key's
    allowed_client_ids, Grafana's role assignments) or one element at a
    time (GitHub's custom property values, a patch). An upsert kind of its
    own: read at item, written by its PUT, PATCH or POST (method)."""
    out = []
    for tf, e in elements.items():
        if tf not in gaps:
            sys.exit(f"overrides.elements: {tf} is not a Terraform gap type")
        if not e.get("item") or bool(e.get("in")) == bool(e.get("key")):
            sys.exit(f"overrides.elements: {tf} needs item, and in (bare values) or key (objects)")
        for sp in specs:
            raw = {p: ops for p, _, ops in sp.paths()}
            it_ = {m.lower(): v for m, v in (raw.get(e["item"]) or {}).items()}
            if "get" not in it_:
                continue
            m = (e.get("method") or "").lower() or next((x for x in ("put", "patch") if x in it_), None) or (
                "post" if "post" in it_ and (post_updates or e["item"] in post_update_paths) else None)
            if m not in it_:
                sys.exit(f"overrides.elements: {tf}: {e['item']} has no write ({m or 'PUT, PATCH or POST'})")
            out.append(dict(shape="upsert", spec=sp, item=e["item"], read=it_["get"], update=(m, it_[m]), element=e, tf=[tf]))
            break
        else:
            sys.exit(f"overrides.elements: {tf}: no spec has GET {e['item']}")
    return out


def element_kind(cfg, r, sp, kind, group, mk, envelope):
    """The descriptor, spec schema and sample of an Element kind
    (overrides.elements): its fields are the element's (a bare value's
    field, or the list's item schema), the path's parameters beside them."""
    e = r["element"]
    item = r["item"]
    wbody = sp.flatten(sp.body_schema(r["update"][1]) or {})
    rbody = sp.flatten(sp.response_schema(r["read"]) or {})
    if envelope and envelope in sp.props(rbody):
        rbody = sp.flatten(sp.props(rbody)[envelope])

    def list_schema(path, where):
        node = where
        for seg in path.split("/") if path else []:
            node = sp.flatten(sp.props(node).get(seg) or {})
        return sp.flatten(node.get("items") or {}) if node.get("type") == "array" else None

    el = {}
    fields, required = {}, []
    if e.get("in"):
        el["in"] = dict(e["in"])
        for f, lst in e["in"].items():
            it = list_schema(lst, wbody) or list_schema(lst, rbody)
            if it is None:
                sys.exit(f"overrides.elements: {r['tf'][0]}: no list {lst} at {item}")
            fs = dict(structural(sp, it))
            fs["description"] = f"The element of {lst} this object is." + (" Set exactly one of " + ", ".join(sorted(e["in"])) + "." if len(e["in"]) > 1 else "")
            fields[f] = fs
        if len(e["in"]) == 1:
            required = list(e["in"])
    else:
        el["key"] = e["key"]
        if e.get("list"):
            el["list"] = e["list"]
        it = list_schema(e["patch"], wbody) if e.get("patch") else None
        if it is None:
            it = list_schema(e.get("list") or "", rbody) or (sp.flatten(rbody.get("items") or {}) if rbody.get("type") == "array" else None)
        if not it or e["key"] not in sp.props(it):
            sys.exit(f"overrides.elements: {r['tf'][0]}: no element with field {e['key']} at {item}")
        st = structural(sp, it)
        for f, v in (st.get("properties") or {}).items():
            fields[f] = v
        required = sorted(set(st.get("required") or []) | {e["key"]})
    if e.get("patch"):
        el["patch"] = e["patch"]
    if e.get("unset"):
        el["unset"] = dict(e["unset"])
    if e.get("whole"):
        # The write is the whole resource as read, the list changed (Okta's
        # PUT /api/v1/apps/{appId} replaces the app).
        el["whole"] = True
    ph = re.findall(r"\{([^}]+)\}", item)
    params = [{"name": p, "field": param_field(item, p)} for p in ph]
    write = mk(r["update"][0], item, r["update"][1])
    desc = {"kind": kind, "group": group, "version": "v1alpha1", "plural": plural(kind), "shape": "upsert",
            "read": mk("GET", item, r["read"]), "update": write, "delete": copy.deepcopy(write), "element": el}
    if params:
        desc["params"] = params
    if envelope:
        desc["envelope"] = envelope
    desc["terraform"] = r["tf"]
    # The resource its sample goes in is one of its own, apart from any
    # other kind's sample at the same path: the fake makes it, empty.
    sample = {p["field"]: f"example-{p['name']}-holder".replace("_", "-") for p in params}
    if e.get("in"):
        f0 = sorted(e["in"])[0]
        sample[f0] = sample_value(sp, f0, fields[f0])
    else:
        for f in required:
            sample[f] = sample_value(sp, f, fields[f])
        for f, v in fields.items():
            if f not in sample and v.get("type") == "string" and not v.get("enum"):
                sample[f] = f"example-{f}".replace("_", "-")
                break
    desc["sample"] = sample
    for p in params:
        fields[p["field"]] = {"type": "string", "description": f"Fills {{{p['name']}}} in {item}."}
    return desc, fields, sorted(set(required) | {p["field"] for p in params}), {p["field"] for p in params}


def element_build(cfg, r, sp, kind, group, env_cfg, ov, kinds, crds, live, spec_query):
    """Adds an Element kind's descriptor and CRD (overrides.elements)."""
    item = r["item"]

    def mk(m, p, op):
        o = {"method": m.upper(), "path": p}
        if cfg.get("versionedMediaTypes"):
            acc, ct = sp.media_types(op)
            h = {k: v for k, v in (("Accept", acc), ("Content-Type", ct)) if v}
            if h:
                o["headers"] = h
        return o
    env_ov = (ov.get("envelope") or {}).get(item)
    envelope = ""
    if env_ov is not None and "response" in env_ov:
        envelope = env_ov["response"] or ""
    else:
        resp = sp.flatten(sp.response_schema(r["read"]) or {})
        envelope = next((e for e in env_cfg.get("response", []) if e in sp.props(resp)), "") or env_cfg.get("always") or ""
    desc, fields, required, param_fields = element_kind(cfg, r, sp, kind, group, mk, envelope)
    desc["readiness"] = "live" if (live.get(f"{kind}.{group}") or {}).get("result") == "pass" else "fake"
    kinds.append(desc)
    schema_props = dict(fields)
    schema_props["connectionRef"] = {"type": "object", "description": f"The {cfg['connectionKind']} to use; default \"default\" in the same namespace.",
                                     "properties": {"name": {"type": "string"}}}
    schema_props["secretRefs"] = {"type": "object", "description": "Fill body fields from Secrets: {<field>: {name, key}}. Use for tokens, passwords and secret values.",
                                  "maxProperties": 64,
                                  "additionalProperties": {"type": "object", "required": ["name", "key"], "properties": {"name": {"type": "string"}, "key": {"type": "string"}}}}
    schema_props["fieldRefs"] = FIELD_REFS
    spec_schema = {"type": "object", "properties": schema_props}
    rules, required = required_rules(required, param_fields, schema_props)
    if rules:
        spec_schema["x-kubernetes-validations"] = rules
    if required:
        spec_schema["required"] = required
    also = cfg["terraform"].get("docsAlso") or {}
    fill_descriptions(spec_schema, [TF_DOCS.get(t) or {} for t in r["tf"] + [o for t in r["tf"] for o in also.get(t) or []]])
    crds.append(crd(group, kind, plural(kind), item, r, spec_schema, desc))
    AUDIT.append({"kind": kind, "item": item, "body": sorted(f for f in fields if f not in param_fields), "resp": [], "anyBody": False, "free": []})


def drop_field(sp, body, dotted, item):
    """body's schema less the field at dotted (metadata.name): no longer
    a property, nor required."""
    body = copy.deepcopy(body)
    node, parts = body, dotted.split(".")
    for part in parts[:-1]:
        props = node.get("properties") or {}
        if part not in props:
            sys.exit(f"overrides.serverNamed: {item} has no field {dotted}")
        props[part] = copy.deepcopy(sp.flatten(props[part]))
        node = props[part]
    if parts[-1] not in (node.get("properties") or {}):
        sys.exit(f"overrides.serverNamed: {item} has no field {dotted}")
    node["properties"] = {k: v for k, v in node["properties"].items() if k != parts[-1]}
    if parts[-1] in (node.get("required") or []):
        node["required"] = [r for r in node["required"] if r != parts[-1]]
    return body


def query_params(sp, path, op):
    """The query parameters a call declares, by name, with their schema and
    description (the path's own and the operation's)."""
    item = sp.doc.get("paths", {}).get(sp.strip + path) or sp.doc.get("paths", {}).get(path) or {}
    out = {}
    for prm in (item.get("parameters") or []) + ((op or {}).get("parameters") or []):
        prm = sp.ref(prm)
        if prm.get("in") == "query" and prm.get("name"):
            out[prm["name"]] = prm
    return out


def _has_body_field(sp, props, path):
    """Return True if a dotted field path exists in the body schema properties."""
    cur = props
    for part in path.split("."):
        if not isinstance(cur, dict) or part not in cur:
            return False
        cur = sp.props(sp.flatten(cur[part]))
    return True


def build(cfg, base):
    prefix = cfg["terraform"]["prefix"]
    gaps = json.load(open(os.path.join(base, cfg["terraform"]["gaps"])))[cfg["terraform"]["provider"]]["gaps"]
    global TRAILING_SLASH
    TRAILING_SLASH = bool(cfg.get("trailingSlash"))
    specs = [Spec(os.path.join(base, s["file"]), s.get("strip", ""), bool(cfg.get("formBodiesAsJSON")))
             for s in cfg["specs"]]
    ov = cfg.get("overrides") or {}
    global SKIP_OPS
    SKIP_OPS = tuple(ov.get("excludeOperationIdPrefixes") or ())
    res = match(find_resources(specs, cfg.get("postUpdates", False), tuple(ov.get("excludePathPrefixes") or ()),
                               ov.get("listReads"), ov.get("assocs") or (), ov.get("creates"), ov.get("deletes"),
                               tuple(ov.get("forget") or ()) + tuple(ov.get("deleteBody") or ()), tuple(ov.get("postUpdates") or ()), ov.get("readFrom"),
                               tuple(ov.get("patchUpserts") or ()), ov.get("updates"), ov.get("keyedBy"), ov.get("createBody")), gaps, prefix,
              # A type a hand-written kind stands in for is that kind's: no
              # spec path is matched to it by its name.
              dict(ov, _handwritten=graphqlgen.claimed_types(cfg, base)))
    res += adopt_resources(specs, ov.get("adopt") or {}, set(gaps), tuple(ov.get("postUpdates") or ()))
    res += state_resources(specs, ov.get("states") or {}, set(gaps), res)
    res += element_resources(specs, ov.get("elements") or {}, set(gaps), tuple(ov.get("postUpdates") or ()), cfg.get("postUpdates", False))
    states = ov.get("states") or {}
    states_used = set()
    create_body_used = set()
    vendor, suffix = cfg["vendor"], cfg["groupSuffix"]
    env_cfg = cfg.get("envelope") or {}
    spec_query = query_from_spec(cfg.get("queryFromSpec"))
    kinds, crds, used = [], [], {}
    # Kinds that may opt in to re-creating in place (vendor.yaml replaceOnChange).
    replace_cfg = cfg.get("replaceOnChange") or {}
    replace_kinds = replace_cfg.get("kinds") or {}
    if isinstance(replace_kinds, list):
        replace_kinds = {k: {} for k in replace_kinds}
    live = live_records(cfg, base)
    # Terraform providers are MPL-2.0: their prose (attribute descriptions,
    # from the registry docs or the schema) is never copied into our files.
    # Descriptions come from the vendor's OpenAPI spec, or none.
    TF_DOCS.clear()
    DESC_STATS.update(fields=0, openapi=0)
    if os.path.exists(os.path.join(base, "terraform-docs.json")):
        sys.exit("hack/gen/terraform-docs.json holds the Terraform provider's prose (MPL-2.0), which is never copied "
                 "into this repository: delete it")
    read_query = ov.get("readQuery") or {}
    read_query_used = set()
    create_query = ov.get("createQuery") or {}
    create_query_used = set()
    delete_query = ov.get("deleteQuery") or {}
    delete_query_used = set()
    delete_ok = ov.get("deleteOK") or {}
    delete_ok_used = set()
    any_value = ov.get("anyValue") or {}
    any_value_used = set()
    extra_fields = ov.get("extraFields") or {}
    extra_fields_used = set()
    id_from_used = set()
    write_only_used = set()
    readable_used = set()
    omit_unchanged_used = set()
    before_delete_used = set()
    create_only_used = set()
    compare_used = set()
    keep_slashes_used = set()
    envelope_used = set()
    secret_outputs_used = set()
    redact_used = set()
    read_from_used = set()
    forget_used = set()
    delete_body_used = set()
    body_array = set(ov.get("bodyArray") or ())
    body_array_used = set()
    body_field = ov.get("bodyField") or {}
    body_field_used = set()
    # overrides.multipart: a path, or {path, files, calls}: string fields
    # that are files all the same, and the calls that send multipart (the
    # others JSON: Grafana k6's load test is made by an upload, changed by a
    # JSON PATCH).
    multipart_cfg = {}
    for e in ov.get("multipart") or ():
        multipart_cfg[e["path"] if isinstance(e, dict) else e] = e if isinstance(e, dict) else {}
    multipart_paths = set(multipart_cfg)
    multipart_used = set()
    bulk_creates = set(ov.get("bulkCreates") or ())
    bulk_creates_used = set()
    read_first = set(ov.get("readFirst") or ())
    read_first_used = set()
    raw_reads = ov.get("rawReads") or {}
    raw_reads_used = set()
    update_body = ov.get("updateBody") or {}
    update_body_used = set()
    carry = ov.get("carry") or {}
    carry_used = set()
    server_named = ov.get("serverNamed") or {}
    absent = ov.get("absent") or {}
    absent_used = set()
    update_query = ov.get("updateQuery") or {}
    update_query_used = set()
    query_fields = ov.get("queryFields") or {}
    query_fields_used = set()
    headers_ov = ov.get("headers") or {}
    headers_used = set()
    param_fields_ov = ov.get("paramFields") or {}
    param_fields_used = set()
    outer_body = ov.get("outerBody") or {}
    outer_body_used = set()
    update_id_ov = ov.get("updateID") or {}
    update_id_used = set()
    updates_used = set()
    writable_ov = ov.get("writable") or {}
    writable_used = set()
    for r in sorted(res, key=lambda r: r["tf"][0]):
        sp = r["spec"]
        tf0 = max(r["tf"], key=lambda g: (len(toks(g)), -len(g)))
        words = tf0[len(prefix) + 1:].split("_")
        kind = camel(words)
        named = ((cfg.get("overrides") or {}).get("kindNames") or {}).get(r["item"]) if not (r.get("adopt") or r.get("element")) else None
        if named:
            # One API resource for several Terraform types (Okta's app_oauth, app_saml, ...).
            kind = named
            words = [named[0].lower() + re.sub(r"([A-Z])", r"_\1", named[1:]).lower()][0].split("_")
        area = words[0] if words[0] != "zero" else "zerotrust"
        group = f"{area}.{suffix}"
        key = (group, kind)
        if key in used:
            if r.get("adopt") or r.get("element"):
                sys.exit(f"overrides.{'adopt' if r.get('adopt') else 'elements'}: {r['tf'][0]}: kind {kind} in {group} is another's")
            continue
        used[key] = True
        if r.get("element"):
            element_build(cfg, r, sp, kind, group, env_cfg, ov, kinds, crds, live, spec_query)
            continue
        item = r["item"]
        ph = re.findall(r"\{([^}]+)\}", item)
        create_op = r.get("create")
        upd = r.get("update")
        body_op = create_op if (r["shape"] in ("crud", "assoc") or create_op is not None) else (upd[1] if upd else None)
        if r.get("adopt"):
            body_op = upd[1]  # nothing creates it: the update's body
        mp_files = None
        if item in multipart_paths:
            mp_sch, mp_files = sp.multipart_schema(body_op, tuple(multipart_cfg[item].get("files") or ()))
            missing = sorted(set(multipart_cfg[item].get("files") or ()) - set(mp_files))
            if missing:
                sys.exit(f"overrides.multipart: {item} has no multipart field {', '.join(missing)}")
            if not mp_sch:
                sys.exit(f"overrides.multipart: {item} has no multipart/form-data body")
            body = sp.flatten(mp_sch)
            multipart_used.add(item)
        else:
            body = sp.flatten(sp.body_schema(body_op) or {"type": "object", "properties": {}})
        if r.get("createBody"):
            body = {"type": "object", "properties": {}}  # the create's body is the template, filled from the path
        if item in extra_fields:
            body = copy.deepcopy(body)
            props = body.setdefault("properties", {})
            req = list(body.get("required") or [])
            for fn, fsch in extra_fields[item].items():
                fcopy = dict(fsch)
                if fcopy.pop("required", False) and fn not in req:
                    req.append(fn)
                props[fn] = fcopy
            if req:
                body["required"] = req
            extra_fields_used.add(item)
        is_body_array = False
        bfield = ""
        array_env = ""
        if item in body_array:
            if item in body_field:
                sys.exit(f"overrides: {item} cannot be in both bodyArray and bodyField")
            if body.get("type") != "array":
                # The array inside a request envelope (PagerDuty's {"subscribers": [...]}):
                # the one named by overrides.envelope, or the body's only field.
                bprops_ = body.get("properties") or {}
                name = ((ov.get("envelope") or {}).get(item) or {}).get("request") or (next(iter(bprops_)) if len(bprops_) == 1 else "")
                if not name or sp.flatten(bprops_.get(name) or {}).get("type") != "array":
                    sys.exit(f"overrides.bodyArray: {item} body is not an array, nor an envelope holding one")
                array_env = name
                body = sp.flatten(bprops_[name])
            body = sp.flatten(body.get("items") or {})
            is_body_array = True
            body_array_used.add(item)
        elif item in body_field:
            if body.get("type") != "array":
                sys.exit(f"overrides.bodyField: {item} body is not an array")
            bfield = body_field[item]
            body_field_used.add(item)
        resp = sp.flatten(sp.response_schema(r["read"]) or {})
        list_at = (r.get("find") or {}).get("list") if r.get("readPath") else ""
        if r["shape"] == "assoc" and (r.get("assoc") or {}).get("list"):
            list_at = r["assoc"]["list"]  # a link's fields are its list element's ({"data": [...]})
        for seg in (list_at or "").split("/") if list_at else []:
            resp = sp.flatten((resp.get("properties") or {}).get(seg) or {})  # readFrom: where the list's array is
        if resp.get("type") == "array" and not bfield:  # a list read: describe one element
            resp = sp.flatten(resp.get("items") or {})
        envelope = ""
        # overrides.envelope: one kind's envelopes, whatever the vendor's
        # (Grafana's ML API wraps every answer in data; Annotation does not).
        env_ov = (ov.get("envelope") or {}).get(item)
        if env_ov is not None:
            envelope_used.add(item)
        elem = None
        if r.get("readPath") and r.get("find"):
            # readFrom: the fields are a list element's; the envelope is the
            # one the writes answer in (Datadog's data), where the list is too.
            elem = resp
            wop = r.get("create") or (r.get("update") or (None, None))[1]
            resp = sp.flatten(sp.response_schema(wop) or {}) if wop else {}
        if env_ov is not None and "response" in env_ov:
            envelope = env_ov["response"] or ""
            if envelope and envelope in (resp.get("properties") or {}):
                resp = sp.flatten(resp["properties"][envelope])  # else the spec describes the inner object
        for e in ([] if env_ov is not None and "response" in env_ov else env_cfg.get("response", [])):
            if e in (resp.get("properties") or {}):
                envelope = e
                resp = sp.flatten(resp["properties"][e])
                break
        if not envelope and env_cfg.get("always") and not (env_ov is not None and "response" in env_ov):
            # The spec describes the inner object only (Vault's "data").
            envelope = env_cfg["always"]
        rp = resp.get("properties") or {}
        if not envelope and env_cfg.get("auto") and len(rp) == 1 and not (env_ov is not None and "response" in env_ov):
            # PagerDuty: {"service": {...}}, named after the resource.
            (e, v), = rp.items()
            if sp.flatten(v).get("type", "object") == "object" and sp.flatten(v).get("properties"):
                envelope = e
                resp = sp.flatten(v)
        if elem is not None:
            resp = elem
        if item in read_first:
            # The read answers the object as a one-element list (Cloudflare's
            # Cloudforce One asset: {"result": [{...}]}): read.envelope <envelope>/0.
            if resp.get("type") != "array":
                sys.exit(f"overrides.readFirst: {item}: the read does not answer a list")
            resp = sp.flatten(resp.get("items") or {})
            read_first_used.add(item)
        if bfield and resp.get("type") == "array":
            resp = {"type": "object", "properties": {bfield: resp}}
        body_env = array_env
        if array_env:
            pass  # bodyArray already unwrapped its envelope
        elif env_ov is not None and "request" in env_ov:
            body_env = env_ov["request"] or ""
            if body_env and body_env in (body.get("properties") or {}):
                body = sp.flatten(body["properties"][body_env])
        for e in ([] if array_env or (env_ov is not None and "request" in env_ov) else env_cfg.get("request", [])):
            writable = [k for k, v in (body.get("properties") or {}).items() if not sp.ref(v).get("readOnly")]
            if e in writable and len(writable) <= 2:
                body_env = e
                body = sp.flatten(body["properties"][e])
                break
        bp = body.get("properties") or {}
        if not body_env and env_cfg.get("auto") and len(bp) == 1 and not (env_ov is not None and "request" in env_ov):
            (e, v), = bp.items()
            if sp.flatten(v).get("type", "object") == "object" and sp.flatten(v).get("properties"):
                body_env = e
                body = sp.flatten(v)
        if item in bulk_creates:
            # A bulk create: it takes an array of objects (inside the request
            # envelope) and answers one, the object first (Cloudflare's
            # secrets: POST [{...}] answers {"result": [{...}]}); the other
            # calls take and answer the object.
            if r["shape"] != "crud" or body.get("type") != "array":
                sys.exit(f"overrides.bulkCreates: {item} is not a CRUD kind whose create takes an array")
            body = sp.flatten(body.get("items") or {})
            bulk_creates_used.add(item)
        if bfield:
            body = {"type": "object", "properties": {bfield: body}, "required": [bfield]}
        # A reference object ({"service": {"id", "type"}}) requires its id on write, but the
        # spec inherits id as readOnly from a shared base. Required and readOnly together can
        # only mean a reference, so keep the id writable (PagerDuty associations).
        ref_id = False
        if body_env and r["shape"] == "crud" and "id" in (body.get("required") or []):
            idp = sp.props(body).get("id")
            # Only when nothing else names the target: no writable field but constants (type).
            real = [k for k, v in sp.props(body).items() if k != "id" and not sp.ref(v).get("readOnly")
                    and len(sp.flatten(v).get("enum") or []) != 1]
            if isinstance(idp, dict) and sp.ref(idp).get("readOnly") and not real:
                body = copy.deepcopy(body)
                for rk in body["required"]:
                    rp_ = sp.flatten(sp.props(body).get(rk) or {})
                    if rp_.get("readOnly"):
                        rp_ = {k: v for k, v in rp_.items() if k != "readOnly"}
                        if len(rp_.get("enum") or []) == 1:
                            rp_["x-hfl-default"] = rp_["enum"][0]  # the constant `type`: default it
                        body["properties"][rk] = rp_
                ref_id = True
        if item in server_named:
            # The server names it (Grafana's time intervals: a create naming
            # one is refused, 400 "metadata.name should be empty"): the field
            # is no spec field; the ID comes from it, and updates send it back.
            body = drop_field(sp, body, server_named[item], item)
        if item in writable_ov:
            # Fields the spec marks read-only that the request does take
            # (PagerDuty's change_tags names a tag by its id): overrides.writable.
            body = copy.deepcopy(body)
            for f in writable_ov[item]:
                prop = (body.get("properties") or {}).get(f)
                if prop is None:
                    sys.exit(f"overrides.writable: {item} has no body field {f}")
                body["properties"][f] = {k: v for k, v in sp.ref(prop).items() if k != "readOnly"}
            writable_used.add(item)
        bprops = {k: v for k, v in (sp.props(body)).items() if not (isinstance(v, dict) and v.get("readOnly"))}
        rprops = set(sp.props(resp))
        # A body that may be any JSON object and names no field (Vault's KV v1
        # secret): its keys are the caller's own, set in spec beside the path
        # fields, and the CRD keeps fields it does not list.
        free_body = (not bprops and r["shape"] != "assoc"
                     and body.get("additionalProperties") not in (None, False))
        id_param = ph[-1] if r["shape"] == "crud" else None
        params, fields = [], {}
        ph_all = ph + [x for x in re.findall(r"\{([^}]+)\}", r.get("coll") or "") if x not in ph] if r["shape"] == "crud" else ph
        pf = param_fields_ov.get(item) or {}
        for p in ph_all:
            if p == id_param:
                continue
            if p in pf:
                # The spec field that fills the placeholder: a body field under
                # another name, which the body sends too (Confluent's tag
                # binding: {typeName} is the body's entityType), or a name of
                # its own where the placeholder's is a body field's (PagerDuty's
                # change_tags: {id} is the entity's, the body's id the tag's).
                param_fields_used.add(item)
                if pf[p] in bprops or "." in pf[p] and _has_body_field(sp, bprops, pf[p]):
                    # A body field, perhaps nested (Datadog's custom forecast:
                    # {budget_id} is attributes.budget_uid).
                    params.append({"name": p, "field": pf[p], "inBody": True})
                    continue
                params.append({"name": p, "field": pf[p]})
                fields[pf[p]] = {"type": "string", "description": f"Fills {{{p}}} in {item}."}
                continue
            f = p if p in bprops and not (ref_id and p == "id") else param_field(item, p)
            if r["shape"] == "assoc" and p == "name":
                # /roles/{name}/grants: the field is the role, not the grant's name.
                segs = item.split("/")
                f = camel([sing(x) for x in segs[segs.index("{name}") - 1].replace("-", "_").split("_")], upper=False)
            params.append({"name": p, "field": f})
            fields[f] = {"type": "string", "description": f"Fills {{{p}}} in {item}."}
        if r.get("keyedBy"):
            if r["keyedBy"] not in bprops:
                sys.exit(f"overrides.keyedBy: {item} has no body field {r['keyedBy']}")
            params.append({"name": r["keyedBy"], "field": r["keyedBy"], "inBody": True})
        desc = {"kind": kind, "group": group, "version": "v1alpha1", "plural": plural(kind), "shape": r["shape"]}
        qf = query_fields.get(item) or []
        def mk(m, p, op):
            o = {"method": m.upper(), "path": p}
            q = spec_query(sp, p, op, bprops)
            for name, prm in query_params(sp, p, op).items():
                # overrides.queryFields: a spec field of its own, sent as this
                # query parameter on each call that takes it (Grafana Cloud's
                # ?region=), never in the body.
                if name in qf:
                    q[name] = "{%s}" % name
                    qf_schema.setdefault(name, prm)
            if q:
                o["query"] = q
            if cfg.get("versionedMediaTypes"):
                acc, ct = sp.media_types(op)
                h = {}
                if acc:
                    h["Accept"] = acc
                if ct:
                    h["Content-Type"] = ct
                if h:
                    o["headers"] = h
            return o
        qf_schema = {}
        if create_op:
            desc["create"] = mk(r.get("createMethod") or "POST", r.get("createPath") or r.get("coll") or item, create_op)
            if r.get("createBody"):
                # The create's whole body, its {param} templates filled from the
                # path's parameters (overrides.createBody).
                for ph_ in re.findall(r"\{([^{}]+)\}", json.dumps(r["createBody"])):
                    if ph_ not in ph:
                        sys.exit(f"overrides.createBody: {item}: no param fills {{{ph_}}}")
                desc["create"]["body"] = copy.deepcopy(r["createBody"])
                create_body_used.add(item)
            if item in create_query:
                # A create that does more than make the item unless told not
                # to (Okta activates a new user, and emails it, unless
                # activate=false); the spec names the query, not the choice.
                desc["create"]["query"] = {**desc["create"].get("query", {}), **{q: str(v).lower() if isinstance(v, bool) else str(v) for q, v in create_query[item].items()}}
                create_query_used.add(item)
        if r.get("find"):
            desc["read"] = mk("GET", r.get("readPath") or r["coll"], r["read"])
            desc["read"]["find"] = {k2: v2 for k2, v2 in r["find"].items() if k2 in ("list", "field") and v2 != ""}
            desc["read"]["find"]["param"] = r.get("findParam") or ph[-1]
            if r.get("readPath"):
                read_from_used.add(item)
        elif r.get("readPath"):
            # readFrom with no field: one object, read on another path.
            desc["read"] = mk("GET", r["readPath"], r["read"])
            read_from_used.add(item)
        else:
            desc["read"] = mk("GET", item, r["read"])
        if item in absent:
            # A read that answers 200 with these when there is none (Atlas's
            # state: UNCONFIGURED): overrides.absent.
            desc["read"]["absent"] = dict(absent[item])
            absent_used.add(item)
        if item in read_first:
            desc["read"]["envelope"] = (envelope + "/" if envelope else "") + "0"
        if item in bulk_creates:
            desc["create"]["bodyArray"] = True
            desc["create"]["envelope"] = (envelope + "/" if envelope else "") + "0"
        if item in raw_reads:
            # The read answers the field's value as it is, not JSON (Workers KV).
            if raw_reads[item] not in bprops:
                sys.exit(f"overrides.rawReads: {item} has no body field {raw_reads[item]}")
            desc["read"]["raw"] = raw_reads[item]
            raw_reads_used.add(item)
        if item in read_query:
            # A read that answers in full only when asked (Vault's UI headers:
            # multivalue=true); the spec names the query but not when it is needed.
            desc["read"]["query"] = {q: str(v).lower() if isinstance(v, bool) else str(v) for q, v in read_query[item].items()}
            read_query_used.add(item)
        if upd:
            desc["update"] = mk(upd[0], r.get("updatePath") or item, upd[1])
            if r.get("updatePath"):
                updates_used.add(item)
            if item in update_body:
                desc["update"]["body"] = dict(update_body[item])
                update_body_used.add(item)
            if item in update_query:
                desc["update"]["query"] = {**desc["update"].get("query", {}), **{q: str(v).lower() if isinstance(v, bool) else str(v) for q, v in update_query[item].items()}}
                update_query_used.add(item)
        if r.get("delete"):
            if r["shape"] == "assoc":
                a = r["assoc"]
                desc["delete"] = dict(mk(a.get("deleteMethod", "POST"), a["delete"], r["delete"]), sendBody=True)
                if a.get("deleteQuery"):
                    # Undone by a query naming the link's fields, with no body
                    # (Grafana's DELETE /teams/{teamId}/groups?groupId=).
                    desc["delete"]["query"] = {**desc["delete"].get("query", {}), **{q: str(v) for q, v in a["deleteQuery"].items()}}
                    desc["delete"]["sendBody"] = bool(a.get("sendBody", False))
                if a.get("deleteEnvelope"):
                    # The undo's body in another wrapper than the create's
                    # (PagerDuty's change_tags: {"add": ...} and {"remove": ...}).
                    desc["delete"]["bodyEnvelope"] = a["deleteEnvelope"]
                if not desc["delete"].get("sendBody"):
                    desc["delete"].pop("sendBody", None)
                if a.get("list"):
                    # Where the read's array is ({"subscribers": [...]}): assocs' list.
                    desc["read"]["find"] = {"list": a["list"]}
            else:
                desc["delete"] = mk("DELETE", r.get("deletePath") or item, r["delete"])
                if r.get("keyedBy"):
                    desc["delete"]["sendBody"] = True  # the body names what to delete
                if item in delete_query:
                    desc["delete"]["query"] = {**desc["delete"].get("query", {}), **{q: str(v).lower() if isinstance(v, bool) else str(v) for q, v in delete_query[item].items()}}
                    delete_query_used.add(item)
                if item in delete_ok:
                    # Statuses the delete answers for what goes only with its
                    # parent (Cloudflare's 409 for a Worker's latest version).
                    codes = delete_ok[item]
                    if not codes or not all(isinstance(c, int) and 300 <= c <= 599 for c in codes):
                        sys.exit(f"overrides.deleteOK: {item} must list HTTP statuses 300-599")
                    desc["delete"]["ok"] = sorted(set(codes))
                    delete_ok_used.add(item)
        if item in (ov.get("beforeDelete") or {}) and "delete" in desc and r["shape"] != "assoc":
            # Calls the API needs before it deletes the item (GitLab stops an
            # environment first); the spec cannot say so.
            desc["beforeDelete"] = [{"method": c["method"].upper(), "path": c["path"]} for c in ov["beforeDelete"][item]]
            before_delete_used.add(item)
        if kind in replace_kinds:
            desc["replace"] = replace_op(replace_cfg, replace_kinds[kind] or {}, kind, r, upd, mk, create_op, sp)
        ci = (r.get("find") or {}).get("caseInsensitive") or cfg.get("caseInsensitive")
        if ci:
            desc["caseInsensitive"] = ci
        if params:
            desc["params"] = params
        if id_param:
            tail = id_param.rsplit("_", 1)[1] if "_" in id_param else ""
            id_from = r.get("idFromOwn") or (ov.get("idFrom") or {}).get(item) or (server_named[item].replace(".", "/") if item in server_named else None)
            if id_from:
                # The path names the item by a field the spec does not tie to
                # it (GitLab's feature_flag_name is the flag's name).
                frm = id_from
                if item in (ov.get("idFrom") or {}) and not r.get("idFromOwn"):
                    id_from_used.add(item)
                if r.get("idFromOwn"):
                    frm = frm.replace(".", "/")  # imported_issuers.0: the first of a list
            elif id_param in rprops:
                frm = id_param
            elif tail in ("iid", "number") and tail in rprops:
                # The path takes another key than the ID: GitLab's issue_iid
                # (iid), GitHub's milestone_number (number).
                frm = tail
            elif "id" in rprops or not rprops:
                frm = "id"
            else:
                frm = id_param if id_param in bprops else "id"
            desc["id"] = {"param": id_param, "from": frm}
            uid = env_cfg.get("updateID")
            if uid and upd and body_env:
                # JSON:API: an update names the item in its body too (data.id),
                # which the create body has no field for (Datadog v2). Only
                # where the update's body schema has the field.
                ub = sp.flatten(sp.body_schema(upd[1]) or {})
                ub = sp.flatten(sp.props(ub).get(body_env) or {})
                if uid in sp.props(ub):
                    desc["updateID"] = uid
        wo = sorted(k for k in bprops if rprops and k not in rprops)
        if item in (ov.get("readable") or {}):
            # Fields a read does return though the spec's response omits them
            # (Sentry's organization settings): compared as usual.
            rd = set(ov["readable"][item])
            if rd - set(wo):
                sys.exit(f"overrides.readable: {item}: not write-only: {', '.join(sorted(rd - set(wo)))}")
            wo = [k for k in wo if k not in rd]
            readable_used.add(item)
        if item in (ov.get("writeOnly") or {}):
            # Fields inside an object that a read masks or leaves out (Grafana's
            # SSO settings.clientSecret): the spec cannot say so.
            wo = sorted(set(wo) | set(ov["writeOnly"][item]))
            write_only_used.add(item)
        if r.get("writeOnlyOwn"):
            # Fields this kind's create takes that its read never answers as
            # sent (an imported bundle's private key): overrides.creates' entry.
            wo = sorted(set(wo) | set(r["writeOnlyOwn"]))
        if wo:
            desc["writeOnly"] = wo
        if cfg.get("createOnlyFromSpec") and r["shape"] == "crud" and upd:
            # vendor.yaml createOnlyFromSpec: body fields the create takes and
            # the update's own body does not are left out of updates, which
            # refuse them (PagerDuty's custom field name and data_type).
            ub = sp.flatten(sp.body_schema(upd[1]) or {})
            if body_env and body_env in (ub.get("properties") or {}):
                ub = sp.flatten(ub["properties"][body_env])
            ubp = {k for k, v in sp.props(ub).items() if not v.get("readOnly")}
            co = sorted(k for k in bprops if ubp and k not in ubp and k not in wo and k not in fields)
            if co:
                desc["createOnly"] = co
        if item in (ov.get("createOnly") or {}):
            # Fields only the create takes that the spec does not show as such:
            # Okta's custom role permissions and resource set resources, which
            # a read never returns and an update refuses (400 E0000003).
            if r["shape"] not in ("crud", "upsert") or not upd:
                sys.exit(f"overrides.createOnly: {item} is not a CRUD or upsert kind with an update")
            missing = sorted(f for f in ov["createOnly"][item] if not _has_body_field(sp, bprops, f))
            if missing:
                sys.exit(f"overrides.createOnly: {item} has no body field {', '.join(missing)}")
            desc["createOnly"] = sorted(set(desc.get("createOnly") or []) | set(ov["createOnly"][item]))
            create_only_used.add(item)
        if r.get("createOnlyOwn"):
            # Fields only this kind's create takes (overrides.creates' entry).
            if not upd:
                sys.exit(f"overrides.creates: {item}: createOnly needs an update (without one, every field is create-only)")
            desc["createOnly"] = sorted(set(desc.get("createOnly") or []) | set(r["createOnlyOwn"]))
        if item in (ov.get("omitUnchanged") or {}):
            # Fields that can be updated, but must be left out of an update
            # when they already match the API (Confluent KafkaCluster refuses
            # a PATCH carrying config together with display_name).
            if r["shape"] != "crud" or not upd:
                sys.exit(f"overrides.omitUnchanged: {item} is not a CRUD kind with an update")
            missing = sorted(set(ov["omitUnchanged"][item]) - set(bprops))
            if missing:
                sys.exit(f"overrides.omitUnchanged: {item} has no body field {', '.join(missing)}")
            desc["omitUnchanged"] = sorted(set(ov["omitUnchanged"][item]))
            omit_unchanged_used.add(item)
        if cfg.get("immutableFromSpec") and r["shape"] == "crud" and upd:
            # vendor.yaml immutableFromSpec: x-immutable body fields are create-only
            # (Confluent Cloud refuses a PATCH naming one, even unchanged).
            imm = {k for k, v in bprops.items() if isinstance(v, dict) and sp.ref(v).get("x-immutable") and k not in wo and k not in fields}
            if imm:
                desc["createOnly"] = sorted(imm | set(desc.get("createOnly") or ()))
        if item in (ov.get("compare") or {}):
            # Fields read back in another form than written (GitLab's
            # application scopes: a string in, an array out).
            desc["compare"] = dict(ov["compare"][item])
            compare_used.add(item)
        if bprops and not rprops and r["shape"] != "assoc":
            # The spec does not say what a read returns (much of Vault's):
            # compare only the fields the API sends back.
            desc["compareReturnedOnly"] = True
        if r.get("adopt"):
            # It exists before the object and after: found by its fields, then
            # updated; deleting the object lets it go (overrides.adopt).
            a = r["adopt"]
            ad = mk("GET", a["list"], r["listOp"])
            if a.get("query"):
                ad["query"] = {q: str(v).lower() if isinstance(v, bool) else str(v) for q, v in a["query"].items()}
            if a.get("at"):
                ad["find"] = {"list": a["at"]}
            ad["match"] = {f: str(v).lower() if isinstance(v, bool) else str(v) for f, v in a["match"].items()}
            desc["adopt"] = ad
            desc["forget"] = str(a.get("reason") or f"{kind} exists before the object and after it: deleting the object leaves it as it is")
        if item in states:
            # A field set by a call per value (overrides.states): Okta's
            # lifecycle activate and deactivate, subscribe and unsubscribe.
            st = states[item]
            calls = {}
            for v, c in st["calls"].items():
                cp = c["path"]
                for ph_ in re.findall(r"\{([^}]+)\}", cp):
                    if ph_ not in ph and ph_ not in re.findall(r"\{([^}]+)\}", r.get("coll") or ""):
                        sys.exit(f"overrides.states: {item}: no param fills {{{ph_}}} in {cp}")
                calls[str(v)] = {"method": (c.get("method") or "POST").upper(), "path": cp}
            desc["state"] = {"field": st["field"], "calls": calls}
            if st.get("delete"):
                if str(st["delete"]) not in calls:
                    sys.exit(f"overrides.states: {item}: delete {st['delete']} has no call")
                desc["state"]["delete"] = str(st["delete"])
                desc.pop("forget", None)  # put in that state, not let go
            fields[st["field"]] = {"type": "string", "enum": sorted(calls),
                                   "description": st.get("description") or "The state to put it in, by its call (" + ", ".join("%s: %s %s" % (v, c["method"], c["path"]) for v, c in sorted(calls.items())) + ")"
                                   + (f"; deleting the object sets {st['delete']}." if st.get("delete") else ". Unset: left as it is.")}
            states_used.add(item)
        if item in (ov.get("deleteBody") or {}):
            # A delete that is a write (GitHub closes an issue: PATCH {"state": "closed"},
            # Grafana removes a permission with a POST of {"permission": ""}), or one
            # that sends a body (Grafana Asserts' DELETE listing the thresholds to
            # remove: sendSpec): overrides.deleteBody. With stays, the resource is
            # still readable after it (a closed issue); without, it is gone.
            db = ov["deleteBody"][item]
            desc["delete"] = {"method": db.get("method", "PATCH").upper(), "path": db.get("path") or item}
            if db.get("sendSpec"):
                desc["delete"]["sendBody"] = True
            elif "body" in db:
                desc["delete"]["body"] = copy.deepcopy(db["body"])
            else:
                sys.exit(f"overrides.deleteBody: {item} needs body or sendSpec")
            for ph_ in re.findall(r"\{([^}]+)\}", desc["delete"]["path"]):
                if ph_ not in ph and ph_ not in re.findall(r"\{([^}]+)\}", r.get("coll") or ""):
                    sys.exit(f"overrides.deleteBody: {item}: no param fills {{{ph_}}} in {desc['delete']['path']}")
            if db.get("stays"):
                desc["stays"] = str(db["stays"])
            delete_body_used.add(item)
        elif r.get("forgetOwn"):
            desc["forget"] = str(r["forgetOwn"])  # overrides.creates' entry
        elif item in (ov.get("forget") or {}):
            # The API cannot delete it: deleting the object lets go of the
            # resource, as Terraform does (an export job, an incident type).
            if r.get("delete"):
                sys.exit(f"overrides.forget: {item} has a delete")
            desc["forget"] = str(ov["forget"][item])
            forget_used.add(item)
        if item in carry or item in server_named:
            # Fields an update (or delete) sends as a read just answered them:
            # overrides.carry (a resourceVersion, a blob sha), and the name the
            # server chose (serverNamed).
            c = carry.get(item) or []
            c = {"update": list(c)} if isinstance(c, list) else dict(c)
            if item in server_named:
                c["update"] = [server_named[item]] + [f for f in c.get("update") or [] if f != server_named[item]]
            for opname, carried in c.items():
                if opname not in ("update", "delete") or not carried:
                    sys.exit(f"overrides.carry: {item}: only update and delete carry fields")
                if opname not in desc:
                    sys.exit(f"overrides.carry: {item} has no {opname}")
                desc[opname]["carry"] = list(carried)
            carry_used.add(item)
        if item in headers_ov:
            # Headers a call needs, which the spec does not describe: fixed, or
            # filled from the spec ({field}) or from the resource as the API
            # answered it last ({current.scope}: Grafana Assistant's
            # X-Resource-Scope, the scope the object has now).
            for call, hs in headers_ov[item].items():
                if call not in ("create", "read", "update", "delete") or call not in desc:
                    sys.exit(f"overrides.headers: {item} has no {call} call")
                desc[call]["headers"] = {**desc[call].get("headers", {}), **{h: str(v) for h, v in hs.items()}}
            headers_used.add(item)
        if item in outer_body:
            # Fields beside the request envelope on every write and the
            # delete (Datadog's sensitive data scanner: {"meta": {}}).
            for call in ("create", "update", "delete"):
                if call in desc:
                    desc[call]["outer"] = copy.deepcopy(outer_body[item])
            outer_body_used.add(item)
        if item in update_id_ov:
            # The update names the item in its body (an update on another
            # path than the item's: Grafana SM's POST /check/update).
            if not upd or "id" not in desc:
                sys.exit(f"overrides.updateID: {item} is not a CRUD kind with an update")
            desc["updateID"] = update_id_ov[item]
            update_id_used.add(item)
        if qf:
            missing = sorted(set(qf) - set(qf_schema))
            if missing:
                sys.exit(f"overrides.queryFields: {item}: no call takes query parameter {', '.join(missing)}")
            clash = sorted(set(qf) & (set(bprops) | {p_["field"] for p_ in params}))
            if clash:
                sys.exit(f"overrides.queryFields: {item}: {', '.join(clash)} is a body or path field already")
            desc["queryFields"] = list(qf)
            for name in qf:
                prm = qf_schema[name]
                fs = dict(structural(sp, sp.flatten(prm.get("schema") or {"type": "string"})))
                if prm.get("description") and not fs.get("description"):
                    fs["description"] = prm["description"]
                fields[name] = fs
            query_fields_used.add(item)
        if envelope:
            desc["envelope"] = envelope
        if body_env:
            desc["bodyEnvelope"] = body_env
        if is_body_array:
            desc["bodyArray"] = True
        if bfield:
            desc["bodyField"] = bfield
        if mp_files is not None:
            desc["multipart"] = {"files": mp_files}
            calls = list(multipart_cfg[item].get("calls") or ())
            if calls:
                if not set(calls) <= {"create", "update"} or any(c not in desc for c in calls):
                    sys.exit(f"overrides.multipart: {item}: calls must be the kind's create or update")
                desc["multipart"]["calls"] = calls
        desc["terraform"] = r["tf"]
        sample = {}
        for p in params:
            if p.get("inBody"):
                continue  # a body field: sampled as one, below
            sample[p["field"]] = f"example-{p['name']}".replace("_", "-")
        for name in desc.get("queryFields") or ():
            sample[name] = f"example-{name}".replace("_", "-")
        for req in (body.get("required") or []):
            if req in bprops and req not in sample:
                sample[req] = sample_value(sp, req, bprops[req])
        for f, v in ((r.get("adopt") or {}).get("sample") or {}).items():
            # An adopted object's sample: what it already holds (Okta's
            # default authorization server is named default).
            if f not in bprops:
                sys.exit(f"overrides.adopt: {r['tf'][0]}: sample names {f}, no body field")
            sample[f] = v
        for p in params:
            if p.get("inBody") and "." in p["field"]:
                # Nested in the body (attributes.budget_uid): set there.
                node, parts = sample, p["field"].split(".")
                for part in parts[:-1]:
                    if not isinstance(node.get(part), dict):
                        node[part] = {}
                    node = node[part]
                node.setdefault(parts[-1], f"example-{p['name']}".replace("_", "-"))
            elif p.get("inBody") and p["field"] not in sample:
                sample[p["field"]] = f"example-{p['name']}".replace("_", "-")
        if r["shape"] == "assoc" and not set(sample) - {p["field"] for p in params}:
            # A link is its fields: one the spec requires none of is sampled
            # whole (PagerDuty's subscriber_id and subscriber_type).
            for k, v in bprops.items():
                sample[k] = sample_value(sp, k, v)
        if not any(isinstance(v, str) and k not in [p["field"] for p in params] for k, v in sample.items()):
            for k, v in bprops.items():
                vv = sp.flatten(v)
                if vv.get("type") == "string" and not vv.get("enum") and k not in wo and not k.endswith("_id") and k != "id":
                    sample[k] = f"example-{k}".replace("_", "-")
                    break
        for ref in re.findall(r"\{([^{}]+)\}", " ".join(str(v) for v in (r.get("adopt") or {}).get("match", {}).values())):
            # An adopting kind's sample names what it finds it by.
            if ref not in sample:
                if ref not in bprops:
                    sys.exit(f"overrides.adopt: {r['tf'][0]}: match names {{{ref}}}, no body field")
                sample[ref] = sample_value(sp, ref, bprops[ref])
        if "state" in desc and desc["state"]["field"] not in sample:
            st = states[item]
            sample[desc["state"]["field"]] = str(st.get("sample") or next(v for v in desc["state"]["calls"] if v != desc["state"].get("delete")))
        if free_body:
            ap = body.get("additionalProperties")
            sample["example"] = sample_value(sp, "example", ap) if isinstance(ap, dict) and ap else "example-value"
        desc["sample"] = sample
        # What a read answers that no request sends (a timestamp, a URL, a
        # status), as sample values: the fake answers them too, so a
        # rehearsal's cassettes look like the API's (kindtest.Rehearse).
        server = server_fields(sp, sp.props(resp), bprops, desc)
        for f, v in ((desc["read"].get("absent") or {}).items() if "." not in "".join(desc["read"].get("absent") or {}) else []):
            # What the fake answers for an object that is there must not read
            # as absent: another enum value than the marker, and a field
            # whose absence is the marker (an id) set.
            fs = sp.flatten(sp.props(resp).get(f) or {})
            if v is None and f not in server and f not in bprops:
                server[f] = sample_value(sp, f, fs) if fs else f"example-{f}"
            elif v is not None and server.get(f) == v:
                other = [e for e in fs.get("enum") or [] if e != v]
                if other:
                    server[f] = other[0]
                else:
                    server.pop(f)
        if "state" in desc:
            server.pop(desc["state"]["field"], None)  # set by its calls, as the spec asks
        if server:
            desc["serverFields"] = server
        # A secret the API makes and answers (an API key's privateKey, a
        # token): never kept in the status; written to spec.writeSecretRef.
        so = r["secretOutputsOwn"] if "secretOutputsOwn" in r else (ov.get("secretOutputs") or {}).get(item)
        if "secretOutputsOwn" in r:
            pass  # the creates entry's own (a root CA's private_key, not its issuer's)
        elif so is not None:
            secret_outputs_used.add(item)
        else:
            so = secret_outputs(sp, sp.props(resp), bprops)
        if so:
            desc["secretOutputs"] = sorted(so)
        rd = (ov.get("redact") or {}).get(item)
        if rd:
            # Fields whose values are sensitive though the kind keeps them
            # (an accessor that is the kind's ID): kept out of cassettes,
            # logs and live records (descriptor's redact).
            redact_used.add(item)
            desc["redact"] = sorted(rd)
        for name in (ov.get("keepSlashes") or {}).get(item) or ():
            # A path parameter whose value keeps its slashes ({+path}: a
            # GitHub file in a directory, .../contents/docs/README.md, as
            # go-github sends it): overrides.keepSlashes.
            found = False
            for op in [desc.get(n) for n in ("create", "read", "update", "delete")] + list(desc.get("beforeDelete") or ()):
                if op and "{" + name + "}" in (op.get("path") or ""):
                    op["path"] = op["path"].replace("{" + name + "}", "{+" + name + "}")
                    found = True
            if not found:
                sys.exit(f"overrides.keepSlashes: {item} has no {{{name}}}")
            keep_slashes_used.add(item)
        # "live" once the kind passed create, update, drift and delete against
        # the real API (kindtest.Live's record); "fake" until then.
        desc["readiness"] = "live" if (live.get(f"{kind}.{desc['group']}") or {}).get("result") == "pass" else "fake"
        kinds.append(desc)
        AUDIT.append({"kind": kind, "item": item, "body": sorted(bprops), "resp": sorted(rprops), "anyBody": free_body,
                      "free": sorted(k for k, v in bprops.items() if isinstance(v, dict)
                                     and sp.flatten(v).get("type", "object") == "object" and not sp.flatten(v).get("properties"))})

        schema_props = dict(fields)
        st = structural(sp, body)
        for k, v in (st.get("properties") or {}).items():
            if k not in schema_props:
                schema_props[k] = v
        schema_props["connectionRef"] = {"type": "object", "description": f"The {cfg['connectionKind']} to use; default \"default\" in the same namespace.",
                                         "properties": {"name": {"type": "string"}}}
        schema_props["secretRefs"] = {"type": "object", "description": "Fill body fields from Secrets: {<field>: {name, key}}. Use for tokens, passwords and secret values.",
                                      "maxProperties": 64,
                                      "additionalProperties": {"type": "object", "required": ["name", "key"], "properties": {"name": {"type": "string"}, "key": {"type": "string"}}}}
        schema_props["fieldRefs"] = FIELD_REFS
        if desc.get("secretOutputs"):
            schema_props["writeSecretRef"] = {
                "type": "object",
                "description": f"A Secret in this namespace to write what the API answers in {', '.join(desc['secretOutputs'])} to, "
                               "one key each; this object owns it. The API may answer them only when it creates the resource: "
                               "without this they are lost. They are never in the status.",
                "properties": {"name": {"type": "string"}}, "required": ["name"]}
        if "replace" in desc:
            schema_props["replaceOnChange"] = {
                "type": "boolean",
                "description": f"Apply a change by re-creating this {kind} in place ({' '.join(f'{q}={v}' for q, v in desc['replace']['query'].items())}): "
                               "the API cannot update it. Off by default: a change is then reported as drift and nothing is sent. "
                               f"A change to {', '.join(desc['replace'].get('identity') or ['its name'])} is never applied this way."}
        required = sorted({p["field"] for p in params if "." not in p["field"]} | set(desc.get("queryFields") or ()) | {x for x in (st.get("required") or []) if x not in wo})
        spec_schema = {"type": "object", "properties": schema_props}
        # A required field may instead come from another object (fieldRefs)
        # or, if the body takes it, a Secret (secretRefs): a CEL rule each,
        # where the field's name can be written in CEL.
        param_fields = {p["field"] for p in params if "." not in p["field"]} | set(desc.get("queryFields") or ())
        rules, required = required_rules(required, param_fields, schema_props)
        if rules:
            spec_schema["x-kubernetes-validations"] = rules
        if free_body:
            spec_schema["description"] = f"The API takes any JSON object as the body of {item}: every field not listed here is sent as one of its keys."
            spec_schema["x-kubernetes-preserve-unknown-fields"] = True
        if required:
            spec_schema["required"] = required
        for fpath in any_value.get(item) or ():
            set_any_value(spec_schema, fpath, item)
            any_value_used.add(item)
        also = cfg["terraform"].get("docsAlso") or {}
        fill_descriptions(spec_schema, [TF_DOCS.get(t) or {} for t in r["tf"] + [o for t in r["tf"] for o in also.get(t) or []]])
        crds.append(crd(group, kind, plural(kind), item, r, spec_schema, desc))
    unused = sorted(set(any_value) - any_value_used)
    if unused:
        sys.exit(f"overrides.anyValue: no generated kind matched {', '.join(unused)}")
    unused = sorted(set(create_query) - create_query_used)
    if unused:
        sys.exit(f"overrides.createQuery: no generated kind creates {', '.join(unused)}")
    unused = sorted(set(read_query) - read_query_used)
    if unused:
        sys.exit(f"overrides.readQuery: no generated kind reads {', '.join(unused)}")
    unused = sorted(set(delete_query) - delete_query_used)
    if unused:
        sys.exit(f"overrides.deleteQuery: no generated kind deletes {', '.join(unused)}")
    unused = sorted(set(delete_ok) - delete_ok_used)
    if unused:
        sys.exit(f"overrides.deleteOK: no generated kind deletes {', '.join(unused)}")
    unused = sorted(set(extra_fields) - extra_fields_used)
    if unused:
        sys.exit(f"overrides.extraFields: no generated kind matched {', '.join(unused)}")
    unused = sorted(set(ov.get("writeOnly") or {}) - write_only_used)
    if unused:
        sys.exit(f"overrides.writeOnly: no generated kind reads {', '.join(unused)}")
    unused = sorted(set(ov.get("readable") or {}) - readable_used)
    if unused:
        sys.exit(f"overrides.readable: no generated kind reads {', '.join(unused)}")
    unused = sorted(set(ov.get("beforeDelete") or {}) - before_delete_used)
    if unused:
        sys.exit(f"overrides.beforeDelete: no generated kind deletes {', '.join(unused)}")
    unused = sorted(set(ov.get("createOnly") or {}) - create_only_used)
    if unused:
        sys.exit(f"overrides.createOnly: no generated kind updates {', '.join(unused)}")
    for name, table, used_ in (("updateQuery", update_query, update_query_used), ("queryFields", query_fields, query_fields_used),
                               ("headers", headers_ov, headers_used), ("paramFields", param_fields_ov, param_fields_used),
                               ("outerBody", outer_body, outer_body_used), ("updateID", update_id_ov, update_id_used),
                               ("updates", ov.get("updates") or {}, updates_used), ("writable", writable_ov, writable_used),
                               ("keyedBy", ov.get("keyedBy") or {}, {(k.get("read") or {}).get("path") for k in kinds})):
        unused = sorted(set(table) - used_)
        if unused:
            sys.exit(f"overrides.{name}: no generated kind at {', '.join(unused)}")
    unused = sorted(set(update_body) - update_body_used)
    if unused:
        sys.exit(f"overrides.updateBody: no generated kind updates {', '.join(unused)}")
    unused = sorted((set(carry) | set(server_named)) - carry_used)
    if unused:
        sys.exit(f"overrides.carry/serverNamed: no generated kind at {', '.join(unused)}")
    unused = sorted(set(absent) - absent_used)
    if unused:
        sys.exit(f"overrides.absent: no generated kind reads {', '.join(unused)}")
    unused = sorted(set(ov.get("omitUnchanged") or {}) - omit_unchanged_used)
    if unused:
        sys.exit(f"overrides.omitUnchanged: no generated kind updates {', '.join(unused)}")
    unused = sorted(set(ov.get("compare") or {}) - compare_used)
    if unused:
        sys.exit(f"overrides.compare: no generated kind reads {', '.join(unused)}")
    unused = sorted(set(ov.get("keepSlashes") or {}) - keep_slashes_used)
    if unused:
        sys.exit(f"overrides.keepSlashes: no generated kind at {', '.join(unused)}")
    unused = sorted(set(ov.get("deletes") or {}) - set(ov.get("creates") or {}) - {(k.get("read") or {}).get("path") for k in kinds if k.get("delete")})
    if unused:
        sys.exit(f"overrides.deletes: no generated kind at {', '.join(unused)} (it needs a DELETE at the path, or overrides.creates)")
    unused = sorted(set(ov.get("idFrom") or {}) - id_from_used)
    if unused:
        sys.exit(f"overrides.idFrom: no generated kind has an ID at {', '.join(unused)}")
    unused = sorted(body_array - body_array_used)
    if unused:
        sys.exit(f"overrides.bodyArray: no generated kind has an array body at {', '.join(unused)}")
    unused = sorted(set(body_field) - body_field_used)
    if unused:
        sys.exit(f"overrides.bodyField: no generated kind has an array body at {', '.join(unused)}")
    unused = sorted(set(ov.get("postUpdates") or ()) - {(k.get("read") or {}).get("path") for k in kinds} - {k["update"]["path"] for k in kinds if k.get("update")})
    if unused:
        sys.exit(f"overrides.postUpdates: no generated kind at {', '.join(unused)}")
    unused = sorted(set(ov.get("patchUpserts") or ()) - {(k.get("read") or {}).get("path") for k in kinds if k["shape"] == "upsert"})
    if unused:
        sys.exit(f"overrides.patchUpserts: no generated upsert kind at {', '.join(unused)} (needs GET, PATCH and DELETE, no PUT)")
    unused = sorted(set(ov.get("redact") or {}) - redact_used)
    if unused:
        sys.exit(f"overrides.redact: no generated kind at {', '.join(unused)}")
    unused = sorted(set(ov.get("secretOutputs") or {}) - secret_outputs_used)
    if unused:
        sys.exit(f"overrides.secretOutputs: no generated kind at {', '.join(unused)}")
    unused = sorted(set(ov.get("readFrom") or {}) - read_from_used)
    if unused:
        sys.exit(f"overrides.readFrom: no generated kind at {', '.join(unused)} (the item and the list's GET must be in the spec)")
    unused = sorted(set(ov.get("createBody") or {}) - create_body_used)
    if unused:
        sys.exit(f"overrides.createBody: no generated kind at {', '.join(unused)} (with readFrom, a POST on its collection and a DELETE)")
    unused = sorted(set(states) - states_used)
    if unused:
        sys.exit(f"overrides.states: no generated kind at {', '.join(unused)}")
    unused = sorted(set(ov.get("deleteBody") or {}) - delete_body_used)
    if unused:
        sys.exit(f"overrides.deleteBody: no generated kind at {', '.join(unused)}")
    unused = sorted(set(ov.get("forget") or {}) - forget_used)
    if unused:
        sys.exit(f"overrides.forget: no generated kind at {', '.join(unused)}")
    unused = sorted(set(ov.get("envelope") or {}) - envelope_used)
    if unused:
        sys.exit(f"overrides.envelope: no generated kind at {', '.join(unused)}")
    for name, table, used_ in (("bulkCreates", bulk_creates, bulk_creates_used), ("readFirst", read_first, read_first_used),
                               ("rawReads", raw_reads, raw_reads_used)):
        unused = sorted(set(table) - used_)
        if unused:
            sys.exit(f"overrides.{name}: no generated kind at {', '.join(unused)}")
    unused = sorted(multipart_paths - multipart_used)
    if unused:
        sys.exit(f"overrides.multipart: no generated kind has a multipart body at {', '.join(unused)}")
    missing = sorted(set(replace_kinds) - {k["kind"] for k in kinds})
    if missing:
        sys.exit(f"replaceOnChange: no generated kind named {', '.join(missing)}")
    if ov.get("orders"):
        order_kinds(cfg, specs, gaps, prefix, kinds, crds, live, ov["orders"])
    # Resources whose only API is GraphQL, written by hand (graphqlgen.py).
    gq_kinds, gq_crds = graphqlgen.build_kinds(cfg, base, gaps, {
        "Spec": Spec, "camel": camel, "plural": plural, "structural": structural, "sample_value": sample_value,
        "required_rules": required_rules, "fill_descriptions": fill_descriptions, "crd": crd,
        "FIELD_REFS": FIELD_REFS, "TF_DOCS": TF_DOCS, "used": used, "live": live,
        "load": lambda p: yaml.load(open(p), Loader=L)})
    kinds += gq_kinds
    crds += gq_crds
    if cfg.get("sql"):
        sql_kinds(cfg, base, gaps, prefix, kinds, crds, live)
    # Every gap type still without a kind, and not ruled out (hardBlocks):
    # a draft kind, never reconciled (drafts.py).
    tfs = os.path.join(base, "terraform-schema.json")
    DRAFT_INFO.clear()
    DRAFT_INFO.update(drafts.build(cfg, base, specs, gaps, prefix, kinds, crds, used, sys.modules[__name__],
                                   json.load(open(tfs)) if os.path.exists(tfs) else None))
    DRAFT_INFO["gaps"] = len(gaps)
    connection = connection_crd(cfg)
    f = {"vendor": vendor, "connection": {"group": f"core.{suffix}", "version": "v1alpha1", "kind": cfg["connectionKind"]}, "kinds": kinds}
    return f, crds, connection


# ---------------------------------------------------------------- order kinds

def order_kinds(cfg, specs, gaps, prefix, kinds, crds, live, orders):
    """overrides.orders: {<Terraform type>: {read, list, id, sortBy, write:
    {method, path}, at, item, field, partial, body, outer, carry}}: a settings kind
    that is the order of other objects, read from the list at read (its
    array at list) and written whole by the reorder call at write, which
    takes the IDs at at, each as item with its id (Datadog's
    datadog_apm_retention_filter_order). The paths and the body's shape are
    checked against the spec."""
    covered = {t for k in kinds for t in k["terraform"]}
    names = {(k["group"], k["kind"]) for k in kinds}
    suffix = cfg["groupSuffix"]

    def find(path, method):
        for sp in specs:
            for p, _, ops in sp.paths():
                if p == path:
                    op = {m.lower(): v for m, v in ops.items()}.get(method)
                    if op is not None:
                        return sp, op
        return None, None

    def at(sp, sch, path):
        for seg in [s for s in path.split("/") if s]:
            sch = sp.flatten((sp.props(sp.flatten(sch)) or {}).get(seg) or {})
        return sp.flatten(sch)

    for tf, o in sorted(orders.items()):
        where = f"overrides.orders.{tf}"
        if tf not in gaps:
            sys.exit(f"{where}: not in the Terraform gap list")
        if tf in covered:
            sys.exit(f"{where}: covered by another kind already")
        covered.add(tf)
        sp, rop = find(o["read"], "get")
        if rop is None:
            sys.exit(f"{where}: no GET {o['read']} in the spec")
        w = o["write"]
        wm = (w.get("method") or "put").lower()
        wsp, wop = find(w["path"], wm)
        if wop is None:
            sys.exit(f"{where}: no {wm.upper()} {w['path']} in the spec")
        lst = at(sp, sp.response_schema(rop) or {}, o.get("list") or "")
        if lst.get("type") != "array":
            sys.exit(f"{where}: the read's answer has no array at {o.get('list') or '(the answer)'}")
        elem = sp.flatten(lst.get("items") or {})
        idf = o.get("id") or "id"
        if not _has_body_field(sp, sp.props(elem), idf):
            sys.exit(f"{where}: the read's elements have no {idf}")
        if o.get("sortBy") and not _has_body_field(sp, sp.props(elem), o["sortBy"]):
            sys.exit(f"{where}: the read's elements have no {o['sortBy']}")
        sent = at(wsp, wsp.body_schema(wop) or {}, o["at"])
        if sent.get("type") != "array":
            sys.exit(f"{where}: the {wm.upper()} body has no array at {o['at']}")
        item = o.get("item")
        if item is not None:
            iprops = wsp.props(wsp.flatten(sent.get("items") or {}))
            missing = sorted(({"id"} | set(item)) - set(iprops))
            if missing:
                sys.exit(f"{where}: the {wm.upper()} body's elements have no {', '.join(missing)}")
        words = tf[len(prefix) + 1:].split("_")
        kind = o.get("kind") or camel(words)
        group = f"{words[0]}.{suffix}"
        if (group, kind) in names:
            sys.exit(f"{where}: {kind} in {group} exists already")
        names.add((group, kind))
        field = o["field"]
        params, fields = [], {}
        for path in (o["read"], w["path"]):
            for p in re.findall(r"\{([^}]+)\}", path):
                if p not in [x["name"] for x in params]:
                    f = param_field(path, p)
                    params.append({"name": p, "field": f})
                    fields[f] = {"type": "string", "description": f"Fills {{{p}}} in {path}."}
        desc = {"kind": kind, "group": group, "version": "v1alpha1", "plural": plural(kind), "shape": "settings",
                "read": {"method": "GET", "path": o["read"]}, "update": {"method": wm.upper(), "path": w["path"]}}
        if o.get("body"):
            desc["update"]["body"] = copy.deepcopy(o["body"])
        if o.get("outer"):
            desc["update"]["outer"] = copy.deepcopy(o["outer"])
        if o.get("carry"):
            # Fields of the read's answer the call sends back (a version).
            desc["update"]["carry"] = list(o["carry"])
        if params:
            desc["params"] = params
        od = {"field": field}
        for k2 in ("list", "id", "sortBy"):
            if o.get(k2):
                od[k2] = o[k2]
        od["at"] = o["at"]
        if item is not None:
            od["item"] = dict(item)
        if o.get("partial"):
            od["partial"] = True
        desc["order"] = od
        desc["terraform"] = [tf]
        sample = {p["field"]: f"example-{p['name']}".replace("_", "-") for p in params}
        sample[field] = ["example-id-1", "example-id-2"]
        desc["sample"] = sample
        # What the read answers beside the list (Datadog's data.id, which the
        # reorder names): the fake answers it too.
        server = server_fields(sp, sp.props(sp.response_schema(rop) or {}), {}, desc)
        for ref in re.findall(r"\{current\.([^{}]+)\}", json.dumps(o.get("body") or {})):
            # A value the reorder call names from the read (data.id): the
            # fake's read answers one.
            node, parts = server, ref.split(".")
            for part in parts[:-1]:
                node[part] = dict(node.get(part) or {}) if isinstance(node.get(part), dict) else {}
                node = node[part]
            node.setdefault(parts[-1], f"example-{parts[-1]}".replace("_", "-"))
        if server:
            desc["serverFields"] = server
        desc["readiness"] = "live" if (live.get(f"{kind}.{group}") or {}).get("result") == "pass" else "fake"
        kinds.append(desc)
        what = "Listed first, in this order; the others keep their order after them." if o.get("partial") else \
            "Every one, in order: the API takes the whole order."
        props = dict(fields)
        props[field] = {"type": "array", "items": {"type": "string"},
                        "description": o.get("description") or f"The IDs of the objects listed at {o['read']}. {what}"}
        props["connectionRef"] = {"type": "object", "description": f"The {cfg['connectionKind']} to use; default \"default\" in the same namespace.",
                                  "properties": {"name": {"type": "string"}}}
        props["secretRefs"] = {"type": "object", "description": "Fill body fields from Secrets: {<field>: {name, key}}. Use for tokens, passwords and secret values.",
                               "maxProperties": 64,
                               "additionalProperties": {"type": "object", "required": ["name", "key"], "properties": {"name": {"type": "string"}, "key": {"type": "string"}}}}
        props["fieldRefs"] = FIELD_REFS
        spec_schema = {"type": "object", "properties": props}
        rules, required = required_rules(sorted({p["field"] for p in params} | {field}), {p["field"] for p in params}, props)
        if rules:
            spec_schema["x-kubernetes-validations"] = rules
        if required:
            spec_schema["required"] = required
        fill_descriptions(spec_schema, [TF_DOCS.get(tf) or {}])
        crds.append(crd(group, kind, plural(kind), w["path"], {"tf": [tf]}, spec_schema, desc))


# ---------------------------------------------------------------- sql kinds

SQL_SCALAR = {"string": "string", "dollarstring": "string", "namestring": "string", "exactstring": "string", "identifier": "string", "identstring": "string", "name": "string", "keyword": "string", "expression": "string",
              "datatype": "string", "number": "number", "integer": "integer", "boolean": "boolean"}
SQL_LISTS = {"strings": "string", "identifiers": "identifier", "names": "name", "keywords": "keyword"}
SQL_SCOPES = {"account": ["name"], "database": ["database", "name"], "schema": ["database", "schema", "name"]}
SQL_SPECIAL = {"@id", "@parent", "@like"}
SQL_ID_DESC = {
    "name": "The {kind}'s name. A plain name (letters, digits, _ and $) is upper-cased, as Snowflake does an unquoted one; "
            "any other is kept as written, and one written in double quotes keeps its case.",
    "database": "The database it is in (a name as for name).",
    "schema": "The schema it is in (a name as for name).",
}
SQL_TEMPLATE_KEYS = ("create", "delete")


def sql_placeholders(t):
    """A template's placeholders, flags ({?f}, {!f}) as their field."""
    return [p.lstrip("?!") for p in re.findall(r"\{([^{}\[\]]+)\}", t)]


SQL_ITEM_TYPES = {"object", "objects"}


def sql_item(where, name, f):
    """A structured field's item: its template's placeholders are its own fields."""
    item = f.get("item") or sys.exit(f"{where}: {name}: type {f['type']} needs item (template, fields)")
    fields = {k: dict(v) for k, v in (item.get("fields") or {}).items()}
    for ph in sql_placeholders(item.get("template") or sys.exit(f"{where}: {name}.item: template is required")):
        if ph.split(":")[0] not in fields:
            sys.exit(f"{where}: {name}.item: {{{ph}}} is not one of its fields")
    unused = sorted(set(fields) - {p.split(":")[0] for p in sql_placeholders(item["template"])})
    if unused:
        sys.exit(f"{where}: {name}.item: fields its template does not write: {', '.join(unused)}")
    out = {"template": item["template"], "fields": {}}
    for k, v in sorted(fields.items()):
        out["fields"][k] = sql_field(where, f"{name}.{k}", v)
    if item.get("sep"):
        out["sep"] = item["sep"]
    return out


def sql_field(where, name, v):
    """A field as the descriptor holds it."""
    if v.get("output"):
        return {"type": v["type"], "output": True}
    out = {"type": v["type"]}
    if v["type"] in SQL_ITEM_TYPES:
        out["item"] = sql_item(where, name, v)
    if v.get("enum") and v["type"] in ("keyword", "keywords"):
        out["enum"] = [str(e).upper() for e in v["enum"]]  # the runtime takes no other value
    return out


def sql_field_at(fields, ph):
    """The field a placeholder names: a field, or a projection {list.sub} of
    an objects (or args) field's elements."""
    if ph in fields:
        return fields[ph]
    base, _, sub = ph.partition(".")
    f = fields.get(base)
    if not sub or not f:
        return None
    if f["type"] == "args":
        return {"name": {"type": "identifier"}, "type": {"type": "datatype"}}.get(sub)
    if f["type"] == "objects":
        return ((f.get("item") or {}).get("fields") or {}).get(sub)
    return None


def sql_schema(name, f):
    """A field's CRD schema from its SQL type."""
    typ = f["type"]
    if typ in SQL_SCALAR:
        out = {"type": SQL_SCALAR[typ]}
        if f.get("enum"):
            out["enum"] = list(f["enum"])
    elif typ in SQL_LISTS:
        item = {"type": "string"}
        if f.get("enum"):
            item["enum"] = list(f["enum"])
        out = {"type": "array", "items": item}
    elif typ == "args":
        out = {"type": "array", "items": {"type": "object", "required": ["name", "type"], "properties": {
            "name": {"type": "string", "description": "The argument's name."},
            "type": {"type": "string", "description": "Its SQL data type (VARCHAR, NUMBER(38, 0))."}}}}
    elif typ == "value":
        out = {"x-kubernetes-preserve-unknown-fields": True, "description": "A string, a number or a boolean."}
    elif typ in SQL_ITEM_TYPES:
        fields = (f.get("item") or {}).get("fields") or {}
        obj = {"type": "object", "properties": {k: sql_schema(k, v) for k, v in sorted(fields.items())}}
        req = sorted(k for k, v in fields.items() if v.get("required"))
        if req:
            obj["required"] = req
        out = obj if typ == "object" else {"type": "array", "items": obj}
    else:
        sys.exit(f"sql: {name}: unknown type {typ}")
    if f.get("description"):
        out["description"] = f["description"]
    return out


def sql_sample(name, f):
    typ = f["type"]
    if typ in SQL_ITEM_TYPES:
        fields = (f.get("item") or {}).get("fields") or {}
        one = {k: sql_sample(k, v) for k, v in sorted(fields.items()) if v.get("required")}
        return one if typ == "object" else [one]
    if f.get("enum"):
        v = f["enum"][0]
        return [v] if typ in SQL_LISTS else v
    return {"string": f"example-{name}".replace("_", "-"), "dollarstring": f"example-{name}".replace("_", "-"), "namestring": f"example_{name}", "exactstring": f"example_{name}", "identifier": f"example_{name}", "identstring": f"example_{name}", "name": f"example_{name}",
            "keyword": "EXAMPLE", "expression": "TRUE", "datatype": "VARCHAR", "number": 1, "integer": 1, "boolean": True,
            "strings": [f"example-{name}".replace("_", "-")], "identifiers": [f"example_{name}"], "names": [f"example_{name}"],
            "keywords": ["EXAMPLE"], "args": [{"name": "val", "type": "VARCHAR"}], "value": "example"}[typ]


def sql_set_words(q):
    """Settings set field by field: their alters' words up to SET (ALTER ACCOUNT SET)."""
    words = q["alter"][sorted(q["alter"])[0]].split()
    return " ".join(words[:words.index("SET") + 1] if "SET" in words else words)


def sql_kinds(cfg, base, gaps, prefix, kinds, crds, live):
    """vendor.yaml sql: kinds the REST API has no resource for, made, read,
    changed and removed with SQL statements (shape sql), from hand-written
    sources: one YAML file per kind in sql.dir (default hack/gen/sql), each
    naming its Terraform types and holding its statements as templates
    (see the runtime's README, "SQL kinds")."""
    sc = cfg["sql"] or {}
    endpoint = sc.get("endpoint") or "/api/v2/statements"
    src_dir = os.path.join(base, sc.get("dir") or "sql")
    files = sorted(fn for fn in os.listdir(src_dir) if fn.endswith(".yaml"))
    if not files:
        sys.exit(f"sql: no sources in {src_dir}")
    covered = {t for k in kinds for t in k["terraform"]}
    names = {(k["group"], k["kind"]) for k in kinds}
    suffix = cfg["groupSuffix"]
    for fn in files:
        src = yaml.load(open(os.path.join(src_dir, fn)), Loader=L)
        where = f"sql/{fn}"
        kind = src.get("kind") or sys.exit(f"{where}: kind is required")
        tfs = list(src.get("terraform") or [])
        if not tfs:
            sys.exit(f"{where}: terraform is required (the types the kind stands in for)")
        for t in tfs:
            if t not in gaps:
                sys.exit(f"{where}: {t} is not in the Terraform gap list")
            if t in covered:
                sys.exit(f"{where}: {t} is covered by another kind already")
            covered.add(t)
        mode = src.get("mode") or "object"
        fields = {k: dict(v) for k, v in (src.get("fields") or {}).items()}
        ids = []
        if mode == "object":
            if src.get("id"):  # named by fields of its own (a user's token: user_name, name)
                ids = list(src["id"])
            else:
                scope = src.get("scope") or sys.exit(f"{where}: an object needs scope (account, database or schema) or id")
                ids = SQL_SCOPES.get(scope) or sys.exit(f"{where}: unknown scope {scope}")
            for f in ids:
                fields.setdefault(f, {})
                fields[f].setdefault("type", "identifier")
                fields[f]["required"] = True
                if f in SQL_ID_DESC:
                    fields[f].setdefault("description", SQL_ID_DESC[f].format(kind=kind))
        if src.get("warehouse"):  # statements that run a query need a warehouse
            fields.setdefault("warehouse", {"type": "identifier", "description":
                               "The warehouse the statements run in (CREATE runs the query). Unset: the Connection's Secret key "
                               "\"warehouse\", else the user's default warehouse."})
        secrets = sorted(f for f, v in fields.items() if v.get("secret"))
        for f in secrets:
            if not fields[f].get("output"):
                sys.exit(f"{where}: {f}: secret is for an output Snowflake answers")
        q = {"mode": mode}
        if ids:
            q["id"] = ids
        q["fields"] = {f: sql_field(where, f, v) for f, v in sorted(fields.items())}
        outputs = {f for f, v in fields.items() if v.get("output")}
        per_field = mode == "set" and not src.get("create")  # settings set field by field (the account's)
        if per_field:
            if not src.get("alter") or src.get("delete"):
                sys.exit(f"{where}: a set kind with no create sets each field by its alter (required) and unsets each by its unset (no delete)")
        else:
            for key in ("create", "delete"):
                q[key] = src.get(key) or sys.exit(f"{where}: {key} is required")
        q["read"] = []
        for rd in src.get("read") or sys.exit(f"{where}: read is required"):
            r = {"statement": rd["statement"]}
            for opt in ("match", "properties", "columns", "listColumns", "without"):
                if rd.get(opt):
                    r[opt] = dict(rd[opt])
            q["read"].append(r)
        for key in ("alter", "unset", "add", "remove"):
            if src.get(key):
                q[key] = dict(src[key])
        if src.get("unsetFirst"):  # a policy Snowflake will not replace in place: unset first
            if not per_field:
                sys.exit(f"{where}: unsetFirst is for a set kind with no create")
            for f in src["unsetFirst"]:
                if f not in (q.get("alter") or {}) or f not in (q.get("unset") or {}):
                    sys.exit(f"{where}: unsetFirst.{f} needs an alter and an unset")
            q["unsetFirst"] = list(src["unsetFirst"])
        if src.get("createColumns"):
            q["createColumns"] = dict(src["createColumns"])
            for f in q["createColumns"]:
                if f not in outputs:
                    sys.exit(f"{where}: createColumns.{f} is not an output")
        if src.get("warehouse"):
            q["warehouse"] = "warehouse"
        if src.get("settle"):  # asynchronous statements: wait for a status (Openflow)
            st = dict(src["settle"])
            if mode != "object":
                sys.exit(f"{where}: settle is for an object")
            if st.get("field") not in outputs or st["field"] not in (q["read"][0].get("columns") or {}):
                sys.exit(f"{where}: settle.field must be an output the first read maps")
            if not st.get("ready"):
                sys.exit(f"{where}: settle.ready is required")
            if q.get("createColumns"):
                sys.exit(f"{where}: settle and createColumns: an answer only the create has would be lost while it waits")
            out = {"field": st["field"]}
            for key in ("ready", "failed", "busy", "gone"):
                if st.get(key):
                    out[key] = [str(v).upper() for v in st[key]]
            if st.get("teardown"):
                out["teardown"] = []
                for step in st["teardown"]:
                    one = {}
                    if step.get("when"):
                        one["when"] = [str(v).upper() for v in step["when"]]
                    if step.get("run"):
                        one["run"] = step["run"]
                    out["teardown"].append(one)
            if st.get("timeout"):
                out["timeout"] = str(st["timeout"])
            unknown = sorted(set(st) - {"field", "ready", "failed", "busy", "gone", "teardown", "timeout"})
            if unknown:
                sys.exit(f"{where}: settle: unknown {', '.join(unknown)}")
            q["settle"] = out
        templates = [q[key] for key in ("create", "delete") if q.get(key)] + [r["statement"] for r in q["read"]] + [t for key in ("alter", "unset", "add", "remove") for t in (q.get(key) or {}).values()]
        templates += [s["run"] for s in (q.get("settle") or {}).get("teardown") or [] if s.get("run")]
        for t in templates:
            for ph in sql_placeholders(t):
                ph = ph.split(":")[0]
                if ph in outputs:
                    sys.exit(f"{where}: {{{ph}}} is an output: Snowflake makes it, no statement sends it")
                if sql_field_at(fields, ph) is None and ph not in SQL_SPECIAL:
                    sys.exit(f"{where}: {{{ph}}} in {t!r} is not a field")
        for key in ("alter", "unset", "add", "remove"):
            for f in q.get(key) or {}:
                if f not in fields:
                    sys.exit(f"{where}: {key}.{f} is not a field")
        readable = set()
        for r in q["read"]:
            readable |= {p.split(":")[0] for p in sql_placeholders(r["statement"])}
            readable |= set(r.get("columns") or {}) | set(r.get("listColumns") or {})
            readable |= {v[1:-1] for v in (r.get("match") or {}).values() if v.startswith("{")}
        readable |= set(q.get("createColumns") or {})
        used = set(ids)  # {@id}, {@parent}, {@like}
        if q.get("warehouse"):
            used.add("warehouse")
        for t in templates:
            used |= {p.split(":")[0].split(".")[0] for p in sql_placeholders(t)}
        if per_field:  # a field only a read selects by (the organization account's name)
            for r in q["read"]:
                used |= {v[1:-1] for v in (r.get("match") or {}).values() if v.startswith("{")}
        unused = sorted(set(fields) - used - outputs)
        if unused:
            sys.exit(f"{where}: fields no statement sends: {', '.join(unused)}")
        area = re.sub(r"([A-Z])", r"_\1", kind[0].lower() + kind[1:]).split("_")[0]
        group = src.get("group", area) + "." + suffix
        if (group, kind) in names:
            sys.exit(f"{where}: {kind} in {group} exists already")
        names.add((group, kind))
        desc = {"kind": kind, "group": group, "version": "v1alpha1", "plural": plural(kind), "shape": "sql",
                "read": {"method": "POST", "path": endpoint}}
        missing = sorted(outputs - readable)
        if missing:
            sys.exit(f"{where}: outputs no read maps: {', '.join(missing)}")
        wo = sorted(f for f in fields if f not in readable and f not in outputs)
        if wo:
            desc["writeOnly"] = wo
        if secrets:
            desc["secretOutputs"] = secrets
        desc["terraform"] = tfs
        sample = dict(src.get("sample") or {})
        for f, v in sorted(fields.items()):
            if v.get("required") and f not in sample:
                sample[f] = sql_sample(f, v)
        desc["sample"] = sample
        desc["readiness"] = "live" if (live.get(f"{kind}.{group}") or {}).get("result") == "pass" else "fake"
        desc["sql"] = q
        kinds.append(desc)
        props = {f: sql_schema(f, v) for f, v in sorted(fields.items()) if f not in outputs}
        props["connectionRef"] = {"type": "object", "description": f"The {cfg['connectionKind']} to use; default \"default\" in the same namespace.",
                                  "properties": {"name": {"type": "string"}}}
        props["secretRefs"] = {"type": "object", "description": "Fill fields from Secrets: {<field>: {name, key}}. Use for tokens, passwords and secret values.",
                               "maxProperties": 64,
                               "additionalProperties": {"type": "object", "required": ["name", "key"], "properties": {"name": {"type": "string"}, "key": {"type": "string"}}}}
        props["fieldRefs"] = FIELD_REFS
        if secrets:
            props["writeSecretRef"] = {
                "type": "object",
                "description": f"A Secret in this namespace to write what Snowflake answers in {', '.join(secrets)} to, "
                               "one key each; this object owns it. Snowflake answers them only when it creates the object: "
                               "without this they are lost. They are never in the status.",
                "properties": {"name": {"type": "string"}}, "required": ["name"]}
        required = sorted(f for f, v in fields.items() if v.get("required"))
        spec_schema = {"type": "object", "properties": props}
        rules, required = required_rules(required, set(ids), props)
        if rules:
            spec_schema["x-kubernetes-validations"] = rules
        if required:
            spec_schema["required"] = required
        fill_descriptions(spec_schema, [TF_DOCS.get(t) or {} for t in tfs])
        what = " ".join(w for w in (q.get("create") or sql_set_words(q)).split()[:4] if re.fullmatch(r"[A-Z][A-Z0-9_]*", w))
        r = {"tf": tfs}
        c = crd(group, kind, plural(kind), f"{endpoint} ({what})", r, spec_schema, desc)
        if src.get("description"):
            c["spec"]["versions"][0]["schema"]["openAPIV3Schema"]["description"] = src["description"].strip()
        crds.append(c)


TF_DOCS = {}     # always empty: Terraform's prose is MPL-2.0 and never copied (kept for graphqlgen's context)
DRAFT_INFO = {}  # vendor.yaml hardBlocks and draftNotes, and the gap count, for the report
DESC_STATS = {}  # spec fields described, and from where, for the report
OUR_FIELDS = {"connectionRef", "secretRefs", "fieldRefs", "replaceOnChange"}
# Not "encoded": Terraform's json_data_encoded is a string, the API's jsonData an object.
TF_SUFFIXES = {"enabled", "mode", "name", "username", "id", "ids", "uid", "type", "config", "settings"}


def attr_norm(name):
    """A field or attribute name, compared ignoring case, _ and -."""
    return re.sub(r"[^a-z0-9]", "", name.lower())


def fill_descriptions(spec_schema, docs):
    """Count the spec fields the OpenAPI spec describes, for the report.
    Nothing is filled in: Terraform's descriptions are MPL-2.0 prose, never
    copied (docs is ignored)."""
    def walk(props, path):
        for name, p in (props or {}).items():
            if not path and name in OUR_FIELDS:
                continue
            here = path + [attr_norm(name)]
            DESC_STATS["fields"] += 1
            if p.get("description"):
                DESC_STATS["openapi"] += 1
            walk(p.get("properties"), here)
            items = p.get("items") or {}
            walk(items.get("properties"), here)
    walk(spec_schema.get("properties"), [])


FIELD_REFS = {
    "type": "object",
    "description": "Fill spec fields from other objects in this namespace: {<field>: {kind, name, fieldPath}}. "
                   "fieldPath defaults to status.id, taken once that object is Ready; apiVersion is needed only where the kind's "
                   "name is in more than one group. Nothing is sent until every reference resolves.",
    "maxProperties": 64,
    "additionalProperties": {"type": "object", "required": ["kind", "name"], "properties": {
        "apiVersion": {"type": "string"}, "kind": {"type": "string"}, "name": {"type": "string"},
        "fieldPath": {"type": "string", "description": "Default status.id."}}},
}

CEL_RESERVED = {"true", "false", "null", "in", "as", "break", "const", "continue", "else", "for", "function", "if",
                "import", "let", "loop", "package", "namespace", "return", "var", "void", "while"}


def cel_escape(name):
    """A property name as CEL reaches it (Kubernetes' escaping), or None."""
    if not name or name[0].isdigit():
        return None
    if name in CEL_RESERVED:
        return f"__{name}__"
    if not re.fullmatch(r"[A-Za-z0-9_./-]+", name):
        return None
    return re.sub(r"__|[-./]", lambda m: {"__": "__underscores__", ".": "__dot__", "-": "__dash__", "/": "__slash__"}[m.group(0)], name)


def cel_typed(p):
    """Whether CEL can see a property: a field with no type (any value), or
    a list of such, is left out of the type CEL compiles against."""
    if not p or "type" not in p:
        return False
    if p["type"] == "array":
        return cel_typed(p.get("items") or {})
    if p["type"] == "object":
        if p.get("properties"):
            return True
        ap = p.get("additionalProperties")
        return isinstance(ap, dict) and cel_typed(ap)
    return True


def required_rules(required, param_fields, props):
    """CEL rules for required spec fields that a reference may fill instead,
    and the fields that stay plainly required (a name or a type CEL cannot
    reach)."""
    rules, plain = [], []
    for f in required:
        e = cel_escape(f)
        if e is None or not cel_typed(props.get(f)):
            plain.append(f)
            continue
        alts = [f"(has(self.fieldRefs) && '{f}' in self.fieldRefs)"]
        how = f"spec.fieldRefs.{f}"
        if f not in param_fields:
            # secretRefs fills body fields only.
            alts.append(f"(has(self.secretRefs) && '{f}' in self.secretRefs)")
            how += f" or spec.secretRefs.{f}"
        rules.append({"rule": f"has(self.{e}) || " + " || ".join(alts),
                      "message": f"spec.{f} is required (or set it from {how})"})
    return rules, plain


def set_any_value(spec_schema, fpath, item):
    """overrides.anyValue: the field at fpath (dotted, "[]" for a list's
    items: custom_fields[].value) takes any value, whatever the vendor's
    spec says: for a spec that describes the field wrongly (PagerDuty's
    custom field value, documented as an object, is the bare value)."""
    node = spec_schema
    parts = fpath.replace("[]", ".[]").split(".")
    for i, part in enumerate(parts):
        last = i == len(parts) - 1
        if part == "[]":
            parent, key = node, "items"
        else:
            parent, key = node.get("properties") or {}, part
        if key not in parent:
            sys.exit(f"overrides.anyValue: {item} has no field {fpath}")
        if last:
            old = parent[key]
            parent[key] = {"x-kubernetes-preserve-unknown-fields": True}
            if old.get("description"):
                parent[key]["description"] = old["description"]
        node = parent[key]


def replace_op(cfg, per_kind, kind, r, upd, mk, create_op, sp):
    """The descriptor's replace: the create call again, with the query that makes
    it replace (Snowflake's createMode=orReplace). Only for a CRUD kind with no
    update, and only where the spec says the create takes that query."""
    if r["shape"] != "crud" or upd:
        sys.exit(f"replaceOnChange: {kind} is {r['shape']}{' with an update' if upd else ''}; only CRUD kinds with no update can be re-created in place")
    query = cfg.get("query") or {}
    if not query:
        sys.exit("replaceOnChange: query is required (e.g. {createMode: orReplace})")
    params = {}
    for prm in (create_op or {}).get("parameters") or []:
        prm = sp.ref(prm)
        if prm.get("in") == "query":
            params[prm.get("name")] = sp.flatten(prm.get("schema") or {}).get("enum")
    for q, v in query.items():
        if q not in params or (params[q] is not None and v not in params[q]):
            sys.exit(f"replaceOnChange: {kind}'s create ({r['coll']}) takes no query {q}={v}")
    op = mk("POST", r["coll"], create_op)
    op["query"] = dict(query)
    identity = per_kind.get("identity", cfg.get("identity"))
    if identity:
        op["identity"] = list(identity)
    return op


def crd(group, kind, plural_, item, r, spec_schema, desc):
    tfs = ", ".join(r["tf"])
    return {
        "apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
        "metadata": {"name": f"{plural_}.{group}", "annotations": {"hfl.dev/terraform": tfs, "hfl.dev/api-path": item, "hfl.dev/readiness": desc["readiness"]}},
        "spec": {"group": group, "scope": "Namespaced",
                 "names": {"kind": kind, "listKind": kind + "List", "plural": plural_, "singular": kind.lower(), "categories": ["hfl", group.split(".")[1]]},
                 "versions": [{"name": "v1alpha1", "served": True, "storage": True, "subresources": {"status": {}},
                               "additionalPrinterColumns": [
                                   {"name": "Ready", "type": "string", "jsonPath": ".status.conditions[?(@.type==\"Ready\")].status"},
                                   {"name": "Reason", "type": "string", "jsonPath": ".status.conditions[?(@.type==\"Ready\")].reason"},
                                   {"name": "Age", "type": "date", "jsonPath": ".metadata.creationTimestamp"}],
                               "schema": {"openAPIV3Schema": {
                                   "type": "object",
                                   "description": f"{kind}: {desc['shape']} resource at {item}. Terraform equivalent: {tfs}. Fields keep the API's own names.",
                                   "properties": {
                                       "apiVersion": {"type": "string"}, "kind": {"type": "string"}, "metadata": {"type": "object"},
                                       "spec": spec_schema,
                                       "status": {"type": "object", "x-kubernetes-preserve-unknown-fields": True, "properties": {
                                           "id": {"type": "string"}, "appliedHash": {"type": "string"}, "observedGeneration": {"type": "integer"},
                                           "atProvider": {"type": "object", "x-kubernetes-preserve-unknown-fields": True},
                                           "conditions": {"type": "array", "items": {"type": "object", "x-kubernetes-preserve-unknown-fields": True}}}}},
                                   "required": ["spec"]}}}]}}


def connection_crd(cfg):
    g, k = f"core.{cfg['groupSuffix']}", cfg["connectionKind"]
    keys = ", ".join(cfg["credentialKeys"])
    return {
        "apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
        "metadata": {"name": f"{plural(k)}.{g}"},
        "spec": {"group": g, "scope": "Namespaced", "names": {"kind": k, "listKind": k + "List", "plural": plural(k), "singular": k.lower()},
                 "versions": [{"name": "v1alpha1", "served": True, "storage": True, "schema": {"openAPIV3Schema": {
                     "type": "object", "description": f"How objects in this namespace reach {cfg['title']}. Objects use the one named \"default\" unless spec.connectionRef names another.",
                     "properties": {"apiVersion": {"type": "string"}, "kind": {"type": "string"}, "metadata": {"type": "object"},
                                    "spec": {"type": "object", "required": ["secretRef"], "properties": {
                                        "baseURL": {"type": "string", "description": cfg.get("baseURLHelp", "API base URL.")},
                                        "secretRef": {"type": "object", "required": ["name"], "description": f"Secret in this namespace with keys: {keys}.",
                                                      "properties": {"name": {"type": "string"}}}}}}}}}]}}


def is_draft(c):
    return (c.get("metadata", {}).get("annotations") or {}).get("hfl.dev/readiness") == "draft"


README_BEGIN = "<!-- BEGIN generated: not covered (krmgen.py, from vendor.yaml hardBlocks and draftNotes) -->"
README_END = "<!-- END generated: not covered -->"


def md_cell(s):
    return " ".join(str(s).split()).replace("|", "\\|")


def not_covered(cfg, f):
    """The README's "Not covered" table: every gap type with no kind, a
    draft's or one vendor.yaml's hardBlocks rules out."""
    drafts_ = [k for k in f["kinds"] if k["readiness"] == "draft"]
    hard, notes = DRAFT_INFO.get("hardBlocks") or {}, DRAFT_INFO.get("draftNotes") or {}
    n = len(drafts_) + len(hard)
    if not n:
        return [f"Every one of the {DRAFT_INFO.get('gaps', 0)} Terraform gap types has a kind."]
    lines = [f"{n} of the {DRAFT_INFO.get('gaps', 0)} Terraform gap types have no kind: "
             + (f"{len(drafts_)} {'is a draft' if len(drafts_) == 1 else 'are drafts'} (generated, never reconciled; "
                "each open item in `zz_generated.report.md`)" if drafts_ else "")
             + (", and " if drafts_ and hard else "")
             + (f"{len(hard)} {'is' if len(hard) == 1 else 'are'} not built (`hardBlocks` in `hack/gen/vendor.yaml`)" if hard else "")
             + ".", "", "| Terraform | Status | Why |", "| --- | --- | --- |"]
    rows = []
    for k in drafts_:
        t = k["terraform"][0]
        why = notes.get(t) or "; ".join(k["draft"]["open"][:2])
        rows.append((t, f"draft `{k['kind']}`", why))
    for t, why in hard.items():
        rows.append((t, "not built", why))
    for t, st, why in sorted(rows):
        lines.append(f"| `{t}` | {st} | {md_cell(why)} |")
    return lines


def write_readme(out, cfg, f):
    """Replace the README's generated "Not covered" part, where it has one."""
    path = os.path.join(out, "README.md")
    if not os.path.exists(path):
        return
    text = open(path).read()
    i, j = text.find(README_BEGIN), text.find(README_END)
    if i == -1 or j == -1:
        return
    body = "\n".join(not_covered(cfg, f))
    text = text[:i + len(README_BEGIN)] + "\n" + body + "\n" + text[j:]
    open(path, "w").write(text)


def main():
    cfg_path, out = sys.argv[1], sys.argv[2]
    cfg = yaml.load(open(cfg_path), Loader=L)
    f, crds, conn = build(cfg, os.path.dirname(os.path.abspath(cfg_path)))
    crd_dir, draft_dir = os.path.join(out, "config", "crds"), os.path.join(out, "config", "crds", "drafts")
    os.makedirs(crd_dir, exist_ok=True)
    for d in (crd_dir, draft_dir):
        if os.path.isdir(d):
            for old in os.listdir(d):
                if old.endswith(".yaml"):
                    os.remove(os.path.join(d, old))
    head = "# Code generated by krm-rest-runtime/gen/krmgen.py. DO NOT EDIT.\n"
    open(os.path.join(out, "types.yaml"), "w").write(head + dump(f))
    real = [c for c in crds if not is_draft(c)] + [conn]
    draft_crds = [c for c in crds if is_draft(c)]
    name = lambda c: f"{c['spec']['group']}_{c['spec']['names']['plural']}.yaml"
    for c in real:
        open(os.path.join(crd_dir, name(c)), "w").write(head + dump(c))
    open(os.path.join(crd_dir, "kustomization.yaml"), "w").write(
        head + dump({"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization",
                     "resources": sorted(name(c) for c in real)}))
    if draft_crds:
        # Drafts' CRDs: validated with the others, installed by no one by default.
        os.makedirs(draft_dir, exist_ok=True)
        for c in draft_crds:
            open(os.path.join(draft_dir, name(c)), "w").write(head + dump(c))
        open(os.path.join(draft_dir, "kustomization.yaml"), "w").write(
            head + "# Draft kinds: never reconciled; not part of config/crds' bundle.\n"
            + dump({"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization",
                    "resources": sorted(name(c) for c in draft_crds)}))
    elif os.path.isdir(draft_dir) and not os.listdir(draft_dir):
        os.rmdir(draft_dir)
    kinds = [k for k in f["kinds"] if k["readiness"] != "draft"]
    drafts_ = [k for k in f["kinds"] if k["readiness"] == "draft"]
    hard = DRAFT_INFO.get("hardBlocks") or {}
    fo = fake_only(cfg, os.path.dirname(os.path.abspath(sys.argv[1])))
    is_fo = lambda k: f"{k['kind']}.{k['group']}" in fo or f"{k['kind']}." in fo
    nfo = sum(1 for k in kinds if is_fo(k))
    tf_cov = sorted({t for k in kinds for t in k["terraform"]})
    lines = [f"# {cfg['title']} kinds", "", "Code generated by krm-rest-runtime/gen/krmgen.py. DO NOT EDIT.", "",
             f"{len(kinds)} kinds, each standing in for Terraform resources {cfg['title']}'s own operators lack. "
             "Readiness `fake`: proven against the fake API built from these descriptors only; not yet run against the real API."
             + (f" Readiness `live` ({nlive}): passed create, update, drift repair and delete against the real API too (`hack/live/records.json`)."
                if (nlive := sum(k["readiness"] == "live" for k in kinds)) else "")
             + (f" `fake only` ({nfo}): the real feature is sold only by contract (hack/live/live.yaml fakeOnly), so the fake API is its only test." if nfo else ""), "",
             f"Terraform gap types: {len(tf_cov)} of {DRAFT_INFO.get('gaps', 0)} covered by these kinds"
             + (f"; {len(drafts_)} drafts (below)" if drafts_ else "")
             + (f"; {len(hard)} not built (vendor.yaml hardBlocks)" if hard else "") + ".", "",
             "| Kind | API group | Shape | API path | Terraform | Readiness |", "| --- | --- | --- | --- | --- | --- |"]
    st = DESC_STATS
    if st.get("fields"):
        described = st["openapi"]
        lines[-2:-2] = [f"Field descriptions: {described} of {st['fields']} spec fields ({100 * described // st['fields']}%), "
                        f"from the OpenAPI spec (never from the Terraform provider: its prose is MPL-2.0).", ""]
    for k in kinds:
        if k.get("unread"):
            api = f"`{k['update']['path']}` (never read)"
        else:
            api = f"`{k['read']['path']}`" + (f" GraphQL `{k['read']['graphql']['operation']}`" if k["read"].get("graphql") else "")
        if k.get("sql"):
            api = "`SQL: " + " ".join(w for w in (k["sql"].get("create") or sql_set_words(k["sql"])).split()[:4] if re.fullmatch(r"[A-Z][A-Z0-9_]*", w)) + "`"
        lines.append(f"| {k['kind']} | `{k['group']}` | {k['shape']} | {api} | {', '.join('`'+t+'`' for t in k['terraform'])} | {'fake only' if is_fo(k) else k['readiness']} |")
    if drafts_:
        lines += ["", f"## Drafts ({len(drafts_)})", "",
                  "A draft kind is generated for every Terraform gap type no kind covers: its spec from the Terraform provider's "
                  "schema (hack/gen/terraform-schema.json), under the API's field names where the closest API path's request body "
                  "has them, and its calls the generator's best candidates. It is never reconciled, and its CRD is in "
                  "config/crds/drafts, outside the install bundle. Each open item below is what keeps it from being a kind; "
                  "krm-rest-runtime's README (\"Draft kinds\") says how to resolve them."]
        for k in drafts_:
            calls = ", ".join(f"{k[c]['method']} `{k[c]['path']}`" for c in ("create", "read", "update", "delete") if k.get(c) and k[c].get("path"))
            lines += ["", f"### {k['kind']} (`{k['terraform'][0]}`)", "",
                      f"`{k['group']}`, {k['shape']}" + (f": {calls}" if calls else ": no calls") + "."]
            if k["draft"].get("candidates"):
                lines.append("Other candidates: " + ", ".join(f"`{c}`" for c in k["draft"]["candidates"]) + ".")
            lines.append("")
            lines += [f"- {md_cell(o)}" for o in k["draft"]["open"]]
    if hard:
        lines += ["", f"## Not built ({len(hard)})", "", "vendor.yaml hardBlocks: no draft is made for these.", "",
                  "| Terraform | Why |", "| --- | --- |"]
        lines += [f"| `{t}` | {md_cell(why)} |" for t, why in sorted(hard.items())]
    open(os.path.join(out, "zz_generated.report.md"), "w").write("\n".join(lines) + "\n")
    write_readme(out, cfg, f)
    print(f"{cfg['vendor']}: {len(kinds)} kinds, {len(tf_cov)} Terraform gap resources"
          + (f", {len(drafts_)} draft{'s' if len(drafts_) > 1 else ''}" if drafts_ else ""), file=sys.stderr)


if __name__ == "__main__":
    main()
