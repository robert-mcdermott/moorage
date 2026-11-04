#!/usr/bin/env bash
# Does a small machine hold many environments? Sixty of them on a real server, with sleep on: they
# deploy, go to sleep, wake one at a time and all at once, and those awake answer quickly while the
# others sleep. It says what that took, and fails if a limit was passed.
#
#   .github/scripts/capacity-check.sh
#
# CAPACITY_APPS and CAPACITY_ENVS say how many apps and how many environments of each (10 and 6), and
# CAPACITY_BALLAST_MB what each app holds in memory (64). Run it inside whatever limits you mean to
# try: CI gives it 4 CPUs and 8 GB with a cgroup.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
port=${MOORAGE_CAPACITY_PORT:-8093}
work=$(mktemp -d)
moor=$work/moor
(cd "$root" && go build -o "$moor" ./cmd/moor)

"$moor" server init --preset local --config "$work/server.toml" --data-dir "$work/data" > /dev/null
sed -i.orig -e "s/127\.0\.0\.1:8080/127.0.0.1:$port/" -e 's/^ssh .*/ssh = ""/' "$work/server.toml"
export MOORAGE_CONFIG=$work/server.toml MOOR_CONFIG=$work/client.toml
"$moor" server run > "$work/server.log" 2>&1 &
server=$!

cleanup() {
  "$moor" server stop --all > /dev/null || true
  if [ "$status" != 0 ]; then
    echo "--- the server's log"
    tail -n 40 "$work/server.log"
  fi
}
status=1
trap cleanup EXIT

for _ in $(seq 1 100); do
  curl -fsS -o /dev/null "http://127.0.0.1:$port/.moorage/health" 2> /dev/null && break
  sleep 0.2
done

python3 "$root/.github/scripts/capacity-check.py" "$moor" "$work" "$port" "$server"
status=0
