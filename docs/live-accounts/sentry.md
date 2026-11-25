# Sentry: a free organization of its own

**Secret:** `krm-live-sentry` (a new version replaces the company org's
token) **JSON:** `{"token": "<auth token>", "org": "<org slug>"}`
**Time:** about 10 minutes. **Cost:** the Developer plan, $0 (one user).

The Sentry live tests used to run in the company's real Sentry org,
limited to objects named `hfl-live-...`. They move to an org of their own,
so a mistake in a test can never touch the company's projects.

## 1. Make the org

1. Sign in to <https://sentry.io/> as the builder, open the org menu (top
   left) > **Create a new organization** of your own naming (`<org>`). If Sentry
   requires a new login for a new org, sign up with the builder's work
   address instead. **Human:** email verification.
2. **Settings > Subscription**: stay on **Developer**; if a Business trial
   started, end it (or let it lapse: it falls back to Developer, with no
   card on file).

## 2. Make the token

**Settings > Developer Settings > Personal Tokens** (User Auth Tokens) >
**Create New Token**, scopes: `org:admin`, `project:admin`, `team:admin`,
`member:read`, `alerts:write`, `event:read`. Copy it. **Hand to the human
here.**

## 3. Store it

Add a new version to `krm-live-sentry` with the new org's slug and token
([README.md](README.md), "To replace a token later"). Then revoke the old
token in the company's org, and run the Sentry live sweep once in the new
org.
