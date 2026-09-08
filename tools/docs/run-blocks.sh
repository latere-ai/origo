#!/bin/sh
# run-blocks.sh <document>
#
# Runs the fenced `sh` blocks of one Markdown document in order, in this
# shell with set -e, so a document is the test of its own commands and a
# step that drifts from the tree fails (spec 013, "Documents as tests").
# ORIGO_TEST_URL and ORIGO_TEST_ADMIN_TOKEN pass through from the
# environment; the URL defaults to the stack's balanced host port. Spec
# 014 runs docs/migration.md through it and spec 018 docs/install.md.
set -eu

if [ $# -ne 1 ]; then
	echo "usage: $0 <document>" >&2
	exit 2
fi
doc=$1
if [ ! -f "$doc" ]; then
	echo "run-blocks.sh: $doc: no such file" >&2
	exit 2
fi
export ORIGO_TEST_URL="${ORIGO_TEST_URL:-http://localhost:30080}"
export ORIGO_TEST_ADMIN_TOKEN="${ORIGO_TEST_ADMIN_TOKEN:-}"

# The blocks, each preceded by a line naming it, as one script.
script=$(awk '
	/^```sh[[:space:]]*$/ && !inblock { inblock = 1; n++; printf "echo \"run-blocks.sh: block %d\"\n", n; next }
	/^```[[:space:]]*$/ && inblock { inblock = 0; next }
	inblock { print }
' "$doc")
if [ -z "$script" ]; then
	echo "run-blocks.sh: $doc has no sh block" >&2
	exit 2
fi
eval "$script"
