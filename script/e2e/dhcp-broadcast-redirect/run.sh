#!/usr/bin/env bash
#
# End to end test for the DHCP broadcast redirect (pkg/dhcpredirect) against a
# real Cilium datapath.
#
# It stands up a kind cluster with no CNI and no kube-proxy, installs Cilium,
# runs Smee in an ordinary pod on the Cilium pod network, and then broadcasts a
# DHCPDISCOVER from a separate container attached to the same docker network as
# the node. The pod has no interface on that network, so the only way the
# handshake can complete is if the eBPF programs carried the packets across in
# both directions.
#
# Requires: docker, kind, kubectl, helm, go. Nothing has to be installed on the
# host beyond that; no root is needed.
#
# Usage: script/e2e/dhcp-broadcast-redirect/run.sh [--keep]
#   --keep   leave the cluster running afterwards for poking at

set -euo pipefail

CLUSTER="${CLUSTER:-tink-dhcp-redirect}"
CILIUM_VERSION="${CILIUM_VERSION:-1.18.3}"
KIND_IMAGE="${KIND_IMAGE:-kindest/node:v1.34.0}"
IMAGE="localhost/tinkerbell-e2e:latest"
PROBE_IMAGE="localhost/dhcpprobe-e2e:latest"

# Must match the reservation in hardware.yaml.
CLIENT_MAC="52:54:00:dc:be:ef"
EXPECTED_IP="172.30.99.10"

# A docker network of this cluster's own. The default "kind" network is shared
# by every kind cluster on the host, which would put their nodes on the same
# broadcast segment as the probe; any of them running a DHCP server can answer
# first and the test would be measuring the wrong thing.
NETWORK="${NETWORK:-tink-dhcp-redirect-e2e}"
SUBNET="${SUBNET:-172.30.0.0/16}"
export KIND_EXPERIMENTAL_DOCKER_NETWORK="${NETWORK}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${HERE}/../../.." && pwd)"
WORK="$(mktemp -d)"
KEEP=false
[[ "${1:-}" == "--keep" ]] && KEEP=true

KUBECTL=(kubectl --context "kind-${CLUSTER}")

log() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
fail() { printf '\n\033[1;31mFAIL: %s\033[0m\n' "$*" >&2; exit 1; }

cleanup() {
  local status=$?
  if [[ ${status} -ne 0 ]]; then
    log "Smee logs"
    "${KUBECTL[@]}" -n tink-e2e logs smee --tail=100 2>/dev/null || true
  fi
  rm -rf "${WORK}"
  if [[ "${KEEP}" == false ]]; then
    log "deleting cluster ${CLUSTER}"
    kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
    docker network rm "${NETWORK}" >/dev/null 2>&1 || true
  else
    log "leaving cluster ${CLUSTER} running (--keep)"
  fi
  exit ${status}
}
trap cleanup EXIT

log "creating an isolated docker network ${NETWORK} (${SUBNET})"
kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
docker network rm "${NETWORK}" >/dev/null 2>&1 || true
docker network create --subnet "${SUBNET}" "${NETWORK}" >/dev/null

log "creating kind cluster ${CLUSTER} (no CNI, no kube-proxy)"
kind create cluster --name "${CLUSTER}" --config "${HERE}/kind.yaml" --image "${KIND_IMAGE}" --wait 30s || true

API_IP="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${CLUSTER}-control-plane")"

log "installing Cilium ${CILIUM_VERSION} with kube-proxy replacement"
helm repo add cilium https://helm.cilium.io/ >/dev/null 2>&1 || true
helm repo update cilium >/dev/null
helm install cilium cilium/cilium \
  --version "${CILIUM_VERSION}" \
  --namespace kube-system \
  --kube-context "kind-${CLUSTER}" \
  --set image.pullPolicy=IfNotPresent \
  --set ipam.mode=kubernetes \
  --set kubeProxyReplacement=true \
  --set k8sServiceHost="${API_IP}" \
  --set k8sServicePort=6443 \
  --set operator.replicas=1 \
  --wait --timeout 8m

"${KUBECTL[@]}" wait --for=condition=Ready node --all --timeout=3m

log "building the tinkerbell binary and the probe"
(cd "${ROOT}" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o "${WORK}/tinkerbell" ./cmd/tinkerbell)
(cd "${ROOT}" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o "${WORK}/dhcpprobe" ./script/e2e/dhcp-broadcast-redirect/probe)

printf 'FROM alpine:3\nCOPY tinkerbell /tinkerbell\nENTRYPOINT ["/tinkerbell"]\n' > "${WORK}/Dockerfile.tinkerbell"
printf 'FROM alpine:3\nCOPY dhcpprobe /dhcpprobe\nENTRYPOINT ["/dhcpprobe"]\n' > "${WORK}/Dockerfile.probe"
docker build -q -t "${IMAGE}" -f "${WORK}/Dockerfile.tinkerbell" "${WORK}" >/dev/null
docker build -q -t "${PROBE_IMAGE}" -f "${WORK}/Dockerfile.probe" "${WORK}" >/dev/null

log "loading the image into the cluster"
kind load docker-image --name "${CLUSTER}" "${IMAGE}"

log "deploying Smee with the DHCP broadcast redirect enabled"
"${KUBECTL[@]}" apply -f "${HERE}/namespace.yaml"

# A controller creates the namespace's default ServiceAccount a moment after the
# namespace itself, and a pod cannot be admitted until it exists.
for _ in $(seq 60); do
  "${KUBECTL[@]}" -n tink-e2e get serviceaccount default >/dev/null 2>&1 && break
  sleep 1
done

"${KUBECTL[@]}" -n tink-e2e create configmap hardware --from-file=hardware.yaml="${HERE}/hardware.yaml" \
  --dry-run=client -o yaml | "${KUBECTL[@]}" apply -f -
"${KUBECTL[@]}" apply -f "${HERE}/tinkerbell.yaml"
"${KUBECTL[@]}" -n tink-e2e wait --for=condition=Ready pod/smee --timeout=3m

log "what the redirect resolved"
"${KUBECTL[@]}" -n tink-e2e logs smee | grep -i "broadcast redirect" || fail "Smee did not report the redirect as active"

# The pod must genuinely be on the pod network, or the test proves nothing.
POD_IP="$("${KUBECTL[@]}" -n tink-e2e get pod smee -o jsonpath='{.status.podIP}')"
HOST_IP="$("${KUBECTL[@]}" -n tink-e2e get pod smee -o jsonpath='{.status.hostIP}')"
[[ "${POD_IP}" == "${HOST_IP}" ]] && fail "the pod is on the host network; this would prove nothing"
log "Smee pod IP ${POD_IP} (node ${HOST_IP}) — no interface on the ${CLIENT_MAC} segment"

log "broadcasting a DHCPDISCOVER from a container on the node's segment"
set +e
OUTPUT="$(docker run --rm --network "${NETWORK}" --mac-address "${CLIENT_MAC}" \
  --cap-add NET_RAW --cap-add NET_ADMIN "${PROBE_IMAGE}" -interface eth0 -timeout 8s -retries 3 2>&1)"
PROBE_STATUS=$?
set -e
echo "${OUTPUT}"

log "redirect counters"
"${KUBECTL[@]}" -n tink-e2e logs smee | grep -iE "broadcast redirect|received DHCP|sent DHCP" | tail -20 || true

[[ ${PROBE_STATUS} -eq 0 ]] || fail "the probe did not complete a DHCP handshake"
grep -q "yiaddr=${EXPECTED_IP}" <<<"${OUTPUT}" || fail "expected the reservation ${EXPECTED_IP}, got:\n${OUTPUT}"

log "PASS: a PXE client on the physical segment completed DHCP against a pod that has no interface on it"
