#!/usr/bin/env bash
#
# Refuse to release a tag that already has a GitHub release, draft or published.
#
#   scripts/no-existing-release.sh <tag>
#
# A release started from the web UI creates the tag that triggers the release
# workflow, and a failed run can leave a draft, so the release may already
# exist; uploading into it would mix two builds. Drafts are only visible to a
# token that can write. Only gh's exact "release not found" lets a release go
# ahead: any other error fails, so a broken check can never pass as "no release".

set -euo pipefail

tag="${1:?usage: $0 <tag>}"

# Outside the checkout: an untracked file there makes GoReleaser refuse the
# tree as dirty.
err="$(mktemp)"
trap 'rm -f "$err"' EXIT

if gh release view "$tag" >/dev/null 2>"$err"; then
	echo "a release for $tag already exists; push a new tag instead" >&2
	exit 1
fi
if ! grep -qx "release not found" "$err"; then
	cat "$err" >&2
	exit 1
fi
