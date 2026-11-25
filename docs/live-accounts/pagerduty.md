# PagerDuty: a developer account and a REST API key

**Secret:** `krm-live-pagerduty` in the project `$KRM_LIVE_PROJECT` names
**JSON:** `{"token": "<REST API key>", "subdomain": "<subdomain>"}` (`subdomain` is only scrubbed from recordings)
**Time:** about 10 minutes. **Cost:** free (developer account).

The live tests make a tag, a team, a business service, an escalation policy
and a service named `hfl-live-...`, and delete them. The escalation policy
targets the account's first user (the owner), so no other user is needed.

## 1. Create the developer account

1. Open <https://developer.pagerduty.com/sign-up/> (or
   <https://developer.pagerduty.com> > **Sign up for a developer account**).
   This is the free developer plan with the full REST API, not the 14-day
   trial at pagerduty.com/sign-up.
2. Fill in: first and last name (the builder's), email
   (the builder's work address), company `High Five Labs`.
3. **Subdomain:** a free one of your choosing (the account is then
   `https://<subdomain>.pagerduty.com`; it is the `subdomain` value). Pick another one if it
   is taken.
4. Region: **US**.
5. **Human:** CAPTCHA if shown, then the verification email ("Activate
   your account"). Set the password from the link.
6. Skip the onboarding wizard (**Skip** / close). Do not add phone
   numbers or invite users.

## 2. Create the API key

1. Sign in at `https://<subdomain>.pagerduty.com`.
2. **Integrations > API Access Keys**
   (`https://<subdomain>.pagerduty.com/api_keys`).
3. Click **Create New API Key**.
4. **Description:** `krm-live-pagerduty`.
5. Leave **Read-only API Key** unticked: it must be a General Access key
   with read and write.
6. Click **Create Key**. Copy the key now; PagerDuty shows it once.

**Hand to the human here:** the key goes straight into Secret Manager.

## 3. Store it

Copy to the clipboard:

```json
{"token":"<the key>","subdomain":"<subdomain>"}
```

```sh
pbpaste | gcloud secrets create krm-live-pagerduty --project=$KRM_LIVE_PROJECT \
  --replication-policy=automatic --labels=purpose=krm-live,vendor=pagerduty --data-file=-
gcloud secrets add-iam-policy-binding krm-live-pagerduty --project=$KRM_LIVE_PROJECT \
  --member=serviceAccount:<runner-service-account> \
  --role=roles/secretmanager.secretAccessor
```

Check: `gcloud secrets versions access latest --secret=krm-live-pagerduty
--project=$KRM_LIVE_PROJECT | python3 -c 'import json,sys;
print(sorted(json.load(sys.stdin)))'` prints `['token']`.

## Done when

- The developer account exists with one user (the builder).
- `krm-live-pagerduty` exists with key `token`.
