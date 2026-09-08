#!/bin/sh
# The test of run-blocks.sh: the blocks of a fixture document run in
# order in one shell, a failing block stops the run before the next, and
# ORIGO_TEST_URL and ORIGO_TEST_ADMIN_TOKEN pass through. Run by
# TestRunBlocks in this directory; exits non-zero on the first failure.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/run-blocks.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
fail() {
	echo "run_blocks_test.sh: $*" >&2
	exit 1
}

# 1. Three blocks in order, one shell: a variable from the first is seen
# by the third, a bash block and prose are ignored, and both variables
# are in the environment.
cat >"$tmp/doc.md" <<'DOC'
# A document

Prose that is not run.

```sh
first=one
printf 'a\n' >> "$OUT"
```

```bash
printf 'ignored\n' >> "$OUT"
```

```sh
printf 'b %s\n' "$ORIGO_TEST_URL" >> "$OUT"
```

```sh
printf 'c %s %s\n' "$first" "$ORIGO_TEST_ADMIN_TOKEN" >> "$OUT"
```
DOC
OUT="$tmp/out" ORIGO_TEST_URL=http://stack.example ORIGO_TEST_ADMIN_TOKEN=tok /bin/sh "$here/run-blocks.sh" "$tmp/doc.md" >"$tmp/log" || fail "the document failed: $(cat "$tmp/log")"
expected='a
b http://stack.example
c one tok'
[ "$(cat "$tmp/out")" = "$expected" ] || fail "blocks ran as: $(cat "$tmp/out")"
grep -q 'run-blocks.sh: block 3' "$tmp/log" || fail "the log does not name block 3"

# 2. The URL defaults to the stack's host port.
cat >"$tmp/default.md" <<'DOC'
```sh
printf '%s\n' "$ORIGO_TEST_URL" > "$OUT"
```
DOC
(unset ORIGO_TEST_URL; OUT="$tmp/out2" /bin/sh "$here/run-blocks.sh" "$tmp/default.md" >/dev/null) || fail "the default run failed"
[ "$(cat "$tmp/out2")" = "http://localhost:30080" ] || fail "default URL: $(cat "$tmp/out2")"

# 3. A failing block stops the run: the second block never runs and the
# exit is non-zero.
cat >"$tmp/failing.md" <<'DOC'
```sh
printf 'ran\n' >> "$OUT"
false
```

```sh
printf 'never\n' >> "$OUT"
```
DOC
if OUT="$tmp/out3" /bin/sh "$here/run-blocks.sh" "$tmp/failing.md" >/dev/null 2>&1; then
	fail "a failing block did not fail the run"
fi
[ "$(cat "$tmp/out3")" = "ran" ] || fail "after the failure: $(cat "$tmp/out3")"

# 4. Usage errors: no argument, a missing file, a document without a
# block.
/bin/sh "$here/run-blocks.sh" >/dev/null 2>&1 && fail "no argument accepted"
/bin/sh "$here/run-blocks.sh" "$tmp/nope.md" >/dev/null 2>&1 && fail "a missing file accepted"
printf 'no blocks\n' >"$tmp/empty.md"
/bin/sh "$here/run-blocks.sh" "$tmp/empty.md" >/dev/null 2>&1 && fail "a document without a block accepted"
echo "run_blocks_test.sh: ok"
