#!/usr/bin/env bash
# Runs one part of the server's tests, so that CI can run them side by side:
#
#   .github/scripts/test-part.sh PART PARTS [flags for go test...]
#
# It deals the tests out in turn, so that each is in exactly one part, including those not written yet.
# A list that came back empty, or a part with nothing in it, fails here: go test would call that a pass.
set -euo pipefail

part=$1
parts=$2
shift 2
package=./internal/server

tests=$(go test "$@" -list '.*' "$package" | grep '^Test' | LC_ALL=C sort)
if [ -z "$tests" ]; then
  echo "found no tests in $package" >&2
  exit 1
fi
mine=$(printf '%s\n' "$tests" | awk -v part="$part" -v parts="$parts" 'NR % parts == part % parts' | paste -sd '|' -)
if [ -z "$mine" ]; then
  echo "part $part of $parts has no tests, of $(printf '%s\n' "$tests" | wc -l | tr -d ' ')" >&2
  exit 1
fi
echo "part $part of $parts: $(printf '%s\n' "$mine" | tr '|' '\n' | wc -l | tr -d ' ') of $(printf '%s\n' "$tests" | wc -l | tr -d ' ') tests"
go test "$@" -timeout 15m -run "^($mine)\$" "$package"
