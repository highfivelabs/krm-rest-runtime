# Pay-as-you-go accounts: a card, a spending alert, nothing left running

Most vendors' live tests run on free accounts. Four need a card on file,
because what we test is billed by use and has no monthly floor:

| Vendor | Plan | What a run uses | Expected cost |
| --- | --- | --- | --- |
| Snowflake | On Demand (no monthly minimum) | DDL through the SQL API; one extra-small warehouse that suspends after 60 seconds | cents to under $1 a run |
| MongoDB Atlas | pay as you go | M0 (free) for most kinds; an M10 (about $0.08 an hour) only for the backup-snapshot kinds | under $1 a month |
| Confluent Cloud | pay as you go, after the trial credits | a Basic cluster ($0 base, billed by use) for the length of a run | under $1 a month |
| HCP Vault Dedicated | pay as you go | a Development cluster (about $0.62 an hour, billed by the minute), made for a run and destroyed after | $1 to $3 a month |

Live runs are for new or changed kinds only, about twice a month per
vendor, so the total should stay under $10 a month. Prices were read on
2026-11-23 from vendor pages and summaries; check the vendor's own pricing
page before entering the card.

## Rules

- **Human:** a person enters the card. Never an agent, never in a chat.
- Every one of these accounts gets a spending alert (below) at about $20 a
  month, sent to the builder's work address.
- Nothing billed by the hour outlives the run. The test deletes what it
  made; the sweep deletes any `hfl-live-` leftover; the run ends with an
  empty sweep. An HCP cluster or an Atlas M10 found running outside a run
  is a bug: delete it, then open an issue.
- Nothing here buys a plan with a monthly floor. That is a decision for the
  builder (Cloudflare Pro, Sentry Team and the like are bought a month at a
  time, only when a kind needs them).

## Snowflake: from the trial to On Demand

1. **Human:** in Snowsight, **Admin > Billing & terms**, add a payment
   method before the trial ends. The account becomes On Demand; nothing
   else changes, and the token keeps working.
2. Cap the warehouses' spend (SQL worksheet, role `ACCOUNTADMIN`):

   ```sql
   CREATE RESOURCE MONITOR HFL_KRM_BUDGET WITH CREDIT_QUOTA = 5
     FREQUENCY = MONTHLY START_TIMESTAMP = IMMEDIATELY
     TRIGGERS ON 80 PERCENT DO NOTIFY
              ON 100 PERCENT DO SUSPEND_IMMEDIATE;
   ALTER ACCOUNT SET RESOURCE_MONITOR = HFL_KRM_BUDGET;
   ```

   Five credits is $10 to $15 at On Demand prices. The monitor covers
   warehouses, not serverless features; the live tests use none.
3. The token in `krm-live-snowflake` lasts 30 days
   ([snowflake.md](snowflake.md)): make a new one with a longer
   `DAYS_TO_EXPIRY` (up to 365) and add it as a new secret version.

## MongoDB Atlas

1. **Human:** **Organization > Billing**, add a payment method.
2. **Organization > Alerts**, add a billing alert: the pending invoice
   over $20, by email.

## Confluent Cloud

1. **Human:** **Billing & payment**, add a payment method before the trial
   credits run out (HFL-650 tracks the trial).
2. Delete any cluster outside a run, including the trial's `cluster_0`:
   clusters are billed while they exist.
3. Set a cost alert at $20 a month if the console offers one; if not, look
   at **Billing & payment > Cost** after each live run.

## HCP Vault Dedicated (Vault Enterprise kinds)

1. **Human:** sign up at <https://portal.cloud.hashicorp.com/>, create an
   organization and a project for the live tests (`<project>`), and add a payment
   method under **Billing**.
2. Make a service principal for the project with the Contributor role
   (**Access control (IAM) > Service principals**); its client ID and
   secret go into the secret `krm-live-hcp` as
   `{"clientId": "...", "clientSecret": "...", "projectId": "..."}`, the
   same way as every other secret ([README.md](README.md)).
3. No cluster is made by hand. A live run of the Enterprise kinds makes a
   Development cluster, runs them against it (Vault's live config takes
   `baseURL`, `token` and `namespace: admin`), and destroys it.
