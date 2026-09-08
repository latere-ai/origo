#!/bin/sh
# down.sh [-name <name>]
#
# Deletes the kind cluster up.sh created (origo by default, origo-<name>
# with -name) and the rendered files under out/kind/<name>/. A no-op
# when the cluster does not exist.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
name=origo
cluster=origo
while [ $# -gt 0 ]; do
	case "$1" in
	-name) name=$2; cluster=origo-$2; shift 2 ;;
	-h | -help | --help) echo "usage: $0 [-name <name>]"; exit 0 ;;
	*) echo "usage: $0 [-name <name>]" >&2; exit 2 ;;
	esac
done
if kind get clusters 2>/dev/null | grep -qx "$cluster"; then
	kind delete cluster --name "$cluster"
fi
rm -rf "$root/out/kind/$name"
if [ "$(cat "$root/out/kind/current" 2>/dev/null)" = "$name" ]; then
	rm -f "$root/out/kind/current"
fi
