#!/bin/sh
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: MIT
#
# The test of deploy-archive.sh: the archive holds deploy/base and
# deploy/examples, every image line in it names the version, no
# placeholder tag survives, and the script refuses a tree whose
# placeholders are gone. The Go test beside this file runs it.
set -eu

dir=$(cd "$(dirname "$0")" && pwd)
bash=$(command -v bash 2>/dev/null || echo /bin/bash)
work="${TMPDIR:-/tmp}/deploy-archive-test.$$"
mkdir -p "$work"
trap 'rm -rf "$work" 2>/dev/null || :' EXIT

"$bash" "$dir/deploy-archive.sh" v9.9.9 "$work/deploy-v9.9.9.tar.gz"
mkdir -p "$work/unpacked"
tar -xzf "$work/deploy-v9.9.9.tar.gz" -C "$work/unpacked"
for f in deploy/base/kustomization.yaml deploy/base/deployment.yaml deploy/examples/kind/kustomization.yaml deploy/examples/kind/origo-stubs.yaml deploy/examples/kind/up.sh; do
  [ -f "$work/unpacked/$f" ] || { echo "FAIL $f is not in the archive"; exit 1; }
done
[ -d "$work/unpacked/deploy/prod" ] && { echo "FAIL deploy/prod is in the archive"; exit 1; }
grep -q 'image: ghcr.io/latere-ai/origod:v9.9.9' "$work/unpacked/deploy/base/deployment.yaml" || { echo "FAIL the base does not pin origod"; exit 1; }
grep -q 'newTag: v9.9.9' "$work/unpacked/deploy/examples/kind/kustomization.yaml" || { echo "FAIL the kind overlay does not pin origod"; exit 1; }
grep -q 'image: ghcr.io/latere-ai/origo-stubs:v9.9.9' "$work/unpacked/deploy/examples/kind/origo-stubs.yaml" || { echo "FAIL the kind overlay does not pin origo-stubs"; exit 1; }
if grep -rn --include='*.yaml' -e ':unreleased' -e ':candidate' -e 'newTag: candidate' "$work/unpacked"; then
  echo "FAIL a placeholder tag survived"; exit 1
fi

# Every ghcr.io image line names the version.
if grep -rhn --include='*.yaml' 'image: ghcr.io/latere-ai/' "$work/unpacked" | grep -v ':v9.9.9'; then
  echo "FAIL an image line is not at v9.9.9"; exit 1
fi

# A tree without the placeholder is refused, not packed.
mkdir -p "$work/tree/tools/release" "$work/tree/deploy"
cp -R "$dir/../../deploy/base" "$dir/../../deploy/examples" "$work/tree/deploy/"
cp "$dir/deploy-archive.sh" "$work/tree/tools/release/"
sed -i.bak 's|origod:unreleased|origod:v0.0.0|' "$work/tree/deploy/base/deployment.yaml" && rm -f "$work/tree/deploy/base/deployment.yaml.bak"
if "$bash" "$work/tree/tools/release/deploy-archive.sh" v9.9.9 "$work/refused.tar.gz" 2>/dev/null; then
  echo "FAIL a tree without the placeholder was packed"; exit 1
fi
[ ! -f "$work/refused.tar.gz" ] || { echo "FAIL the refused archive was written"; exit 1; }

echo "deploy_archive_test.sh passed"
