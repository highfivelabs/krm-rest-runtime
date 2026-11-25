#!/usr/bin/env bash
# Copyright 2026 High Five Labs
# SPDX-License-Identifier: Apache-2.0
#
# Stores a vendor's live-test credentials in Secret Manager, read from
# stdin so the token never reaches shell history, argv or a log:
#
#   task live:secret VENDOR=github         # then paste the JSON, Ctrl-D
#   pbpaste | task live:secret VENDOR=okta
#
# The JSON object's keys are the vendor's Connection Secret keys, plus
# baseURL and whatever its hack/live/live.yaml reads with {{cred:KEY}}
# (docs/live.md has the table). Makes krm-live-<vendor> if it is new, else
# adds a version. Prints the key names it stored, never the values.
set -o errexit -o nounset -o pipefail

vendor=${1:?usage: $0 VENDOR [PROJECT]}
project=${2:-${KRM_LIVE_PROJECT:?set KRM_LIVE_PROJECT (or pass PROJECT) to the Secret Manager project for live-test credentials}}
[[ $vendor =~ ^[a-z0-9]+$ ]] || { echo "live:secret: VENDOR must be lower-case letters and digits, not '$vendor'" >&2; exit 2; }
secret="krm-live-${vendor}"

[[ -t 0 ]] && echo "Paste the credentials JSON for ${secret}, then Ctrl-D:" >&2
payload=$(cat)

# Check it is a JSON object of non-empty values, without echoing it.
keys=$(python3 -I -c '
import json, sys
try:
    o = json.loads(sys.stdin.read())
except ValueError as e:
    sys.exit("live:secret: not JSON (%s at line %d); nothing stored" % (e.msg, e.lineno))
if not isinstance(o, dict) or not o:
    sys.exit("live:secret: want a non-empty JSON object; nothing stored")
empty = sorted(k for k, v in o.items() if v in ("", None))
if empty:
    sys.exit("live:secret: empty values for %s; nothing stored" % ", ".join(empty))
print(" ".join(sorted(o)))
' <<<"$payload")

if gcloud secrets describe "$secret" --project="$project" --quiet >/dev/null 2>&1; then
  gcloud secrets versions add "$secret" --project="$project" --data-file=- --quiet >/dev/null <<<"$payload"
  echo "live:secret: new version of ${secret} in ${project}: keys ${keys}" >&2
else
  gcloud secrets create "$secret" --project="$project" --replication-policy=automatic \
    --labels=purpose=krm-live,vendor="$vendor" --data-file=- --quiet >/dev/null <<<"$payload"
  echo "live:secret: created ${secret} in ${project}: keys ${keys}" >&2
fi
