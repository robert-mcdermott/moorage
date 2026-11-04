#!/bin/sh
# Says which of CI's slower jobs on real systems a pull request needs, as name=true or
# name=false lines for $GITHUB_OUTPUT: those whose files it touches. Without a pull request
# to compare with ($BASE), which is on main and for a release, it needs them all. So it does if
# anything goes wrong in finding out, or the workflow or the dependencies change.
set -u

all() {
	for job in installer trust service upgrade isolation backup; do
		echo "$job=true"
	done
	exit 0
}

[ -n "${BASE:-}" ] || all
changed=$(git diff --name-only "$BASE" HEAD 2>/dev/null) || all
if printf '%s\n' "$changed" | grep -Eq '^(\.github/workflows/ci\.yml|\.github/scripts/ci-changes\.sh|go\.mod|go\.sum)$'; then
	all
fi

touches() {
	if printf '%s\n' "$changed" | grep -Eq "$2"; then
		echo "$1=true"
	else
		echo "$1=false"
	fi
}

touches installer '^(install\.sh|install\.ps1|\.github/scripts/install-check)'
touches trust '^(internal/truststore/|internal/localdns/|internal/ingress/tls|internal/cli/(trust|resolver)|\.github/scripts/trust-check)'
touches service '^(cmd/|internal/(service|shim|reconcile|server|platform)/|internal/cli/(service|server)|\.github/scripts/service-check)'
touches isolation '^(cmd/|internal/(isolate|shim|reconcile|oneoff|build|server|platform|config)/|\.github/scripts/isolation-check)'
touches backup '^(cmd/|internal/(backup|store|secrets|server|layout)/|internal/cli/backup|\.github/scripts/backup-check)'
touches upgrade '^(cmd/|internal/(store|shim|reconcile|server|platform|config|secrets)/|\.github/scripts/upgrade-check)'
