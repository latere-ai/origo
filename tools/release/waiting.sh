#!/bin/sh
# Name every spec that is still at `testing` and says it waits on the
# thing that just happened.
#
# A spec records what holds it as a `Waits on: <token>.` line in its
# Outcome. When a run proves that token, the specs naming it are exactly
# the ones whose status is now out of date. Nothing else in the tree
# connects a green run to the specs waiting behind it, which is how four
# specs sat closed-but-unrecorded after the v0.1.3 release.
#
# Usage: waiting.sh <specs dir> <token>
# Writes one line per spec to stdout and exits 0 whether or not it finds
# any: a release is not failed because a document is behind.
set -eu

dir=${1:?usage: waiting.sh <specs dir> <token>}
token=${2:?usage: waiting.sh <specs dir> <token>}

for f in "$dir"/[0-9][0-9][0-9]-*.md; do
	[ -e "$f" ] || continue
	# The status line of the frontmatter, which is the first one.
	status=$(sed -n 's/^status: *\(.*\)$/\1/p' "$f" | head -1)
	[ "$status" = "testing" ] || continue
	if grep -qF "Waits on: $token" "$f"; then
		printf '%s\n' "${f##*/}"
	fi
done
