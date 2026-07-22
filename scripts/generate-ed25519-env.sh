#!/bin/sh
set -eu

oa_key=$(mktemp /tmp/taskgate-oa-ed25519.XXXXXX.pem)
gateway_key=$(mktemp /tmp/taskgate-gateway-ed25519.XXXXXX.pem)
cleanup() {
  rm -f "$oa_key" "$gateway_key"
}
trap cleanup EXIT INT TERM

openssl genpkey -algorithm ED25519 -out "$oa_key"
openssl genpkey -algorithm ED25519 -out "$gateway_key"

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
printf '# Distribute this Gateway public key to receipt verifiers: %s\n' "$(raw_public "$gateway_key")"
