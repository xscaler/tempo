#!/usr/bin/env bash
# Idempotent cosign keyless signing of a multi-arch image.
#
#   xs-sign.sh <repo>@<index-digest>
#
# Each of the index and its per-arch manifests is verified against the exact
# workflow identity first and signed only when the registry reports
# "no signatures found". Every other verify failure, including a signature
# from a different identity, fails the script. Expects CERT_IDENTITY and
# CERT_OIDC_ISSUER in the environment, plus crane and cosign in PATH.
set -uo pipefail

ref="${1:?usage: xs-sign.sh <repo>@<index-digest>}"
repo="${ref%@*}"

# Only stderr is wanted: cosign prints the verified payload on stdout.
verify() {
  # shellcheck disable=SC2069
  cosign verify \
    --certificate-identity "$CERT_IDENTITY" \
    --certificate-oidc-issuer "$CERT_OIDC_ISSUER" \
    "$1" 2>&1 >/dev/null
}

ensure_signed() { # <ref> [cosign sign flags]
  local target="$1" out
  shift
  if out="$(verify "$target")"; then
    echo "signed: ${target}"
    return 0
  fi
  if ! printf '%s' "$out" | grep -q 'no signatures found'; then
    echo "::error::cosign verify failed for ${target}:"
    printf '%s\n' "$out"
    return 1
  fi
  echo "signing ${target}"
  cosign sign --yes "$@" "$target" || return 1
  if ! out="$(verify "$target")"; then
    echo "::error::signature on ${target} does not verify after signing:"
    printf '%s\n' "$out"
    return 1
  fi
  echo "signed: ${target}"
}

ensure_signed "$ref" --recursive || exit 1

children="$(crane manifest "$ref" | jq -r '.manifests[].digest')" || exit 1
for child in $children; do
  ensure_signed "${repo}@${child}" || exit 1
done
