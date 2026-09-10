#!/bin/sh
# up.sh [-name <name>] [-port-offset <n>] <origod.tar> <origo-stubs.tar>
#
# Creates the kind stack of spec 013: the cluster from kind.yaml (origo
# by default, origo-<name> with -name; -port-offset adds n to every host
# port of the ports table), loads the two image tarballs, installs
# Cilium and metrics-server at the versions versions.env pins, generates
# the Secrets the overlay does not carry (ORIGO_TOKEN_KEY and the source
# stub's CA), applies the overlay with the test-source component, and
# waits until every pod is ready and every host port answers, at most 5
# minutes. The rendered files live under out/kind/<name>/. down.sh
# deletes the cluster.
set -eu

usage() {
	echo "usage: $0 [-name <name>] [-port-offset <n>] <origod.tar> <origo-stubs.tar>"
}

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
name=origo
cluster=origo
PORT_OFFSET=0
while [ $# -gt 0 ]; do
	case "$1" in
	-name) name=$2; cluster=origo-$2; shift 2 ;;
	-port-offset) PORT_OFFSET=$2; shift 2 ;;
	-h | -help | --help) usage; exit 0 ;;
	-*) usage >&2; exit 2 ;;
	*) break ;;
	esac
done
if [ $# -ne 2 ]; then
	usage >&2
	exit 2
fi
origod_tar=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
stubs_tar=$(cd "$(dirname "$2")" && pwd)/$(basename "$2")
for tar in "$origod_tar" "$stubs_tar"; do
	if [ ! -f "$tar" ]; then
		echo "up.sh: $tar: no such file" >&2
		exit 2
	fi
done
for tool in kind kubectl helm curl openssl ssh-keygen awk; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "up.sh: $tool is not on PATH" >&2
		exit 2
	fi
done

# shellcheck source=versions.env
. "$here/versions.env"
out="$root/out/kind/$name"
mkdir -p "$out"
start=$(date +%s)
deadline=$((start + 300))

# remaining prints the seconds left of the 5 minute budget, at least 1.
remaining() {
	left=$((deadline - $(date +%s)))
	if [ "$left" -lt 1 ]; then left=1; fi
	echo "$left"
}

# wait_for <what> <command...> polls the command every 2 seconds until it
# succeeds or the budget is spent.
wait_for() {
	what=$1
	shift
	while ! "$@" >/dev/null 2>&1; do
		if [ "$(date +%s)" -ge "$deadline" ]; then
			echo "up.sh: $what did not become ready within 5 minutes" >&2
			kubectl get pods -A >&2 || true
			exit 1
		fi
		sleep 2
	done
	echo "up.sh: $what ready"
}

# ssh_answers reports whether an SSH listener on the host port has
# completed a handshake. ssh-keyscan needs no key and no account: it
# reads the host key the listener presents and nothing else.
ssh_answers() {
	test -n "$(ssh-keyscan -T 5 -p "$1" 127.0.0.1 2>/dev/null)"
}

sha256_check() {
	if command -v sha256sum >/dev/null 2>&1; then
		echo "$1  $2" | sha256sum -c - >/dev/null
	else
		echo "$1  $2" | shasum -a 256 -c - >/dev/null
	fi
}

port() {
	echo $(($1 + PORT_OFFSET))
}

# 1. The cluster, from kind.yaml with the offset added to every hostPort
# line: the one place the offset is applied.
awk -v off="$PORT_OFFSET" '/^[[:space:]]*hostPort:/ { sub(/[0-9]+[[:space:]]*$/, $NF + off) } { print }' "$here/kind.yaml" >"$out/kind.yaml"
kind create cluster --name "$cluster" --config "$out/kind.yaml"
context="kind-$cluster"
kubectl config use-context "$context" >/dev/null

# 2. The candidate images, before anything is applied.
kind load image-archive --name "$cluster" "$origod_tar"
kind load image-archive --name "$cluster" "$stubs_tar"

# 3. Cilium at the pinned chart version, in place of kindnet.
helm repo add cilium https://helm.cilium.io --force-update >/dev/null
helm install cilium cilium/cilium --version "$CILIUM_CHART_VERSION" --namespace kube-system \
	--set ipam.mode=kubernetes >/dev/null
echo "up.sh: cilium $CILIUM_CHART_VERSION installing"

# 4. metrics-server: the release manifest checked against its sha256,
# the image by digest, and the flag that accepts kind's kubelet
# certificates.
curl -fsSL -o "$out/metrics-server.upstream.yaml" \
	"https://github.com/kubernetes-sigs/metrics-server/releases/download/$METRICS_SERVER_VERSION/components.yaml"
sha256_check "$METRICS_SERVER_MANIFEST_SHA256" "$out/metrics-server.upstream.yaml"
awk -v image="$METRICS_SERVER_IMAGE" '
	/image: registry.k8s.io\/metrics-server\/metrics-server:/ { sub(/image: .*/, "image: " image) }
	{ print }
	/--metric-resolution=/ { sub(/--metric-resolution=.*/, "--kubelet-insecure-tls"); print }
' "$out/metrics-server.upstream.yaml" >"$out/metrics-server.yaml"
kubectl apply -f "$out/metrics-server.yaml" >/dev/null

# 5. The Secrets the overlay does not carry: ORIGO_TOKEN_KEY, generated
# the way the install document does, the SSH host keys of spec 024, the
# same set on every pod, and the source stub's CA with the two SANs, its
# certificate alone in the ConfigMap the nodes mount.
openssl ecparam -genkey -name prime256v1 -out "$out/token-key.pem" 2>/dev/null
rm -f "$out/ssh_host_ed25519_key" "$out/ssh_host_ed25519_key.pub" "$out/ssh_host_ecdsa_key" "$out/ssh_host_ecdsa_key.pub"
ssh-keygen -q -t ed25519 -N "" -C "" -f "$out/ssh_host_ed25519_key"
ssh-keygen -q -t ecdsa -b 256 -N "" -C "" -f "$out/ssh_host_ecdsa_key"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 3650 \
	-keyout "$out/ca.key" -out "$out/ca.crt" -subj "/CN=origo-stubs CA" \
	-addext "subjectAltName=DNS:origo-stubs.origo.svc,DNS:localhost" 2>/dev/null
{
	kubectl create secret generic origod-token-key --namespace origo --dry-run=client -o yaml \
		--from-file=ORIGO_TOKEN_KEY="$out/token-key.pem"
	echo ---
	kubectl create secret generic origod-ssh-host-key --namespace origo --dry-run=client -o yaml \
		--from-file=ssh_host_ed25519_key="$out/ssh_host_ed25519_key" \
		--from-file=ssh_host_ecdsa_key="$out/ssh_host_ecdsa_key"
	echo ---
	kubectl create secret generic origo-stubs-ca --namespace origo --dry-run=client -o yaml \
		--from-file=ca.crt="$out/ca.crt" --from-file=ca.key="$out/ca.key"
	echo ---
	kubectl create configmap origo-stub-ca --namespace origo --dry-run=client -o yaml \
		--from-file=stub-ca.pem="$out/ca.crt"
} >"$out/secrets.yaml"

# 6. The overlay with the component and the Secrets, as one kustomization.
cat >"$out/kustomization.yaml" <<KUSTOMIZATION
# Rendered by up.sh: the kind overlay, the test-source component, and the
# generated Secrets. cluster.Apply of a test targets this directory.
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: origo
resources:
  - ../../../deploy/examples/kind
  - secrets.yaml
components:
  - ../../../deploy/examples/kind/test-source
KUSTOMIZATION
kubectl apply -k "$out" >/dev/null

# 7. Everything ready: Cilium, metrics-server, the pods of the tables,
# the bucket, and every host port of the ports table.
wait_for "cilium" kubectl -n kube-system rollout status daemonset/cilium --timeout=10s
wait_for "metrics-server" kubectl -n kube-system rollout status deployment/metrics-server --timeout=10s
wait_for "minio" kubectl -n origo rollout status deployment/minio --timeout=10s
wait_for "the bucket" kubectl -n origo wait --for=condition=complete job/minio-init --timeout=10s
wait_for "origo-stubs" kubectl -n origo rollout status deployment/origo-stubs --timeout=10s
wait_for "origod-0, origod-1, origod-2" kubectl -n origo rollout status statefulset/origod --timeout=10s
wait_for "port $(port 30080)" curl -fsS "http://localhost:$(port 30080)/version"
for n in 0 1 2; do
	wait_for "port $(port $((30180 + n)))" curl -fsS "http://localhost:$(port $((30180 + n)))/version"
	wait_for "port $(port $((30190 + n)))" curl -fsS "http://localhost:$(port $((30190 + n)))/metrics"
done
wait_for "port $(port 30081)" curl -fsS "http://localhost:$(port 30081)/jwks"
wait_for "port $(port 30082)" curl -fsS "http://localhost:$(port 30082)/requests"
wait_for "port $(port 30083)" curl -fsS "http://localhost:$(port 30083)/deliveries"
wait_for "port $(port 30084)" curl -fsS --cacert "$out/ca.crt" "https://localhost:$(port 30084)/ca.pem"
wait_for "port $(port 30085)" curl -fsS "http://localhost:$(port 30085)/"
wait_for "port $(port 30086)" curl -fsS -X DELETE "http://localhost:$(port 30086)/requests"
wait_for "port $(port 30022)" ssh_answers "$(port 30022)"
for n in 0 1 2; do
	wait_for "port $(port $((30122 + n)))" ssh_answers "$(port $((30122 + n)))"
done
wait_for "port $(port 30900)" curl -fsS "http://localhost:$(port 30900)/minio/health/live"

# Every port answering does not mean the nodes hold the issuer's keys:
# a node that started before the stubs fetched nothing and retries once
# a minute (spec 007). The stack is up when a token minted at the
# issuer's host port reaches the authorizer through every node, which
# denies the probe id: each node's own port is checked, because the
# balanced port answers from whichever node has the keys first, and a
# test that names a node, or deletes the one that answered, would find
# the others still without them.
token=$(curl -fsS -X POST "http://localhost:$(port 30081)/mint" -d '{"sub":"dev"}' | sed 's/.*"token":"\([^"]*\)".*/\1/')
identity_ready() {
	for p in 30080 30180 30181 30182; do
		[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token" \
			"http://localhost:$(port $p)/v1/repos/00000000-0000-0000-0000-000000000001")" = 403 ] || return 1
	done
}
wait_for "the identity path on every node" identity_ready

# 8. The CA on the runner, so a test trusts the source's host port, and
# the name of this cluster as the current one, so cluster.Apply of a test
# finds the rendered kustomization without asking kubectl.
cp "$out/ca.crt" "$root/test/e2e/testdata/stub-ca.pem"
printf '%s\n' "$name" >"$root/out/kind/current"
echo "up.sh: cluster $cluster is up in $(($(date +%s) - start))s; ORIGO_TEST_URL=http://localhost:$(port 30080)"
