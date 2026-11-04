#!/usr/bin/env bash
# Runs a server with its own CA, on a CI runner that is thrown away afterwards:
# nothing trusts its certificates; moor trust puts the CA in the system's real
# trust store, and moor, a Go program using that store as browsers do, accepts
# them; moor trust --remove takes it back. On macOS it also tries the resolver
# rule for *.localhost. Not for your own machine: it changes system settings.
set -euo pipefail

if [ "${CI:-}" != "true" ]; then
  echo "this changes system settings, and is meant for CI's throwaway runners" >&2
  exit 2
fi

system=$(uname -s)
work=$(mktemp -d)
moor=$work/moor
case "$system" in MINGW* | MSYS* | CYGWIN*) moor=$work/moor.exe ;; esac
CGO_ENABLED=0 go build -o "$moor" ./cmd/moor # as releases are

# Root, for the system's list. Linux's sudo is asked for by moor trust itself.
privileged() { "$@"; }
if [ "$system" = Darwin ]; then
  privileged() { sudo -E "$@"; }
  # A runner has nobody to confirm the dialog that macOS would show. This may
  # be refused too; then the trust steps below say what macOS said.
  sudo security authorizationdb write com.apple.trust-settings.admin allow || true
fi

export MOORAGE_CONFIG=$work/server.toml MOOR_CONFIG=$work/client.toml
"$moor" server init --domain apps.test --config "$MOORAGE_CONFIG" --data-dir "$work/data" > /dev/null
sed -i.orig -e 's/= ":80"/= "127.0.0.1:18080"/' -e 's/= ":443"/= "127.0.0.1:18443"/' -e 's/= ":2222"/= ""/' "$MOORAGE_CONFIG"
hosts=/etc/hosts
case "$system" in MINGW* | MSYS* | CYGWIN*) hosts=$WINDIR/System32/drivers/etc/hosts ;; esac
if [ "$hosts" = /etc/hosts ]; then
  echo "127.0.0.1 moorage.apps.test" | sudo tee -a /etc/hosts > /dev/null
else
  echo "127.0.0.1 moorage.apps.test" >> "$hosts"
fi

"$moor" server run > "$work/server.log" 2>&1 &
server=$!
status=1
cleanup() {
  kill "$server" 2> /dev/null && wait "$server" 2> /dev/null || true
  if [ "$status" != 0 ]; then
    echo "--- the server's log"
    tail -n 30 "$work/server.log"
  fi
}
trap cleanup EXIT
for _ in $(seq 1 50); do
  curl -fsS -o /dev/null http://127.0.0.1:18080/.moorage/health 2> /dev/null && break
  sleep 0.2
done

url=https://moorage.apps.test:18443
token=$(tr -d '\r\n' < "$work/data/keys/admin-token")
apps() { "$moor" apps --server "$url" --token "$token"; }

failures=0
check() { # description, condition
  if eval "$2"; then
    echo "ok    $1"
  else
    echo "FAIL  $1"
    failures=$((failures + 1))
  fi
}

check "before: the certificate is not trusted" '! apps > /dev/null 2>&1'

fingerprint=$("$moor" trust --print 2>&1 > /dev/null | sed -n 's/.*SHA-256 \([0-9A-F:]*\).*/\1/p')
echo "the CA's fingerprint: $fingerprint"
check "the CA has a fingerprint" '[ -n "$fingerprint" ]'
check "the server's log says it too" 'grep -q "$fingerprint" "$work/server.log"'
check "a wrong fingerprint is refused" '! "$moor" trust --server "$url" --token x --fingerprint AB:CD > /dev/null 2>&1'
check "without a fingerprint, nothing here can confirm it" '! "$moor" trust --server "$url" --token x < /dev/null > /dev/null 2>&1'
check "still not trusted" '! apps > /dev/null 2>&1'

# The doctor fails for the DNS this runner lacks: only what it says matters here.
doctor_says() { "$moor" server doctor --json 2> /dev/null || true; }

echo "--- moor trust"
installed=true
if ! said=$(privileged "$moor" trust --server "$url" --token x --fingerprint "$fingerprint" 2>&1); then
  echo "$said"
  # macOS wants a person to confirm a change of trust, and may leave no way around
  # that here. Anything else is a failure.
  if [ "$system" = Darwin ] && [[ "$said" == *"no user interaction"* ]]; then
    echo "::warning::macOS refused to change the trust store with nobody to confirm: moor trust was not checked for real on this runner"
    installed=false
    check "moor trust says what to do about it" '[[ "$said" == *"at the screen"* ]]'
  else
    failures=$((failures + 1))
    echo "FAIL  moor trust"
    installed=false
  fi
else
  echo "$said"
fi
if [ "$installed" = true ]; then
  check "after: moor accepts its certificates" 'apps > /dev/null'
  if [ "$system" = Linux ]; then
    check "and so does curl" 'curl -fsS -o /dev/null "$url/.moorage/health"'
  fi
  check "doctor sees it" '[[ "$(doctor_says)" == *"this machine trusts the internal CA"* ]]'
  check "moor trust again says so" '[[ "$("$moor" trust 2> /dev/null)" == *"already trusts"* ]]'

  echo "--- moor trust --remove"
  privileged "$moor" trust --remove --server "$url" --token x --fingerprint "$fingerprint"
  check "after removing it: not trusted" '! apps > /dev/null 2>&1'
fi

if [ "$system" = Darwin ]; then
  echo "--- the resolver rule"
  kill "$server" && wait "$server" 2> /dev/null || true
  config=$work/local.toml
  export MOORAGE_CONFIG=$config
  "$moor" server init --preset local --config "$config" --data-dir "$work/local" > /dev/null
  sed -i.orig -e 's/127\.0\.0\.1:8080/127.0.0.1:18081/' -e 's/= ":2222"/= ""/' "$config"
  resolves() { dscacheutil -q host -a name resolver-check.localhost | grep 127.0.0.1 > /dev/null; }
  # Some versions of macOS resolve these names by themselves, so this is no proof
  # of what the rule does: the system's view of it and the responder are, below.
  if resolves; then echo "this macOS already resolves *.localhost"; else echo "this macOS does not resolve *.localhost by itself"; fi
  "$moor" server resolver install
  cat /etc/resolver/localhost
  "$moor" server run > "$work/server.log" 2>&1 &
  server=$!
  for _ in $(seq 1 50); do
    curl -fsS -o /dev/null http://127.0.0.1:18081/.moorage/health 2> /dev/null && break
    sleep 0.2
  done
  sleep 2 # the system takes up new rules on its own time
  scutil --dns | grep -B1 -A4 'domain.*: localhost' || true
  check "the system has taken up the rule" 'scutil --dns | grep -A4 "domain.*: localhost" | grep "port.*: 15353" > /dev/null'
  check "the responder answers for the domain" '[ "$(dig @127.0.0.1 -p 15353 +short resolver-check.localhost A)" = 127.0.0.1 ]'
  check "and refuses anything else" 'dig @127.0.0.1 -p 15353 example.com A | grep "status: REFUSED" > /dev/null'
  check "after: *.localhost resolves" 'resolves'
  check "moor itself resolves it" '"$moor" apps --server http://moorage.localhost:18081 > /dev/null'
  check "doctor agrees" '[[ "$(doctor_says)" == *"localhost resolves on this machine"* ]]'
  "$moor" server resolver remove
  check "the rule is gone" '[ ! -e /etc/resolver/localhost ]'
  check "the setting is gone" '! grep -q "^dns" "$config"'
fi

if [ "$failures" != 0 ]; then
  echo "$failures checks failed"
  exit 1
fi
status=0
echo "trust works on $system"
