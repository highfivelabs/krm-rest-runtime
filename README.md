# krm-rest-runtime

The shared engine behind High Five Labs' REST extension repos
(github-, gitlab-, cloudflare-, datadog-, grafana-, pagerduty-, okta-,
mongodbatlas-, confluent-, vault- and snowflake-krm-extensions): Kubernetes
kinds for vendor configuration that Terraform providers manage and the
vendors' own operators do not.

A vendor repository holds no controller code of its own. It holds:

- `types.yaml`: one descriptor per kind, saying which REST calls create,
  read, update and delete it, which spec fields fill the URL's path
  parameters, and which Terraform resources it stands in for
- `config/crds/`: one CRD per kind
- a `rest.Vendor`: base URL, credential keys, how to authenticate

and this module does the rest.

| Package | What |
| --- | --- |
| `gen/krmgen.py` | The generator: a vendor's OpenAPI spec + Terraform's resource list → `types.yaml`, CRDs, report. Keeps only API resources that fill a Terraform gap. |
| `gen/graphqlgen.py` | The generator's hand-written kinds: one file each (`hack/gen/graphql/<name>.yaml`, `hack/gen/kinds/<name>.yaml`: the GraphQL documents or REST calls, how the spec fills them, the fields), for resources a vendor manages only through GraphQL, through GraphQL and REST both, or through REST calls no OpenAPI item pairs. See "GraphQL kinds" and "Hand-written kinds" below |
| `gen/examples.py` | A vendor's `examples/<Kind>/with-<Parent>.yaml`: each kind its live test gives another resource's ID, shown with that resource as an object of its own kind, joined by `spec.fieldRefs` (from `hack/live/live.yaml`); and, from the repository's Upjet facts (`hack/upjet/*.json`, delivered by the factory), each reference from a kind's string field to a kind served here, unless `hack/gen/examples-skip.yaml` says why it does not hold |
| `gen/drafts.py` | The generator's draft kinds: a skeleton for every Terraform gap type no kind covers, never reconciled (see "Draft kinds" below) |
| `gen/tfschema.py` | A vendor's `hack/gen/terraform-schema.json`: the Terraform provider's own schema (`terraform providers schema -json`, at the version `terraform-gaps.json` pins) of the gap types no kind covers, the drafts' fields: facts only, no descriptions (the provider is MPL-2.0). Run it when that version moves or a gap loses its kind, and commit the result |
| `descriptor` | The `types.yaml` format and its checks |
| `reconciler` | One generic controller, on unstructured objects: create, read back, repair drift every 10 minutes, delete with a finalizer |
| `rest` | The HTTP client: JSON, vendor auth, 404 as `ErrNotFound` |
| `fake` | An in-memory API built from the same descriptors, for tests |
| `kindtest` | `EveryKind` (every kind through create, update, drift repair and delete on the fake), `Live` (the same against the real API, docs/live.md), `Replay` (the same against a live run's recorded answers: no network, no credentials, docs/live.md "Replay"), `Rehearse` (the same for every kind against the fake, recorded to `hack/fake/cassettes` with `KRM_FAKE_RECORD=1` and replayed in every `go test`: a request and response example of every kind, docs/live.md "Rehearsal"), `ValidCRDs` (the API server's own CRD validation) `ValidExamples` (every kind's sample and live spec against its CRD schema, as the API server checks an object), `ValidExampleFiles` (every example file against the CRDs, then made and deleted on the fake, a parent first) and `RunManifest` (any manifest through the controllers on the fake, as the MCP server's `plan_on_fake`) |
| `cassette` | Records a live run's calls and plays them back, strictly in order, for `Replay` |
| `crdcheck` | An object checked against a CRD's schema as the API server does on create; used by `ValidExamples` and the MCP server's `validate_manifest` |
| `manager` | `Main`: the vendor's manager, `--groups` to run only some API groups |
| `mcpserver` | `Main`: the vendor's kinds served to agents over MCP (stdio). See "For agents" below. |

## For agents (MCP)

`mcpserver` serves a vendor's kinds over the Model Context Protocol, from the
same `types.yaml` and CRDs the controllers run from, plus the live tests'
`hack/live/live.yaml` and `records.json`. A vendor repository runs it from
`cmd/mcp`:

```go
mcpserver.Main(mcpserver.Options{Vendor: extensions.Vendor, Types: extensions.Types, Hooks: extensions.Hooks})
```

It reads `config/crds` and `hack/live` from `--dir` (default: the working
directory), so `go run ./cmd/mcp` from the repository root is all a
`.mcp.json` needs. Nothing it does reaches the vendor's real API.

| Tool | Answers |
| --- | --- |
| `list_kinds` | every kind: apiVersion, shape, Terraform types, live or fake, one line of description; filter by text or group |
| `find_kinds_for_terraform` | Terraform resource types → the kinds that stand in for them |
| `describe_kind` | description, every spec and status field (type, required, enum, default, the URL parameter it fills, write-only, create-only), the REST calls, the live record |
| `example` | a manifest with its Secret and Connection: the live-tested spec where there is one (`tested: live` and the date), else the sample the fake test runs (`fake`), else one from the required fields (`none`) |
| `validate_manifest` | each document against its CRD schema as the API server checks it (CEL rules aside), the fields that fill URL parameters, and fields the API server would drop |
| `plan_on_fake` | the real controller run on the manifest against the fake API: every REST call with its body, Ready or not, the status; with `delete: true` the delete calls too |

## Field descriptions

A field takes its description from the vendor's OpenAPI spec, or has none.
Never from a Terraform provider: providers are MPL-2.0, and their prose
(the registry docs, the schema's descriptions) is not copied into these
repositories, nor paraphrased. What the generator takes from Terraform is
facts: type names, attribute names, types, required, computed, sensitive.
`krmgen.py` refuses to run while a vendor repository still has a
`hack/gen/terraform-docs.json`. Each vendor's report says how many fields
have a description.

## Shapes

| Shape | API looks like | Example |
| --- | --- | --- |
| `crud` | POST a collection, server assigns an ID; GET/PATCH/DELETE the item | a GitLab project |
| `upsert` | the caller names the item; PUT creates or replaces it (or a `create` makes it and nothing changes it: a POST to make it, a DELETE to remove it) | a GitHub Actions secret; a GitLab runner controller's scope |
| `settings` | a singleton that always exists; GET and PATCH/PUT, never deleted | GitHub's Actions permissions |
| `assoc` | a link with no identity: POST makes it, a list shows it, the same body sent to an undo path removes it | a Snowflake grant (`grants:revoke`) |
| `sql` | no REST resource: SQL statements sent to the vendor's SQL API make, read, change and remove it (see "SQL kinds") | a Snowflake masking policy |
| `set` | the whole set of a parent's elements, each an API object of its own: the spec lists them all, and the reconciler adds, changes and removes elements (one call each) until the API's list holds exactly those; deleting the object removes them (see "Set kinds") | a GitHub repository's labels |

Beyond the shape, a descriptor can say:

- `read.find`: the API lists but cannot get one (Snowflake roles); read the
  list and pick the element whose `field` (dotted for a nested one:
  `user.id`) equals path parameter `param`. The array is at `find.list`, or
  else in the kind's envelope (Cloudflare's `{"result": [...]}`), or else is
  the answer; the element found is the object. A find with neither `field`
  nor `param` finds the list's first element: a list of at most one, there
  or not (GitLab's runner controller's `instance_level_scopings`). An
  upsert updated on the very path it is read from, in a list there, is
  written there (a team's one EMU group mapping: PATCH and GET
  `.../external-groups`, `{"groups": [...]}`; a GitLab user's identity,
  `PUT /users/{id}` and its `identities`), and may be deleted on a path of
  its own (`DELETE /users/{id}/identities/{provider}`).
- `read.envelope` with `read.find`: where in the element found the object
  is (Datadog's managed organizations list each as `{"id", "type",
  "attributes": {...}}`: `envelope: attributes`); the fake answers its
  objects so.
- `read.find.list` with a `*` part: the list is kept further down, in each
  element of another list, and their lists are joined (Datadog's
  EventBridge sources, every account's: `data/attributes/accounts/*/event_hubs`).
- A query or header template naming a path parameter or the ID (`{id}`):
  filled when the call is sent (Datadog reads a team connection from its
  collection, `?filter[connection_ids]={id}`). A template's `{a|b}` is the
  first of those fields the spec sets.
- `id.find`: the create answers no ID (Cloudflare's list items are added by
  an asynchronous bulk POST that answers an operation): the new resource is
  found in a list (`id.find`: a GET, its array at `find.list`, `query`) by
  `match`, where a `{field}` the spec leaves out must be missing from the
  element too. It is looked for before a create as well, so that one made
  by a create whose answer named nothing is not made again.
- A kind with no item path (Datadog's team connections, EventBridge
  sources): made on its collection, read from its list (`read.find`), and
  deleted by a DELETE of the collection whose `delete.body` names it
  (`{"data": [{"id": "{id}", ...}]}`, with `delete.bodyArray`). The fake keeps
  each under the collection by its ID.
- `{+param}` in a path: a value that keeps its slashes, each part between
  them escaped (RFC 6570's reserved expansion: GitHub's branch `feature/x`
  at `.../git/refs/heads/{+branch}`, as go-github escapes a ref); a part
  that is empty, `.` or `..` is refused. `{param}` escapes a slash
  (GitLab's file path, `docs%2FREADME.md`).
- `id.match`: the ID is part of the create's answer, the part a template's
  `{<id param>}` stands for (GitLab's epic board is read over REST by the
  number in the global ID its GraphQL create answers:
  `gid://gitlab/Boards::EpicBoard/{id}`).
- `read.find.key`, in place of `field` and `param`: the answer there is
  not a list but an object keyed by a template of path parameters, whose
  value is the object (Vault's destinations of a synced secret,
  `{"associated_destinations": {"gh/my-dest": {...}}}`: `key:
  "{type}/{name}"`). The fake answers such an object, and keeps the item
  where it is deleted.
- `read.body`: a CRUD kind's read that names the object in the body it
  sends rather than in its path, a POST of the body's template filled from
  path parameters and the ID, sent as it is (an RPC API's
  `GetCollector {"id": "{id}"}`; Vault's `.../secret-id-accessor/lookup
  {"secret_id_accessor": "{id}"}`), or, with `find`, a POST listing them
  all (Grafana IRM's `GetRoles {}`). It may be an upsert's, naming it by a
  param (Fleet Management's collector `{"id": "{id}"}`). Its update and
  delete name it the same way (`body`); `bareBody` on an update or a delete sends that call's body
  outside the kind's `bodyEnvelope` (Fleet Management's create takes
  `{"collector": {...}}`, its delete `{"id": ...}`). The fake keeps such
  objects by kind, path parameters and ID, and finds them by the ID the
  call's body names.
- A path that is one `{+param}` and nothing else (`/{+path}`: Vault's
  generic endpoint) matches any path; the fake answers it only where no
  other kind's route does.
- `read.find.field: ""`: the list names its items only (Vault's LIST
  answers `{"data": {"keys": ["a", "b"]}}`); the element that equals the
  path parameter is the object, which is then empty.
- A read on another path than the writes (`read.path` differs from the
  update's, or names fewer parameters): one object, not a list (Grafana's
  public dashboard, read at `.../public-dashboards` and updated and deleted
  at `.../public-dashboards/{uid}`).
- `read.absent`: the read answers 200, not 404, when there is none, with
  these fields so (Atlas's push-based log export: `state: UNCONFIGURED`; a
  null value: the field is missing or empty, as a search deployment's
  `id`); such an answer counts as not found. The fake answers it too. An
  upsert made and deleted on other paths than its read (Cloudflare's email
  routing DNS: on by `POST`, off by `DELETE .../email/routing/dns`, read as
  `enabled` at `.../email/routing`, where `EmailRoutingSettings` is read and
  written too) is a mark in what that read answers: the fake sets its
  server fields there on create and its absent values on delete, in the
  other kind's object where there is one, which stays.
- `whole` on an update: the write replaces the resource, and the kind is
  one part of it (Okta's SAML app settings: `settings.app` of an app, PUT
  `/api/v1/apps/{appId}` taking the whole app). The update sends the
  resource as a read just before answers it, outside the kind's envelope
  too, with each of the body's fields in place of the read's at the
  `bodyEnvelope` (replaced, not merged: a key the spec drops goes), as
  Terraform's provider does.
- `carry` on an update or a delete: fields (dotted inside objects) the call
  sends as a read just before answers them, else as the spec sets them: a
  write the API takes only with what it last answered (a Kubernetes-style
  API's `metadata.resourceVersion`, a GitHub file's blob `sha`), or with
  the name the server chose. A delete that carries reads first, and sends
  them as its body. A carried field goes even when it is create-only (a
  Flink statement's PUT requires its immutable name). The fake refuses a
  write without the current value (409) and moves the value on after each
  update.
- `adopt`, on a CRUD kind with no create: the resource exists before the
  object (Okta's default policies). Creating the object lists `adopt.path`
  and takes the element whose fields `adopt.match` gives (a value, or a
  `{field}` from the spec), then updates it as usual; the kind has
  `forget`, so deleting the object leaves it. The fake holds one already,
  made from the kind's sample.
- `element`: the kind is one element of a list another resource holds,
  which the API reads and writes only as part of that resource. The kind
  reads the resource (`read`) and writes it (`update`; its `delete` is the
  same write). `element.in: {<spec field>: <list>}` for a list of bare
  values: the element is that field's value (Vault's OIDC key's
  `allowed_client_ids`; with several, the spec sets exactly one: a Grafana
  role's `users`, `teams` and `service_accounts`). `element.key` (and
  `list`, `""` when the read answers the list itself) for a list of
  objects, named by that field; the element is the body. A write sends
  every list `in` names, as just read, with the element added or taken out
  (read-modify-write, as Terraform's providers do); with `element.patch:
  <field>` it sends only this element, in a one-element list at that field,
  and takes it out by sending it with `unset`'s fields (GitHub's custom
  property values: `PATCH {"properties": [...]}`, `{"value": null}`). A
  spec that names another element than the status takes the old one out in
  the same write. With `element.whole`, the write is the whole resource as
  just read, its lists changed, for an API whose write replaces the
  resource (Okta's `PUT /api/v1/apps/{appId}`, an OIDC app's redirect
  URIs). The fake holds, empty, the resource each Element kind's sample
  goes in, and in a rehearsal the one a `live.yaml` spec names (GitHub's
  custom property values of the run's repository).
- `state`: a field the API sets by a call per value rather than in a body
  (`field`, and `calls: {<value>: {method, path}}`): Okta's org factor
  (`POST .../lifecycle/activate` or `.../deactivate`), a feature, a
  subscription. The spec field of that name asks for a value; when the read
  answers another (ignoring case), its call is made after the rest of the
  spec is applied. It is never sent in a body. `state.delete`: the value
  deleting the object puts the resource in, in place of a delete (a factor
  is deactivated); the resource stays. A settings kind may have no
  `update`, only its state, and the state may have one value (Okta's
  domain verification: `VERIFIED`, by `POST .../verify`, made again on each
  sync until the read answers it); a call may be a DELETE (an app token's
  `REVOKED`, the state deleting the object leaves it in). A settings kind
  read on another kind's item path (Okta's domain verification and
  certificate, on the Domain's) acts on that kind's resource: the fake holds
  one for its sample, and answers no other path for it. `state.before`:
  by value, calls made in order before that value's call (Grafana Asserts
  auto-configures its stack's datasets, `POST /v2/stack/datasets/auto-setup`,
  before `POST /v2/stack/enable`); the fake answers them. A state call with
  `sendBody` sends the spec's other fields, `{}` when it has none (Grafana's
  keeper activation, `POST .../keepers/{name}/activate`); a state may be
  read inside the kind's envelope (the keeper's `status.active`).
- A delete path's `{current.field}` placeholder: the field as the resource
  was last read (`status.atProvider`, else a read just before): Grafana k6's
  schedule, read and written at `.../load_tests/{id}/schedule`, deleted at
  `/schedules/{current.id}`; Confluent's mirror topic, made and read on its
  cluster link, deleted as the topic (`.../topics/{current.mirror_topic_name}`).
  The fake finds the object those fields name.
- `create.body` templates: `{param}` is a path parameter, filled when the
  create is sent: an upsert named by the caller and made on its collection
  by naming it in a list (Okta's trusted server,
  `POST .../associatedServers {"trusted": ["{associatedServerId}"]}`).
  An upsert with a create may have no update: a change is drift.
- `query` on any operation: sent with that call only (a read that answers
  in full only when asked: Vault's `multivalue=true`).
- `headers` on any operation: sent with that call only (MongoDB Atlas pins
  each endpoint's API version in `Accept` and `Content-Type`).
- A `query` or `headers` value may be a template: `{field}` (dotted for a
  nested one) is the spec's field (Confluent's
  `?environment={environment.id}`); `{current.field}` is the resource's field
  as the API last answered it: the read before an update, and
  `status.atProvider` before a delete (Grafana Assistant authorizes a write
  by the scope the object has now, `X-Resource-Scope: {current.scope}`,
  while the update's body names the scope it should have). On an assoc's
  delete, `{field}` is the undone link's field (Kafka's
  `DELETE .../acls?principal=...`). A header whose template is empty is not
  sent.
- `queryFields`: spec fields that are not sent in the body; they only fill
  templates (Grafana Cloud's access policies take `?region=` on every call).
- `params[].inBody`: the path parameter's field is a body field too, sent in
  both (Confluent's tag binding is deleted at
  `.../type/{typeName}/name/{qualifiedName}/tags/{tagName}`: the body's
  `entityType`, `entityName` and `typeName`).
- `outer` on any operation: fields sent at the top of the request body,
  beside `bodyEnvelope` rather than in it (Datadog's sensitive data scanner:
  `{"data": {...}, "meta": {}}` on every write; a DELETE with only `outer`
  sends it as its whole body).
- `body` on an operation: fields merged into its request body. A string
  value that is a template is filled when it is sent: `{param}` is a path
  parameter or the ID, `{current.field}` the resource as the API answered it
  last; a value that is one template whole keeps that value's type (Grafana
  Synthetic Monitoring's check update sends the `tenantId` the API gave it).
- `request` on a create, an update or a delete: the call's whole body,
  built from the spec, for an API whose calls each take the object in
  another shape (Datadog's datastore item: a create of
  `{"data": {"attributes": {"values": ["{value}"], ...}}}`, an update of
  `{..., "item_key": "{item_key}", "item_changes": {"ops_set": "{value}"}}`,
  a delete of `{..., "item_key": "{item_key}"}`). A string that is one
  `{field}` is that spec field as it is (dotted for a nested one), a path
  parameter or the ID; `{current.field}` the resource as last read; text
  around templates makes a string; a template nothing fills leaves its
  member out. It is sent as it is, in place of the spec's body and outside
  the kind's `bodyEnvelope`. The fake reads each such body back into the
  spec's fields.
- `delete.body`, `delete.sendBody`: a delete that sends a body: a fixed one,
  inside the kind's envelope (GitHub closes an issue with a PATCH of
  `{"state": "closed"}`; Grafana removes a permission with a POST of
  `{"permission": ""}`; Datadog deletes a suite with a POST to
  `/suites/bulk-delete` naming `{public_id}`), or the spec's own body
  (Grafana Asserts' DELETE lists the thresholds it removes). A delete that is
  not a DELETE leaves the resource gone unless the kind says it `stays`.
- `delete.bodyEnvelope`, on an assoc: the undo's body is wrapped in this
  instead of the kind's `bodyEnvelope` (PagerDuty's `change_tags` makes a tag
  link with `{"add": [...]}` and undoes it with `{"remove": [...]}`).
- An update whose path does not name the item names it in its body
  (`updateID`): Grafana Synthetic Monitoring's `POST /check/update`,
  Datadog's `PUT /cost/budget`, which creates a budget too.
- `envelope` / `bodyEnvelope`: the field responses and bodies are wrapped
  in (`result`, `data`, `spec`, PagerDuty's `{"service": {...}}`). An ID
  beside the envelope rather than in it (Confluent) is found too.
- `bodyArray`: wraps a single-item request body in a one-element JSON array
  (`[body]`) when sending it (Atlas's `POST .../accessList`).
- `bodyField`: the spec field whose array value is sent as the whole
  request body (a bare JSON array) and where an unwrapped array response is
  placed (`{bodyField: array}`, Cloudflare's `CloudConnectorRules`).
- `multipart`: encodes Create and Update request bodies as
  `multipart/form-data` instead of JSON (`files` names the file-upload
  fields holding `{name, content, contentBase64, contentType}` objects);
  `calls: [create]` makes only those calls multipart, the others JSON
  (Grafana k6 uploads a load test and changes it with a JSON PATCH).
- `envelope` on a call: where that call's answer holds the object, instead
  of the kind's `envelope`; a number in the path indexes an array
  (Cloudflare's bulk create answers `{"result": [{...}]}`: `result/0`; a
  Cloudforce One asset is read as a one-element list). `bodyArray` on a
  call: that call alone sends the object as a one-element array (the bulk
  create's `[{...}]`, where the PATCH takes the object).
- `read.raw`: the read answers not JSON but the named field's value, as it
  is (Workers KV answers a key's value as its bytes). A write's answer is
  then not taken for the object: it is read back.
- `params[].field` dotted (with `inBody`): a path parameter filled from a
  field nested in the body (Datadog's custom forecast is read at
  `.../budget/{budget_id}/custom-forecast`, its body's
  `attributes.budget_uid`). Written on a path without that parameter (its
  update `PUT /cost/budget/custom-forecast`), or made on a collection that
  does not name it (`POST .../custom_frameworks` makes
  `.../custom_frameworks/{handle}/{version}`), the body names the item.
- `order`, on a settings kind: the kind is the order of other objects. Its
  spec's `field` lists their IDs; the read is their list (`list`, the
  array's slash path; `id`, the element field holding an ID; `sortBy`, an
  element field the API orders them by, for a list answered in another
  order); the update is the reorder call, whose body takes the IDs at `at`,
  each as `item` with its `id` (or bare, without `item`), beside the call's
  own `body` and `outer` (whose `{current.field}` values come from the
  read: Datadog's sensitive data scanner names its configuration,
  `{current.data.id}`). With `partial`, the spec lists some of them, which
  go first; the others keep their order after them, and only the listed
  ones are compared (Datadog's custom allocation rules, as the provider
  keeps rules made in the UI at the end). Deleting the object leaves the
  order as it is.
- `secretOutputs`: response fields that hold a secret the API makes (an
  API key's `privateKey`, a token's value), often answered only on create.
  They are never kept in `status.atProvider`; an object that sets
  `spec.writeSecretRef.name` gets them in that Secret (one key each), which
  it owns, and a Secret of that name it does not own is never written.
  The generator finds them by name among response-only fields (`...token`,
  `...secret`, `...password`, `...privateKey`, ...);
  `overrides.secretOutputs: {<item path>: [fields]}` sets them instead
  (`[]`: none). The manager needs create, update on Secrets for it.
- `redact`: fields whose values are sensitive though the kind keeps them
  (a Vault token's `accessor`, which is its ID and can look the token up
  and revoke it): a live run redacts them in cassettes, logs and records
  as it does `secretOutputs`, and then everywhere else the value appears
  (docs/live.md). Nothing else about the kind changes. Set by a
  hand-written kind's `redact:` or `overrides.redact: {<item path>:
  [fields]}`.
- `rotate`: the resource is a token that expires and is made again before
  it does (Terraform's `grafana_*_rotating_token`): `{prefix, lifetime,
  window, seconds, deleteOnDelete}` name spec fields (the kind's
  `queryFields`: never sent, never compared), `{name, expiresAt, ttl}` the
  body fields its create sets. Each token is named
  `<prefix>-<expiry in Unix seconds>` and lives `lifetime` (a duration such
  as `24h`, or with `seconds: true` whole seconds), sent as `expiresAt`
  (RFC 3339) or `ttl` (seconds). Once the time left is within `window` (or
  the expiry is unknown), the sync makes a new token, writes its
  `secretOutputs` to `spec.writeSecretRef`, writes the new ID to the status,
  then deletes the old one (one whose delete fails stays in
  `status.rotation.retired` and is tried again). The object is requeued for
  `status.rotation.rotateAt` (at most the resync) rather than polled.
  Deleting the object deletes the tokens it replaced, and the last one only
  where the spec's `deleteOnDelete` field is true (else it is left to
  expire, as the provider's `delete_on_destroy`). `EveryKind` moves the
  clock past the expiry and checks a new token replaced the old; a
  rehearsal runs on its fixed clock, so its names and expiries replay.
- `serverFields`: fields the API's read answers and no request sends
  (timestamps, URLs, states), with sample values, from the OpenAPI read
  response; the fake answers them beside what was sent. Never spec fields.
- `compareReturnedOnly`: the spec does not describe what a read returns
  (much of Vault); only fields the API sends back are compared, and the
  others are written once each time the spec changes, like write-only ones.
- `delete.ok`: statuses, besides 2xx and 404, that count as a done delete:
  the API refuses to delete what goes only with its parent (Cloudflare's 409
  for a Worker's latest version), so the object is let go and the resource
  left to its parent.
- `beforeDelete`: calls made before the delete (GitLab stops an
  environment first); the fake answers 403 to a delete until they are made.
- `compare`: fields read back in another form, and the rule that says when
  they are the same (`splitSpace`: `"api read_user"` and
  `["read_user", "api"]`; `enabledObject`: `true` and
  `{"url": ..., "enabled": true}`; `nodeIDs`: `["gid://x/1"]` and
  `{"nodes": [{"id": "gid://x/1"}]}`; `subsetByID`: a list of objects read
  back with more elements than written, in any order, each written one the
  same as the element read with its `id`: Cloudflare's managed transforms);
  the fake reads them back in that form.
- `compare` inside objects and lists' elements: a field dotted, `[]` for
  each element of a list, each compared with the element of the same index
  (GitLab's value stream stages write `startEventLabelId` and read back
  `startEventLabel {id}`: `stages[].startEventLabelId: slugObject`).
- `caseInsensitive`: fields the API re-cases (Snowflake uppercases
  identifiers); a change of case alone is not drift.
- `replace`: a CRUD kind with no update that can be re-created in place
  (Snowflake's `CREATE OR REPLACE`: the create call again, with `query`
  `createMode: orReplace`). Only objects that set `spec.replaceOnChange: true`
  use it; the field is in the kind's CRD only when the descriptor has a
  `replace`, and the reconciler refuses it on any other kind. Without it a
  changed spec is reported as drift (`Ready=False`, `SyncFailed`) and nothing is
  sent. A change to a field in `identity` (the name, or a function's name and
  arguments) is never applied this way: re-creating would make a second
  resource. The first create never replaces, so an existing resource of the
  same name is not overwritten.

## GraphQL kinds

Some resources have no REST API at all: GitLab's compliance frameworks,
saved replies and dependency proxy settings, GitHub's branch protection
rules. A descriptor op with `graphql` is a GraphQL request instead of a REST
call, a `POST` of `{query, operationName, variables}` to the op's `path`,
the vendor's endpoint (GitLab's `/api/graphql`, GitHub's `/graphql`); every
op of such a kind is one, and its shape is `crud`, `upsert` or `settings`;
or else the kind mixes the two, and is `crud` (below).

```yaml
create:
  method: POST
  path: /api/graphql
  graphql:
    operation: GroupSavedReplyCreate      # operationName; unique in a vendor
    query: "mutation GroupSavedReplyCreate($groupId: GroupID!, $name: String!, $content: String!) { groupSavedReplyCreate(input: {groupId: $groupId, name: $name, content: $content}) { savedReply { id name content } errors } }"
    variables: {groupId: "gid://gitlab/Group/{groupId}", name: "{name}", content: "{content}"}
    result: groupSavedReplyCreate/savedReply   # where in data the object is
    errors: groupSavedReplyCreate/errors       # the mutation's own errors
```

- `variables` are templates: `"{field}"` alone is that path parameter's
  value, or the body field's as it is (a number stays a number, a field the
  spec leaves out leaves the variable out); text around placeholders
  (`"gid://gitlab/Group/{groupId}"`) makes a string of path parameters and
  the ID; `"$body"` is the whole body (`{params: $body}`); objects and lists
  are filled member by member, any other value sent as it is. The ID of a
  `crud` kind is the global ID its create's result answers (`id.from`),
  `{id}` in the other ops' variables.
- `result` is where in the answer's `data` the object is; on a read, null
  there is not found. A find read (`read.find`) finds the object in the
  list there (`project/targetBranchRules/nodes`). `exists`, on a read, names
  a result field that says whether the object exists
  (`isCatalogResource`: anything but true is not found).
- A `200` is not success: top-level `errors`, or a non-empty list at
  `errors` (GitLab's payload `errors: [String!]!`), fails the call, and the
  object is not Ready (retried at the next resync, as a 4xx is). An error
  typed `NOT_FOUND` (GitHub's), or one the vendor's `rest.Vendor.NotFound`
  recognizes when told status 200 (GitLab's "does not exist or you don't
  have permission"), is not found: a delete is done, a read finds nothing.
- `rest.Vendor.RESTPath` is the end of a Connection's base URL that is the
  REST API's own, which the endpoint takes the place of: GitLab's `/api/v4`
  (`https://gitlab.example.com/api/v4` posts to `.../api/graphql`), GitHub
  Enterprise Server's `/v3` (`https://host/api/v3` → `https://host/api/graphql`;
  `https://api.github.com` → `https://api.github.com/graphql`). REST and
  GraphQL kinds share one Connection and its credentials.
- A read alias makes the API answer the spec's own field names
  (`enable: containerScanningForRegistryEnabled`); the `nodeIDs` compare rule
  takes a list of IDs read back as a connection (`{"nodes": [{"id": ...}]}`).
- The fake API answers GraphQL kinds by operation name, from the
  descriptors (a small store keyed by the object's ID, or its path
  parameters), so `EveryKind` and `Rehearse` run them as any other kind;
  cassettes record each posted document and its variables.
- A kind of both APIs (`crud`): some calls GraphQL, the others REST.
  GitHub makes an enterprise's organization only over GraphQL
  (`createEnterpriseOrganization`) and reads, changes and deletes it over
  REST (`/orgs/{id}`, the ID its create's result answers, `id.from:
  login`); GitLab makes, changes and destroys an epic board over GraphQL
  and reads it only over REST, by the number in its global ID (`id.match`).
  An object made or changed over GraphQL and read over REST is read back
  over REST at once, so its status holds what drift is compared with; what
  the create could not set (GitHub's organization description) is then
  updated, and a read that finds nothing there is an error, never a reason
  to create again. The fake keeps it by its ID, for both APIs' calls.

The generator writes these descriptors from `vendor.yaml`'s `graphql:
{endpoint: /api/graphql, dir: graphql}` and one hand-written file per kind
in `hack/gen/<dir>/` (`gen/graphqlgen.py` documents the format): its
Terraform types (gap types), shape, `params` (spec fields that say where it
is), `fields` (the body, as an OpenAPI schema's properties), `required`,
`writeOnly`, `createOnly`, `compare`, and per op the document, variables,
result (and `errors`, which a mutation selecting `errors` gets by default).
It checks that each document is one named operation whose declared
variables are the file's, and that each placeholder is a param, a field or
the ID. The documents come from the vendor's GraphQL reference and the
pinned Terraform provider's source, which each file cites.

## Hand-written kinds

`graphql`'s files, and those of `handwritten: {dir: kinds}`, may describe
an op as a REST call instead of a GraphQL document: `{method, path, query,
headers, body, find, ok}`. The kind is then written by hand from the
vendor's REST reference and the Terraform provider, as a GraphQL kind is,
for REST resources no OpenAPI item pairing finds: a GitHub branch (POST
`.../git/refs` with `ref: "refs/heads/{branch}"`, a `body` template filled
from path parameters; read at `.../git/ref/heads/{+branch}`), a GitLab
user's identity (written by `PUT /users/{id}`, found in its `identities`,
deleted at `.../identities/{provider}`), one field of a GitLab group at the
Group kind's own path (`PUT /groups/{id}`, cleared by the same call with
`{file_template_project_id: null}`, `stays`), or operations a vendor's
OpenAPI leaves out (GitHub's EMU group mappings). A param's `placeholder`
names it in paths where that differs from its field (`group_id` fills
`{id}`); `inBody: true` sends it in the body too; `multipart: [avatar]`
sends a multipart body with those file fields. A param's `field` names a
body field (dotted: `attributes.source`) that fills a placeholder or a
template, with no spec field of its own. Beside them a file may give the
kind's `envelope`, `bodyEnvelope`, `bodyArray`, `compareReturnedOnly`,
`updateID` and `forget` (a crud kind with no delete), `unread` (a settings
kind with an update and no read: see below), `anyBody` (the spec
keeps unknown fields and sends them as the body: Vault's generic endpoint),
`carry` on an update or a delete, an `id` that is a template or has `find` (`id.find` above), a read's `body`
(a POST read naming the object, above) and `find.key`, and per call
`envelope` (Vault's token create's `auth`), `bodyArray`, `absent`, `carry`,
`outer`, `sendBody`, `bareBody` and `request` (the call's whole body, built
from the spec), as the descriptor has them; `queryFields` (fields never in
a body) and `rotate` (a token made again before it expires) as the kind's.
`gen/graphqlgen.py` documents the format.

A hand-written settings kind may be `unread: <reason>`, with an `update`
and no `read`, where the API has no read the spec can name and Terraform's
provider reads nothing back either (Okta's resource owners: `POST
/governance/api/v1/resource-owners` sets the owners of the resources it
lists, and owners are listed only filtered by each resource's parent ORN,
which neither the spec nor the POST names). Its update is sent when the
spec changes (`status.appliedHash`) and only then, as Terraform sends its
update when the configuration changes; nothing is compared, so a change made
outside Kubernetes is not seen or put back; `status.atProvider` is what the
last write answered; deleting the object leaves the resource as it is. The
fake keeps what was written; EveryKind checks the write is sent once, again
on a spec change, never on a resync or a delete.

## Set kinds

A kind of shape `set` stands in for Terraform resources that hold a whole
set of a parent's elements, each an API object of its own (Terraform's
`github_issue_labels` beside the one-label `github_issue_label`;
`github_app_installation_repositories`):

```yaml
shape: set
set: {field: labels, key: name, param: name, list: ""}  # list: where the read's array is
read:   {method: GET, path: "/repos/{owner}/{repo}/labels", query: {per_page: "100"}}
create: {method: POST, path: "/repos/{owner}/{repo}/labels"}            # adds one, the element its body
update: {method: PATCH, path: "/repos/{owner}/{repo}/labels/{name}"}    # changes one (optional)
delete: {method: DELETE, path: "/repos/{owner}/{repo}/labels/{name}"}   # removes one
```

`spec.<field>` lists every element: objects, or bare values that are each
the key (repository IDs: an element call then sends no body). Each sync
reads the list (every page), adds the elements it lacks, changes those
whose fields differ, then removes the ones the spec leaves out: the set is
authoritative, as Terraform's is, so an element made outside Kubernetes is
removed. `key` names an element, as the list answers it; its value fills
the element calls' `{param}`, and their other placeholders come from the
element's fields. Deleting the object removes the elements last applied (a
404, or a status the delete's `ok` lists, is done: GitHub keeps an
installation's last repository). The fake keeps each element at its own
path under the list's, as the one-element kinds keep theirs.

Where the list names an element only by a URL (Okta's linked objects,
`{"_links": {"self": {"href": ".../api/v1/users/<id>"}}}`), `set.keyURL`
is that URL's slash path in an element: its last segment is the key, and
`key` the key's name in the spec's elements. An element may be added and
removed on paths outside the list's (Okta links a user at
`/api/v1/users/{userId}/linkedObjects/{primaryName}/{primaryUserId}` and
lists them at `/api/v1/users/{primaryUserId}/linkedObjects/{associatedName}`):
the fake keeps each at its delete's path and lists those of the list's
parameters.

Where the list holds only the elements' keys (Vault's LIST,
`{"data": {"keys": [...]}}`), `set.readEach` reads each element by its
key. Where it holds objects that name each element but leave fields out
(GitHub's team members, listed by login without the role each one's
membership holds), `set.readEach` reads each one too, and its answer's
fields join the list element's. `set.holder` is the resource the elements are in, which the kind
makes first, if a read of it finds none, from the spec's `holder.fields`
and its create's `body`, and deletes, with the elements, when the object
is deleted (Terraform's `vault_database_secrets_mount`: `POST
/sys/mounts/{path}` with `type: database`, then one `POST
.../config/{name}` per connection). A change to a holder field is not
sent. Where one call both adds and changes an element (Vault's POST), the
fake takes it as a change of an element it holds.

Where two fields name an element together, `set.keys` lists them in place
of `key` and `param`, and each fills the element calls' `{field}`s, in
their paths or their query (Atlas's managed namespaces of a global
cluster: `DELETE .../managedNamespaces?db={db}&collection={collection}`);
the fake keeps such an element at its delete's path and query. A set with
no `update` whose elements are objects refuses an element whose fields
differ from the API's, rather than leave it: it cannot be changed in
place, so remove it from the list, then add it back (as Terraform refuses
a changed managed namespace).

`set.batch` is a second list in the same resource, read in the same
answer and written another way: added to by one call that sends only the
elements the API lacks, as `{<field>: [...]}`, and removed only all
together (Atlas's custom zone mappings: `POST .../customZoneMapping`,
`DELETE .../customZoneMapping`):

```yaml
set:
  field: managedNamespaces
  keys: [db, collection]
  list: managedNamespaces
  batch:
    field: customZoneMappings   # the spec's list; left out, the API's is left as it is
    key: location               # names an element
    at: customZoneMapping       # where the read answers them: {location: value}
    value: zone                 # the field those values stand for (answered as the zone's ID)
    add:   {method: POST, path: ".../customZoneMapping"}
    clear: {method: DELETE, path: ".../customZoneMapping"}
```

Elements are compared with the answer by key, and by value with the
elements last applied (`status.atProvider`), since the API answers the
value in another form (a zone's ID for its name), as Terraform compares
them. A spec that drops an element, or changes one's value, is refused
unless it lists none, which clears them all; the API's form of the list is
kept in `status.atProvider` at `at`. Deleting the object clears the list if
it lists any. The fake answers each value as a 24-hex ID made from it.

## SQL kinds

Snowflake's REST API has no resource for much of what Terraform manages
(policies, most integrations, file formats, resource monitors, shares,
attachments, parameters, grants to shares); Terraform's provider manages
them with SQL. A kind of shape `sql` does the same through Snowflake's SQL
API (`POST /api/v2/statements`, `sqlapi.yaml` in the pinned
`snowflake-rest-api-specs`): its descriptor's `sql` holds the statements as
templates, and package `sqlapi` fills and sends them one at a time.

| Mode | Create | Read | Change | Delete |
| --- | --- | --- | --- | --- |
| `object` | `CREATE` | `SHOW ... LIKE`, then any `DESCRIBE`; no row whose `match` columns hold the object's values is "not found" | one `ALTER` per field that drifted (`alter`; a list: `add`, `remove`), `UNSET` for a field the spec drops (`unset`) | `DROP ... IF EXISTS` |
| `set` | `ALTER <other object> SET` (a parameter, a policy attached to a user, a tag, an owner) | `SHOW PARAMETERS`, `SHOW ... POLICIES ON USER`, `DESCRIBE USER` | the create again | `ALTER ... UNSET`; a field only the delete names comes from the spec (the role ownership goes back to) |
| `set` with no `create` | nothing: what they are on exists (the account) | the reads; one that selects by no field and finds nothing finds nothing set | one `alter` per field that differs (`unsetFirst`: its `unset` first, when the read finds one set), `unset` for a field last applied that the spec drops | `unset` for each field it applied that a read still finds set |
| `grant` | `GRANT` (or any link: a constraint, a policy on a table) | `SHOW GRANTS`; a list field in `match` needs a row per element; mapped `columns` must read back as the spec has them | undo what was made, make the new | `REVOKE` |

A template is the statement's text with `{field}` for a spec field and
`[...]` around a part left out when a field in it is not set; `{@id}` is the
object's name (its `id` fields, `database`, `schema`, `name`, as quoted
identifiers), `{@parent}` all of it but the last, `{@like}` the last as a
`SHOW ... LIKE` pattern, and `{field:string}` a field written as another
type. An object's statement may name all its `id` fields instead of
`{@id}` (`ALTER USER {user} ADD PROGRAMMATIC ACCESS TOKEN {name}`); an `id`
field may be a string Snowflake keeps as written (a Postgres mirror's
`mirror_name => '...'`). A read's `columns` map spec fields to its columns
(`a.b.c` joins three as a name's parts; `match` takes such a key too) or,
with `properties`, to `DESCRIBE`'s property rows (`null`: the value that
means "not set", DESCRIBE USER's `null`; a first read none of whose
properties is set finds nothing). `listColumns` fills a list from a column
of every row (`SHOW DATABASES IN FAILOVER GROUP`, no rows an empty list);
`without` leaves a row's own value out of a list it maps (SHOW FAILOVER
GROUPS lists the group's own account among its allowed accounts).

**Settings set field by field** (a `set` kind with no `create`: the
account's parameters, resource monitor and policies, Terraform's
`snowflake_current_account`, each changed by an `ALTER ACCOUNT` of its own).
Each field has its `alter` and `unset`; only a field that differs from what
the reads answer is sent, and `status.appliedFields` lists those set, so a
field the spec drops is unset, and deleting the object unsets each of them
(never the rest). A field a read maps and finds not set is not unset
(Snowflake refuses to unset a policy that is not set). `unsetFirst` lists
fields Snowflake will not replace in place (a password policy on the
account): set to another value, it is unset first. A field with no `alter`
only selects (the organization account's `name`): a read that selects by a
field and finds nothing is an error, as what the settings are on must exist.
A `properties` read whose `value` is `a.b.c` joins those columns as a name
(`POLICY_REFERENCES`' `POLICY_DB.POLICY_SCHEMA.POLICY_NAME` by
`POLICY_KIND`). The fake holds what they are on from the start (the
sample's fields no statement sets), and a statement such a kind shares with
a kind of one setting (`ALTER ACCOUNT SET PASSWORD POLICY ...`) sets both, as
in Snowflake they are the same setting.

More than one value at a time:

- `{?field}` and `{!field}`: a boolean that writes nothing and is set only
  when true (false): `[ SECURE{?is_secure}]` turns a keyword on and off,
  `ALTER ... [ SET SECURE{?is_secure}][ UNSET SECURE{!is_secure}]` both ways.
- Types `object` and `objects`: a structured value, or a list of them, each
  written by its `item`'s template from its own `fields` (`item.sep`
  between elements, default `, `): a table's columns
  `{name} {type}[ NOT NULL{?not_null}][ DEFAULT {default}]`, nested to any
  depth. `{list.sub}` writes each element's field
  (`DROP FUNCTION {@id}({arg.type})`).
- `add` and `remove`: for a list field a read maps that Snowflake has no
  SET for, the statements that add the elements the spec has and Snowflake
  does not, and remove those it has and the spec does not, the field holding
  just those (`ALTER FAILOVER GROUP {@id} ADD {allowed_databases} TO
  ALLOWED_DATABASES`); removed elements are written by their exact names.
- `createColumns`: outputs read from the create's own answer (a
  programmatic access token's `token_secret`, answered only then); with the
  kind's `secretOutputs` they go to `spec.writeSecretRef`, never the status.
- `warehouse`: the spec field naming the warehouse a kind's statements run
  in (a materialized view's CREATE runs its query; `POLICY_REFERENCES` and
  `INFORMATION_SCHEMA` reads need one), sent as the request's `warehouse`;
  unset, the Connection Secret's key `warehouse`, else the user's default.
  Other kinds' requests are unchanged (no `warehouse` key).

**No spec value reaches SQL as written.** Each field has a type that says
how it is written: an identifier is double-quoted with `"` doubled (a plain
name upper-cased, as Snowflake does an unquoted one; one written in double
quotes keeps its case); a string is single-quoted with `'` doubled and `\`
escaped; numbers, booleans, keywords (enum values) and data types are
checked against what they may be and written bare (a keyword with an
`enum` only as one of its values, which may be words: `CALLED ON NULL
INPUT`); lists are of those. A `dollarstring` is written in `$$` and may
hold no `$$`; a `namestring` and an `exactstring` are a name, or an
identifier's exact name, as a string constant (`'"DB"."SC"."T"'`,
`'MY_SCHEMA'`). An `expression` (a policy's body) is SQL by design: it is
checked to be one balanced expression with no `;` and no comment outside
its quotes. A template's own text may hold only keywords (`SYSTEM$GET_TAG`)
and punctuation, constant strings and quoted identifiers of the same, never
a `$$`. Every request
asks for exactly one statement (`multi_statement_count: 1`), and a
`status.id` is read back only as a quoted object name, never as SQL.

What happens: a `202` is polled by its statement handle
(`GET /api/v2/statements/{handle}`) until it ends, every partition of an
answer is read, and a `422` whose message says the object "does not exist
or not authorized" is not found (as Terraform's provider reads it). An
object is made only by its own `CREATE`, which refuses a name that is
taken: an object of the same name made elsewhere is never taken over, and
deleting the Kubernetes object then drops nothing. A changed name is
refused (it would make a second object): delete and recreate. A field with
no `alter` is drift that stops, like a REST kind's create-only field. Only
fields a read maps (or selects by) are compared; the others are write-only
(the descriptor's `writeOnly`: secrets, certificates, a signature
Snowflake re-formats) and are written again when the spec changes. Fields
`output: true` are what Snowflake makes (a storage integration's IAM user
ARN): read into `status.atProvider`, never sent.

**Asynchronous objects** (`settle`: Openflow deployments, runtimes and
connectors). Snowflake answers their statements at once and does the work
after, reporting a status: `settle.field` is the output the first read
maps it to, `ready` the statuses the object is done in, `failed` those a
wait ends in as an error, `busy` those of work in flight (Snowflake
refuses another statement meanwhile). After each statement the object is
read back; while it is not ready the controller requeues every
`SettleRetry` (15s) with `Ready=False, Settling` and the status's value in
the message, instead of blocking, and sends the next statement (a change
is one statement per field) only once it is. A wait longer than
`settle.timeout` (default 30m, as Terraform's) is an error, retried with
backoff; the Ready reason once it settles is the one the wait was for
(`Created`, `Updated`). `status.settling` keeps the wait (since when, and
the write-only fields already written, so that none is written twice). A
delete reads the status: in `gone` (DELETED) the `DROP` is sent; `busy`, it
waits; else the first `teardown` step whose `when` holds the status (a
step with none: any other) runs its statement (`ALTER OPENFLOW RUNTIME
IF EXISTS {@id} TERMINATE`; none: it waits) and the delete waits for the
next status, the finalizer staying until the `DROP` is sent. A resync
judges the status too: a failure status is an error, a status neither
ready nor busy (a deployment `NOT_HEALTHY`) a wait.

**Create-only fields are watched.** A field Snowflake takes only in the
`CREATE` and never reads back (a view's query, a policy's signature, an
Openflow connector's definition) has no drift to see: `status.createdWith`
keeps a hash of each as the object was created, and a spec that changes or
drops one is refused (`SyncFailed`: delete and recreate, or put it back)
rather than ignored. An object made before the hashes were kept takes its
spec as it is at the next sync. Hashes only: no value is in the status.

The fake answers exactly the statements the templates make, read back with
the same templates (the most specific one wins: the one whose literal text
the statement uses most; an optional part is left out unless the statement
needs it), keeps each object by kind and name, and answers `SHOW` and
`DESCRIBE` with the rows the reads map, every value as text (outputs made
up; kinds sharing a read answer every column any maps); a create answers
`202` first. An asynchronous object's status is `busy` (its first; a
change's `UPDATING` where the kind has it) for the reads of one sync after
each statement, then `ready` (a teardown's: its waiting step's status,
then `gone`); while busy it refuses another statement, and it refuses a
`DROP` before it is gone, as Snowflake does: `EveryKind` checks that a
create, an update and a delete each waited. It refuses a statement of a kind with a `warehouse` sent
without one, as Snowflake does. `EveryKind` and
`Rehearse` run sql kinds through create, read back, update, drift repair
and delete on it.

## Draft kinds

The generator makes a kind only where an API resource fits one of the
runtime's shapes; every other Terraform gap type gets a draft kind, so the
gaps are a to-do list with most of the work done. A draft has readiness
`draft` and:

- a spec from the Terraform provider's schema for the type
  (`hack/gen/terraform-schema.json`, written by `gen/tfschema.py`):
  attributes, nested blocks, required, sensitive (noted: set it from
  `spec.secretRefs`), no descriptions (MPL-2.0 prose); outputs (computed-only attributes) are
  named in the CRD's `status.atProvider`. Each field takes the API's name
  where the request body of the closest API path has the same field
  (ignoring case, `_` and `-`), a path parameter its placeholder's; the
  others are Terraform's names camelCased, listed as an open item to
  confirm;
- calls: the generator's best candidates, the spec's paths sharing the
  type's words (create: POST on the collection; read: GET on the item;
  update: PATCH or PUT on it; delete: DELETE on it), and other candidate
  paths with their methods (`draft.candidates`);
- `draft.open`: every unresolved part, named: `no API in spec: ...`,
  `create: ...`, `read: no GET on the item path ...; found only in a list`,
  `update: ...`, `delete: no DELETE on the item path ...`, `path: ...`
  (the match lacks some of the type's words, or another kind has the path),
  `envelope: ...`, `body: ...`, `fields from Terraform schema; confirm
  against API: ...`, and first, the vendor's own note on what blocks it
  (`draftNotes`, below): `known blocker: ...`.

A draft is never reconciled: the manager runs no controller for it (it
logs how many it left out), the reconciler refuses one, the fake API has
no route for its calls, fieldRefs cannot name one, `plan_on_fake` skips
one, and `EveryKind`, `Rehearse`, `Replay` and `Live` skip each with its
open items as the reason (last, so the other kinds keep their cassettes'
object names); a `live.yaml` may not list one. Its CRD is written to
`config/crds/drafts/`, outside `config/crds`' kustomization (the install
bundle); `ValidCRDs` validates it and `ValidExamples` checks its sample
against it with the others. `zz_generated.report.md` counts only real kinds
as covering a Terraform type and lists each draft with its open items; the
MCP server's `describe_kind` answers a draft's open items.

A Terraform type that will not be a kind (not a resource, a retired API,
the owner's decision) gets no draft: `vendor.yaml` `hardBlocks: {<type>:
<reason>}`. `draftNotes: {<type>: <reason>}` records what keeps a draft from
being a kind. Both must name gap types no kind covers, so generation fails
on a stale entry. A README with the markers `<!-- BEGIN generated: not
covered ... -->` and `<!-- END generated: not covered -->` gets its "Not
covered" table generated from them between the markers.

To turn a draft into a kind, resolve each open item in the generator's
inputs, never in the generated files:

1. The calls. A path the matching missed or took wrongly: `overrides.map`
   (`<type>: <item path>`). A read only in a list: `overrides.readFrom` or
   `listReads`. A create elsewhere: `overrides.creates`; an update
   elsewhere: `overrides.updates`; a delete by another call:
   `overrides.deletes` or `deleteBody`; none: `overrides.forget`. Calls the
   spec leaves out: a supplement in `hack/gen/supplement/` (sourced from the
   vendor's API reference and the pinned Terraform provider), listed in
   `specs`. An API with no OpenAPI item at all: a hand-written kind
   (`handwritten`, "Hand-written kinds"), a GraphQL kind (`graphql`) or an
   SQL kind (`sql`).
2. The fields. A kind's spec is the API's request body: the camelCased
   Terraform fields are only a guide to which body fields matter. Body
   fields the spec leaves out: `overrides.extraFields`.
3. Anything the runtime cannot express yet (the `known blocker`) is a
   runtime feature first.

Regenerate: once a kind stands in for the Terraform type, the draft is
gone (remove its `draftNotes` entry; generation says so). Then run
`EveryKind` and record the rehearsal as for any new kind.

## Generator options (vendor.yaml)

| Option | For |
| --- | --- |
| `trailingSlash: true` | every API path ends in `/` (Sentry); matching ignores it, requests keep it |
| `envelope: {response: [...], request: [...]}` | named wrappers |
| `envelope: {auto: true}` | a single-key wrapper named after the resource (PagerDuty) |
| `envelope: {always: data}` | the spec describes only the inner object (Vault) |
| `envelope: {updateID: id}` | JSON:API: an update names the item in its body too (`data.id`), where the update's schema has the field (Datadog) |
| `overrides.patchUpserts` | `[<item path>, ...]`: a path with GET, PATCH and DELETE but no PUT is an upsert whose PATCH turns it on and DELETE resets it (Cloudflare's zone DNSSEC); opt-in, since an ordinary item has the same methods |
| `overrides.deleteBody` | `{<item path>: {method: PATCH, path: <delete path>, body: {state: closed}, stays: <reason>}}`: a delete that sends a body. A write the API takes instead of a delete (GitHub closes an issue or a pull request: with `stays`, the resource is still readable after it); a POST of a fixed body (Grafana's permission items, `{permission: ""}`, after which it is gone); a POST on another `path` naming the item by a `{param}` template (Datadog's `/suites/bulk-delete`, `public_ids: ["{public_id}"]`); or `sendSpec: true` in place of `body`, a delete that sends the spec (Grafana Asserts' thresholds). `method` defaults to PATCH, `path` to the item's; the descriptor's `delete.body` (or `sendBody`) and `stays` |
| `overrides.outerBody` | `{<item path>: {meta: {}}}`: fields sent beside the request envelope on the create, the update and the delete, the delete's whole body if it has no other (Datadog's sensitive data scanner); the descriptor's `outer` |
| `overrides.queryFields` | `{<item path>: [region]}`: spec fields of their own, never in the body, each sent as the query parameter of its name on every call whose spec declares it (Grafana Cloud's `?region=`; a Confluent switchover pair's `?environment=`, on all calls but its create); schema and description from the spec's parameter, required; the descriptor's `queryFields` and `{field}` query templates |
| `overrides.headers` | `{<item path>: {<create, read, update or delete>: {<header>: <value>}}}`: headers a call needs that the spec does not describe; a value may be a `{field}` or `{current.field}` template (Grafana Assistant: `update: {X-Resource-Scope: "{current.scope}"}`) |
| `overrides.updateQuery` | `{<item path>: {name: value}}`: query parameters every update sends, as `deleteQuery` for deletes |
| `overrides.paramFields` | `{<item path>: {<placeholder>: <field>}}`: the spec field that fills a path parameter. A body field under another name, dotted for a nested one, is sent in the body too (Confluent's tag binding: `{typeName: entityType, qualifiedName: entityName}`; Datadog's custom forecast: `{budget_id: attributes.budget_uid}`; the descriptor's `params[].inBody`); any other name is a field of its own, where the placeholder's name is taken by a body field (PagerDuty's change_tags: `{id: entityID}`, the body's `id` being the tag's) |
| `overrides.updates` | `{<item path>: {path: <update path>, method: post}}`: updated on another path than the item's, which names the item in its body (`updateID`): Grafana Synthetic Monitoring's `POST /check/update`; Datadog's and Grafana Asserts' PUT to the collection that creates too (`creates`' `{path, method: put}`). An item path with only GET and DELETE becomes an upsert written there (Datadog's custom forecast: `PUT /cost/budget/custom-forecast`, named by a body field: `paramFields`) |
| `overrides.updateID` | `{<item path>: <field>}`: the body field an update names the item by, set to its ID where the spec leaves it out (`id`; a rules file's `name`); the descriptor's `updateID` |
| `overrides.keyedBy` | `{<collection path>: <body field>}`: objects with no item path, named by a body field on their collection: POST makes one, GET lists them all (the read finds the field), PUT changes the one its body names, DELETE with the body removes it (Datadog's v1 Azure and GCP integrations, by `tenant_name` and `project_id`); an upsert whose key is a path parameter in no path (`params[].inBody`) |
| `overrides.writable` | `{<item path>: [id]}`: body fields the spec marks read-only that the request does take (PagerDuty's change_tags names a tag by its `id`) |
| `overrides.forget` | `{<item path>: <reason>}`: a CRUD kind whose API has no delete (Atlas's snapshot export jobs, PagerDuty's incident types): deleting the object lets go of the resource and leaves it in the API, as Terraform does; the descriptor's `forget` |
| `overrides.envelope` | `{<item path>: {response: data, request: ""}}`: one kind's envelopes instead of the vendor's (`""` none); `response` unwraps the read schema's property of that name, or names a wrapper the spec leaves out (Grafana's ML API answers `{"data": ...}`, its Annotation does not) |
| `postUpdates: true` | writes are a POST to the path read from (Vault) |
| `overrides.postUpdates` | `[<item path>, ...]`: the same, for these paths only (Okta's threat insight settings, Cloudflare's Total TLS); a path that ends in an ID, with GET and POST and nothing else, is settings named by that ID (Okta's profile mapping). With `creates`, an item made elsewhere is updated by a POST to its own path (Vault's TPM group) |
| `versionedMediaTypes: true` | per-operation vendor media types become headers (Atlas) |
| `caseInsensitive: [name]` | applied to every kind |
| `overrides.map` / `exclude` / `excludePaths` / `excludePathPrefixes` | hand corrections to matching (a `map` value may list several paths: one Terraform type split across kinds, Grafana's permission items per user, team and role); prefixes drop data-plane APIs on other hosts (Confluent's `/kafka/v3`) |
| `overrides.synonyms` / `wordSynonyms` | path words that mean a Terraform word (`groups` → `project` in Atlas; `""` drops a word) |
| `overrides.placeholderWords` | a regex whose group turns a placeholder into words (Vault's `{aws_mount_path}` → `aws`) |
| `overrides.kindNames` | name the kind for an API path that stands in for several Terraform types (Okta's `/apps/{appId}` → `Application`) |
| `overrides.excludeOperationIdPrefixes` | operations to ignore (Vault's `enterprise-stub-*`, which describe no body) |
| `overrides.creates` | `{<item path>: <create path>}` for an item made by a POST on a different path than its own (Sentry's `POST /organizations/{org}/teams/` makes `/teams/{org}/{slug}/`), or on the same path (`<item path>: <item path>`) for an `upsert` kind whose initial create is `POST` and updates are `PUT` or `PATCH` (GitLab's `RepositoryFile`, Atlas's search deployment); a create path that names the item too (`.../ca/{name}/import` for `.../ca/{name}`, with an update) makes an upsert created there; `{path, method: put}` for a create that is not a POST (Datadog's `PUT /cost/budget`); a list of `{path, terraform, update: false, createOnly, secretOutputs}`: a kind of its own per create path, each for the Terraform type it names, at the same item path (Vault's KMIP CA, generated or imported; a PKI root CA, generated as an issuer, beside the issuer's own kind); an entry's `idFrom` (dotted; a number indexes a list: `imported_issuers.0`, the first issuer a PKI import made), `forget` (a reason: this kind leaves what its create made, though another kind at the item path deletes it) and `writeOnly` are its own |
| `overrides.readQuery` | `{<item path>: {name: value}}`: query parameters a read needs to answer in full (Vault's UI headers: `multivalue: true`) or dynamically from spec (`ref: "{branch}"`); the descriptor's `read.query` |
| `overrides.createQuery` | `{<item path>: {name: value}}`: query parameters every create sends (Okta's user create: `activate: false`, or it activates the user and emails it); the descriptor's `create.query` |
| `overrides.deleteQuery` | `{<item path>: {name: value}}`: query parameters every delete sends (GitLab's `RepositoryFile`: `branch: "{branch}"`, `commit_message: "{commit_message}"`); the descriptor's `delete.query` |
| `overrides.deleteOK` | `{<item path>: [409]}`: statuses a delete answers for a resource that goes only with its parent, counted as done: the object is let go and the resource left to its parent, as Terraform does (Cloudflare's latest `WorkerVersion` and active `WorkersDeployment`); the descriptor's `delete.ok` |
| `overrides.extraFields` | `{<item path>: {<field>: <schema>}}`: extra body fields injected into the request body schema when the OpenAPI spec omits them (e.g. `branch` and `commit_message` on GitLab's `RepositoryFile` multipart upload) |
| `overrides.anyValue` | `{<item path>: ["custom_fields[].value"]}`: fields (dotted, `[]` for a list's items) that take any value, whatever the spec says: for a spec that describes a field wrongly (PagerDuty documents a custom field's `value` as an object; the API takes the bare value) |
| `terraform.docsAlso` | `{<type>: [<other types>]}`: Terraform resources whose docs also describe a kind's fields, for a partial resource whose own docs do not (Grafana's `grafana_data_source_config`: `grafana_data_source`) |
| `overrides.createOnly` | `{<item path>: [field, ...]}`: body fields (top-level or dotted inside objects, e.g. `attributes.name`) only the create takes, added to what `createOnlyFromSpec` finds; left out of every update, a change reported as drift (Okta's custom role `permissions`, Datadog's `IntegrationFastlyAccount`). On an upsert, the first write (the create, or the PUT or POST that makes it) sends them and later ones leave them out (Vault's GCP KMS key `purpose`); the descriptor's `createOnly` |
| `overrides.carry` | `{<item path>: [field, ...]}` or `{<item path>: {update: [...], delete: [...]}}`: fields, dotted inside objects, an update (and a delete) sends as a read just before answers them, else as the spec sets them: Grafana's app platform `metadata.resourceVersion` (a playlist update without it: 422), a GitHub file's blob `sha` (and the delete's `message` and `branch`, from the spec); the descriptor's `carry` |
| `overrides.serverNamed` | `{<item path>: <field>}`: the server names the object and refuses a create that does (Grafana's time intervals: 400 "metadata.name should be empty"): the field (dotted) is no spec field, the ID comes from it (unless `idFrom` says otherwise), and updates carry it back |
| `overrides.absent` | `{<item path>: {<field>: <value or null>}}`: what a read answers, with 200, when there is none (Atlas's `state: UNCONFIGURED`, or no `id`); the descriptor's `read.absent`. The kind's server field samples avoid it |
| `overrides.adopt` | `{<Terraform type>: {item, list, at, query, match, sample, reason}}`: a resource the vendor makes, taken over rather than created (Okta's `okta_auth_server_default`: `{item: /api/v1/authorizationServers/{authServerId}, list: /api/v1/authorizationServers, match: {default: true}}`): a CRUD kind of its own beside any kind at `item`, read and updated there, found in `list` (its array at `at`, with `query`) by `match` (values, or `{field}` from the spec), never deleted (`reason`, the descriptor's `forget`); `sample` sets fields of the kind's sample (what the default holds) |
| `overrides.updateBody` | `{<item path>: {<field>: <value>}}`: fields merged into the `update` request body (Datadog's `RumApplication`: `type: rum_application_update` on PATCH where create takes `rum_application_create`); the descriptor's `update.body` |
| `overrides.deletes` | `{<item path>: <delete path>}`, with the item in `creates`: deleted on another path than it is read from (Grafana's `DELETE /admin/users/{user_id}` for `/users/{user_id}`). The path may name it by `{current.field}`, a field its read answers (Confluent's mirror topic, deleted as `.../topics/{current.mirror_topic_name}`); for an item with no `creates`, such a delete makes its settings kind an upsert (Grafana k6's schedule, `/cloud/v6/schedules/{current.id}`) |
| `overrides.elements` | `{<Terraform type>: {item, in: {<field>: <list>}} \| {item, key, list, patch, unset}, method, whole}`: a kind of its own, one element of a list the resource at `item` holds (the descriptor's `element`): read there, written by its PUT, PATCH or POST (`method`); its fields are the element's, from the list's item schema (Vault's `allowed_client_ids`, Grafana's role `users`/`teams`/`service_accounts`, GitHub's custom property values with `patch: properties, unset: {value: null}`) |
| `overrides.states` | `{<item path>: {field, calls: {<value>: {method, path}}, delete, sample, terraform}}`: a field set by a call per value (the descriptor's `state`), added to the kind at the item path, or, with `terraform`, a settings kind of its own where the path has only a GET (Okta's feature, org factor, Okta Support, subscriptions); `delete` stands in for a delete call or `forget` (an adopted authenticator is deactivated) |
| `overrides.createBody` | `{<item path>: {<field>: <value or "{param}">}}`, with `readFrom` and a POST on its collection: an upsert the caller names, made by that body (path parameters fill its templates) and read from the list (Okta's trusted server, `{trusted: ["{associatedServerId}"]}`); the spec has no body fields; the descriptor's `create.body` |
| `overrides.redact` | `{<item path>: [accessor]}`: fields (dotted; the last part is matched) whose values a live run redacts from cassettes, logs and records though the kind keeps them (an accessor that is the kind's ID); the descriptor's `redact` |
| `overrides.writeOnly` | `{<item path>: [settings.clientSecret]}`: fields, dotted inside objects, that a read masks or leaves out though the spec says it returns them; not compared for drift (Grafana's SSO secrets) |
| `overrides.idFrom` | `{<item path>: <field>}`: the response field the path's last placeholder takes, when the generator cannot tell (GitLab's `{feature_flag_name}` is `name`, not `id`); the descriptor's `id.from` |
| `overrides.beforeDelete` | `{<item path>: [{method, path}]}`: calls made, in order, before the delete, on the item's path parameters; a 404 is ignored (GitLab's `POST /projects/{id}/environments/{environment_id}/stop`: an environment must be stopped before it can be deleted); the descriptor's `beforeDelete` |
| `overrides.keepSlashes` | `{<item path>: [path]}`: path parameters whose values keep their slashes (`{+path}`: a GitHub file in a directory, `.../contents/docs/README.md`, as go-github sends it) |
| `overrides.compare` | `{<item path>: {<field>: <rule>}}`: top-level fields the API reads back in another form than it takes. `splitSpace`: a string of space-separated words, read back as an array of them in any order (GitLab's application `scopes`). `enabledObject`: a value read back as `{"enabled": value}` (GitHub's branch protection `enforce_admins`). `subsetByID`: a list of objects read back with every element the API has, compared by `id` (Cloudflare's managed transforms); the descriptor's `compare` |
| `overrides.bodyArray` | `[<item path>, ...]`: request body is a JSON array of one item (`[body]`), or such an array inside a request envelope (PagerDuty's `{"subscribers": [...]}`: the one `overrides.envelope` names, or the body's only field); CRD takes the item's fields (`bodyArray: true`, Atlas's `AccessListApiKey`) |
| `overrides.bodyField` | `{<item path>: <field>}`: request and unwrapped response are a bare JSON array, placed in `spec.<field>` (`bodyField: <field>`, Cloudflare's `CloudConnectorRules`) |
| `overrides.multipart` | `[<item path>, ...]`: request body is `multipart/form-data`; binary upload fields become `{name, content, contentBase64, contentType}` objects (`multipart: {files: [...]}`). An entry `{path, files: [file], calls: [create]}` also makes upload fields of string fields the spec does not type as binary (Cloudflare's caption `file`), and makes only those calls multipart (Grafana k6's load test, updated with JSON; the descriptor's `multipart.calls`) |
| `overrides.bulkCreates` | `[<item path>, ...]`: a CRUD kind whose create takes an array of objects and answers one, the new object first (Cloudflare's secrets, CF1 sites, content scanning payloads); the other calls take the object. The descriptor's `create.bodyArray` and `create.envelope: <envelope>/0` |
| `overrides.readFirst` | `[<item path>, ...]`: the item's read answers it as a one-element list (Cloudflare's Cloudforce One asset); the descriptor's `read.envelope: <envelope>/0` |
| `overrides.rawReads` | `{<item path>: <field>}`: the read answers that body field's value as raw bytes, not JSON (Cloudflare's Workers KV `value`); the descriptor's `read.raw` |
| `overrides.orders` | `{<Terraform type>: {read, list, id, sortBy, write: {method, path}, at, item, field, partial, body, outer, carry, kind}}`: a settings kind (shape `settings`, the descriptor's `order`) that is the order of other objects: read from the list at `read` (its array at `list`; ordered by `sortBy`), written whole by the reorder call `write`, which takes the IDs at `at`, each as `item` with its `id`; `field` names the spec's list of IDs (the provider's attribute); `body`, `outer` and `carry` (fields of the read's answer it sends back, as `update.carry`) are the call's own (Datadog's `*_order` resources, custom allocation rules and tag pipeline rulesets). The paths, the read's array and the call's body are checked against the spec |
| `formBodiesAsJSON: true` | an operation the spec describes only as a form (`multipart/form-data`, `application/x-www-form-urlencoded`) takes the form's fields as a JSON body, less file uploads (GitLab's Grape endpoints) |
| `overrides.listReads` / `assocs` | declare `read.find` collections and `assoc` links, which a spec cannot express; an assoc's `list` says where its read's array is (`subscribers`; Datadog's role users, `data`: the link's fields are then that list element's), and its read sends `readQuery`'s parameters on every page (`page[size]`). An assoc's `deleteQuery: {groupId: "{groupId}"}` undoes it with the link's fields in the query and no body (Grafana's `DELETE /teams/{teamId}/groups?groupId=`; `sendBody: true` sends the body too); `deleteEnvelope: remove` wraps the undo's body in another envelope than the create's (PagerDuty's change_tags). The paths' own resource, if any, is dropped for the link |
| `overrides.readFrom` | `{<item path>: {path: <list path>, field: <element field, dotted if nested>, list: <where the array is>, param: <placeholder, default the item's last>}}`: an item the API writes (PUT, or POST to its collection) and deletes but never gets one of, read by finding it in a list elsewhere: GitHub's `PUT .../organization-roles/teams/{team_slug}/{role_id}` listed at `.../organization-roles/{role_id}/teams` by `slug`; PagerDuty's team members, `members[].user.id`. Fields come from the list's element; the envelope from what the writes answer (Datadog's `data`, where its lists are too). An upsert, CRUD or settings kind; with `postUpdates`, one written by a POST to its own path (an upsert with a delete, or with a `deleteBody`, removed by another POST: Grafana's permission items; else settings). `field: ""` finds a bare name (Vault's KMIP scopes in `list: data/keys`). With no `field`, `path` is read as the one object itself: a read on another path than the writes (Grafana's public dashboard, Vault's tokenization key configuration, Grafana Assistant's terms) |
| `graphql: {endpoint: /api/graphql, dir: graphql}` | kinds for resources whose only API is GraphQL, one hand-written file each in `hack/gen/<dir>/` (default `graphql`); see "GraphQL kinds" |
| `handwritten: {dir: kinds}` | kinds written by hand in `hack/gen/<dir>/` (default `kinds`), in the format of the `graphql` files, whose calls are REST (see "Hand-written kinds"); also how a `set` kind is written. A Terraform type a hand-written kind stands in for is never matched to a spec path by its name |
| `sql: {dir: sql}` | kinds of shape `sql` (see "SQL kinds"), one hand-written source each in `hack/gen/<dir>/*.yaml`: `kind`, `terraform` (gap types no other kind covers), `mode` (`object`, `set`, `grant`), `scope` for an object (`account`, `database`, `schema`: its name fields) or `id` (its own name fields: `[user, name]`), `group`, `warehouse: true` (a `warehouse` field the statements run in), `fields` (`type`, `required`, `enum` (also checked by the runtime for a keyword), `output`, `secret` (an output written to `spec.writeSecretRef`), `item` (`template`, `fields`, `sep`) for `object` and `objects`, `description`), `create`, `createColumns`, `read` (`statement`, `match`, `properties`, `columns`, `listColumns`, `without`), `alter`, `unset`, `add`, `remove`, `delete`, `settle` (`field`, `ready`, `failed`, `busy`, `teardown` (`when`, `run`), `gone`, `timeout`: an object whose statements are asynchronous), `sample`. Statements come from the vendor's SQL reference and the pinned Terraform provider's SDK, which the source names |
| `hardBlocks: {<type>: <reason>}` | Terraform gap types that will not be kinds (not a resource, a retired API, the owner's decision): no draft is made for them; the report and the README's generated "Not covered" list them (see "Draft kinds") |
| `draftNotes: {<type>: <reason>}` | what keeps a gap type's draft from being a kind: its first open item, and its row in the README's generated "Not covered" |
| `replaceOnChange: {query: {createMode: orReplace}, identity: [name], kinds: {Alert: {}, Function: {identity: [name, arguments]}}}` | the allowlist of kinds whose objects may opt in to re-creating in place (`spec.replaceOnChange`); each must be CRUD with no update, and its create must take the query. Kinds not listed get no such field (Snowflake) |

## Conventions

- A reference object whose `id` and `type` are required but inherited as
  read-only (PagerDuty's `{"service": {"id", "type"}}` on an association) is
  written anyway: the body would otherwise be empty. `type` defaults to its
  single enum value.
- Spec fields keep the vendor API's own names, so its API reference applies
  field for field. Path parameters are spec fields too.
- `spec.secretRefs: {<field>: {name, key}}` fills body fields from Secrets;
  a dotted field fills one inside an object (Confluent's Flink connection's
  `spec.auth_data.data`, a JSON string of its credentials).
  Write-only fields (never returned by the API) are re-sent only when what
  is sent changes (`status.appliedHash`).
- `spec.fieldRefs: {<field>: {kind, name, fieldPath}}` fills a spec field
  (a path parameter or a body field) from another object of the vendor's
  in the same namespace: by default its `status.id`, once it is Ready
  (`fieldPath: spec.x` takes a spec field at once; `apiVersion` is needed
  only where the kind's name is in more than one group). Nothing is sent
  until every reference resolves; meanwhile the object is
  `Ready=False, WaitingForReference`. The values used are kept in
  `status.resolvedRefs`, so the object can still be read and deleted after
  the one it refers to is gone. Deletes are not ordered: deleting the
  parent first leaves the child to the API, which deletes it with its
  parent (the child's delete then answers 404, which is done) or refuses
  the parent's delete until the child is gone (retried until then). A
  required field may be set either way: the CRD says so in a CEL rule
  (`x-kubernetes-validations`) instead of `required`, except for a field
  CEL cannot type, which stays plainly required.
- Objects are namespaced and reach the vendor through the namespace's
  `Connection` named `default` (or `spec.connectionRef.name`). Its Secret
  holds the vendor's `CredentialKeys`, or every key of one of
  `rest.Vendor.AltCredentialKeys`: an API of the vendor that signs in
  otherwise, on a Connection of its own (Atlas's App Services Admin API
  takes a programmatic API key, `publicKey` and `privateKey`, where the
  Atlas Admin API takes a service account), which the vendor's
  `Authorize` tells apart by the keys it is given.
- A request body that may be any JSON object and names no field (Vault's
  KV v1 secret) becomes a CRD whose spec keeps unknown fields: the
  caller's keys sit beside the path fields and are sent as the body
  (`secretRefs` fills them from Secrets). review.py flags it `any-body`.
- A vendor that answers "no such object" other than with a 404 (Vault: a
  500 naming a missing PKI issuer or key) says so in `rest.Vendor.NotFound`,
  in its hand-written vendor.go.
- Errors the API calls the caller's fault (4xx but 429) are not retried
  until the spec changes or the next resync.
- Two annotations, on any kind, let an object take over a resource that
  exists already without making or deleting anything (a Terraform offramp):
  - `krm.hfl.dev/adopt-id: <id>` names the resource a CRUD kind's object
    stands for, by the ID `status.id` would hold. While `status.id` is
    empty the first reconcile reads the resource by it instead of creating
    one, records it in `status.id`, and writes only what the spec changes
    (nothing, when the spec is the resource). Upsert and settings kinds,
    whose path is all spec fields, need none: they are read before they are
    written.
  - `krm.hfl.dev/deletion-policy: abandon` leaves the resource when the
    object is deleted: the finalizer goes, no delete is sent. Anything
    else, or none, deletes it.

## What the tests prove

`EveryKind` proves the descriptors and the reconciler agree with each
other, through a fake that answers as the descriptors say the vendor's API
does. It does not prove the vendor's API answers that way: every generated
kind starts with readiness `fake` and stays there until it has run against
the real API. A draft (readiness `draft`, "Draft kinds") is proven by
nothing but its CRD's validity: the tests skip it.

## Live tests

`kindtest.Live` runs the same create, read, update, drift and delete
against the vendor's real API, for the kinds a vendor repository lists in
`hack/live/live.yaml`, and records each result in `hack/live/records.json`;
the generator then marks a kind that passed `readiness: live`. It is
compiled only with `-tags live`, runs only with `KRM_LIVE=1`, and reads
credentials from Secret Manager (`krm-live-<vendor>` in the project `$KRM_LIVE_PROJECT` names,
stored with `task live:secret VENDOR=<vendor>`), skipping, naming the
secret, until that exists. Vault and GitLab need no account (`task
live:vault` in vault-krm-extensions, `hack/live/gitlab-ce.sh`).
How to run each vendor: [docs/live.md](docs/live.md).
Cassettes are public: besides secrets, live.yaml's `redactVars`,
`redactFields` and `redactSubdomains` name what identifies the test
account or a person (a login saved from a `login` field is one already),
redacted wherever a run records it; `cmd/rescrub` cleans committed
cassettes the same way (docs/live.md, "Identities").
