# Snowflake: a trial account and a programmatic access token

**Secret:** `krm-live-snowflake` in the project `$KRM_LIVE_PROJECT` names
**JSON:** `{"token": "<PAT>", "baseURL": "https://<org>-<account>.snowflakecomputing.com", "tokenType": "PROGRAMMATIC_ACCESS_TOKEN"}`
**Time:** about 15 minutes. **Cost:** free; the trial lasts **30 days**
from sign-up and needs no card. Run the live tests inside that window.

Each run makes one database (`HFL_LIVE_<stamp>_<hex>`), schemas, tags, a
warehouse that starts suspended and never runs a query, and roles, and
deletes them.

## 1. Start the trial

1. Open <https://signup.snowflake.com/>.
2. Fill in: first and last name (the builder's), email
   (the builder's work address), company `High Five Labs`, role, country.
   Click **Continue**.
3. **Edition:** Enterprise. **Cloud provider:** Google Cloud Platform.
   **Region:** US Central1 (Iowa). Click **Get started**; skip the survey.
4. **Human:** the activation email ("Activate your Snowflake account").
   Choose a username and password. Snowflake may ask to
   enrol MFA on first sign-in: **Human**.
5. In Snowsight, open the account menu (bottom left, your name) >
   **Account** > **View account details**. Copy **Account/Server URL**:
   `<org>-<account>.snowflakecomputing.com`. `baseURL` is that with
   `https://` in front.

## 2. Make the test user and its token

In Snowsight, **Projects > Worksheets** > **+ SQL Worksheet**, role
`ACCOUNTADMIN` (top right), and run:

```sql
USE ROLE ACCOUNTADMIN;

-- A user only for the live tests (not the builder's own login).
CREATE USER HFL_KRM_LIVE TYPE = PERSON DEFAULT_ROLE = ACCOUNTADMIN
  COMMENT = 'krm-live-snowflake: live tests';
GRANT ROLE ACCOUNTADMIN TO USER HFL_KRM_LIVE;

-- A token is accepted only from a user under a network policy. Runs come
-- from offload VMs with changing addresses, so the policy allows any.
CREATE NETWORK POLICY HFL_KRM_LIVE_ANY ALLOWED_IP_LIST = ('0.0.0.0/0')
  COMMENT = 'krm-live-snowflake: offload VMs have no fixed address';
ALTER USER HFL_KRM_LIVE SET NETWORK_POLICY = HFL_KRM_LIVE_ANY;

-- The token: shown once, in the result's token_secret column.
ALTER USER HFL_KRM_LIVE ADD PROGRAMMATIC ACCESS TOKEN KRM_LIVE
  ROLE_RESTRICTION = 'ACCOUNTADMIN'
  DAYS_TO_EXPIRY = 30
  COMMENT = 'krm-live-snowflake';
```

Copy `token_secret` from the last result now. Names here are
`HFL_KRM_...`, never `HFL_LIVE_...`, which the tests' sweep deletes.

The same in the UI, if SQL is refused: **Governance & security > Users &
roles** > `HFL_KRM_LIVE` > **Programmatic access tokens** > **Generate new
token**, name `KRM_LIVE`, 30 days, **One specific role** `ACCOUNTADMIN`,
**Generate**.

**Hand to the human here:** the token goes straight into Secret Manager.

## 3. Store it

Copy to the clipboard:

```json
{"token":"<token_secret>","baseURL":"https://<org>-<account>.snowflakecomputing.com","tokenType":"PROGRAMMATIC_ACCESS_TOKEN"}
```

```sh
pbpaste | gcloud secrets create krm-live-snowflake --project=$KRM_LIVE_PROJECT \
  --replication-policy=automatic --labels=purpose=krm-live,vendor=snowflake --data-file=-
gcloud secrets add-iam-policy-binding krm-live-snowflake --project=$KRM_LIVE_PROJECT \
  --member=serviceAccount:<runner-service-account> \
  --role=roles/secretmanager.secretAccessor
```

Check: `gcloud secrets versions access latest --secret=krm-live-snowflake
--project=$KRM_LIVE_PROJECT | python3 -c 'import json,sys;
print(sorted(json.load(sys.stdin)))'` prints `['baseURL', 'token',
'tokenType']`.

## Done when

- The trial account exists; note its end date (sign-up + 30 days).
- User `HFL_KRM_LIVE` has a token restricted to `ACCOUNTADMIN`.
- `krm-live-snowflake` exists with keys `token`, `baseURL`, `tokenType`.
