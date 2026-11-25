# Cloudflare Zero Trust: the free plan on the test account

**Secret:** `krm-live-cloudflare` (unchanged; its token gains permissions)
**Time:** about 10 minutes. **Cost:** Zero Trust Free, $0 for up to 50
users. Cloudflare asks for a payment method even on the free plan; nothing
is charged unless a paid plan is chosen.

About 60 Cloudflare kinds are Zero Trust (Access, Gateway, tunnels, DLP,
device posture, DEX). They are skipped today with "Zero Trust is not set
up on the account".

## 1. Turn Zero Trust on

1. Sign in to the test account (the one `krm-live-cloudflare`'s
   `accountId` names) at <https://dash.cloudflare.com/>.
2. **Zero Trust** in the sidebar. Choose a team name, `<team>` (the team
   domain becomes `<team>.cloudflareaccess.com`).
3. Plan: **Free**. **Human:** add the payment method Cloudflare asks for.
   Do not choose Standard or any add-on.

## 2. Widen the token

**My Profile > API Tokens**, edit the test token, and add, for the test
account only:

- Account > Zero Trust: Edit
- Account > Access: Apps and Policies: Edit
- Account > Access: Organizations, Identity Providers, and Groups: Edit
- Account > Access: Service Tokens: Edit
- Account > Cloudflare Tunnel: Edit

The token's value does not change, so the secret needs no new version.
Add the team name as a new key, `teamName`, in a new secret version (as in
[README.md](README.md)).

## 3. Leave alone

The test account's identity providers for logging in to the dashboard, and
any setting that changes how people sign in: the live tests only touch
objects named `hfl-live-...`.
