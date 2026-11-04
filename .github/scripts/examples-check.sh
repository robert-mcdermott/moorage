#!/usr/bin/env bash
# Deploys every app of examples/ to a Moorage server of its own, from the
# app's directory as a user would, and checks each app's pages and API. It
# needs the network: the first deploys download Go, Node.js, uv and Python.
#
#   .github/scripts/examples-check.sh
#
# MOORAGE_EXAMPLES_PORT is the server's port (8080).
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
port=${MOORAGE_EXAMPLES_PORT:-8080}
# A space in the path, as the system-wide location on macOS has: builds run under it.
work="$(mktemp -d)/with a space"
mkdir -p "$work"
moor=$work/moor
(cd "$root" && go build -o "$moor" ./cmd/moor)

"$moor" server init --preset local --config "$work/server.toml" --data-dir "$work/data" > /dev/null
sed -i.orig "s/127\.0\.0\.1:8080/127.0.0.1:$port/" "$work/server.toml"
export MOORAGE_CONFIG=$work/server.toml
"$moor" server run > "$work/server.log" 2>&1 &

# What runs container-go, if this machine has an engine for it.
engine=$(command -v podman || command -v docker || true)

# Apps outlive the server by design: moor server stop --all stops the server,
# wherever it runs, and its apps, in containers or not.
cleanup() {
  "$moor" server stop --all || true
  if [ -n "$engine" ]; then
    for image in $("$engine" images -q --filter label=io.moorage.app=guestbook); do
      "$engine" image rm -f "$image" > /dev/null || true
    done
  fi
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

failures=0
check() { # description, condition
  if eval "$2"; then
    echo "ok    $1"
  else
    echo "FAIL  $1"
    failures=$((failures + 1))
  fi
}
url() { echo "http://127.0.0.1:$port$1"; }
# body APP PATH [curl options…], headers APP PATH and code APP PATH [curl
# options…] answer a request to APP: its body, its headers, its status.
body() { curl -sS -H "Host: $1.localhost" "${@:3}" "$(url "$2")" || true; }
headers() { curl -sS -o /dev/null -D - -H "Host: $1.localhost" "$(url "$2")" | tr -d '\r' || true; }
code() { curl -sS -o /dev/null -w '%{http_code}' -H "Host: $1.localhost" "${@:3}" "$(url "$2")" || true; }
contains() { [[ $1 == *"$2"* ]]; }
json=(-H 'Content-Type: application/json')

for example in python-fastapi go-stdlib node-express static-site; do
  echo "--- deploying $example"
  (cd "$root/examples/$example" && "$moor" deploy)
done

echo "--- tasks (python-fastapi)"
check "the page" 'contains "$(body tasks /)" "<title>Tasks</title>"'
check "the health check" 'contains "$(body tasks /healthz)" "ok"'
check "adds a task" '[ "$(code tasks /api/tasks -X POST "${json[@]}" -d "{\"title\": \"from CI\"}")" = 201 ]'
check "lists it" 'contains "$(body tasks /api/tasks)" "from CI"'
(cd "$root/examples/python-fastapi" && "$moor" env set TASKS_TITLE="CI tasks")
check "a variable changes the title, in a new release" 'contains "$(body tasks /api/info)" "\"title\":\"CI tasks\",\"release\":\"v2\""'
check "the task outlives the release" 'contains "$(body tasks /api/tasks)" "from CI"'
(cd "$root/examples/python-fastapi" && "$moor" rollback --with-config)
check "a rollback brings the title back" 'contains "$(body tasks /api/info)" "\"title\":\"Tasks\""'

echo "--- links (go-stdlib)"
check "adds a link" '[ "$(code links /links -d "url=https://example.com/ci&code=ci")" = 303 ]'
check "redirects to it" 'contains "$(headers links /ci)" "Location: https://example.com/ci"'
check "counts the visit" 'contains "$(body links /)" "<td class=\"n\">1</td>"'
check "an unknown link is not found" '[ "$(code links /nope)" = 404 ]'

echo "--- poll (node-express)"
asset=$(body poll / | sed -n 's/.*src="\(\/assets\/[^"]*\.js\)".*/\1/p')
check "the page, built by Vite" '[ -n "$asset" ] && [ "$(code poll "$asset")" = 200 ]'
check "its assets are cached for good" 'contains "$(headers poll "$asset")" "Cache-Control: public, max-age=31536000, immutable"'
check "takes a vote" '[ "$(code poll /api/votes -X POST "${json[@]}" -d "{\"option\": \"Go\"}")" = 201 ]'
check "counts it" 'contains "$(body poll /api/poll)" "\"option\":\"Go\",\"votes\":1"'
check "refuses an unknown answer" '[ "$(code poll /api/votes -X POST "${json[@]}" -d "{\"option\": \"COBOL\"}")" = 400 ]'

echo "--- handbook (static-site)"
check "a clean URL" 'contains "$(body handbook /about)" "About this handbook"'
check "the .html address redirects" 'contains "$(headers handbook /about.html)" "Location: /about"'
check "an old link redirects" 'contains "$(headers handbook /docs/onboarding)" "Location: /guide/onboarding"'
check "its own 404 page" '[ "$(code handbook /nope)" = 404 ] && contains "$(body handbook /nope)" "not in the handbook"'
check "headers for every file" 'contains "$(headers handbook /)" "X-Frame-Options: DENY"'
check "a cache lifetime for the stylesheet" 'contains "$(headers handbook /assets/style.css)" "Cache-Control: public, max-age=86400"'

if [ -n "$engine" ]; then
  echo "--- deploying container-go, with $engine"
  (cd "$root/examples/container-go" && "$moor" deploy)
  echo "--- guestbook (container-go)"
  check "the page" 'contains "$(body guestbook /)" "<title>Guestbook</title>"'
  check "says which release it is" 'contains "$(body guestbook /)" "Running v1 in a container"'
  check "the release command ran, with no shell in the image" '[ -e "$work/data/data/guestbook/production/guestbook.version" ]'
  check "takes an entry" '[ "$(code guestbook /sign -d "name=CI&message=from+the+check")" = 303 ]'
  (cd "$root/examples/container-go" && "$moor" deploy)
  check "the entry outlives the release" 'contains "$(body guestbook /)" "from the check"'
  check "the new release serves" 'contains "$(body guestbook /)" "Running v2 in a container"'
  check "runs as the image says" '[ "$("$engine" ps --filter label=io.moorage.app=guestbook --format "{{.Names}}" | xargs "$engine" inspect --format "{{.Config.User}}")" = 65532:65532 ]'
else
  echo "--- no container engine: skipping container-go"
fi

if [ "$failures" != 0 ]; then
  echo "$failures checks failed"
  exit 1
fi
status=0
echo "every example deployed and answered"
