#!/usr/bin/env bash
# Upgrades a server from the last published release to this code, in a directory
# of its own, as a user would: the new binary takes over the old one's data
# directory, and adopts the app it left running.
#
#   .github/scripts/upgrade-check.sh
#
# MOORAGE_UPGRADE_PORT is the server's port (18090). It changes nothing outside
# its directory, and needs the network to download the release.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
port=${MOORAGE_UPGRADE_PORT:-18090}
work=$(mktemp -d)
new=$work/moor-new
old=$work/moor-old
(cd "$root" && go build -o "$new" ./cmd/moor)

case "$(uname -s)" in Darwin) os=darwin ;; *) os=linux ;; esac
case "$(uname -m)" in arm64 | aarch64) arch=arm64 ;; *) arch=amd64 ;; esac
base=https://github.com/robert-mcdermott/moorage/releases/latest/download
curl -fsSL -o "$old" "$base/moor-$os-$arch"
curl -fsSL -o "$work/SHA256SUMS" "$base/SHA256SUMS"
want=$(awk -v f="moor-$os-$arch" '$2 == f { print $1 }' "$work/SHA256SUMS")
got=$(shasum -a 256 "$old" | awk '{ print $1 }')
if [ -z "$want" ] || [ "$want" != "$got" ]; then
  echo "the download does not match its checksum: $got, not $want" >&2
  exit 1
fi
chmod +x "$old"
echo "from: $("$old" version)"
echo "to:   $("$new" version)"

export MOORAGE_CONFIG=$work/server.toml MOOR_CONFIG=$work/client.toml
"$old" server init --preset local --config "$MOORAGE_CONFIG" --data-dir "$work/data" > /dev/null
sed -i.orig -e "s/127\.0\.0\.1:8080/127.0.0.1:$port/" -e 's/^ssh .*/ssh = ""/' "$MOORAGE_CONFIG"

server=
start() { # binary
  "$1" server run >> "$work/server.log" 2>&1 &
  server=$!
  for _ in $(seq 1 100); do
    curl -fsS -o /dev/null "http://127.0.0.1:$port/.moorage/health" 2> /dev/null && return 0
    sleep 0.1
  done
  echo "the server did not start" >&2
  return 1
}
stop() {
  kill "$server" 2> /dev/null && wait "$server" 2> /dev/null || true
}
status=1
cleanup() {
  stop
  "$new" server stop --all > /dev/null 2>&1 || true
  if [ "$status" != 0 ]; then
    echo "--- the server's log"
    tail -n 40 "$work/server.log"
  fi
}
trap cleanup EXIT

failures=0
check() { # description, condition
  if eval "$2"; then
    echo "ok    $1"
  else
    echo "FAIL  $1"
    failures=$((failures + 1))
  fi
}
body() { curl -sS -H "Host: $1.localhost" "http://127.0.0.1:$port$2" || true; }
ps_field() { "$new" ps --app svc --json | jq -r ".[0].$1"; }

echo "--- with the last release"
start "$old"
"$old" deploy "$root/testdata/apps/binary" --app svc
"$old" deploy --static "$root/examples/static-site/public" --app site
printf 'a-secret-from-before' | "$old" env set --app svc --secret KEPT_SECRET > /dev/null
"$old" env set --app svc --no-restart PLAIN=kept > /dev/null
before=$(body svc /)
pid=$("$old" ps --app svc --json | jq -r '.[0].pid')
id=$("$old" ps --app svc --json | jq -r '.[0].id')
stop

echo "--- with this code"
start "$new"
"$new" version
check "the app was adopted, not restarted" '[ "$(ps_field pid)" = "$pid" ] && [ "$(ps_field id)" = "$id" ]'
check "it still answers" '[ "$(body svc /)" = "$before" ]'
check "the static site still answers" '[ -n "$(body site /about)" ]'
check "the new version is what runs" '[[ "$("$new" server doctor --json || true)" == *"running $("$new" version | awk "{print \$2}")"* ]]'
check "the server can be claimed, and says so" 'grep -q "claim it with the setup link" "$work/server.log"'
check "the CLI lists its apps" '"$new" apps | grep svc > /dev/null'
check "and its variables, with the secret hidden" '[[ "$("$new" env --app svc)" == *KEPT_SECRET* ]] && [[ "$("$new" env --app svc)" != *a-secret-from-before* ]]'

echo "--- a new release, and back"
"$new" deploy "$root/testdata/apps/binary" --app svc
check "a deploy works" '"$new" releases --app svc | grep "\*" > /dev/null'
check "the secret reached the new release" '[[ "$("$new" env --app svc)" == *KEPT_SECRET* ]]'
"$new" rollback --app svc
check "a rollback to the old release's build works" '[ "$(ps_field status)" = running ]'
check "the rolled-back app answers" '[ -n "$(body svc /)" ]'
stop

echo "--- stopping it all"
"$new" server stop --all
check "no shim is left" '[ -z "$(ls "$work/data/run/shims" 2> /dev/null)" ]'

if [ "$failures" != 0 ]; then
  echo "$failures checks failed"
  exit 1
fi
status=0
echo "the upgrade from the last release works"
