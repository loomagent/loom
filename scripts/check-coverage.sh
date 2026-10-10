#!/usr/bin/env bash
#
# Enforce a floor on total statement coverage.
#
# CI uses -coverpkg=./... so cross-provider acceptance tests count the code they
# actually execute. Keep this statement-coverage ratchet separate from the
# branch-coverage target, and review uncovered failure paths by hand.
set -euo pipefail

profile="${1:-coverage.out}"
floor="${COVERAGE_FLOOR:-90}"

# Runnable examples are main packages with no tests of their own. CI runs them instead,
# which checks more than coverage would, so they stay out of the total.
filtered="$(mktemp)"
trap 'rm -f "${filtered}"' EXIT
grep -v '/examples/' "${profile}" > "${filtered}"

total="$(go tool cover -func="${filtered}" | awk '/^total:/ {sub(/%/, "", $3); print $3}')"
if [[ -z "${total}" ]]; then
	echo "check-coverage: could not read a total from ${profile}" >&2
	exit 1
fi

printf 'total statement coverage: %s%% (floor %s%%)\n' "${total}" "${floor}"
if awk "BEGIN {exit !(${total} + 0 < ${floor})}"; then
	echo "check-coverage: coverage ${total}% is below the ${floor}% floor" >&2
	exit 1
fi
