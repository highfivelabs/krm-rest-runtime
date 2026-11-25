# Live-test vendor accounts

One guide per vendor whose live tests need a real account
([`../live.md`](../live.md) explains the tests). Each guide says what to
sign up for, what to click, which token to make, and how to store it in
Secret Manager. It is written so an in-browser agent can do most of it.
The steps marked **Human** are for a person: email verification, CAPTCHA,
payment details, MFA, and pasting the token into a terminal.

| Vendor | Guide | Secret (project `$KRM_LIVE_PROJECT`) | JSON keys | Cost |
| --- | --- | --- | --- | --- |
| GitHub | [github.md](github.md) | `krm-live-github` | `token`, `org` | free organization |
| PagerDuty | [pagerduty.md](pagerduty.md) | `krm-live-pagerduty` | `token`, `subdomain` | free developer account |
| Okta | [okta.md](okta.md) | `krm-live-okta` | `apiToken`, `baseURL` | Integrator Free Plan; deactivated after 180 days without a sign-in |
| MongoDB Atlas | [mongodbatlas.md](mongodbatlas.md) | `krm-live-mongodbatlas` | `clientId`, `clientSecret`, `orgId`, `orgName` | free organization; pay as you go for an M10 ([billing.md](billing.md)) |
| Snowflake | [snowflake.md](snowflake.md) | `krm-live-snowflake` | `token`, `baseURL`, `tokenType` | On Demand after the trial ([billing.md](billing.md)) |
| Cloudflare | [cloudflare-zero-trust.md](cloudflare-zero-trust.md) | `krm-live-cloudflare` | `apiToken`, `accountId`, `zoneId`, `zoneName`, `teamName` | Free plan and Zero Trust Free (card on file, $0); a zone of your own (its domain is `zoneName`) |
| Datadog | none (made by the builder) | `krm-live-datadog` | `apiKey`, `appKey`, `site` | the Free plan after the 14-day trial |
| Sentry | [sentry.md](sentry.md) | `krm-live-sentry` | `token`, `org` | a free org of its own (Developer plan) |
| Confluent | none (made by the builder) | `krm-live-confluent` | see the vendor's live.yaml | pay as you go after the trial credits ([billing.md](billing.md)) |
| Grafana Cloud | [grafana-cloud.md](grafana-cloud.md) | `krm-live-grafana-cloud` | `token`, `baseURL`, `rootURL` | Grafana Cloud Free |
| HCP Vault | [billing.md](billing.md) | `krm-live-hcp` | `clientId`, `clientSecret`, `projectId` | pay as you go, a Development cluster per run |

Vault, GitLab and Grafana need no account for most kinds (sandboxes in
containers). Their Enterprise or Cloud kinds use Grafana Cloud Free and an
HCP Vault cluster made per run; GitLab's Premium and Ultimate kinds use a
free 30-day trial license, taken only when such a kind is added.

## What stays on the fake API

CI never calls a real API: `hack/ci.sh` runs every kind against the fake
API and replays the recorded live runs. Live runs are for a kind that is
new or changed, about twice a month per vendor. A kind whose real feature
is sold only by contract (Cloudflare's Enterprise products, Okta Identity
Governance, PagerDuty Automation Actions) is marked `fakeOnly` in the
vendor's live.yaml ([../live.md](../live.md)) and never runs live.

## Rules for every account

- Sign up with the builder's work address.
  Use a password manager entry named `krm-live-<vendor>`.
- A card goes only on the pay-as-you-go accounts in
  [billing.md](billing.md) (Snowflake, MongoDB Atlas, Confluent, HCP),
  and Cloudflare's, which Zero Trust Free asks for; each gets a spending
  alert. A person enters it, never an agent. Anywhere else, if a page
  insists on a card, stop. Nothing here buys a plan with a monthly floor
  without the builder's say (decided 2026-11-23).
- Never paste a token into a chat, an issue, a pull request or a file in a
  repository. It goes from the vendor's page into Secret Manager, nowhere
  else.
- Name anything you create by hand `hfl-...` but never `hfl-live-...`:
  the live tests' sweep deletes `hfl-live-*` (and `HFL_LIVE_*`) leftovers.

## Storing the secret

Write the JSON in a scratch editor, copy it, then (on the laptop, logged in
to gcloud as the builder):

```sh
pbpaste | gcloud secrets create krm-live-<vendor> --project=$KRM_LIVE_PROJECT \
  --replication-policy=automatic --labels=purpose=krm-live,vendor=<vendor> --data-file=-
```

`pbpaste` keeps the token out of shell history. The same thing with a
check that the JSON parses and has no empty values (it adds a version if
the secret exists):

```sh
cd ~/Development/git/github.com/highfivelabs/krm-work/krm-rest-runtime
pbpaste | task live:secret VENDOR=<vendor>
```

To replace a token later: `pbpaste | gcloud secrets versions add
krm-live-<vendor> --project=$KRM_LIVE_PROJECT --data-file=-`.

Check it, printing only the key names:

```sh
gcloud secrets versions access latest --secret=krm-live-<vendor> --project=$KRM_LIVE_PROJECT \
  | python3 -c 'import json,sys; print(sorted(json.load(sys.stdin)))'
```

Then let the offload runner read it (per secret, not the project):

```sh
gcloud secrets add-iam-policy-binding krm-live-<vendor> --project=$KRM_LIVE_PROJECT \
  --member=serviceAccount:<runner-service-account> \
  --role=roles/secretmanager.secretAccessor
```

Clear the clipboard afterwards (`pbcopy </dev/null`).

## Then

Tell an agent "run the <vendor> live tests on an offload VM". It runs
`KRM_LIVE=1 go test -tags live -run TestEveryKindLive -v .` in the vendor
repository, commits `hack/live/records.json` and regenerates readiness.

## Why these guides exist

LIVE1-1 (builder, 2026-11-15), verbatim: "it looks like this requires me to
open accounts on these services. that will take some time. please build a
guide for each that i can follow or do as much as you can in browser
sessions, orepare linear tasks that outline it for each and set that due end
of day today so an in browser agent can do it mostly for me on each"

The Linear issues are HFL-603 (GitHub), HFL-604 (PagerDuty), HFL-605 (Okta),
HFL-606 (MongoDB Atlas) and HFL-607 (Snowflake).

## Cloudflare, Datadog and Sentry (VENDKEYS)

VENDKEYS (Open decisions page, 2026-11-16), the builder, verbatim:
"establish new accounts." Then, on how it was done: "I reused our Sentry
account but all others were created afresh". The secrets
`krm-live-cloudflare`, `krm-live-datadog` and `krm-live-sentry` (and
`krm-live-confluent`) were in Secret Manager by 07:12 UTC that day, so
these three have no sign-up guide. Linear: HFL-609 (Datadog), HFL-610
(Cloudflare), HFL-640 (Sentry); wind-down HFL-642 and its children.

**Sentry is the company's real account, not a test one.** Its live tests
must only ever touch objects named `hfl-live-...`: the run's own objects
and the sweep's deletions (`sentry-krm-extensions/hack/live/live.yaml`,
`sweep:` lists teams, projects and monitors by slug and deletes only the
`hfl-live-` ones). Never run the sweep with a broader prefix there, and
never name anything by hand `hfl-live-...` in that org.
