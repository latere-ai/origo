#!/bin/sh
# install-walk.sh
#
# Spec 018's install check: docs/install.md is walked against a cluster an
# operator would have, from the candidate build of this tree. It builds the
# two candidate images, checks the git inside the node image is at or above
# the 2.40 floor `origod check` enforces (spec 017, spec 018), creates a bare
# kind cluster from deploy/examples/kind/kind.yaml with Cilium installed (the
# file turns kind's own network plugin off, and a real cluster arrives with
# one), loads the images, and runs the fenced sh blocks of the document
# through run-blocks.sh. It ends with the conformance contract through
# ORIGO_LIVE_URL: from the suite's side an installation with no stub wiring
# is a live one, and the six groups of spec 021's skip list are exactly the
# cases that need it.
#
# The cluster is made with `kind create cluster`, not with up.sh, because an
# operator has a cluster and no script of ours. The overlay the document
# applies is the kind example without its test-source component, so the
# installation carries no test source. The kubeconfig lives in a directory
# of its own, so the walk never changes the current context of the caller's
# kubeconfig, and the cluster and that directory are removed on the way
# out, passed or failed. kind needs Docker, or Podman when there is no
# Docker; the images are built and saved with the same engine.
#
# Needs on PATH: kind, helm, kubectl (lateregate puts pinned ones first),
# curl, go, and a container engine. The fixed host ports of kind.yaml
# (30080 to 30900) must be free, so no other kind stack of the repository
# may be up.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
cd "$root"

cluster=origo-install
origod_image=ghcr.io/latere-ai/origod:candidate
stubs_image=ghcr.io/latere-ai/origo-stubs:candidate

for tool in kind helm kubectl curl go awk sort; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "install-walk.sh: $tool is not on PATH" >&2
		exit 2
	fi
done
if command -v docker >/dev/null 2>&1; then
	engine=docker
elif command -v podman >/dev/null 2>&1; then
	engine=podman
	export KIND_EXPERIMENTAL_PROVIDER=podman
else
	echo "install-walk.sh: neither docker nor podman is on PATH" >&2
	exit 2
fi
if kind get clusters 2>/dev/null | grep -qx "$cluster"; then
	echo "install-walk.sh: a kind cluster named $cluster already exists; delete it first" >&2
	exit 2
fi

# The cleanup is armed once the guards have passed, so a refused run
# removes nothing it did not make.
work=$(mktemp -d)
cleanup() {
	status=$?
	if [ "$status" -ne 0 ]; then
		echo "install-walk.sh: failed with status $status; the installation as it stood:" >&2
		diagnose
	fi
	kind delete cluster --name "$cluster" >/dev/null 2>&1 || true
	rm -rf "$work"
	exit "$status"
}

# diagnose says what the installation looked like when a block failed. The
# SSH step of the document is the one that reaches a host port with
# something other than curl, so the account says which hop is missing: the
# node's published ports, the port from inside the node, and the two
# spellings of the loopback host the document and up.sh use.
diagnose() {
	kubectl -n origo get all -o wide || true
	kubectl -n origo get endpoints || true
	kubectl -n origo logs -l app.kubernetes.io/name=origod --tail=60 --prefix || true
	kubectl -n origo describe pod origod-0 || true
	"$engine" port "$cluster-control-plane" || true
	"$engine" exec "$cluster-control-plane" timeout 5 bash -c 'head -c 64 </dev/tcp/127.0.0.1/30022' || true
	for host in 127.0.0.1 localhost; do
		getent ahosts "$host" || true
		ssh-keyscan -v -T 5 -p 30022 "$host" 2>&1 | tail -5 || true
	done
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

export KUBECONFIG="$work/kubeconfig"

echo "install-walk.sh: building the candidate images with $engine"
"$engine" build -t "$origod_image" .
"$engine" build -f Dockerfile.stubs -t "$stubs_image" .
"$engine" save -o "$work/origod.tar" "$origod_image"
"$engine" save -o "$work/origo-stubs.tar" "$stubs_image"

got=$("$engine" run --rm --entrypoint git "$origod_image" --version | awk '{print $3}')
echo "install-walk.sh: git $got in the node image"
if [ "$(printf '%s\n2.40\n' "$got" | sort -V | head -1)" != "2.40" ]; then
	echo "install-walk.sh: git $got is below 2.40" >&2
	exit 1
fi

# The Cilium chart version is pinned in one place, which up.sh sources too.
# shellcheck source=/dev/null
. deploy/examples/kind/versions.env
kind create cluster --name "$cluster" --config deploy/examples/kind/kind.yaml
helm repo add cilium https://helm.cilium.io --force-update
helm install cilium cilium/cilium --version "$CILIUM_CHART_VERSION" \
	--namespace kube-system --set ipam.mode=kubernetes
kubectl -n kube-system rollout status daemonset/cilium --timeout=300s
kind load image-archive --name "$cluster" "$work/origod.tar"
kind load image-archive --name "$cluster" "$work/origo-stubs.tar"

echo "install-walk.sh: walking docs/install.md"
ORIGO_INSTALL_IMAGE=$origod_image ORIGO_INSTALL_MANIFESTS=deploy/examples/kind \
	tools/docs/run-blocks.sh docs/install.md

echo "install-walk.sh: the conformance contract against the installation"
token=$(curl -sf -X POST http://localhost:30081/mint -d '{"sub":"install"}' \
	| sed 's/.*"token":"\([^"]*\)".*/\1/')
ORIGO_LIVE_URL=http://localhost:30180 ORIGO_LIVE_TOKEN=$token \
	go test -v -count=1 -timeout 15m -tags=e2e ./test/conformance/... -run 'TestContract'
