#!/bin/sh
set -eu

oa_key=$(mktemp /tmp/taskgate-oa-ed25519.XXXXXX.pem)
gateway_key=$(mktemp /tmp/taskgate-gateway-ed25519.XXXXXX.pem)
audit_anchor_key=$(mktemp /tmp/taskgate-audit-anchor-ed25519.XXXXXX.pem)
cleanup() {
  rm -f "$oa_key" "$gateway_key" "$audit_anchor_key"
}
trap cleanup EXIT INT TERM

openssl genpkey -algorithm ED25519 -out "$oa_key"
openssl genpkey -algorithm ED25519 -out "$gateway_key"
openssl genpkey -algorithm ED25519 -out "$audit_anchor_key"

raw_private() {
  openssl pkey -in "$1" -outform DER | tail -c 32 | base64 | tr -d '\n'
}

raw_public() {
  openssl pkey -in "$1" -pubout -outform DER | tail -c 32 | base64 | tr -d '\n'
}

printf 'OA_RECEIPT_KEY_ID=oa-ed25519-v1\n'
printf 'OA_RECEIPT_PRIVATE_KEY=%s\n' "$(raw_private "$oa_key")"
printf 'OA_RECEIPT_PUBLIC_KEY=%s\n' "$(raw_public "$oa_key")"
printf 'GATEWAY_RECEIPT_KEY_ID=gateway-ed25519-v1\n'
printf 'GATEWAY_RECEIPT_PRIVATE_KEY=%s\n' "$(raw_private "$gateway_key")"
gateway_public=$(raw_public "$gateway_key")
printf '# Gateway publishes this active key at /.well-known/taskgate/query-receipt-keyring.json\n'
printf '# Historical/verifier bundle entry:\n'
printf '# GATEWAY_RECEIPT_KEYRING_JSON=[{"key_id":"gateway-ed25519-v1","public_key":"%s"}]\n' "$gateway_public"
audit_anchor_public=$(raw_public "$audit_anchor_key")
printf 'GATEWAY_AUDIT_ANCHOR_KEY_ID=audit-anchor-ed25519-v1\n'
printf 'GATEWAY_AUDIT_ANCHOR_PRIVATE_KEY=%s\n' "$(raw_private "$audit_anchor_key")"
printf '# Distribute this audit-anchor public key to the external anchor/WORM service: %s\n' "$audit_anchor_public"
