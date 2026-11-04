#!/usr/bin/env bash
# Runs install.sh for real on a CI runner, as a user would, against the published
# releases: piped into a shell, upgrading, under a running server, and as root.
# Not for your own machine: it installs into /usr/local/bin.
set -euo pipefail

if [ "${CI:-}" != "true" ]; then
  echo "this installs into /usr/local/bin, and is meant for CI's throwaway runners" >&2
  exit 2
fi

root=$(cd "$(dirname "$0")/../.." && pwd)
script=$root/install.sh
work=$(mktemp -d)
port=18095
server=

status=1
cleanup() {
  if [ -n "$server" ]; then
    kill "$server" 2> /dev/null && wait "$server" 2> /dev/null || true
  fi
  if [ "$status" != 0 ] && [ -f "$work/server.log" ]; then
    echo "--- the server's log"
    tail -n 30 "$work/server.log"
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
contains() { [[ $1 == *"$2"* ]]; }

latest=$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/robert-mcdermott/moorage/releases/latest)
latest=${latest##*/}
latest=${latest#v}
echo "the latest release is $latest"

echo "--- piped into sh, with nothing set"
home=$work/home
mkdir -p "$home"
said=$(cat "$script" | HOME=$home sh 2>&1)
echo "$said"
moor=$home/.local/bin/moor
check "it is in ~/.local/bin" '[ -x "$moor" ]'
check "it is the latest release" 'contains "$("$moor" version)" "moor $latest "'
check "it says how to add it to the PATH" 'contains "$said" "export PATH="'
check "it says what to do next" 'contains "$said" "server init --preset local"'
check "it leaves nothing beside it" '[ "$(ls -A "$home/.local/bin")" = moor ]'

echo "--- piped into bash"
said=$(cat "$script" | MOOR_INSTALL_DIR=$work/bash bash 2>&1)
check "it works there too" 'contains "$("$work/bash/moor" version)" "moor $latest "'

echo "--- a given release, then the latest, under a running server"
pinned=$work/pinned
said=$(cat "$script" | MOOR_VERSION=v0.1.0 MOOR_INSTALL_DIR=$pinned sh 2>&1)
check "it installs the release it is asked for" 'contains "$("$pinned/moor" version)" "moor 0.1.0 "'
export MOORAGE_CONFIG=$work/server.toml MOOR_CONFIG=$work/client.toml
"$pinned/moor" server init --preset local --config "$MOORAGE_CONFIG" --data-dir "$work/data" > /dev/null
sed -i.orig -e "s/127\.0\.0\.1:8080/127.0.0.1:$port/" -e 's/^ssh .*/ssh = ""/' "$MOORAGE_CONFIG"
"$pinned/moor" server run > "$work/server.log" 2>&1 &
server=$!
for _ in $(seq 1 100); do
  curl -fsS -o /dev/null "http://127.0.0.1:$port/.moorage/health" 2> /dev/null && break
  sleep 0.1
done
health() { curl -fsS "http://127.0.0.1:$port/.moorage/health"; }
check "a server runs from it" 'contains "$(health)" "\"version\":\"0.1.0\""'
said=$(cat "$script" | MOOR_INSTALL_DIR=$pinned sh 2>&1)
echo "$said"
check "it replaces it" 'contains "$("$pinned/moor" version)" "moor $latest "'
check "it says it upgraded" 'contains "$said" "Upgraded from 0.1.0"'
check "it says to restart the server" 'contains "$said" "server restart"'
check "the server goes on, with the old version" 'contains "$(health)" "\"version\":\"0.1.0\""'
kill "$server" && wait "$server" 2> /dev/null || true
server=
"$pinned/moor" server run >> "$work/server.log" 2>&1 &
server=$!
for _ in $(seq 1 100); do
  curl -fsS -o /dev/null "http://127.0.0.1:$port/.moorage/health" 2> /dev/null && break
  sleep 0.1
done
check "restarted, it runs the new one" 'contains "$(health)" "\"version\":\"$latest\""'
kill "$server" && wait "$server" 2> /dev/null || true
server=
unset MOORAGE_CONFIG MOOR_CONFIG

echo "--- nothing replaces it, if what it downloads is wrong"
said=$(cat "$script" | MOOR_VERSION=v0.0.1 MOOR_INSTALL_DIR=$pinned sh 2>&1) && check "a release that does not exist fails" 'false' || check "a release that does not exist fails" 'contains "$said" "failed to download"'
check "and what was there stays" 'contains "$("$pinned/moor" version)" "moor $latest "'

echo "--- as root, from a saved copy, as the documentation says"
cp "$script" "$work/install-moor.sh"
said=$(sudo sh "$work/install-moor.sh" 2>&1)
echo "$said"
check "it is in /usr/local/bin" '[ -x /usr/local/bin/moor ]'
check "it belongs to root" '[ "$(ls -ln /usr/local/bin/moor | awk "{print \$3}")" = 0 ]'
check "it runs" 'contains "$(/usr/local/bin/moor version)" "moor $latest "'
check "it says how to set up a server" 'contains "$said" "server init --domain"'
check "with sudo" 'contains "$said" "sudo "'

if [ "$failures" != 0 ]; then
  echo "$failures checks failed"
  exit 1
fi
status=0
echo "install.sh works on $(uname -s)/$(uname -m)"
