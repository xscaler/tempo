#!/usr/bin/env bash
# Idempotent cosign keyless signing for a digest-pinned image reference.
#
#   xs-sign.sh <repo>@<digest>
#
# Verifies first. Signs only when the registry answers that there are no
# signatures. Any other verify failure (identity mismatch, auth, network)
# fails the script, so a transient error never turns into a second signature
# and a wrong signer is never papered over. Expects CERT_IDENTITY_REGEXP and
# CERT_OIDC_ISSUER in the environment.
set -uo pipefail

ref="${1:?usage: xs-sign.sh <repo>@<digest>}"

verify() {
  cosign verify \
    --certificate-identity-regexp "$CERT_IDENTITY_REGEXP" \
    --certificate-oidc-issuer "$CERT_OIDC_ISSUER" \
    "$ref" 2>&1 >/dev/null
}

if out="$(verify)"; then
  echo "already signed and verified: ${ref}"
  exit 0
fi

if ! printf '%s' "$out" | grep -qiE 'no signatures found|no matching signatures'; then
  echo "::error::cosign verify failed for ${ref} for a reason other than a missing signature:"
  printf '%s\n' "$out"
  exit 1
fi

echo "no signature on ${ref}, signing"
cosign sign --yes "$ref" || exit 1
if ! out="$(verify)"; then
  echo "::error::signature on ${ref} does not verify after signing:"
  printf '%s\n' "$out"
  exit 1
fi
echo "signed and verified: ${ref}"
