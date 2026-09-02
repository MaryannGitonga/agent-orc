#!/usr/bin/env bash
#
# Enforce agent-orc's commit policy over a commit range.
#
#   scripts/check-commits.sh [<range>]
#
# <range> defaults to origin/main..HEAD. Every commit in the range must:
#   * have a one-line, conventionally-prefixed subject of at most 72 chars,
#   * carry no AI-attribution trailer,
#   * carry a DCO Signed-off-by trailer matching its author,
#   * be cryptographically signed.
#
# Signature *verification* (that GitHub trusts the key) is checked separately
# in CI via the API, since the public key is not on the runner.

set -uo pipefail

RANGE="${1:-origin/main..HEAD}"

# Trailers that must never appear. Matched case-insensitively.
FORBIDDEN_PATTERNS=(
	'co-authored-by:[[:space:]]*claude'
	'co-authored-by:[[:space:]]*copilot'
	'co-authored-by:[[:space:]]*codex'
	'co-authored-by:[[:space:]]*.*\[bot\]'
	'generated with \[claude code\]'
	'🤖 generated with'
	'claude-session:'
	'assisted-by:[[:space:]]*claude'
)

CONVENTIONAL='^(feat|fix|ci|docs|test|refactor|chore|perf|build|revert)(\([a-z0-9._/-]+\))?!?: .+'
MAX_SUBJECT=72

fail=0
err() {
	printf '  ✗ %s\n' "$1"
	fail=1
}

commits=$(git rev-list --no-merges "$RANGE") || {
	echo "cannot resolve range '$RANGE'" >&2
	exit 2
}

if [ -z "$commits" ]; then
	echo "no commits in range '$RANGE' — nothing to check"
	exit 0
fi

for sha in $commits; do
	subject=$(git log -1 --format=%s "$sha")
	body=$(git log -1 --format=%b "$sha")
	author="$(git log -1 --format='%an <%ae>' "$sha")"
	printf '%s %s\n' "${sha:0:8}" "$subject"

	# 1. Subject: conventional prefix, single line, length-capped.
	if ! printf '%s' "$subject" | grep -Eq "$CONVENTIONAL"; then
		err "subject must start with feat|fix|ci|docs|test|refactor|chore|perf|build|revert followed by ': '"
	fi
	if [ "${#subject}" -gt "$MAX_SUBJECT" ]; then
		err "subject is ${#subject} chars, max is $MAX_SUBJECT"
	fi

	# 2. Body: sign-off trailers only, no prose paragraphs, no attribution.
	while IFS= read -r line; do
		[ -z "$line" ] && continue
		if ! printf '%s' "$line" | grep -Eq '^[A-Za-z-]+:[[:space:]]'; then
			err "body must contain trailers only, found prose: '$line'"
		fi
	done <<<"$body"

	for pattern in "${FORBIDDEN_PATTERNS[@]}"; do
		if printf '%s\n%s' "$subject" "$body" | grep -Eiq "$pattern"; then
			err "contains a forbidden attribution trailer matching /$pattern/"
		fi
	done

	# 3. DCO: a Signed-off-by matching the commit author.
	if ! printf '%s' "$body" | grep -Fqx "Signed-off-by: $author"; then
		err "missing DCO sign-off 'Signed-off-by: $author' (commit with -s)"
	fi

	# 4. Signature present. 'N' means unsigned; anything else is a signature
	#    this runner may or may not have the key to verify.
	if [ "$(git log -1 --format='%G?' "$sha")" = "N" ]; then
		err "commit is not signed (set commit.gpgsign=true or commit with -S)"
	fi
done

if [ "$fail" -ne 0 ]; then
	echo
	echo "commit policy violations found — see the commit policy in README.md" >&2
	exit 1
fi

echo
echo "all commits in '$RANGE' satisfy the commit policy"
