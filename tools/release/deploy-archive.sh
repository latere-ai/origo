#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: MIT
#
# deploy-<version>.tar.gz of spec 017: deploy/base and deploy/examples
# with both images pinned to the version, so an operator's overlay
# references one artifact. The base's Deployment carries the origod
# image, the kind overlay pins origod through its images field and
# origo-stubs on the stub pod, and the script refuses to pack while any
# placeholder tag (unreleased, candidate) remains.
#
# Usage: deploy-archive.sh VERSION OUT.tar.gz
set -euo pipefail

version="${1:?usage: deploy-archive.sh VERSION OUT}"
out="${2:?usage: deploy-archive.sh VERSION OUT}"
root="$(cd "$(dirname "$0")/../.." && pwd)"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/deploy"
cp -R "$root/deploy/base" "$root/deploy/examples" "$tmp/deploy/"

pin() {
  local file="$1" from="$2" to="$3"
  grep -q "$from" "$file" || { echo "deploy-archive: $file does not carry $from" >&2; exit 1; }
  sed -i.bak "s|$from|$to|g" "$file" && rm -f "$file.bak"
}
pin "$tmp/deploy/base/deployment.yaml" "ghcr.io/latere-ai/origod:unreleased" "ghcr.io/latere-ai/origod:$version"
pin "$tmp/deploy/examples/kind/kustomization.yaml" "newTag: candidate" "newTag: $version"
pin "$tmp/deploy/examples/kind/origod.yaml" "ghcr.io/latere-ai/origod:candidate" "ghcr.io/latere-ai/origod:$version"
pin "$tmp/deploy/examples/kind/origo-stubs.yaml" "ghcr.io/latere-ai/origo-stubs:candidate" "ghcr.io/latere-ai/origo-stubs:$version"

if grep -rn --include='*.yaml' -e ':unreleased' -e ':candidate' -e 'newTag: candidate' "$tmp/deploy"; then
  echo "deploy-archive: an image is not pinned to $version" >&2
  exit 1
fi

mkdir -p "$(dirname "$out")"
tar -czf "$out" -C "$tmp" deploy
echo "deploy-archive: wrote $out with every image at $version"
