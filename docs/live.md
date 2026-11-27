# Live tests

`kindtest.EveryKind` proves a vendor's descriptors and the reconciler agree,
against a fake built from the same descriptors. `kindtest.Live` runs the same
steps against the vendor's real API, so a kind's readiness can move from
`fake` to `live`:

1. **create**: the reconciler creates the object; Ready.
2. **read**: reconciling again finds nothing to change (`Unchanged`). If the
   API answers differently from what was sent (a default, a re-cased name, a
   duration in seconds), this fails and says which field.
3. **update**: a field changes in the spec (`change:`); the reconciler sends
   it (`Updated`), and the next read finds nothing to change.
4. **drift**: the harness writes another value straight to the API, as a
   person in the vendor's console would; the reconciler puts the spec's back.
5. **delete**: the object is deleted; the API no longer answers for it
   (`gone`), within `deleteWait` (default 60 s).

Kinds that cannot be updated skip 3 and 4. A settings kind is never deleted:
its old values are put back. An assoc kind's delete is the reconciler's own
undo call.

## What keeps it apart

Only one path reaches a vendor, and `go test ./...` or `task check` cannot
take it (the same locks as ack-extensions' docs/live-runs.md):

| Lock | Where |
| --- | --- |
| The code that reads credentials and reaches a vendor (`Live`, `LiveSweep` and the sweep, the Secret Manager read) is compiled only with `-tags live`; built without it, `Live` and `LiveSweep` skip and say so. The steps themselves (`kindtest/live_harness.go`) are untagged, shared with `Replay`, and read no credential: they use what `Live` hands them. In GitHub Actions every credential and `{{env:VAR}}` value is masked in the log (`::add-mask::`), since Actions masks only its own secrets | `kindtest/live_run.go` (tagged), `kindtest/live_off.go` (`!live`); each vendor's `live_test.go` is tagged too |
| `task check` and CI vet the tagged code and run only the runtime's own live tests, against an `httptest` fake on 127.0.0.1; no vendor task but `live:*` runs `-tags live` | `Taskfile.yml`, `.github/workflows/ci.yml` |
| An explicit opt-in on every run: `KRM_LIVE=1` | `kindtest/live_run.go` |
| Credentials in the environment (`KRM_LIVE_CREDS`) are refused unless the API is on this machine (127.0.0.1, localhost, ::1): a sandbox's dev token. A real account's token is read from Secret Manager at run time, into the in-memory Connection Secret, and never written to disk, the environment or a log. A credential a kind needs besides the vendor's own (an integration's key) also comes from Secret Manager, never from a stored CI secret: a vendor's `live_test.go` may read another vendor's secret into the environment for `{{env:VAR}}` (Datadog's Cloudflare and Confluent integrations read `krm-live-cloudflare` and `krm-live-confluent`), so the live runner needs read access to those secrets too. `{{env:VAR}}` values are scrubbed from cassettes and masked in Actions logs like credentials | `kindtest/live_run.go` (`loopback`) |
| Everything a run makes is named `hfl-live-<stamp>-<hex>-NN`, deleted when the kind finishes (pass or fail), and swept by the next run after two hours | `live.yaml` `sweep:` |
| Only `hack/live/records.json` (results, no secrets) and `hack/live/cassettes/` (the calls, scrubbed: no headers, credential values, base URL or run names) are written back to the repository | `writeRecords`, `liveRun.end` |

## Turning it on

A vendor repository holds:

- `hack/live/live.yaml`: the secret, the allowlist of kinds in the order they
  run, and what each needs (below)
- `live_test.go`: `TestEveryKindLive` (`kindtest.Live`) and `TestLiveSweep`
  (`kindtest.LiveSweep`)
- `hack/live/records.json`: the last live result of each kind, written by the
  tests and committed

`live_test.go` is compiled only with `-tags live`; without `KRM_LIVE=1`
both tests skip. With it, the credentials come from Secret Manager, or, for
a sandbox on this machine only, from `KRM_LIVE_CREDS` (a JSON object):

```sh
gcloud secrets versions access latest --secret=<secret> --project=<project>
```

If the secret does not exist, or cannot be read, the test **skips** and says
which secret in which project; it never passes silently and never fails for
want of credentials. A secret that exists but lacks a credential key fails.

The secret's payload is a JSON object: the vendor's Connection Secret keys,
plus `baseURL` where the API has no single host, plus whatever `live.yaml`
reads with `{{cred:KEY}}` (GitHub's test organization, Atlas's organization
ID). Every vendor's secret is `krm-live-<vendor>` in the project `$KRM_LIVE_PROJECT` names
(`project:` in live.yaml; `KRM_LIVE_PROJECT` overrides it). Only the
offload runner's service account and the builder should be able to read
them (`roles/secretmanager.secretAccessor` on each secret, not the project):

| Vendor | Secret | JSON keys | Account |
| --- | --- | --- | --- |
| github | `krm-live-github` | `token`, `org` | a test organization; classic PAT with `repo`, `admin:org`, `delete_repo` |
| pagerduty | `krm-live-pagerduty` | `token`, `subdomain` | developer account; REST API key, full access |
| okta | `krm-live-okta` | `apiToken`, `baseURL` | Integrator (developer) org; API token of a super administrator |
| mongodbatlas | `krm-live-mongodbatlas` | `clientId`, `clientSecret`, `orgId`, `orgName` | free organization; service account, Organization Owner |
| snowflake | `krm-live-snowflake` | `token`, `baseURL`, `tokenType` | trial account; programmatic access token of an ACCOUNTADMIN user |
| vault | none | `token`, `baseURL` | `task live:sandbox:vault`: a dev server in kind, root token `root` |
| gitlab | none (`krm-live-gitlab` for a hosted one) | `token`, `baseURL` | `hack/live/gitlab-ce.sh`: GitLab CE in Docker, admin token |
| cloudflare | `krm-live-cloudflare` | `apiToken`, `accountId`, `zoneId`, `zoneName` | a new account for tests; a zone of your own (its domain is `zoneName`) |
| datadog | `krm-live-datadog` | `apiKey`, `appKey`, `site` | a new trial org on US5 for tests |
| sentry | `krm-live-sentry` | `token`, `org` | a dedicated test organization; internal integration token ([live-accounts/sentry.md](live-accounts/sentry.md)) |

Keys used only to keep names out of recordings (`zoneName`, `subdomain`,
`orgName`) are scrubbed like every credential: a recording holds
`{{cred:KEY}}` where the value was. Set them to your sandbox's names.


How to open each account and make its token, step by step:
[`live-accounts/`](live-accounts/README.md).

Store one from stdin, so the token never reaches shell history or argv
(it checks the JSON, creates the secret or adds a version, and prints only
the key names):

```sh
task live:secret VENDOR=github           # paste {"token":"...","org":"..."}, then Ctrl-D
pbpaste | task live:secret VENDOR=okta
task live:secret VENDOR=okta PROJECT=<another-project>   # another project
```

## Running

```sh
KRM_LIVE=1 go test -tags live -run TestEveryKindLive -v .                 # every listed kind
KRM_LIVE=1 KRM_LIVE_KINDS=IssueLabel go test -tags live -run TestEveryKindLive -v .
KRM_LIVE=1 KRM_LIVE_SWEEP=1 go test -tags live -run TestLiveSweep -v .    # delete every leftover
task live:vault                     # vault: no account; a dev server in kind (task live:sandbox:vault:down)
hack/live/gitlab-ce.sh              # gitlab: no account, ~4 GB, minutes to boot
```

GitLab CE belongs on an offload VM (`offload run --machine e2-standard-4
-- hack/live/gitlab-ce.sh`); the Vault sandbox is small enough for a laptop. Then commit `hack/live/records.json`, run
`hack/gen/generate.sh` (readiness follows the records) and commit that too.
`python3 ../krm-rest-runtime/gen/review.py .` counts `fake` and `live` kinds
and flags `live-failed`.

| Variable | Meaning |
| --- | --- |
| `KRM_LIVE=1` | run live; otherwise skip |
| `KRM_LIVE_CREDS` | credentials JSON, instead of Secret Manager; only for an API on 127.0.0.1, localhost or ::1 |
| `KRM_LIVE_PROJECT` | the Secret Manager project, instead of live.yaml's `project:` |
| `KRM_LIVE_KINDS` | only these kinds (comma-separated) |
| `KRM_LIVE_RUN` | the run's name (default `hfl-live-<UTC YYMMDDhhmm>-<4 hex>`) |
| `KRM_LIVE_RECORDS` | where results go (default `hack/live/records.json`) |
| `KRM_LIVE_SWEEP=1` | let `TestLiveSweep` delete every leftover, whatever its age |
| `KRM_LIVE_CASSETTES` | where cassettes are written and replayed from (default `hack/live/cassettes`); `off`: none written |
| `KRM_LIVE_REQUIRE=1` | no credentials fails the run instead of skipping it (the scheduled re-record) |

## Replay

A passing live run also writes each kind's calls to the API, and what the API
answered, to `hack/live/cassettes/<Kind>.<group>.json` (and the run's before
and after calls to `_before.json`, `_after.json`). `kindtest.Replay` runs the
same steps again with those answers instead of the API:

```go
// replay_test.go: no build tag; runs in go test ./... and CI
func TestEveryKindReplay(t *testing.T) {
	kindtest.Replay(t, Types, Vendor, Hooks, liveYAML)
}
```

No network (the base URL is `http://replay.invalid`, and the transport never
dials), no credentials (stand-ins: `replay-<key>` for every `{{cred:KEY}}`,
`replay-env-<VAR>` for every `{{env:VAR}}` the cassettes name), no waiting (a
replica's lag, a slow delete and a busy 409 are already in the recording; the
replay's clock moves only when it would have slept, so a deadline is still
reached, at once): every kind's create, read, update, drift and delete in a
fraction of a second. So the vendor's real quirks, the ones the tables below
record, are tested on every PR, from a fork too, by anyone and any agent,
without an account.

A replay is strict. Each call must be the next one recorded: the same method,
path (query in any order) and JSON body (as values: key order and spacing
aside), compared as the cassette holds them: the request's credentials and
names turned into their placeholders (so sending the wrong credential fails)
and redacted by the same rules. The first call that differs fails the kind
and names both:

```
replay Widget.things.example.hfl.dev: request 4 (PATCH /orgs/acme/widgets/270000001):
body {"color":"green"}, recorded {"color":"green","size":"L"}
```

That is either a regression, or a change to the runtime that is meant (it now
sends a field it did not): then re-record the kind with a live run, and the
cassette's diff shows what changed on the wire. A recorded call never made
fails too, and so does a body sent as multipart where JSON was recorded (or
the reverse); multipart bodies themselves are not compared (their boundary
differs every time). `replayAnyBody: <reason>` in live.yaml stops a kind's
bodies being compared at all, for one whose bodies differ every run in a way
redaction does not cover (none needs it today).

What a cassette keeps and leaves out:

| | |
| --- | --- |
| kept | method, path relative to the base URL, request body, response status, `Content-Type` and body; when the segment began (its `{{date:N}}`'s today) and against what; a kind's (and the after calls') saved `{{var:...}}` values as they were, so a kind replays as recorded when the before calls were re-recorded since |
| never kept | request headers (where the credentials go), response headers but `Content-Type` |
| replaced | every credential and every `{{env:VAR}}` value a template used by `{{cred:KEY}}`, `{{env:VAR}}`, also URL-encoded, JSON-escaped and in base64 (`{{cred:KEY\|url}}`, …); the base URL by `{{baseURL}}`, its host's account label (`integrator-1234567` in `integrator-1234567.okta.com`) by `{{tenant}}`; the run's and kind's names by `{{run}}`, `{{name}}` (and their `_` and upper-case forms). Where values overlap, the longer is replaced. Values shorter than 4 characters are left |
| redacted (`{{redacted}}`) | a string under a field named for a secret (`secret`, `password`, `token`, `api_key`, `private_key`, `signing_key`, `encrypted_value`, `credential`, `routing_key`, `dsn`, …, anywhere in the name, any case: `client_secret`, `secret_string`, `tokenSigningKey`) when it looks like one (a field named for it: any value; a field whose name only contains it, or one under a secret-named object: one that is not plain words, so `token_endpoint_auth_method: client_secret_post` is kept and compared); `Authorization` and `Cookie` values; a person: the name, handle, login, avatar and the like of an object that describes one (an `author`/`owner`/`created_by` object, one typed `User` or `user_reference`, one holding a real email address), and `author_name`-style fields; in any value, path, query, form body or JSON held in a string: a PEM private key, a token by its shape (`ddapp_`, `ddsat_`, `cfat_`, `ghp_`, `glpat-`, `xoxb-`, AWS key IDs, JWTs) and an email address (`{{email}}`; not at example.com, .test or .invalid); in prose (an error's text, a field's value), a value written after a secret's name (`secret_id_accessor: "711b…"`, `api_key=…`) that is not words, a URL, an enum or a time; a kind's `secretOutputs` and `redact` fields (descriptor), whatever their names (a TURN app's `key`, a Vault token's `accessor`); the account's and people's identities live.yaml names (below, "Identities"), however plain they look. Once a value (eight characters or more, not plain words) is redacted anywhere in a run, it is redacted everywhere it is, in every encoding, as a whole word or number, in every cassette, log line and record of the run: the error that quotes it, a later call that sends it back, a saved var; the cassettes written before the run learned it are redacted again at its end. A value that is a whole segment of a recorded path (eight characters or more, not token-shaped, not an email address) is an ID, not redacted, so the paths that use it still match; one that is also a secret (a share token that is its own ID) is written as `{{id:N}}` everywhere it appears, and a replay gives it a stand-in. A name that describes a secret rather than holding one (`access_token_url`, `token_endpoint_auth_method`, `secret_type`, `key_id`) and a value that is plainly not one (words, a URL, an upper-case enum, a timestamp) are kept and compared |

A redacted value is not compared, and a replay answers a redacted response
field with what it sent under that name (an API that echoes a credential
back). Wherever the recording has `{{redacted}}` in a call (a path segment, a
query or form parameter, a JSON value), whatever the replay sends there
matches it: a value it read redacted, or its own (a spec's value the run
redacted because a response elsewhere held it under a secret's name), which
a response that redacted the same field then answers. The recording is redacted again by the current rules before each
comparison, so a rule that redacts more does not break cassettes recorded
before it (one that redacts less needs a re-record). To bring committed
cassettes up to today's rules without a live run, run
`go run github.com/highfivelabs/krm-rest-runtime/cmd/rescrub` from the
vendor repository's root (`-check` lists what would change and writes
nothing); it reads each kind's `secretOutputs` and `redact` from
types.yaml, redacts `hack/live/cassettes` and `hack/fake/cassettes` again,
and shares what each cassette's redaction learns with the others, as a run
does (a rehearsal's cassettes, under `hack/fake`, each keep their own, as
the rehearsal does: the fake answers the same sample values to every kind).
It reads live.yaml's identities too and finds them in the live cassettes as
a run does, and redacts what the live cassettes teach in
`hack/live/records.json`. Values it cannot find by a rule (a login a
cassette recorded before live.yaml named it, in a field no rule names) are
given to it, never in a file: `-redact-env VAR[,VAR]` (each variable one
value per line) or `-redact-stdin` (one per line), redacted in every
directory, the rehearsal's too. `-check` shows each change as it would be,
never the value it takes out. Run `go test ./...` after it: the replays must still pass. Before a cassette is written it is checked once more: if a credential
or `{{env:VAR}}` value (in any of those encodings), a private key or a
token-shaped value is still in it, it is not written, its kind's record
says fail ("cassette withheld"), and the run fails naming where (never the
value). The same scrubbing and redaction apply to the
reasons `records.json` keeps.

Re-recording: any live run rewrites the cassettes of the kinds that pass, and
leaves a failing kind's last passing one, as `records.json` keeps `lastPass`;
`_after.json` is rewritten only with the `_before.json` of the same run.
Commit both. Every re-record changes IDs, timestamps and `recorded`; a diff
beyond those is the API having changed: worth reading before merging.
`KRM_LIVE_CASSETTES=<dir>` records to or replays from another directory;
`KRM_LIVE_CASSETTES=off` records nothing.

Replay does not replace live runs. It checks the runtime against the API as it
answered when recorded; only a live run finds that the API now answers
otherwise. The scheduled re-record (`.github/workflows/live-rerecord.yml` in
this repository, called from each vendor's `.github/workflows/live.yml`) runs
the live tests with `KRM_LIVE_REQUIRE=1` (no credentials fails, rather than
skips), regenerates readiness, replays, and opens a pull request with the new
records and cassettes; the night's results are kept as a workflow artifact
even when a step fails.

## Rehearsal

`kindtest.Rehearse` runs the same live steps against the runtime's fake API
instead of the vendor's: every kind, not only those a live run has reached.
The kinds `live.yaml` lists run with their live specs (a `skip` is ignored:
its prerequisite is the real account's), then every other kind with its
sample.

```go
// rehearse_test.go: no build tag; runs in go test ./... and CI
func TestEveryKindRehearsal(t *testing.T) {
	kindtest.Rehearse(t, Types, Vendor, nil, liveYAML)
}
```

`KRM_FAKE_RECORD=1 go test -run TestEveryKindRehearsal .` records each
kind's calls and the fake's answers to `hack/fake/cassettes/<Kind>.<group>.json`;
every other `go test` replays them as strictly as a replay of a live run, so
a change in what the runtime sends fails the kind and names the call.
Recording again changes nothing unless a call or an answer did: the run's
name is fixed, the clock only moves when it would have slept, and every
cassette says it was recorded on 2000-01-01 against the fake.

They are a full request and response example of every kind to build
against, in the live cassettes' format and with the same placeholders. The
fake's answers carry what was sent plus the kind's `serverFields`: the
fields the vendor's OpenAPI spec says a read answers and no request sends
(timestamps, URLs, states), with sample values. What they are not:

| | |
| --- | --- |
| not the vendor's answers | every cassette's `target` is the fake; where a kind has a live cassette, that one shows what the API really answered |
| not readiness | a rehearsal writes no records; a kind is `live` only from a live run |
| setup calls are stand-ins | `before` and `after` calls make what a real account needs first (a project, a user, a mount); the fake needs none of it, so a stand-in answers them: a write echoes its body with an ID, a read answers an ID, a delete 204, each marked `"_stub"`; a value they would `save` that the stand-in lacks is `rehearsal-<name>` |

Credentials are stand-ins (`rehearsal-<key>`, `rehearsal-env-<VAR>`),
scrubbed to their placeholders like a live run's.

## Names, cleanup, sweep

Everything a run makes is named after the run: `{{name}}` is
`hfl-live-<stamp>-<hex>-NN`, `{{name_}}` the same with underscores for APIs
that refuse hyphens (Snowflake). A failed step still deletes what it made (or
puts settings back). Each run starts by sweeping leftovers older than two
hours, found by the `sweep:` entries; `TestLiveSweep` sweeps them all.

## live.yaml

```yaml
secret: krm-live-gitlab
target: GitLab                       # recorded with each result
version: {path: /version, field: version}   # appended to target
before:                              # once, before any kind; after: at the end
- {method: POST, path: /groups, body: {name: "{{run}}", path: "{{run}}"}, save: {group: id}}
after:
- {method: DELETE, path: "/groups/{{var:group}}", ok: [404]}
sweep:
- {list: "/groups?search=hfl-live-", name: path, id: id, delete: "/groups/{id}"}
kinds:
- kind: GroupLabel
  replaceSample: true                # else spec is merged over the sample
  spec: {groupID: "{{var:group}}", name: "{{name}}", color: "#FF0000"}
  change: {color: "#00FF00"}         # the update step
  drift: "#0000FF"                   # what the drift step writes
- kind: SysConfigCors
  stays: a read still answers after the delete   # gone is not checked
- kind: Plugin
  skip: needs a plugin binary        # listed, not run
- kind: Campaign
  fakeOnly: Okta Identity Governance, sold only by contract   # never run live
```

`skip` is a prerequisite we lack for now: an account, a second user, a
self-serve plan we could buy for a month. `fakeOnly` is for good: the real
feature is sold only by contract (an Enterprise plan, a sales-only add-on),
so no test account will have it and the fake API is the kind's only test.
Every live run skips it ("fake only: ..."); the rehearsal still runs it on
the fake. The generated report shows it as `fake only`, and the MCP
server's `describe_kind` sets `live.fakeOnly`. A kind is never both.

A kind on another of the vendor's APIs, or needing other credentials, runs
on a connection of its own, filled from the same secret:

```yaml
connections:
  oncall:
    baseURL: "{{cred:oncallURL}}"          # "" or absent: the run's base URL
    creds: {token: "{{cred:oncallToken}}", scheme: none}
kinds:
- kind: OncallIntegration
  connection: oncall                     # skips, naming oncallURL and oncallToken, until the secret has them
```

A replay or a rehearsal runs it on the run's base URL with stand-in
credentials, so it needs no keys.

### Identities

The repositories are public, so a cassette must not name the test account
or a person: the authenticated user's login, an account's subdomain. They
are plain words, which the secret rules keep and compare, so live.yaml
names them:

```yaml
redactVars: [me]                 # saved vars that hold one
redactFields: [added_by]         # JSON fields whose values are one, anywhere
redactSubdomains: [workers.dev]  # the label before workers.dev in any host: the account's
before:
- {method: GET, path: /user, save: {me: login}}
```

A var saved from a `login`, `username`, `handle` or `email` field (the last
part of its path) is one without being named. Each value found (four
characters or more) is redacted wherever the run recorded it, from the
moment it is known, as a whole word, in every encoding, in every cassette,
log line and record, and in the cassettes written before it was found at
the run's end. Where one is a whole segment of a path (eight characters or
more) it is an ID, written `{{id:N}}` there and everywhere else in that
cassette (its saved var too), so a replay sends a stand-in where the
recording sent the login and matches it exactly; elsewhere it is
`{{redacted}}`, which a replay matches with whatever it sends. A rehearsal
finds none: the fake's values name no one. For cassettes committed before
live.yaml named them, run `cmd/rescrub` (above, "Replay").

Per kind also: `before`/`after` calls, `noDrift: <reason>`, `busyWait` (how long
the update and drift steps retry a 409 from a resource still being made), `noUpdate:
<reason>` (neither the update nor the drift step runs), `driftFull: true`
(the drift step sends the whole object as read, plus the spec's fields a
read never returns, such as a secret; for an update that replaces it), and
`deleteWait`. Calls take `headers` (Atlas's versioned media types), `ok`
(statuses that count as success) and `save` (response fields, `users/0/id`,
kept as `{{var:NAME}}`). A `sweep:` entry may name `before`, a path POSTed
with `{id}` filled before the delete (Okta's `.../lifecycle/deactivate`). Templates: `{{run}}`, `{{run_}}`, `{{name}}`,
`{{name_}}`, `{{cred:KEY}}`, `{{env:VAR}}`, `{{var:NAME}}` (a string that is
only that keeps the saved value's type), `{{date:N}}` (N days from today).

A GraphQL kind (README, "GraphQL kinds") runs the same steps: its drift
step sends the update mutation with the one changed field, and its
variables leave the others out. A mutation that requires them (GitLab's
saved replies take `name: String!` and `content: String!`) is refused
with a top-level error; `driftFull: true` sends the whole object, as read
and as the spec gives it. Cassettes record the endpoint's path
(`/api/graphql`) and each posted document and its variables.

## Records and readiness

`hack/live/records.json` holds one record per kind, keyed `Kind.group`:
`result` (`pass` or `fail`), the `steps` it got through, the `reason` it
failed, the `run`, `date`, `target`, and `lastPass`. The generator sets
`readiness: live` (types.yaml, the CRD's `hfl.dev/readiness` annotation and
the report) for each kind whose record passed, and `fake` otherwise; a later
failure takes it back to `fake` and keeps `lastPass`.

## When the API does not answer as sent

What the Vault run found, and where each fix went (never `types.yaml` or
`config/crds` by hand):

| Symptom | Cause | Fix |
| --- | --- | --- |
| read right after create answers `values = null` | the read returns only the first value unless asked | `overrides.readQuery` (descriptor `read.query`) |
| create fails `missing data fields` | a body that is any JSON object had no CRD field at all | the generator keeps unknown spec fields for such a body |
| delete never shows `gone` | a missing PKI issuer or key is an HTTP 500 | `rest.Vendor.NotFound` in vendor.go |
| a read answers nothing; a write runs the plugin binary | the kind used a deprecated untyped path | `overrides.map` to the typed path; the generator no longer takes a sibling item for a collection |
| a kind is `immutable` but the API updates it | an update is a POST to the item's own path | the generator, under `postUpdates` |
| the drift step's write is refused (`secret_key is empty`) | the API wants fields a read never returns | `driftFull` sends the spec's too |
| drift on a field is never seen | the read renames it (Vault's `method_name` reads as `name`) | not fixed: `noDrift` with the reason |

What the Grafana run found (Grafana OSS 13.3.1 in Docker, `task live:grafana`
in grafana-krm-extensions):

| Symptom | Cause | Fix |
| --- | --- | --- |
| create fails `response has no "id"` | a create answers `{"teamId": ..}`, `{"orgId": ..}` | `overrides.idFrom` |
| `grafana_user` is the caller's own profile (`/user`), which cannot be created or deleted | the user Terraform manages is made on `/admin/users`, read on `/users/{id}`, deleted on `/admin/users/{id}` | `overrides.map`, `creates` and `deletes` (the generator and fake take a delete path) |
| a read right after the write reports drift in `settings` | the read masks every secret inside it as `*********` | `overrides.writeOnly` with a dotted path; drift checks skip nested write-only fields |
| create answers 403 `orgs:create`, `users:create`, `settings:read` | a service-account token is never a server administrator | the Connection's `token` may be `user:password` (basic auth), as Terraform's `auth` |


What the GitLab run found that needed the runtime (GitLab CE in Docker,
`task live:gitlab` in gitlab-krm-extensions):

| Symptom | Cause | Fix |
| --- | --- | --- |
| delete answers 403 | an environment must be stopped before it can be deleted | `overrides.beforeDelete` (descriptor `beforeDelete`): `POST .../environments/{environment_id}/stop` first |
| read right after create reports drift in `scopes` | an application's scopes are written as a space-separated string and read back as an array | `overrides.compare: {scopes: splitSpace}` (descriptor `compare`) |

What the GitHub run found that needed the runtime (a free organization on
api.github.com, `hack/live/live.yaml` in github-krm-extensions):

| Symptom | Cause | Fix |
| --- | --- | --- |
| read right after create creates it again (`already_exists`, `key is already in use`) | a large numeric ID (a release's 270123456) decoded as a float64 and was written `2.70123456e+08` into the read path, a 404 | `reconciler.IDString` writes whole numbers as integers; the fake's IDs start at 270000000 so every vendor's fake test catches it |
| a secret is updated on every read | the hash of what was sent was taken after the hook that seals the value, with a fresh nonce each time | the hash is taken before the hooks |
| read right after create reports drift in `enforce_admins` | branch protection takes booleans and reads them back as `{"url": ..., "enabled": true}` | `overrides.compare: {enforce_admins: enabledObject}` |
| an organization owner's team role reads back as `maintainer`, whatever is sent | GitHub makes an owner a maintainer of every team | not fixed: `noUpdate` with the reason (a second member would be needed) |

What the Snowflake run found that needed the runtime (a trial account,
`KRM_LIVE=1 go test -tags live -run TestEveryKindLive .` in
snowflake-krm-extensions):

| Symptom | Cause | Fix |
| --- | --- | --- |
| a function or procedure's create fails `response has no "id"` | the API names it by its signature, `NAME(TYPE, ...)` (`{nameWithArgs}`), and the create answers only a status message | `overrides.idFrom` may be a template over the spec: `"{name}({arguments[].datatype})"` (descriptor `id.from`, `ID.Compose`) |
| a read right after creating a view reports drift in `query` | the read answers the whole `CREATE VIEW ... AS <query>` statement | `overrides.compare: {query: endsWith}` (descriptor `compare`) |
| the live harness names the wrong field (`name = "HFL_..."`, sent `"hfl_..."`) when a read differs | the harness's explanation did not fold case-insensitive fields as the reconciler does | `reconciler.Normalize` now folds `caseInsensitive` fields too; the harness and the reconciler share it |

What the PagerDuty run found (developer account, `KRM_LIVE=1 go test
-tags live -run TestEveryKindLive .` in pagerduty-krm-extensions):

| Symptom | Cause | Fix |
| --- | --- | --- |
| `POST /tags` answers 400 `Incorrect parameters` | the spec's `allOf` inherits `type` read-only from a shared base, and the next part requires it (`"type": "tag"`) | the generator: a field one `allOf` part requires on write is writable; a one-value enum is defaulted |
| update answers 400 `field.name: doesn't match any valid key` | a custom field's `name`, `data_type`, `field_type` are set at create only | vendor.yaml `createOnlyFromSpec: true` (descriptor `createOnly`): fields the update's body schema lacks are left out of updates; a change to one is reported, not sent |
| read right after create reports drift in `start_time` | times read back in the account's zone (`-06:00`) | the reconciler: two RFC 3339 times naming the same instant are the same |
| an update adds a schedule layer each time; read back reports drift in `schedule_layers` | layers without their IDs are new layers; layers read back highest priority first | a vendor hook fills layer IDs (pagerduty-krm-extensions `hooks.go`); `overrides.compare: {schedule_layers: unordered}` |
| restoring a settings kind after the run answers 400 | the harness sent the old values without the body envelope | `kindtest`: the restore wraps the body as the drift step does |

What the MongoDB Atlas run found (a free organization;
13 of 31 kinds live, mongodbatlas-krm-extensions):

| Symptom | Cause | Fix |
| --- | --- | --- |
| the sweep's list answers 406 `INVALID_VERSION_DATE` | Atlas wants a versioned media type on every call; sweep calls have no headers | `rest.Vendor.Headers` (Accept `application/vnd.atlas.2025-03-12+json`): Atlas answers a date with the latest version on or before it |
| an update answers 400 `INVALID_ATTRIBUTE` | some fields are taken on create only (a service account's `secretExpiresAfterHours`, an invitation's `username`) | a vendor hook drops them once the object exists |
| delete never shows `gone` | a deleted API key reads as 400 `API_KEY_NOT_FOUND` | `rest.Vendor.NotFound` |
| read right after create reports drift in `roles` | an API key's roles are written as names and read back as `[{orgId, roleName}]` | not fixed in the runtime: `overrides.writeOnly` (a compare rule could map them) |
| putting settings back answers 400 `INVALID_ATTRIBUTE` (`cloud`) | the restore sent path parameters the read also returns | the harness restores body fields only |

What the Okta run found that needed the runtime (an Integrator org,
`hack/live/live.yaml` in okta-krm-extensions):

| Symptom | Cause | Fix |
| --- | --- | --- |
| a user made by a test is activated, and Okta emails it an activation link | `POST /api/v1/users` activates unless the query says `activate=false`; the spec names the query, not the choice | `overrides.createQuery` (descriptor `create.query`): the create is sent with it |
| the sweep cannot delete a leftover app, network zone, hook, IdP or authorization server | Okta deletes those only once deactivated | a `sweep:` entry's `before` (and `overrides.beforeDelete` for the kinds) |
| an update answers 400 E0000003 (`not well-formed`) for a custom role, resource set, email domain | `permissions`, `resources`, `brandId` are taken only by the create; a read never returns them, so `createOnlyFromSpec` (which leaves write-only fields out) does not find them | `overrides.createOnly` names them |

What the Sentry run found (sentry.io, a Developer-plan organization,
`hack/live/live.yaml` in sentry-krm-extensions):

| Symptom | Cause | Fix |
| --- | --- | --- |
| read right after create reports drift in `project` | a cron monitor takes its project's slug and reads back the whole project object | `overrides.compare: {project: slugObject}` (descriptor `compare`) |
| delete never shows `gone` (still readable after ten minutes) | a deleted cron monitor reads by its ID until a deletion task runs; its slug is freed at once | `overrides.idFrom: slug` (no runtime change) |
| drift on an organization setting is never seen | the spec's organization response leaves out 24 of the fields an update takes, so the generator made them write-only; the API does return them | `overrides.readable` lists them: they are compared as usual |

What the Datadog run found (a trial organization on us5; datadog-krm-extensions,
`KRM_LIVE=1 go test -tags live -run TestEveryKindLive -v .`):

| Symptom | Cause | Fix |
| --- | --- | --- |
| update answers 400 `Required parameter 'id' is missing`, or `the resource ID in the URL path does not match the resource ID specified in the request body` | JSON:API: an update names the item in `data.id` as well as in its path | `envelope: {updateID: id}` in vendor.yaml (descriptor `updateID`): the update's body carries the ID, where the update's schema has the field |
| drift step's write answers 400 `got type "" expected one of "roles"` | a JSON:API write must name the type | `driftFull` in live.yaml, with `change:`/`drift:` giving the whole `attributes` object |
| create fails `response has no "id"` | a v1 create answers its key elsewhere (a webhook's `name`, a private location's `private_location/id`, a secure embed's `attributes/token`) | `overrides.idFrom` |

What the Confluent Cloud run found (a trial organization,
`hack/live/live.yaml` in confluent-krm-extensions):

| Symptom | Cause | Fix |
| --- | --- | --- |
| a read or delete answers 400 `Environment ID not specified` | clusters, Flink pools, plugins, provider integrations and most networking are read and deleted only with `?environment=<id>` | vendor.yaml `queryFromSpec: [environment]`: a required query parameter so named is filled from the spec (`{environment.id}`; descriptor `query` templates, `Kind.FillQuery`) |
| an update answers 422 `Field is immutable` | a PATCH naming an `x-immutable` field is refused, even with the value it has (a subscription's `notification_type`, a cluster's `cloud`) | vendor.yaml `immutableFromSpec: true`: such fields are `createOnly` |
| an update right after create answers 409 | a Flink compute pool refuses changes until it is provisioned | live.yaml `busyWait`: the update and drift steps retry a 409 that long |
| a cluster's update answers 422 `Name cannot be updated together with cluster type` | the reconciler sends the whole spec, `config` included, with the new `display_name` | not fixed: `noUpdate:` with the reason |
| a deleted API key is never `gone` | a key that does not exist answers 403 `forbidden_access`, as one the caller may not see | not fixed: `stays:` with the reason |
| a CA is refused, `Certificate chain was parsed as empty list` | `certificate_chain` is PEM text, not base64 | live.yaml |

What the Cloudflare run found that needed the runtime (a free account,
`hack/live/live.yaml` in cloudflare-krm-extensions):

| Symptom | Cause | Fix |
| --- | --- | --- |
| a read right after a create or update answers 404, the reconciler creates the object again: a second Workers route, or 409 "a Worker with the name ... already exists" | Cloudflare answers some reads from a replica that has not seen the write yet | `reconciler.NotFoundRecheck`: a CRUD object a read misses is read once more, 2 s later, before it is created again |
