#!/bin/sh
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: MIT
#
# The test of release.sh (spec 017): against a stub whose GET /readyz
# answers ok and whose GET /version serves v1.2.3, the smoke passes with
# TAG=v1.2.3 and standard input closed, writes its evidence, and fails
# naming the mismatch with another TAG. The Go test beside this file
# starts the stub and names it in STUB_URL.
set -eu

dir=$(cd "$(dirname "$0")" && pwd)
: "${STUB_URL:?STUB_URL names the stub server}"
: "${SHARED_STUB_URL:?SHARED_STUB_URL names the shared-hostname stub}"
# release.sh is a bash script; under the hermetic gate PATH holds no
# /bin, where macOS keeps bash, so the interpreter is resolved here.
bash=$(command -v bash 2>/dev/null || echo /bin/bash)
evidence="${TMPDIR:-/tmp}/release-evidence.$$.md"

# 1. A matching tag passes with standard input closed, the case that
#    tripped the first pipeline run: nothing is piped into the smoke.
if ! out=$(BASE_URL="$STUB_URL" TAG=v1.2.3 OUTPUT_MD="$evidence" "$bash" "$dir/release.sh" 0<&- 2>&1); then
  echo "FAIL release.sh failed with a matching tag:"
  echo "$out"
  exit 1
fi
case "$out" in
  *"served version matches the tag (v1.2.3)"*"release smoke passed"*) ;;
  *) echo "FAIL release.sh passed without the version line:"; echo "$out"; exit 1 ;;
esac
grep -q 'Served version: `v1.2.3`' "$evidence" || { echo "FAIL the evidence does not record the served version"; cat "$evidence"; exit 1; }
rm -f "$evidence" 2>/dev/null || :

# 2. Another tag fails, naming both versions.
if out=$(BASE_URL="$STUB_URL" TAG=v9.9.9 "$bash" "$dir/release.sh" 0<&- 2>&1); then
  echo "FAIL release.sh passed with a mismatched tag:"
  echo "$out"
  exit 1
fi
case "$out" in
  *"version mismatch: served v1.2.3, expected v9.9.9"*) ;;
  *) echo "FAIL the mismatch is not named:"; echo "$out"; exit 1 ;;
esac

# 3. No tag records the served version and passes.
out=$(BASE_URL="$STUB_URL" "$bash" "$dir/release.sh" 0<&- 2>&1) || { echo "FAIL release.sh failed without a tag:"; echo "$out"; exit 1; }
case "$out" in
  *"served version recorded (v1.2.3)"*) ;;
  *) echo "FAIL the served version is not recorded:"; echo "$out"; exit 1 ;;
esac

# 4. A root that is not Origo's fails while the landing check is on, so
#    case 5 proves LANDING=0 skipped it rather than passing anyway.
if out=$(BASE_URL="$SHARED_STUB_URL" "$bash" "$dir/release.sh" 0<&- 2>&1); then
  echo "FAIL release.sh passed with a root that does not name the version:"
  echo "$out"
  exit 1
fi
case "$out" in
  *"the page does not name the served version v1.2.3"*) ;;
  *) echo "FAIL the landing failure is not named:"; echo "$out"; exit 1 ;;
esac

# 5. LANDING=0 is the installation that shares its hostname with the
#    browsing interface: the probes are still Origo's, / is not checked,
#    and the evidence says so.
out=$(BASE_URL="$SHARED_STUB_URL" LANDING=0 OUTPUT_MD="$evidence" "$bash" "$dir/release.sh" 0<&- 2>&1) \
  || { echo "FAIL release.sh failed with LANDING=0:"; echo "$out"; exit 1; }
case "$out" in
  *"/ is served by the browsing interface on this hostname (LANDING=0)"*) ;;
  *) echo "FAIL LANDING=0 is not reported:"; echo "$out"; exit 1 ;;
esac
grep -q 'was not checked: the browsing interface serves it' "$evidence" \
  || { echo "FAIL the evidence does not record the skipped landing check"; cat "$evidence"; exit 1; }
rm -f "$evidence" 2>/dev/null || :

echo "release_test.sh passed"
