# Grafana Cloud: a free stack for the Cloud-only kinds

**Secret:** `krm-live-grafana-cloud` in the project `$KRM_LIVE_PROJECT` names
**JSON:** `{"token": "<service account token>", "baseURL": "https://<stack>.grafana.net/api", "rootURL": "https://<stack>.grafana.net"}`
(the same keys as `krm-live-grafana`, the OSS sandbox's, so the same live
config runs against either).
**Time:** about 10 minutes. **Cost:** Grafana Cloud Free, $0, no card, no
expiry (usage is capped; the tests send next to nothing).

Grafana OSS in a container covers most kinds. About 67 are Grafana Cloud
only (Asserts, OnCall and IRM, the grafana.com API, k6, Synthetic
Monitoring, Fleet Management, the Machine Learning and AI apps, and
others; `skip: Grafana Cloud only` in
`grafana-krm-extensions/hack/live/live.yaml`). Some Enterprise-only kinds
(reporting, custom roles) may also answer on a Cloud stack: try them there
before marking any of them fake only.

## 1. Sign up and make the stack

1. Open <https://grafana.com/auth/sign-up/create-user>. Sign up with the
   builder's work address. **Human:** email verification.
2. Name the stack, `<stack>` (its URL becomes
   `https://<stack>.grafana.net`).
   Region: any US region. Plan: **Free**. Do not start the Pro trial.

## 2. Make the token

In the stack: **Administration > Users and access > Service accounts >
Add service account**, name `krm-live`, role **Admin**, **Create**.
Then **Add service account token**, no expiry, **Generate token**, and
copy it. **Hand to the human here.**

Kinds on Grafana.com's own API (access policies, stacks) need a Cloud
access policy token as well; the live config names that key when a kind
needs it, and skips the kind until the secret has it.

## 3. Store it

As in [README.md](README.md), "Storing the secret", with `<vendor>` =
`grafana-cloud`.
