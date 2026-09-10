#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: MIT
#
# Post-deploy smoke for a released origod. The release pipeline runs it
# after the rollout and attaches its markdown evidence to the release.
#
# Required tools: curl, grep.
#
# Environment:
#   BASE_URL       the public origin, default https://git.latere.ai
#   TAG            release tag, for evidence output; the served version must match
#   COMMIT         release commit, for evidence output
#   DEPLOY_URL     deploy workflow URL, for evidence output
#   OUTPUT_MD      optional path for markdown evidence
#   EXPECTED_ASSET, SERVICE_TOKEN  accepted (pipeline contract), unused

set -euo pipefail

BASE_URL="${BASE_URL:-https://git.latere.ai}"
BASE_URL="${BASE_URL%/}"
TAG="${TAG:-}"
COMMIT="${COMMIT:-unknown}"
DEPLOY_URL="${DEPLOY_URL:-}"
OUTPUT_MD="${OUTPUT_MD:-}"

pass() { printf 'OK %s\n' "$*"; }
fail() { printf 'FAIL %s\n' "$*" >&2; exit 1; }

for cmd in curl grep; do
  command -v "$cmd" >/dev/null || fail "$cmd is required"
done

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

check_status() {
  local label="$1" path="$2" want="$3" out="$4" code
  # --retry rides out the transient 5xx a load balancer emits during a
  # rollout cutover.
  code=$(curl -sS --retry 5 --retry-delay 2 --retry-max-time 30 \
    -o "$out" -w '%{http_code}' "${BASE_URL}${path}") || fail "$label: curl failed"
  [ "$code" = "$want" ] || fail "$label: expected $want, got $code (body: $(head -c 200 "$out" 2>/dev/null || true))"
  pass "$label ($code)"
}

check_status "GET /readyz" "/readyz" "200" "$tmp/readyz"
grep -qx ok "$tmp/readyz"

check_status "GET /version" "/version" "200" "$tmp/version"
served=$(grep -o '"version":"[^"]*"' "$tmp/version" | head -1 | cut -d'"' -f4)
[ -n "$served" ] || fail "GET /version: no version in $(cat "$tmp/version")"
if [ -n "$TAG" ]; then
  [ "$served" = "$TAG" ] || fail "version mismatch: served $served, expected $TAG"
  pass "served version matches the tag ($served)"
else
  pass "served version recorded ($served)"
fi

# The landing page (spec 022). A browser that opens the installation must
# read a page, not a credential dialog: the 200 without an Authorization
# header is that proof, and the version on it is the one just rolled out.
check_status "GET /" "/" "200" "$tmp/root"
grep -q "$served" "$tmp/root" || fail "GET /: the page does not name the served version $served"
pass "the landing page names the served version"

if [ -n "$OUTPUT_MD" ]; then
  {
    echo "<!-- release-evidence -->"
    echo
    echo "## Release Evidence"
    echo
    echo "- Tag: \`${TAG:-unknown}\`"
    echo "- Commit: \`${COMMIT}\`"
    [ -n "$DEPLOY_URL" ] && echo "- Deploy: ${DEPLOY_URL}"
    echo "- Served version: \`${served}\`"
    echo "- Smoke: \`GET /readyz\`, \`GET /version\`, and \`GET /\` returned 200"
  } > "$OUTPUT_MD"
fi

printf '\nrelease smoke passed\n'
