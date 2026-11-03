#!/usr/bin/env bash
# Installs moor as a system service on a CI runner (systemd on Linux,
# launchd on macOS), deploys a sample app, restarts the service while the
# app is under load, and checks that the app kept running and serving
# (spike S2). On Linux it also measures a shim's memory. Runs as a user
# with passwordless sudo; the runner is thrown away afterwards.
set -euo pipefail

moor=/usr/local/bin/moor
go build -o moor ./cmd/moor
sudo install -m 0755 moor "$moor"

sudo "$moor" server init --preset local
sudo "$moor" server install
sudo "$moor" server status

sudo "$moor" deploy testdata/apps/binary --app svc
ps_json() { sudo "$moor" ps --app svc --json; }
app_pid=$(ps_json | jq -r '.[0].pid')
app_port=$(ps_json | jq -r '.[0].port')
echo "the app runs as pid $app_pid on port $app_port"
curl -fsS -H 'Host: svc.localhost' http://127.0.0.1:8080/

# Load on the app's own port, and through the router, during a restart.
# A refused request fails at once, so the time from the first failure to the
# last says how long the router was down better than the count does.
now() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }
load() { # url host file
  local ok=0 failed=0 first=0 last=0 end=$((SECONDS + 12))
  while [ $SECONDS -lt $end ]; do
    if curl -fsS -m 2 -o /dev/null -H "Host: $2" "$1" 2>/dev/null; then
      ok=$((ok + 1))
    else
      failed=$((failed + 1)) last=$(now)
      if [ "$first" = 0 ]; then first=$last; fi
    fi
  done
  echo "$ok $failed $(perl -e 'printf "%.2f", $ARGV[1] - $ARGV[0]' "$first" "$last")" > "$3"
}
load "http://127.0.0.1:$app_port/" svc.localhost direct.txt &
load "http://127.0.0.1:8080/" svc.localhost routed.txt &
sleep 3
server_pid=$(sudo "$moor" server status | sed -n 's/.*, pid \([0-9]*\).*/\1/p')
sudo "$moor" server restart
wait
read -r direct_ok direct_failed _ < direct.txt
read -r routed_ok routed_failed routed_down < routed.txt
echo "during the restart: $direct_ok requests to the app's port, $direct_failed failed;" \
  "$routed_ok through the router, $routed_failed refused in the ${routed_down}s the router was down"

sudo "$moor" server status
new_server_pid=$(sudo "$moor" server status | sed -n 's/.*, pid \([0-9]*\).*/\1/p')
new_app_pid=$(ps_json | jq -r '.[0].pid')
echo "server pid $server_pid -> $new_server_pid; app pid $app_pid -> $new_app_pid"
curl -fsS -H 'Host: svc.localhost' http://127.0.0.1:8080/

status=0
if [ "$direct_failed" != 0 ]; then echo "FAIL: requests to the app failed during the restart"; status=1; fi
if [ "$new_app_pid" != "$app_pid" ]; then echo "FAIL: the app was restarted with the server"; status=1; fi
if perl -e 'exit !($ARGV[0] > 3)' "$routed_down"; then echo "FAIL: the router was down for over 3s"; status=1; fi
if [ -z "$new_server_pid" ] || [ "$new_server_pid" = "$server_pid" ]; then echo "FAIL: the server did not restart"; status=1; fi

if [ "$(uname)" = Linux ]; then
  data=$(sudo sed -n 's/^data_dir *= *"\(.*\)"$/\1/p' /etc/moorage/server.toml)
  for state in $(sudo sh -c "ls $data/run/shims/*.state.json"); do
    shim_pid=$(sudo jq -r .shim_pid "$state")
    echo "shim $shim_pid memory:"
    sudo grep -E '^(Rss|Pss|Private_Clean|Private_Dirty):' "/proc/$shim_pid/smaps_rollup"
  done
fi

sudo "$moor" server uninstall
if curl -fsS -m 2 -o /dev/null "http://127.0.0.1:$app_port/"; then
  echo "FAIL: the app still runs after uninstall"; status=1
fi
exit $status
