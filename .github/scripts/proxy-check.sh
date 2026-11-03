#!/usr/bin/env bash
# Runs a server behind a real nginx, configured by moor server proxy-config: the
# doctor checks the whole path, and a deploy and a push go through it. nginx runs
# as this user, from a directory of its own, on ports of its own.
set -euo pipefail

work=$(mktemp -d)
moor=$work/moor
go build -o "$moor" ./cmd/moor

export MOORAGE_CONFIG=$work/server.toml MOOR_CONFIG=$work/client.toml
# *.localhost resolves on systemd's systems, which saves a wildcard DNS record.
# curl does not accept a wildcard of one label, so its names are listed too.
"$moor" server init --domain localhost --behind-proxy --config "$MOORAGE_CONFIG" --data-dir "$work/data" > /dev/null
sed -i.orig -e 's/= ":2222"/= ""/' "$MOORAGE_CONFIG"

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 -keyout "$work/proxy.key" -out "$work/proxy.crt" \
  -subj "/CN=the proxy" -addext "subjectAltName=DNS:moorage.localhost,DNS:*.localhost,DNS:handbook.localhost,DNS:pushed.localhost" 2> /dev/null

"$moor" server proxy-config nginx --cert "$work/proxy.crt" --key "$work/proxy.key" --http-port 18080 --https-port 18443 > "$work/moorage.conf"
cat "$work/moorage.conf"
cat > "$work/nginx.conf" <<CONF
pid $work/nginx.pid;
error_log $work/nginx-error.log;
events {}
http {
    access_log $work/nginx-access.log;
    client_body_temp_path $work/body;
    proxy_temp_path $work/proxy;
    fastcgi_temp_path $work/fastcgi;
    uwsgi_temp_path $work/uwsgi;
    scgi_temp_path $work/scgi;
    include $work/moorage.conf;
}
CONF
nginx -v
nginx -e "$work/nginx-error.log" -t -c "$work/nginx.conf"

"$moor" server run > "$work/server.log" 2>&1 &
server=$!
nginx -e "$work/nginx-error.log" -c "$work/nginx.conf"
status=1
cleanup() {
  kill "$(cat "$work/nginx.pid")" 2> /dev/null || true
  "$moor" server stop --all || true
  if [ "$status" != 0 ]; then
    echo "--- the server's log"
    tail -n 30 "$work/server.log"
    echo "--- nginx's log"
    tail -n 30 "$work/nginx-error.log"
  fi
}
trap cleanup EXIT
for _ in $(seq 1 50); do
  curl -fsS --cacert "$work/proxy.crt" -o /dev/null https://moorage.localhost:18443/.moorage/health 2> /dev/null && break
  sleep 0.2
done

url=https://moorage.localhost:18443
token=$(tr -d '\r\n' < "$work/data/keys/admin-token")

echo "--- the doctor, through nginx"
"$moor" server doctor --public-url "$url" --ca-cert "$work/proxy.crt" --timeouts

failures=0
check() { # description, condition
  if eval "$2"; then
    echo "ok    $1"
  else
    echo "FAIL  $1"
    failures=$((failures + 1))
  fi
}
client=(--server "$url" --token "$token" --ca-cert "$work/proxy.crt")
get() { curl -sS --cacert "$work/proxy.crt" "$@"; }

echo "--- a deploy through nginx, with its log streamed back"
(cd examples/static-site && "$moor" deploy "${client[@]}")
check "the site answers" 'get https://handbook.localhost:18443/about | grep -q "About this handbook"'
check "plain HTTP is redirected" '[ "$(curl -sS -o /dev/null -w "%{redirect_url}" http://handbook.localhost:18080/about)" = https://handbook.localhost:18443/about ]'
check "apps know the scheme" '"$moor" apps "${client[@]}" | grep -q https://handbook.localhost'

echo "--- a push over HTTPS through nginx, larger than nginx's default limit"
repo=$work/repo
mkdir "$repo"
cp -R examples/static-site/. "$repo"/
head -c 8000000 /dev/urandom > "$repo/public/large.bin"
(
  cd "$repo"
  git init -q -b main
  git add .
  git -c user.name=CI -c user.email=ci@example.com commit -qm "A large commit"
  git -c http.sslCAInfo="$work/proxy.crt" push "https://moor:$token@moorage.localhost:18443/git/pushed.git" main 2>&1 | tee "$work/push.log"
)
check "the push shows its deploy" 'grep -q "remote: ✔ live" "$work/push.log"'
check "the pushed site answers" 'get https://pushed.localhost:18443/about | grep -q "About this handbook"'
check "the large file went through" '[ "$(get -o /dev/null -w "%{size_download}" https://pushed.localhost:18443/large.bin)" = 8000000 ]'

if [ "$failures" != 0 ]; then
  echo "$failures checks failed"
  exit 1
fi
status=0
echo "everything works behind nginx"
