#!/usr/bin/env bash
# Copyright 2026 High Five Labs
# SPDX-License-Identifier: Apache-2.0
#
# Everything CI checks, in one place: .github/workflows/ci.yml runs this
# script, and so does anyone opening a pull request. Until CI runs on every
# pull request, each one carries the --report output in its description.
#
#   hack/ci.sh            run the checks
#   hack/ci.sh --report   run them and print a Markdown proof for the PR
#
# -tags live is only vetted, and only the runtime's own live tests (against
# an httptest fake on 127.0.0.1) run with it.
set -uo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
report=false
[[ ${1:-} == --report ]] && report=true

cleanup=()
trap 'for c in "${cleanup[@]}"; do eval "$c"; done' EXIT

log=$(mktemp)
cleanup+=("rm -f '$log'")
rows=() failed=0 start=$SECONDS

step() {
  local name=$1; shift
  local t=$SECONDS rc
  echo "::group::$name" >>"$log"
  echo "--- $name" >&2
  ( set -e; "$@" ) >>"$log" 2>&1
  rc=$?
  echo "::endgroup::" >>"$log"
  if ((rc == 0)); then rows+=("| $name | ✅ pass | $((SECONDS - t))s |")
  else rows+=("| $name | ❌ fail ($rc) | $((SECONDS - t))s |"); failed=1
    echo "    failed; last lines:" >&2; tail -20 "$log" | sed 's/^/    /' >&2
  fi
}

step "gofmt" bash -c 'out=$(gofmt -l .); [[ -z $out ]] || { echo "$out"; exit 1; }'
step "go build ./..." go build ./...
step "go vet ./..." go vet ./...
step "go vet -tags live ./..." go vet -tags live ./...
step "go test ./..." go test -count=1 ./...
step "go test -tags live ./kindtest/" go test -count=1 -tags live ./kindtest/
step "python3 gen/examples_test.py" python3 gen/examples_test.py

if $report; then
  sha=$(git rev-parse HEAD)
  dirty=$(git status --porcelain | wc -l | tr -d ' ')
  {
    echo "### Local CI"
    echo
    if ((failed)); then echo "**Result: ❌ failed**"; else echo "**Result: ✅ all checks passed**"; fi
    echo
    echo "- Commit: \`$sha\`$( ((dirty)) && echo " **plus $dirty uncommitted change(s)**" || echo " (clean tree)")"
    echo "- Toolchain: $(go version | cut -d' ' -f3-)"
    echo "- Ran: $(date -u +%Y-%m-%dT%H:%M:%SZ) on $(uname -sm), $((SECONDS - start))s, via \`hack/ci.sh --report\`"
    echo
    echo "| Step | Result | Time |"
    echo "| --- | --- | --- |"
    printf '%s\n' "${rows[@]}"
    echo
    echo "<details><summary>go test output</summary>"
    echo
    echo '```'
    awk '/^::group::go test/{p=1;next} /^::endgroup::/{p=0} p' "$log" | tail -60
    echo '```'
    echo
    echo "</details>"
  }
fi
exit $failed
