# GitHub: a test organization and a classic token

**Secret:** `krm-live-github` in the project `$KRM_LIVE_PROJECT` names
**JSON:** `{"token": "ghp_...", "org": "<test org login>"}`
**Time:** about 10 minutes. **Cost:** free.

The live tests make one private repository in the test organization per
run (`hfl-live-<stamp>-<hex>`), put labels, milestones, topics and Actions
settings on it, add an organization Actions variable, and delete it all.
Use a new organization that holds nothing else, never `highfivelabs`.

## 1. Create the organization

1. Sign in to GitHub as the builder. **Human** if the
   browser is not signed in, or GitHub asks for a 2FA code.
2. Open <https://github.com/account/organizations/new?plan=free>
   (or <https://github.com/organizations/plan> and choose **Free**).
3. **Organization name:** a free name of your choosing, such as
   `<you>-krm-live`. Note it: it is the `org` value.
4. **Contact email:** the builder's work address.
5. **This organization belongs to:** My personal account.
6. Tick the terms box. **Human** if a CAPTCHA ("Verify your account")
   appears. Click **Next**.
7. On "Add organization members", click **Skip this step**. Answer or skip
   the survey.
8. Open <https://github.com/organizations/<org>/settings/actions>
   and check **Actions permissions** is "Allow all actions and reusable
   workflows" (the default). The tests change one repository's Actions
   permissions, which needs Actions enabled at the organization.

## 2. Create the token

A classic personal access token, because fine-grained tokens cannot yet do
every call the kinds make on an organization.

1. Open <https://github.com/settings/tokens/new> (Settings > Developer
   settings > Personal access tokens > Tokens (classic) > Generate new
   token (classic)). **Human** if GitHub asks for sudo mode (password or
   passkey).
2. **Note:** `krm-live-github (<org>)`.
3. **Expiration:** 90 days.
4. Scopes, tick exactly these:
   - `repo` (the whole group)
   - `admin:org` (the whole group)
   - `delete_repo`
5. Click **Generate token**. Copy the `ghp_...` value now; GitHub shows it
   once.

**Hand to the human here:** the token goes straight into Secret Manager
(next step), not into the agent's transcript.

## 3. Store it

Copy this JSON with the real values to the clipboard:

```json
{"token":"ghp_...","org":"<org>"}
```

```sh
pbpaste | gcloud secrets create krm-live-github --project=$KRM_LIVE_PROJECT \
  --replication-policy=automatic --labels=purpose=krm-live,vendor=github --data-file=-
gcloud secrets add-iam-policy-binding krm-live-github --project=$KRM_LIVE_PROJECT \
  --member=serviceAccount:<runner-service-account> \
  --role=roles/secretmanager.secretAccessor
```

Check (prints only key names): `gcloud secrets versions access latest
--secret=krm-live-github --project=$KRM_LIVE_PROJECT | python3 -c 'import
json,sys; print(sorted(json.load(sys.stdin)))'` should print `['org',
'token']`.

## Done when

- The organization exists and the builder owns it.
- `krm-live-github` exists with keys `org` and `token`.
- A calendar reminder is set for 85 days out to rotate the token
  (`gcloud secrets versions add krm-live-github ...`).
