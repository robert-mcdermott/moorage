#!/usr/bin/env bash
# Makes the key that Moorage's releases are signed with: run once, by whoever looks after the project's releases.
#
#   .github/scripts/new-release-key.sh
#
# The private half goes straight to this repository's Actions secrets, as MOORAGE_RELEASE_KEY, and no copy is kept
# here. The public half goes in internal/upgrade/release-keys.txt, to be committed, and every moor from then on will
# only take a release signed with it. If the secret is ever lost, make a new key: moor cannot then upgrade itself
# across that release, and people run the installer once.
set -euo pipefail

cd "$(dirname "$0")/../.."
keys=internal/upgrade/release-keys.txt
if grep -qv '^#' "$keys"; then
  echo "$keys already has a key. To replace it, take that line out first." >&2
  exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
public=$(go run ./internal/upgrade/releasekey new "$work/key")
gh secret set MOORAGE_RELEASE_KEY < "$work/key"
echo "$public" >> "$keys"
echo "The secret is set, and the public key is in $keys, ready to be committed."
