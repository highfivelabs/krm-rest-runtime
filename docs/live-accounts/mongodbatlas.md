# MongoDB Atlas: a free organization and a service account

**Secret:** `krm-live-mongodbatlas` in the project `$KRM_LIVE_PROJECT` names
**JSON:** `{"clientId": "mdb_sa_id_...", "clientSecret": "mdb_sa_sk_...", "orgId": "<24 hex>", "orgName": "<org name>"}` (`orgName` is only scrubbed from recordings)
**Time:** about 15 minutes. **Cost:** free. No cluster is created: the
listed kinds need none.

Each run makes one project (`hfl-live-<stamp>-<hex>`) in the organization,
sets project settings, makes a project service account, an organization
service account and an API key, and deletes them all.

## 1. Create the organization

1. Open <https://www.mongodb.com/cloud/atlas/register>.
2. Sign up with the builder's work address (or **Sign up with
   Google** on that address). **Human:** CAPTCHA and the verification
   email.
3. The onboarding asks for a goal, language and a first cluster. Answer
   anything, and on the "Deploy your cluster" page click **I'll do this
   later** (or close it). Do not create a cluster and do not enter a card.
4. Atlas makes an organization and a first project. Rename the
   organization: **Organizations** menu (top left) > the org >
   **Organization Settings** > name it (for example `<you>-krm-live`; it is the `orgName` value) > Save. Leave the
   default project alone; the tests make their own.
5. Copy the **Organization ID** from Organization Settings (24 hex
   characters; also in the URL
   `https://cloud.mongodb.com/v2#/org/<orgId>/settings/general`). That is
   `orgId`.

## 2. Let the API be called from any address

Runs come from offload VMs with changing addresses. In **Organization
Settings**, set **Require IP Access List for the Atlas Administration API**
to **Off**. With it off and the service account's access list empty,
calls are accepted from any address. This organization holds nothing but
test projects, so that is acceptable here.

## 3. Create the service account

1. **Organization Access Manager** (the org's **Access Manager** in the
   left bar) > tab **Applications** > **Service Accounts** >
   **Create Service Account**.
2. **Name:** `krm-live`. **Description:** `krm-live-mongodbatlas`.
3. **Client Secret Expiration:** the longest offered (up to 365 days).
4. **Organization Permissions:** `Organization Owner`.
5. Click **Create**. Copy the **Client ID** (`mdb_sa_id_...`) and the
   **Client Secret** (`mdb_sa_sk_...`) now; the secret is shown once.
6. Skip **API Access List** (leave it empty) and click **Done**.

**Hand to the human here:** the client secret goes straight into Secret
Manager.

## 4. Store it

Copy to the clipboard:

```json
{"clientId":"mdb_sa_id_...","clientSecret":"mdb_sa_sk_...","orgId":"<24 hex>","orgName":"<org name>"}
```

```sh
pbpaste | gcloud secrets create krm-live-mongodbatlas --project=$KRM_LIVE_PROJECT \
  --replication-policy=automatic --labels=purpose=krm-live,vendor=mongodbatlas --data-file=-
gcloud secrets add-iam-policy-binding krm-live-mongodbatlas --project=$KRM_LIVE_PROJECT \
  --member=serviceAccount:<runner-service-account> \
  --role=roles/secretmanager.secretAccessor
```

Check: `gcloud secrets versions access latest
--secret=krm-live-mongodbatlas --project=$KRM_LIVE_PROJECT | python3 -c
'import json,sys; print(sorted(json.load(sys.stdin)))'` prints
`['clientId', 'clientSecret', 'orgId']`.

## Done when

- The organization (`orgName`) exists with no cluster and no card.
- `krm-live-mongodbatlas` exists with keys `clientId`, `clientSecret`,
  `orgId`.
- A reminder is set before the client secret expires.
