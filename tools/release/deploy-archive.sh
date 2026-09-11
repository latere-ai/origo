#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: MIT
#
# deploy-<version>.tar.gz of spec 017: deploy/base and deploy/examples
# with both images pinned to the version, plus the installation page
# that describes them, so an operator's overlay references one artifact
# and the prose they follow is the prose for the manifests they have.
# The page ships in the archive because the page on main is always
# newer than the newest release: an install walk against v0.1.1 read a
# page describing an SSH surface that release did not carry. The base's Deployment carries the origod
# image, the kind overlay pins origod through its images field and
# origo-stubs on the stub pod, and the script refuses to pack while any
# placeholder tag (unreleased, candidate) remains.
#
# The namespace both images are published under is ORIGO_IMAGE_NAMESPACE,
# which the release workflow sets from the repository that runs it. The
# manifests in the tree carry the default namespace as their placeholder,
# so the script rewrites the namespace and the version together and a
# fork's archive names the fork's own packages.
#
# Usage: deploy-archive.sh VERSION OUT.tar.gz
set -euo pipefail

version="${1:?usage: deploy-archive.sh VERSION OUT}"
out="${2:?usage: deploy-archive.sh VERSION OUT}"
root="$(cd "$(dirname "$0")/../.." && pwd)"

# The namespace written in the tree, and the one the archive is to name.
default_ns="ghcr.io/latere-ai"
ns="${ORIGO_IMAGE_NAMESPACE:-$default_ns}"
case "$ns" in
  *[[:upper:]]*|*' '*|"") echo "deploy-archive: ORIGO_IMAGE_NAMESPACE '$ns' is not a lowercase image reference prefix" >&2; exit 1 ;;
esac

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/deploy"
cp -R "$root/deploy/base" "$root/deploy/examples" "$tmp/deploy/"
cp "$root/docs/install.md" "$tmp/install.md"

pin() {
  local file="$1" from="$2" to="$3"
  grep -q "$from" "$file" || { echo "deploy-archive: $file does not carry $from" >&2; exit 1; }
  sed -i.bak "s|$from|$to|g" "$file" && rm -f "$file.bak"
}
pin "$tmp/deploy/base/deployment.yaml" "$default_ns/origod:unreleased" "$ns/origod:$version"
pin "$tmp/deploy/examples/kind/kustomization.yaml" "newTag: candidate" "newTag: $version"
pin "$tmp/deploy/examples/kind/origod.yaml" "$default_ns/origod:candidate" "$ns/origod:$version"
pin "$tmp/deploy/examples/kind/origo-stubs.yaml" "$default_ns/origo-stubs:candidate" "$ns/origo-stubs:$version"

# The kind overlay's images field selects the base's image by name, so
# the selector moves with the name it selects or kustomize rewrites
# nothing.
if [ "$ns" != "$default_ns" ]; then
  pin "$tmp/deploy/examples/kind/kustomization.yaml" "name: $default_ns/origod" "name: $ns/origod"
fi

if grep -rn --include='*.yaml' -e ':unreleased' -e ':candidate' -e 'newTag: candidate' "$tmp/deploy"; then
  echo "deploy-archive: an image is not pinned to $version" >&2
  exit 1
fi

# Nothing in a fork's archive names the namespace the tree was written
# with: a leftover would pull an image the fork cannot have published.
if [ "$ns" != "$default_ns" ] && grep -rn --include='*.yaml' "$default_ns/" "$tmp/deploy"; then
  echo "deploy-archive: $default_ns survives in an archive for $ns" >&2
  exit 1
fi

mkdir -p "$(dirname "$out")"
tar -czf "$out" -C "$tmp" deploy install.md
echo "deploy-archive: wrote $out with every image at $ns at $version"
