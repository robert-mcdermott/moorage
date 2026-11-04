#!/usr/bin/env bash
# Tries the web console in a real browser, against a real server with a real app: every
# page at desktop and phone width, and what scripts add. It needs Node.js and Playwright.
#
#   BROWSER=webkit .github/scripts/console-check.sh
#
# BROWSER is chromium (the default), firefox or webkit, and MOORAGE_CONSOLE_PORT the
# server's port (8090). Where Playwright is not installed globally, install it beside the
# checkout first: npm install --no-save playwright
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
port=${MOORAGE_CONSOLE_PORT:-8090}
work=$(mktemp -d)
moor=$work/moor
(cd "$root" && go build -o "$moor" ./cmd/moor)

"$moor" server init --preset local --config "$work/server.toml" --data-dir "$work/data" > /dev/null
sed -i.orig -e "s/127\.0\.0\.1:8080/127.0.0.1:$port/" -e 's/^ssh .*/ssh = ""/' "$work/server.toml"
# WebKit does not resolve *.localhost by itself, as Chromium and Firefox do.
console=moorage.localhost
if [ "${BROWSER:-chromium}" = webkit ]; then
  console=localhost
  sed -i.orig 's/^console_host .*/console_host = "localhost"/' "$work/server.toml"
fi
export MOORAGE_CONFIG=$work/server.toml
export MOOR_CONFIG=$work/client.toml
"$moor" server run > "$work/server.log" 2>&1 &

cleanup() {
  "$moor" server stop --all > /dev/null || true
  if [ "$status" != 0 ]; then
    echo "--- the server's log"
    tail -n 50 "$work/server.log"
  fi
}
status=1
trap cleanup EXIT

for _ in $(seq 1 50); do
  curl -fsS -o /dev/null "http://127.0.0.1:$port/.moorage/health" 2> /dev/null && break
  sleep 0.2
done

# An app that says things: plain lines, structured lines, warnings and errors.
app=$work/app
mkdir -p "$app"
cat > "$app/moorage.toml" <<'TOML'
[app]
name = "shop"
runtime = "command"

[build]
command = "echo preparing the shop; sleep 1; echo built it"

[processes.web]
command = "python3 -m http.server $PORT"

[processes.web.health]
path = "/"

[processes.worker]
command = "python3 talk.py"

[env]
GREETING = "hello"
TOML
cat > "$app/talk.py" <<'PY'
import itertools, json, sys, time
for i in itertools.count():
    print(json.dumps({"level": "info", "msg": "processed a job", "job": i}), flush=True)
    if i % 3 == 0:
        print(json.dumps({"level": "warn", "msg": "the queue is getting long", "depth": i}), flush=True)
    if i % 4 == 0:
        print(f"ERROR: could not reach the database (attempt {i})", file=sys.stderr, flush=True)
    time.sleep(0.5)
PY
(cd "$app" && "$moor" deploy --server "http://127.0.0.1:$port" > /dev/null)
echo "ok    the app is deployed"

export CONSOLE_URL="http://$console:$port"
export CONSOLE_CLAIM
CONSOLE_CLAIM=$(cat "$work/data/keys/claim-code")
export CONSOLE_MOOR=$moor CONSOLE_APP_DIR=$app CONSOLE_SERVER="http://127.0.0.1:$port"
NODE_PATH="${NODE_PATH:-}:$root/node_modules:$(npm root -g)" node "$root/.github/scripts/console-check.cjs"
status=0
