#!/bin/sh
# Tries what ci-changes.sh decides, against a small repository and a stand-in for gh. It decides whether tests run, so that a
# mistake in it would look like tests that pass.
set -eu

script=$(cd "$(dirname "$0")" && pwd)/ci-changes.sh
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
mkdir "$dir/bin" "$dir/repo"
export PATH="$dir/bin:$PATH" GITHUB_REPOSITORY=owner/repo GH_NOTES="$dir/notes"
cat > "$dir/bin/gh" <<'STUB'
#!/bin/sh
if [ -e "$GH_NOTES.broken" ]; then exit 1; fi
cat "$GH_NOTES" 2>/dev/null || true
STUB
chmod +x "$dir/bin/gh"
cd "$dir/repo"
git init -q -b main .
git config user.name Test
git config user.email test@example.com
mkdir -p internal/truststore internal/store docs .github/workflows
echo one > internal/store/a.go
echo one > internal/truststore/a.go
echo one > docs/a.md
echo one > .github/workflows/ci.yml
git add . && git commit -qm first
first=$(git rev-parse HEAD)

failed=0
expect() { # what it should say, as name=value words, with the environment already set
	name=$1
	shift
	got=$(EVENT=${EVENT:-pull_request} BASE=${BASE:-} BEFORE=${BEFORE:-} "$script")
	for want in "$@"; do
		if ! printf '%s\n' "$got" | grep -qx "$want"; then
			echo "$name: wanted $want, in:" >&2
			printf '%s\n' "$got" >&2
			failed=1
		fi
	done
}

# A change to the code, in a pull request.
echo two > internal/truststore/a.go
git add . && git commit -qm code
tree=$(git rev-parse 'HEAD^{tree}')
BASE=$first
expect "a pull request" tested=false code=true docs=false trust=true service=false installer=false "record=trust" note=true

# The same tree on main, with that note: what ran does not run again, and what did not does.
echo "ci-passed-$tree-trust" > "$GH_NOTES"
BASE="" BEFORE=$first EVENT=push
expect "main after it" tested=true code=false docs=false trust=false service=true installer=true upgrade=true record=all note=true

# With that done too, nothing runs, and there is nothing new to note.
echo "ci-passed-$tree-trust ci-passed-$tree-all" > "$GH_NOTES"
expect "main again" tested=true code=false docs=false trust=false service=false real=false record=all note=false

# Another tree's note counts for nothing.
echo "ci-passed-0000000000000000000000000000000000000000-all" > "$GH_NOTES"
expect "another tree" tested=false code=true trust=true service=true record=all

# If it cannot find out, it runs everything.
touch "$GH_NOTES.broken"
expect "gh failing" tested=false code=true trust=true service=true
rm "$GH_NOTES.broken"

# Asked for by hand, it runs everything whatever has been noted.
echo "ci-passed-$tree-all" > "$GH_NOTES"
EVENT=workflow_dispatch
expect "by hand" tested=false code=true trust=true service=true record=all note=true
EVENT=push
: > "$GH_NOTES"

# A change only to the documentation.
second=$(git rev-parse HEAD)
echo two > docs/a.md
echo two > CHANGELOG.md
git add . && git commit -qm words
BASE=$second BEFORE="" EVENT=pull_request
expect "the documentation, in a pull request" tested=false code=false docs=true trust=false service=false record=none note=true
tree=$(git rev-parse 'HEAD^{tree}')
echo "ci-passed-$tree-none" > "$GH_NOTES"
BASE="" BEFORE=$second EVENT=push
expect "the documentation, on main" tested=true code=false docs=false trust=false service=false note=false
: > "$GH_NOTES"

# Documentation with code is code.
third=$(git rev-parse HEAD)
echo three > docs/a.md
echo three > internal/store/a.go
git add . && git commit -qm both
BASE=$third BEFORE="" EVENT=pull_request
expect "both" code=true docs=false backup=true upgrade=true trust=false

# A change to the workflow wants everything.
fourth=$(git rev-parse HEAD)
echo two > .github/workflows/ci.yml
git add . && git commit -qm workflow
BASE=$fourth
expect "the workflow" code=true trust=true service=true record=all

# Comparing with something that is not there is not knowing.
BASE=0000000000000000000000000000000000000000
expect "no such base" code=true trust=true service=true record=all
BASE="" BEFORE=0000000000000000000000000000000000000000 EVENT=push
expect "a new branch" code=true trust=true service=true record=all

if [ "$failed" = 0 ]; then
	echo "ci-changes.sh decides as it should"
fi
exit "$failed"
