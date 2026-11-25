# Okta: an Integrator Free Plan org and an API token

**Secret:** `krm-live-okta` in the project `$KRM_LIVE_PROJECT` names
**JSON:** `{"apiToken": "00...", "baseURL": "https://integrator-NNNNNNN.okta.com"}`
**Time:** about 10 minutes. **Cost:** free; the Integrator Free Plan does
not expire.

The live tests make a trusted origin, a user type, a hook key, a group and
a network zone named `hfl-live-...`, and delete them.

## 1. Create the org

1. Open <https://developer.okta.com/signup/> and choose **Sign up for
   Integrator Free Plan** (not the Workforce Identity trial, which
   expires).
2. Fill in: first and last name (the builder's), work email
   (the builder's work address), country.
3. **Human:** CAPTCHA if shown, then the activation email ("Activate your
   Okta account"). Set the password, and enrol the MFA factor Okta asks for
   (Okta Verify or a passkey).
4. Note the org URL from the activation email or the browser's address
   bar. The admin console is `https://integrator-NNNNNNN-admin.okta.com`;
   **`baseURL` is the same without `-admin`**:
   `https://integrator-NNNNNNN.okta.com`.

The signup user is a super administrator, which the API token needs (the
kinds create user types and network zones).

## 2. Create the API token

1. In the admin console: **Security > API**, tab **Tokens**
   (`https://integrator-NNNNNNN-admin.okta.com/admin/access/api/tokens`).
2. Click **Create token**.
3. **Name:** `krm-live-okta`.
4. **API calls made with this token must originate from:** Any IP. (Runs
   come from offload VMs with changing addresses.)
5. Click **Create token**. Copy the token value (starts `00`) now; Okta
   shows it once. **Human** if Okta asks to re-verify MFA.

The token acts as the super administrator who made it. It expires after 30
days without use; a live run uses it, and a new one is made the same way.

**Hand to the human here:** the token goes straight into Secret Manager.

## 3. Store it

Copy to the clipboard:

```json
{"apiToken":"00...","baseURL":"https://integrator-NNNNNNN.okta.com"}
```

```sh
pbpaste | gcloud secrets create krm-live-okta --project=$KRM_LIVE_PROJECT \
  --replication-policy=automatic --labels=purpose=krm-live,vendor=okta --data-file=-
gcloud secrets add-iam-policy-binding krm-live-okta --project=$KRM_LIVE_PROJECT \
  --member=serviceAccount:<runner-service-account> \
  --role=roles/secretmanager.secretAccessor
```

Check: `gcloud secrets versions access latest --secret=krm-live-okta
--project=$KRM_LIVE_PROJECT | python3 -c 'import json,sys;
print(sorted(json.load(sys.stdin)))'` prints `['apiToken', 'baseURL']`.

## Done when

- The Integrator org exists and the builder is its super administrator.
- `krm-live-okta` exists with keys `apiToken` and `baseURL` (no `-admin`,
  no trailing slash).
