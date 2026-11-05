#!/bin/sh
# Says what CI needs to run, as name=value lines for $GITHUB_OUTPUT. It tests what has changed, and
# no code twice:
#
#   tested  this exact tree has already passed, in a pull request say, and nothing that it ran need run again
#   code    the jobs that test the code, which a change only to the documentation does not need
#   docs    the job that checks the documentation, for such a change
#   tree, record, note
#           what the job at the end keeps a note of: which tree passed, with which of the slower jobs
#   installer, trust, service, upgrade, isolation, backup, real
#           the slower jobs on real systems. A pull request runs those whose files it touches. Main runs the rest.
#
# It falls back on running more, if anything goes wrong in finding out: a mistake here should cost time, and
# never a test. A run that was asked for by hand ($EVENT is workflow_dispatch) runs them all.
#
# It reads $BASE, what a pull request is compared with, and $BEFORE, what a push to main came after.
set -u

slower="installer trust service upgrade isolation backup real"

tree=$(git rev-parse 'HEAD^{tree}')
echo "tree=$tree"

# What this tree has passed with. A note from a fork's pull request does not count, as that ran with a workflow anyone could have changed.
earlier=""
tested=false
if [ "${EVENT:-}" != workflow_dispatch ]; then
	notes=$(gh api "repos/$GITHUB_REPOSITORY/actions/artifacts?per_page=100" --jq \
		"[.artifacts[] | select(.expired == false and .workflow_run.repository_id == .workflow_run.head_repository_id and (.name | startswith(\"ci-passed-$tree-\"))) | .name] | join(\" \")" 2>/dev/null) || notes=""
	for note in $notes; do
		case "$note" in
		ci-passed-"$tree"-*) ;;
		*) continue ;;
		esac
		tested=true
		ran=${note#ci-passed-"$tree"-}
		case "$ran" in
		all) earlier=$slower ;;
		none) ;;
		*) earlier="$earlier $(printf '%s' "$ran" | tr . ' ')" ;;
		esac
	done
fi

has() {
	case " $earlier " in
	*" $1 "*) return 0 ;;
	esac
	return 1
}

# Decide what the change wants, from what it touches.
changed=""
compare=""
if [ -n "${BASE:-}" ]; then
	compare=$BASE
elif [ -n "${BEFORE:-}" ] && [ "${EVENT:-}" != workflow_dispatch ]; then
	compare=$BEFORE
fi
if [ -n "$compare" ]; then
	changed=$(git diff --name-only "$compare" HEAD 2>/dev/null) || changed=""
fi

docs_only=false
if [ -n "$changed" ] && ! printf '%s\n' "$changed" | grep -Eqv '^(docs/|spec/|[^/]*\.md$|LICENSE$)'; then
	docs_only=true
fi

# Whether a pull request's change touches what a slower job tests. Anything else wants them all: main and a run by hand, and
# a change to this, to the workflow or to the dependencies.
everything=true
if [ -n "${BASE:-}" ] && [ -n "$changed" ] && ! printf '%s\n' "$changed" | grep -Eq '^(\.github/workflows/ci\.yml|\.github/scripts/ci-changes|go\.mod|go\.sum)'; then
	everything=false
fi

wants() {
	[ "$docs_only" = true ] && return 1
	[ "$everything" = true ] && return 0
	case "$1" in
	installer) pattern='^(install\.sh|install\.ps1|\.github/scripts/install-check)' ;;
	trust) pattern='^(internal/truststore/|internal/localdns/|internal/ingress/tls|internal/cli/(trust|resolver)|\.github/scripts/trust-check)' ;;
	service) pattern='^(cmd/|internal/(service|shim|reconcile|server|platform|upgrade)/|internal/cli/(service|server|upgrade)|\.github/scripts/service-check)' ;;
	isolation) pattern='^(cmd/|internal/(isolate|shim|reconcile|oneoff|build|server|platform|config)/|\.github/scripts/isolation-check)' ;;
	backup) pattern='^(cmd/|internal/(backup|store|secrets|server|layout)/|internal/cli/backup|\.github/scripts/backup-check)' ;;
	real) pattern='^(cmd/|internal/(forge|telemetry|notify)/|internal/gitserver/|internal/ingress/(acme|proxy)|testdata/grpc/|\.github/scripts/real-services)' ;;
	upgrade) pattern='^(cmd/|internal/(store|shim|reconcile|server|platform|config|secrets)/|\.github/scripts/upgrade-check)' ;;
	esac
	printf '%s\n' "$changed" | grep -Eq "$pattern"
}

record=""
missing=false
fresh=false
for job in $slower; do
	if has "$job"; then
		echo "$job=false"
		record="$record $job"
	elif wants "$job"; then
		echo "$job=true"
		record="$record $job"
		fresh=true
	else
		echo "$job=false"
		missing=true
	fi
done

echo "tested=$tested"
if [ "$tested" = true ] || [ "$docs_only" = true ]; then
	echo "code=false"
else
	echo "code=true"
	fresh=true
fi
if [ "$tested" = false ] && [ "$docs_only" = true ]; then
	echo "docs=true"
	fresh=true
else
	echo "docs=false"
fi

# What passing this run will mean for this tree. A tree that only the documentation job looked at is noted as well, so that merging it does not check it again.
if [ "$missing" = false ]; then
	echo "record=all"
elif [ -z "$record" ]; then
	echo "record=none"
else
	echo "record=$(echo $record | tr ' ' .)"
fi
echo "note=$fresh"
