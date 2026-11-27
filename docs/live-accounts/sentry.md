# Sentry: a test organization and an internal integration token

**Secret:** `krm-live-sentry` in the project `$KRM_LIVE_PROJECT` names
**JSON:** `{"token": "<internal integration token>", "org": "<org slug>"}`,
plus `"baseURL": "https://de.sentry.io/api/0"` for an EU organization
**Time:** about 10 minutes. **Cost:** the Developer plan, $0 (one user).

The live tests make one team and one project in the test organization per
run (`hfl-live-<stamp>-<hex>`), work inside them, flip one Slack-threading
setting and put it back, and delete it all
(`sentry-krm-extensions/hack/live/live.yaml`). Use a new organization that
holds nothing else. The sweep deletes leftovers named `hfl-live-...`; that
prefix is cleanup hygiene, not what keeps other data safe.

## 1. Create the organization

1. Sign in to <https://sentry.io/> as the builder. **Human** if the
   browser is not signed in, or Sentry asks for a 2FA code.
2. Open the organization menu (top left) > **Create a new organization**.
   If Sentry asks for a new login instead, sign up with the builder's work
   address. **Human:** email verification.
3. **Organization name:** a free name of your choosing. Note its slug
   (the `<slug>` in `https://<slug>.sentry.io/`, and **Settings > General
   Settings > Organization Slug**): it is the `org` value.
4. **Data storage location:** note the region you pick. For the EU
   (Germany), the JSON needs `"baseURL": "https://de.sentry.io/api/0"`;
   for the US, leave `baseURL` out.
5. **Settings > Subscription:** stay on **Developer**. If a Business trial
   started, end it (or let it lapse: it falls back to Developer, with no
   card on file).

## 2. Create the token

An internal integration's token, because it belongs to this organization
and reaches nothing else. An organization auth token (`sntrys_...`)
cannot create the team and project each run makes.

1. In the new organization: **Settings > Developer Settings > Custom
   Integrations > Create New Integration > Internal Integration**.
2. **Name:** `krm-live-tests`.
3. **Webhook URL:** leave empty. **Alert Rule Action:** off.
4. **Permissions**, set exactly these:

   | Resource | Permission |
   | --- | --- |
   | Project | Admin |
   | Team | Admin |
   | Release | No Access |
   | Issue & Event | Read |
   | Organization | Read & Write |
   | Member | Read |
   | Alerts | Read & Write |

5. Leave every webhook event unticked. Click **Save Changes**.
6. On the integration's page, under **Tokens**, copy the token.

**Hand to the human here:** the token goes straight into Secret Manager
(next step), not into the agent's transcript.

## 3. Store it

Copy this JSON with the real values to the clipboard (add
`"baseURL":"https://de.sentry.io/api/0"` for an EU organization):

```json
{"token":"...","org":"<slug>"}
```

Then, in a krm-rest-runtime checkout:

```sh
pbpaste | task live:secret VENDOR=sentry
```

It checks the JSON, adds a version to `krm-live-sentry` (or creates the
secret) and prints only the key names: `baseURL` (if set), `org`, `token`.
A new secret also needs the runner's read access
([README.md](README.md), "Storing the secret").

## 4. Retire the old credentials

1. Revoke any older Sentry token the live tests used: delete it on the
   page it was made on (an internal integration's **Tokens**, or
   **User Settings > Personal Tokens**).
2. Disable the secret's older versions, so nothing can read them:

   ```sh
   gcloud secrets versions list krm-live-sentry --project=$KRM_LIVE_PROJECT
   gcloud secrets versions disable <version> --secret=krm-live-sentry --project=$KRM_LIVE_PROJECT
   ```

   Repeat for every version but the newest.

## 5. Run it

In sentry-krm-extensions, on an offload VM, at a commit an operator has
approved:

```sh
KRM_LIVE=1 KRM_LIVE_APPROVE=<that commit> go test -tags live -run TestEveryKindLive -v .
```

`KRM_LIVE_APPROVE` is the provenance gate: HEAD's full commit, which the
operator read and named; without it the run is refused before any
credential is read ([../live.md](../live.md)).

## 6. If the first setup call answers 403

The run's first calls make its team and its project
(`before:` in live.yaml). A 403 there means the integration's permissions
are too narrow: raise **Team** or **Project** (whichever call failed) to
**Admin**, **Save Changes**, and run again.

## Done when

- The organization exists, holds nothing else, and the builder owns it.
- `krm-live-sentry`'s newest version has keys `org` and `token` (and
  `baseURL` for the EU), and its older versions are disabled.
- No older Sentry token for the live tests is still valid.
