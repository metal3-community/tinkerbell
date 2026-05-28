#!/usr/bin/env bash
# Generate a self-signed CA + serving cert for the conversion webhook for
# local dev / docker-compose integration tests. Production deployments
# should use cert-manager via the helm chart (helm/tinkerbell/templates/
# webhook-cert.yaml).
#
# Usage:
#   ./script/dev-webhook-cert.sh [--out DIR] [--svc-host HOST]
#
# Outputs (under DIR, default: out/webhook-certs):
#   ca.crt             — CA certificate (PEM); use as ConversionWebhookCABundleFile
#   tls.crt + tls.key  — webhook server's cert+key; use as ConversionWebhookCertFile / KeyFile
#
# The SAN list covers the typical reach paths:
#   - tinkerbell (docker-compose service name)
#   - tinkerbell.svc / .svc.cluster.local (kube Service DNS)
#   - 127.0.0.1 (local)
# Override with --svc-host to add an additional name (e.g., the helm release).

set -euo pipefail

OUT="out/webhook-certs"
SVC_HOST=""

while [[ $# -gt 0 ]]; do
	case "$1" in
		--out)
			OUT="$2"; shift 2 ;;
		--svc-host)
			SVC_HOST="$2"; shift 2 ;;
		*)
			echo "unknown arg: $1" >&2
			exit 2 ;;
	esac
done

mkdir -p "$OUT"
cd "$OUT"

# 1. CA
openssl req -x509 -newkey rsa:2048 -keyout ca.key -out ca.crt \
	-days 365 -nodes -sha256 \
	-subj '/CN=tinkerbell-webhook-dev-ca' 2>/dev/null

# 2. Serving cert with SAN list
SAN_NAMES=(
	"DNS:tinkerbell"
	"DNS:tinkerbell.default.svc"
	"DNS:tinkerbell.default.svc.cluster.local"
	"DNS:localhost"
	"IP:127.0.0.1"
)
if [[ -n "$SVC_HOST" ]]; then
	SAN_NAMES+=("DNS:$SVC_HOST")
fi

# Join with commas: SAN list goes on a single openssl -addext line.
SAN_JOINED=$(IFS=, ; echo "${SAN_NAMES[*]}")

openssl req -new -newkey rsa:2048 -keyout tls.key -out tls.csr \
	-nodes -sha256 \
	-subj '/CN=tinkerbell-webhook' \
	-addext "subjectAltName=$SAN_JOINED" 2>/dev/null

# 3. Sign the CSR with the CA, carrying the SAN extension forward
cat > tls.ext <<EOF
subjectAltName=$SAN_JOINED
extendedKeyUsage=serverAuth
EOF
openssl x509 -req -in tls.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
	-out tls.crt -days 365 -sha256 \
	-extfile tls.ext 2>/dev/null

# Tidy up intermediate artifacts.
rm -f tls.csr tls.ext ca.srl ca.key

echo "wrote: $(pwd)/ca.crt"
echo "wrote: $(pwd)/tls.crt"
echo "wrote: $(pwd)/tls.key"
