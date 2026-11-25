# SPDX-License-Identifier: Apache-2.0
"""Hand-written kinds: resources whose only API is GraphQL (GitLab's
compliance frameworks and saved replies, GitHub's branch protection rules),
made over one API and read or changed over the other (GitHub's enterprise
organizations, GitLab's epic boards), or REST resources whose calls no
OpenAPI path pairing finds (GitHub's branches: made on .../git/refs, read at
.../git/ref/heads/{+branch}; one field of a GitLab group, at the Group
kind's own path).

No OpenAPI spec describes them as one resource, so each is written by hand,
one file per kind in the vendor's hack/gen/<graphql.dir>/ (default
graphql/) or hack/gen/<handwritten.dir>/ (default kinds/), citing its
sources: the vendor's API reference and the pinned Terraform provider,
whose documents and calls are copied as they are. A file:

    terraform: [gitlab_group_saved_reply]   # gap types it stands in for
    kind: GroupSavedReply                   # optional: from the first type
    shape: crud                             # crud, upsert or settings
    params:                                 # spec fields that say where it is
      groupPath: {type: string, description: ...}
    fields:                                 # the body, an OpenAPI schema's properties
      name: {type: string, description: ...}
    required: [name]                        # body fields required
    writeOnly: [secret]                     # body fields no read answers
    createOnly: [...]                       # body fields only the create takes
    compare: {field: rule}                  # as overrides.compare
    id: id                                  # crud: where the create's result has the ID
    serverFields: {createdAt: "..."}        # what reads answer beside the body
    sample: {...}                           # optional: a valid spec
    create: {query: <document>, variables: {...}, result: a/b, errors: a/errors}
    read:   {query, variables, result, find: {field: id}, exists: isCatalogResource}
    update: ...
    delete: ...

Variables are templates (descriptor.GraphQL): "{field}" alone is a path
parameter's value or a body field's as it is, text around placeholders
makes a string of path parameters ("gid://gitlab/Group/{groupId}"),
"$body" the whole body. A crud kind's ID is the path parameter "id".
A mutation that selects errors answers them at <its field>/errors unless
the file says otherwise (errors: "").

An op with method and path (and no document) is a REST call (a kind may
mix the two only as crud: GitHub's createEnterpriseOrganization, then
PATCH /orgs/{id}):

    read:   {method: GET, path: "/groups/{groupPath}/epic_boards/{id}",
             find: {list: identities, field: provider, param: provider},
                                            # or find: {list: data/x, key: "{type}/{name}"}:
                                            # an object keyed by params, not a list
             query: {...}, headers: {...}}
    create: {method: POST, path: "/repos/{owner}/{repo}/git/refs",
             body: {ref: "refs/heads/{branch}"}}   # merged into the body
    delete: {method: PUT, path: "/groups/{id}", body: {x: null}}
    read:   {method: POST, path: "/api/v1/SeveritiesService.GetSeverityByID",
             body: {severityID: "{id}"}}  # an RPC read: the object named in
                                          # the body, sent as it is
    delete: {method: POST, path: "...DeleteCollector", body: {id: "{id}"},
             bareBody: true}              # not in the kind's bodyEnvelope
    create: {method: POST, path: /auth/token/create, envelope: auth}
                                          # this call answers the object there
    update: {..., carry: [version]}       # sent as a read just answered it

Path placeholders are params or the ID; {+name} keeps the value's slashes.
More kind keys for these:

    params: {group_id: {type: integer, placeholder: id, inBody: true}}
                                            # placeholder: its name in paths
                                            # where that is not the field's;
                                            # inBody: sent in the body too
    id: {from: id, match: "gid://gitlab/Boards::EpicBoard/{id}"}
                                            # the ID is part of the field
    stays: <why>                            # the delete leaves it readable
    set: {field: labels, key: name, param: name, list: ""}
                                            # (with readEach: {method, path}: the
                                            # list holds keys, this reads one;
                                            # holder: {create, read, delete,
                                            # fields}: the resource the
                                            # elements are in, made first and
                                            # deleted with them)
                                            # shape set: the field listing every
                                            # element, the element field naming
                                            # one, the element calls' placeholder
                                            # it fills, where the read's list is
    set: {field: namespaces, keys: [db, collection], list: namespaces,
          batch: {field: zones, key: location, at: zoneMap, value: zone,
                  add: {method, path}, clear: {method, path}}}
                                            # keys: in place of key and param,
                                            # the fields naming an element
                                            # together, filling the element
                                            # calls' path and query {field}s;
                                            # batch: a second list read at `at`
                                            # ({key: value}), added to in one
                                            # call ({field: [new ones]}), removed
                                            # only all together by clear
    multipart: [avatar]                     # a multipart/form-data body;
                                            # these fields are file uploads
    params: {source: {field: attributes.source}}
                                            # a body field (dotted) fills the
                                            # placeholder: no spec field
    envelope: data                          # the kind's answers' wrapper
    bodyEnvelope: data                      # its requests' wrapper
    bodyArray: true                         # every body a one-element array
    compareReturnedOnly: true               # compare only what reads answer
    updateID: id                            # the update names the item in its body
    forget: <why>                           # crud with no delete
    unread: <why>                           # settings with no read: the
                                            # update sent when the spec
                                            # changes, nothing compared
    id: {from: id, find: {path, list, query, match}}
                                            # the create answers no ID: found
                                            # in a list by what the spec sets
    create: {..., envelope: data/0, bodyArray: true}
    read:   {..., absent: {enabled: false}}
    update: {..., carry: [sha], outer: {meta: {}}}
    delete: {..., sendBody: true}           # per call, as the descriptor's
    update: {..., request: {data: {attributes: {item_key: "{item_key}",
             item_changes: {ops_set: "{value}"}}}}}
                                            # the call's whole body, built from
                                            # the spec: "{field}" is a spec field
                                            # (dotted), a param, the ID or
                                            # current.<field>; unset, left out
    state: {field: validationStatus, calls: {VERIFIED: {method: POST, path: ...}},
            delete: <value>}                # a field set by a call per value
    redact: [accessor]                      # response fields whose values are
                                            # sensitive though the kind keeps
                                            # them (its ID): redacted in
                                            # cassettes, logs and records
    queryFields: [region]                   # fields never sent in a body: they
                                            # fill query and header templates
    rotate: {prefix: name_prefix, lifetime: expire_after,
             window: early_rotation_window, deleteOnDelete: delete_on_destroy,
             name: name, expiresAt: expiresAt}     # (or seconds: true, ttl: f)
                                            # a token made again before it
                                            # expires (descriptor.Rotate)
    anyBody: true                           # the body is any JSON object: the
                                            # spec keeps unknown fields, sent
                                            # as they are (Vault's generic
                                            # endpoint)
"""
import glob, os, re, sys

OP_RE = re.compile(r"^\s*(query|mutation)\s+([A-Za-z_][A-Za-z0-9_]*)\s*(\(([^)]*)\))?\s*\{", re.S)
ROOT_RE = re.compile(r"\s*([A-Za-z_][A-Za-z0-9_]*)\s*(:\s*([A-Za-z_][A-Za-z0-9_]*))?")
TEMPLATE_RE = re.compile(r"\{([^{}]+)\}")


def template_names(t):
    if isinstance(t, str):
        return [] if t == "$body" else TEMPLATE_RE.findall(t)
    if isinstance(t, dict):
        return [n for v in t.values() for n in template_names(v)]
    if isinstance(t, list):
        return [n for v in t for n in template_names(v)]
    return []


REST_KEYS = {"method", "path", "find", "query", "headers", "body", "ok", "bareBody", "envelope", "bodyArray", "absent", "carry", "outer", "sendBody", "request", "whole"}


def rest_operation(fn, name, src, params, fields, id_param, placeholders):
    """A REST call from a file's create/read/update/delete."""
    where = f"{fn}: {name}"
    bad = sorted(set(src) - REST_KEYS)
    if bad:
        sys.exit(f"{where}: a REST call takes {', '.join(sorted(REST_KEYS))}, not {', '.join(bad)}")
    method = str(src.get("method") or "").upper()
    path = src.get("path") or ""
    want = {"create": ("POST", "PUT", "PATCH"), "read": ("GET", "POST") if src.get("body") is not None else ("GET",),
            "update": ("PUT", "PATCH", "POST"), "delete": ("DELETE", "PUT", "PATCH", "POST")}[name]
    if method not in want or not path.startswith("/"):
        sys.exit(f"{where}: a REST {name} is {' or '.join(want)} on a path (/...)")
    for n in re.findall(r"\{\+?([^}]+)\}", path):
        if n not in placeholders and n != id_param:
            sys.exit(f"{where}: {{{n}}} in {path} is neither a param nor the ID")
    out = {"method": method, "path": path}
    for key in ("query", "headers"):
        if src.get(key):
            out[key] = {k: str(v) for k, v in src[key].items()}
    if src.get("body") is not None:
        if name == "read" and method == "GET":
            sys.exit(f"{where}: a read that sends a body is a POST")
        out["body"] = src["body"]
    if src.get("bareBody"):
        if name in ("read", "create") or src.get("body") is None:
            sys.exit(f"{where}: bareBody is for an update's or a delete's body, sent outside the kind's bodyEnvelope")
        out["bareBody"] = True
    if src.get("ok"):
        if name != "delete":
            sys.exit(f"{where}: only a delete counts other statuses as done")
        out["ok"] = list(src["ok"])
    if src.get("envelope"):
        # Where this call's answer holds the object (data/0: a list's first).
        out["envelope"] = str(src["envelope"]).replace(".", "/")
    if src.get("bodyArray"):
        if name == "read":
            sys.exit(f"{where}: a read sends no body")
        out["bodyArray"] = True
    if src.get("absent"):
        if name != "read":
            sys.exit(f"{where}: only a read says there is none (absent)")
        out["absent"] = dict(src["absent"])
    if src.get("carry"):
        if name not in ("update", "delete"):
            sys.exit(f"{where}: an update or a delete carries fields")
        out["carry"] = list(src["carry"])
    if src.get("whole"):
        # The write replaces the resource: send it as read, the body in place.
        if name != "update":
            sys.exit(f"{where}: whole is for an update")
        out["whole"] = True
    if src.get("outer"):
        if name == "read":
            sys.exit(f"{where}: a read sends no body")
        out["outer"] = dict(src["outer"])
    if src.get("sendBody"):
        if name != "delete" or src.get("body") is not None:
            sys.exit(f"{where}: sendBody is for a delete with no body of its own")
        out["sendBody"] = True
    if src.get("request") is not None:
        # The call's whole body, built from the spec (Datadog's datastore
        # item: each call takes the item in another shape).
        if name == "read" or not isinstance(src["request"], dict):
            sys.exit(f"{where}: request is a create's, an update's or a delete's whole body (an object)")
        for k_ in ("body", "bareBody", "sendBody", "bodyArray", "carry"):
            if src.get(k_):
                sys.exit(f"{where}: request is the whole body: not with {k_}")
        for n in template_names(src["request"]):
            top = n.split(".")[0]
            if n.startswith("current."):
                if name == "create":
                    sys.exit(f"{where}: request: a create has no current resource ({{{n}}})")
            elif top not in fields and n not in params and n not in placeholders and n != id_param:
                sys.exit(f"{where}: request: {{{n}}} is neither a param, a field nor the ID")
        out["request"] = src["request"]
    if src.get("find"):
        if name != "read":
            sys.exit(f"{where}: only a read finds")
        f = src["find"]
        find = {k: f[k] for k in ("list", "field", "param", "key") if f.get(k)}
        if find.get("key"):
            # An object keyed by a template of params, not a list (Vault's
            # {"associated_destinations": {"gh/my-dest": {...}}}).
            if find.get("field") or find.get("param"):
                sys.exit(f"{where}: find.key is in place of field and param")
            bad = [n for n in TEMPLATE_RE.findall(find["key"]) if n not in placeholders]
            if bad:
                sys.exit(f"{where}: find.key {find['key']}: {', '.join(bad)} not a param")
        elif find.get("field") and not find.get("param"):
            find["param"] = id_param or ""
        if find.get("param") and find["param"] not in placeholders and find["param"] != id_param:
            sys.exit(f"{where}: find.param {find['param']} is neither a param nor the ID")
        out["find"] = find
    return out


def operation(fn, name, src, kind, params, fields, id_param):
    """One descriptor operation from a file's create/read/update/delete."""
    where = f"{fn}: {name}"
    doc = src.get("query") or ""
    m = OP_RE.match(doc)
    if not m:
        sys.exit(f"{where}: the document must be one named query or mutation (mutation Name(...) {{ ... }})")
    kw, op_name, var_defs = m.group(1), m.group(2), m.group(4) or ""
    if (kw == "query") != (name == "read"):
        sys.exit(f"{where}: a read is a query, a write a mutation")
    declared = set(re.findall(r"\$([A-Za-z_][A-Za-z0-9_]*)\s*:", var_defs))
    variables = src.get("variables") or {}
    if declared != set(variables):
        sys.exit(f"{where}: the document declares ${', $'.join(sorted(declared)) or '(none)'}; variables gives {', '.join(sorted(variables)) or '(none)'}")
    for n in template_names(variables):
        if n not in params and n not in fields and n != id_param:
            sys.exit(f"{where}: {{{n}}} is neither a param, a field nor the ID")
        if n in fields and name in ("read", "delete"):
            sys.exit(f"{where}: {{{n}}}: a {name} is told only params and the ID")
    root = ROOT_RE.match(doc[m.end():])
    root = root.group(1) if root else ""
    if '"' in doc:
        sys.exit(f"{where}: values go in variables, not in the document")
    # One line: GraphQL ignores white space outside strings, and the
    # document has none.
    g = {"operation": op_name, "query": " ".join(doc.split()), "variables": variables}
    result = src.get("result", "")
    if result:
        if result.split("/")[0] != root:
            sys.exit(f"{where}: result {result} is not under the operation's field {root}")
        g["result"] = result
    errors = src.get("errors")
    if errors is None and kw == "mutation" and re.search(r"\berrors\b", doc):
        errors = f"{root}/errors"
    if errors:
        g["errors"] = errors
    if src.get("exists"):
        if name != "read":
            sys.exit(f"{where}: only a read says whether the object exists")
        g["exists"] = src["exists"].replace(".", "/")
    out = {"method": "POST", "path": None, "graphql": g}
    if src.get("find"):
        if name != "read":
            sys.exit(f"{where}: only a read finds")
        f = src["find"]
        out["find"] = {"field": f["field"], "param": f.get("param", id_param)}
    return out


def claimed_types(cfg, base):
    """The Terraform types the vendor's hand-written kinds stand in for."""
    import yaml
    out = set()
    for key, default in (("graphql", "graphql"), ("handwritten", "kinds")):
        c = cfg.get(key)
        if c is None:
            continue
        for path in glob.glob(os.path.join(base, c.get("dir", default), "*.yaml")):
            with open(path) as f:
                out |= set((yaml.safe_load(f) or {}).get("terraform") or [])
    return out


def build_kinds(cfg, base, gaps, ctx):
    """The vendor's GraphQL kinds and their CRDs. ctx holds the generator's
    helpers and state: camel, plural, structural, required_rules,
    fill_descriptions, crd, Spec, FIELD_REFS, TF_DOCS, used, live."""
    gq, hw = cfg.get("graphql"), cfg.get("handwritten")
    if not gq and not hw:
        return [], []
    endpoint = (gq or {}).get("endpoint") or ""
    if gq and not endpoint.startswith("/"):
        sys.exit("graphql.endpoint: the GraphQL endpoint's path, as the vendor documents it (/api/graphql)")
    prefix, suffix = cfg["terraform"]["prefix"], cfg["groupSuffix"]
    files = []
    for key, c, default in (("graphql", gq, "graphql"), ("handwritten", hw, "kinds")):
        if c is None:
            continue
        got = sorted(glob.glob(os.path.join(base, c.get("dir", default), "*.yaml")))
        if not got:
            sys.exit(f"{key}: no kinds in {c.get('dir', default)}/")
        files += got
    kinds, crds, op_names = [], [], {}
    sp = ctx["Spec"].__new__(ctx["Spec"])
    sp.doc, sp.strip, sp.form_bodies, sp.name = {}, "", False, "graphql"
    for path in files:
        fn = os.path.relpath(path, base)
        src = ctx["load"](path)
        tf = src.get("terraform") or []
        missing = [t for t in tf if t not in gaps]
        if not tf or missing:
            sys.exit(f"{fn}: terraform must list gap types ({', '.join(missing) or 'none listed'} not in the gap list)")
        words = max(tf, key=len)[len(prefix) + 1:].split("_")
        kind = src.get("kind") or ctx["camel"](words)
        group = f"{words[0]}.{suffix}"
        if (group, kind) in ctx["used"]:
            sys.exit(f"{fn}: {kind} in {group} is already a kind")
        ctx["used"][(group, kind)] = True
        shape = src.get("shape")
        if shape not in ("crud", "upsert", "settings", "set"):
            sys.exit(f"{fn}: shape is crud, upsert, settings or set")
        params = dict(src.get("params") or {})
        fields = src.get("fields") or {}
        # params: {source: {field: attributes.source}}: a placeholder a body
        # field fills, dotted inside objects (keyedBy's, or a delete body's
        # template); the field is the body's, not a spec field of its own.
        body_params = {}
        for p, ps in list(params.items()):
            f = (ps or {}).get("field")
            if f:
                top = str(f).split(".")[0]
                if top not in fields:
                    sys.exit(f"{fn}: params.{p}.field {f} is not a field")
                body_params[p] = str(f)
        both = (set(params) - set(body_params)) & set(fields)
        if both:
            sys.exit(f"{fn}: {', '.join(sorted(both))} both a param and a field")
        id_param = "id" if shape == "crud" else None
        if id_param and id_param in set(params) | set(fields):
            sys.exit(f"{fn}: id is the ID, not a spec field")
        # A param's name in paths and variables: the field's, or its own
        # placeholder (GitLab's group file template is PUT /groups/{id}, the
        # Group kind's own path; its field is group_id).
        holder = {p: (s_ or {}).get("placeholder") or p for p, s_ in params.items()}
        desc = {"kind": kind, "group": group, "version": "v1alpha1", "plural": ctx["plural"](kind), "shape": shape}
        if src.get("unread") and (shape != "settings" or "read" in src or "update" not in src or any(n in src for n in ("create", "delete", "state"))):
            sys.exit(f"{fn}: unread (why it is never read) is for a settings kind with an update and no read")
        need = {"crud": ("create", "read", "delete") if not src.get("forget") else ("create", "read"), "upsert": ("read", "delete"),
                "settings": ("update",) if src.get("unread") else ("read", "update") if not src.get("state") else ("read",),
                "set": ("create", "read", "delete")}[shape]
        rest_ops, gql_ops = [], []
        # A set's element calls name the element: its key's placeholder, and
        # others filled from the element's fields.
        element = set()
        if shape == "set":
            sc = src.get("set") or {}
            keys = list(sc.get("keys") or [])
            if not sc.get("field") or (not keys and (not sc.get("key") or not sc.get("param"))):
                sys.exit(f"{fn}: a set kind needs set: {{field, key, param}}, or {{field, keys}}")
            if keys and (sc.get("key") or sc.get("param") or sc.get("keyURL") or sc.get("readEach")):
                sys.exit(f"{fn}: set.keys (the fields that name an element together) is in place of key and param, and not with keyURL or readEach")
            if sc["field"] not in fields:
                sys.exit(f"{fn}: set.field {sc['field']} is not a field")
            items = (fields[sc["field"]] or {}).get("items") or {}
            props = set((items.get("properties") or {}).keys())
            bad = [f for f in keys if f not in props]
            if bad:
                sys.exit(f"{fn}: set.keys: {', '.join(bad)} not a field of an element")
            element = ({sc["param"]} if sc.get("param") else set()) | props
            desc["set"] = {k_: sc[k_] for k_ in ("field", "key", "param", "list", "keyURL", "keys") if sc.get(k_)}
            allowed_ = set(holder.values())
            if sc.get("batch"):
                # A second list of the resource, added to in one call and
                # removed only all together (descriptor Set.Batch).
                bs = sc["batch"]
                need_ = ("field", "key", "at", "value", "add", "clear")
                if any(not bs.get(k_) for k_ in need_) or set(bs) - set(need_):
                    sys.exit(f"{fn}: set.batch takes {', '.join(need_)}, each set")
                if bs["field"] not in fields or bs["field"] == sc["field"]:
                    sys.exit(f"{fn}: set.batch.field {bs['field']} is not a field of its own")
                bprops = ((fields[bs["field"]] or {}).get("items") or {}).get("properties") or {}
                if bs["key"] not in bprops or bs["value"] not in bprops:
                    sys.exit(f"{fn}: set.batch: key and value are fields of an element of {bs['field']}")
                desc["set"]["batch"] = {"field": bs["field"], "key": bs["key"], "at": str(bs["at"]).replace(".", "/"), "value": bs["value"],
                                        "add": rest_operation(fn, "create", bs["add"], params, fields, None, allowed_),
                                        "clear": rest_operation(fn, "delete", bs["clear"], params, fields, None, allowed_)}
            if sc.get("readEach"):
                # The list holds keys only: the call that reads one element.
                desc["set"]["readEach"] = rest_operation(fn, "read", sc["readEach"], params, fields, None, allowed_ | {sc["param"]})
            if sc.get("holder"):
                # The resource the elements are in, made first and deleted with them.
                hs = sc["holder"]
                bad = [f for f in hs.get("fields") or [] if f not in fields or f == sc["field"]]
                if bad:
                    sys.exit(f"{fn}: set.holder.fields: {', '.join(bad)} not a field of the holder")
                desc["set"]["holder"] = {n: rest_operation(fn, m, hs[n], params, fields, None, allowed_)
                                         for n, m in (("create", "create"), ("read", "read"), ("delete", "delete"))}
                if hs.get("fields"):
                    desc["set"]["holder"]["fields"] = list(hs["fields"])
        for name in ("create", "read", "update", "delete"):
            if name not in src:
                if name in need:
                    sys.exit(f"{fn}: a {shape} kind needs {name}")
                continue
            if "method" in src[name]:  # a REST call (a GraphQL op has a query document, no method)
                allowed = set(holder.values()) | (element if name != "read" else set())
                desc[name] = rest_operation(fn, name, src[name], params, fields, id_param, allowed)
                rest_ops.append(name)
                continue
            if not endpoint:
                sys.exit(f"{fn}: {name} is a GraphQL document, and vendor.yaml names no graphql.endpoint")
            op = operation(fn, name, src[name], kind, set(holder.values()), fields, id_param)
            op["path"] = endpoint
            prev = op_names.get(op["graphql"]["operation"])
            if prev:
                sys.exit(f"{fn}: operation {op['graphql']['operation']} is {prev}'s too: names are unique in a vendor")
            op_names[op["graphql"]["operation"]] = fn
            desc[name] = op
            gql_ops.append(name)
        if shape == "upsert" and "update" not in desc and "create" not in desc:
            sys.exit(f"{fn}: an upsert kind needs update or create")
        if rest_ops and gql_ops and shape != "crud":
            sys.exit(f"{fn}: a kind of GraphQL and REST calls both is crud")
        if shape == "set" and gql_ops:
            sys.exit(f"{fn}: a set kind's calls are REST")
        if params:
            desc["params"] = []
            for p, ps in params.items():
                e = {"name": holder[p], "field": p}
                if (ps or {}).get("inBody"):
                    e["inBody"] = True
                if p in body_params:
                    # A body field, dotted inside objects (attributes.source),
                    # fills the placeholder: no spec field of its own.
                    e["field"], e["inBody"] = body_params[p], True
                desc["params"].append(e)
        if id_param:
            idsrc = src.get("id", "id")
            if isinstance(idsrc, dict):
                desc["id"] = {"param": id_param, "from": str(idsrc.get("from", "id")).replace(".", "/")}
                if idsrc.get("match"):
                    if "{" + id_param + "}" not in idsrc["match"]:
                        sys.exit(f"{fn}: id.match has no {{{id_param}}}")
                    desc["id"]["match"] = idsrc["match"]
                if idsrc.get("find"):
                    # The create answers no ID: the list the new resource is
                    # found in, by the fields the spec sets (descriptor ID.Find).
                    f = idsrc["find"]
                    if not str(f.get("path") or "").startswith("/") or not f.get("match"):
                        sys.exit(f"{fn}: id.find needs path and match")
                    for n in re.findall(r"\{\+?([^}]+)\}", f["path"]):
                        if n not in holder.values():
                            sys.exit(f"{fn}: id.find: {{{n}}} in {f['path']} is not a param")
                    fd = {"method": "GET", "path": f["path"]}
                    if f.get("query"):
                        fd["query"] = {k: str(v) for k, v in f["query"].items()}
                    if f.get("list"):
                        fd["find"] = {"list": str(f["list"]).replace(".", "/")}
                    fd["match"] = {k: str(v) for k, v in f["match"].items()}
                    desc["id"]["find"] = fd
            else:
                desc["id"] = {"param": id_param, "from": str(idsrc).replace(".", "/")}
        for key in ("envelope", "bodyEnvelope"):
            if src.get(key):
                if gql_ops:
                    sys.exit(f"{fn}: {key} is for a kind of REST calls")
                desc[key] = str(src[key]).replace(".", "/")
        if src.get("bodyArray"):
            desc["bodyArray"] = True
        if src.get("compareReturnedOnly"):
            desc["compareReturnedOnly"] = True
        if src.get("updateID"):
            desc["updateID"] = src["updateID"]
        if src.get("forget"):
            if shape != "crud" or "delete" in desc:
                sys.exit(f"{fn}: forget (why it is never deleted) is for a crud kind with no delete")
            desc["forget"] = src["forget"]
        if src.get("unread"):
            # Never read, as Terraform's provider never reads it: written
            # when the spec changes (descriptor Kind.Unread).
            desc["unread"] = src["unread"]
        if src.get("stays"):
            if "delete" not in desc or desc["delete"].get("method") == "DELETE":
                sys.exit(f"{fn}: stays is for a delete that is not a DELETE")
            desc["stays"] = src["stays"]
        if src.get("state"):
            # A field set by a call per value (Okta's domain verification:
            # POST .../verify for VERIFIED), as overrides.states.
            st = src["state"]
            calls = {str(v): rest_operation(fn, "delete", c, params, fields, id_param, set(holder.values()))  # any write method
                     for v, c in (st.get("calls") or {}).items()}
            if not st.get("field") or not calls:
                sys.exit(f"{fn}: state needs field and calls")
            desc["state"] = {"field": st["field"], "calls": calls}
            if st.get("delete"):
                if str(st["delete"]) not in calls:
                    sys.exit(f"{fn}: state.delete {st['delete']} has no call")
                desc["state"]["delete"] = str(st["delete"])
            if st.get("before"):
                # Calls made, in order, before a value's call (Grafana Asserts'
                # dataset auto-setup before its enable).
                before = {}
                for v, ops in st["before"].items():
                    if str(v) not in calls:
                        sys.exit(f"{fn}: state.before.{v}: no call sets {v}")
                    before[str(v)] = [rest_operation(fn, "delete", c, params, fields, id_param, set(holder.values())) for c in ops]
                desc["state"]["before"] = before
            if st["field"] not in fields:
                fields[st["field"]] = {"type": "string", "enum": sorted(calls),
                                       "description": st.get("description") or "The state to put it in, by its call ("
                                       + ", ".join("%s: %s %s" % (v, c["method"], c["path"]) for v, c in sorted(calls.items())) + ")"
                                       + (f"; deleting the object sets {st['delete']}." if st.get("delete") else ". Unset: left as it is.")}
        if src.get("multipart"):
            bad = [f for f in src["multipart"] if f not in fields]
            if bad or gql_ops:
                sys.exit(f"{fn}: multipart: file fields of a REST kind ({', '.join(bad) or 'not with GraphQL'})")
            desc["multipart"] = {"files": list(src["multipart"])}
        for key in ("writeOnly", "createOnly"):
            vals = src.get(key) or []
            bad = [f for f in vals if f.split(".")[0] not in fields]
            if bad:
                sys.exit(f"{fn}: {key}: no field {', '.join(bad)}")
            if vals:
                desc[key] = sorted(vals)
        if src.get("createOnly") and (shape != "crud" or "update" not in desc):
            sys.exit(f"{fn}: createOnly is for crud kinds with an update")
        if src.get("compare"):
            desc["compare"] = dict(src["compare"])
        desc["terraform"] = list(tf)
        required = list(src.get("required") or [])
        bad = [f for f in required if f not in fields]
        if bad:
            sys.exit(f"{fn}: required: no field {', '.join(bad)}")
        sample = src.get("sample")
        if sample is None:
            sample = {p: f"example-{p}" for p in params if p not in body_params}
            for r in required:
                sample[r] = ctx["sample_value"](sp, r, fields[r])
        desc["sample"] = sample
        if src.get("serverFields"):
            desc["serverFields"] = dict(src["serverFields"])
        if src.get("secretOutputs"):
            desc["secretOutputs"] = sorted(src["secretOutputs"])
        if src.get("redact"):
            # Fields whose values are sensitive though the kind keeps them
            # (a Vault token's accessor, its ID): never in a cassette.
            desc["redact"] = sorted(src["redact"])
        qf = list(src.get("queryFields") or [])
        if src.get("rotate"):
            # A token made again before it expires (descriptor.Rotate): the
            # spec fields it names are never sent nor compared.
            rt = dict(src["rotate"])
            known = {"prefix", "lifetime", "window", "seconds", "deleteOnDelete", "name", "expiresAt", "ttl"}
            bad = sorted(set(rt) - known)
            if bad:
                sys.exit(f"{fn}: rotate takes {', '.join(sorted(known))}, not {', '.join(bad)}")
            if shape != "crud" or "delete" not in desc:
                sys.exit(f"{fn}: rotate is for a crud kind with a delete")
            for key in ("prefix", "lifetime", "window", "deleteOnDelete"):
                f = rt.get(key)
                if key != "deleteOnDelete" and not f:
                    sys.exit(f"{fn}: rotate.{key}: the spec field")
                if f and f not in fields:
                    sys.exit(f"{fn}: rotate.{key}: {f} is not a field")
                if f and f not in qf:
                    qf.append(f)
            if not rt.get("name") or not (rt.get("expiresAt") or rt.get("ttl")):
                sys.exit(f"{fn}: rotate needs name, and expiresAt or ttl: the body fields the create sets")
            desc["rotate"] = rt
        if qf:
            bad = [f for f in qf if f not in fields]
            if bad:
                sys.exit(f"{fn}: queryFields: no field {', '.join(bad)}")
            desc["queryFields"] = qf
        desc["readiness"] = "live" if (ctx["live"].get(f"{kind}.{group}") or {}).get("result") == "pass" else "fake"
        kinds.append(desc)

        schema_props = {}
        for p, s in params.items():
            if p in body_params:
                continue
            ps = {"type": "string"}
            ps.update({k: v for k, v in (s or {}).items() if k in ("type", "description", "enum", "format", "pattern")})
            schema_props[p] = ps
        st = ctx["structural"](sp, {"type": "object", "properties": fields, "required": required})
        schema_props.update(st.get("properties") or {})
        conn = cfg["connectionKind"]
        schema_props["connectionRef"] = {"type": "object", "description": f"The {conn} to use; default \"default\" in the same namespace.",
                                         "properties": {"name": {"type": "string"}}}
        schema_props["secretRefs"] = {"type": "object", "description": "Fill body fields from Secrets: {<field>: {name, key}}. Use for tokens, passwords and secret values.",
                                      "maxProperties": 64,
                                      "additionalProperties": {"type": "object", "required": ["name", "key"], "properties": {"name": {"type": "string"}, "key": {"type": "string"}}}}
        schema_props["fieldRefs"] = ctx["FIELD_REFS"]
        if desc.get("secretOutputs"):
            schema_props["writeSecretRef"] = {
                "type": "object",
                "description": f"A Secret in this namespace to write what the API answers in {', '.join(desc['secretOutputs'])} to, "
                               "one key each; this object owns it. The API may answer them only when it creates the resource: "
                               "without this they are lost. They are never in the status.",
                "properties": {"name": {"type": "string"}}, "required": ["name"]}
        spec_schema = {"type": "object", "properties": schema_props}
        if src.get("anyBody"):
            if gql_ops:
                sys.exit(f"{fn}: anyBody is for a REST kind")
            spec_schema["x-kubernetes-preserve-unknown-fields"] = True
        spec_params = set(params) - set(body_params)
        rules, req = ctx["required_rules"](sorted(spec_params | set(st.get("required") or [])), spec_params, schema_props)
        if rules:
            spec_schema["x-kubernetes-validations"] = rules
        if req:
            spec_schema["required"] = req
        ctx["fill_descriptions"](spec_schema, [ctx["TF_DOCS"].get(t) or {} for t in tf])
        ops = [desc[n]["graphql"]["operation"] for n in gql_ops]
        calls = [f"{desc[n]['method']} {desc[n]['path']}" for n in rest_ops]
        item = f"{endpoint} (GraphQL: {', '.join(ops)})" if ops else ""
        if calls:
            item = "; ".join(([item] if item else []) + calls)
        crds.append(ctx["crd"](group, kind, desc["plural"], item, {"tf": tf}, spec_schema, desc))
    return kinds, crds
