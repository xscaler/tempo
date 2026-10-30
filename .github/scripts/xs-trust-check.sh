#!/usr/bin/env bash
# Decides whether an image that already exists in the registry may be reused.
# It was not built by this run, so it is never signed or attested here. It is
# accepted only with all three of:
#   1. a cosign signature from the exact release identity,
#   2. a provenance attestation from the exact release identity,
#   3. a revision label equal to the requested commit on both arches.
#
#   xs-trust-check.sh <repo> <tag> <index-digest> <commit> <output-file>
#
# Writes amd64= and arm64= lines to <output-file>. Any failure prints the
# "exists without trusted provenance" error and exits 1. This script never
# calls cosign sign or attest. Needs crane, cosign and gh, CERT_IDENTITY,
# CERT_OIDC_ISSUER, GITHUB_REPOSITORY and GH_TOKEN. Steps 1 and 2 need a
# GHCR image with attestations, so only step 3 is exercised against a local
# registry; the signature step is exercised there only for rejection.
set -euo pipefail

repo="${1:?repo}"
tag="${2:?tag}"
digest="${3:?index digest}"
commit="${4:?commit}"
out="${5:?output file}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

untrusted() {
  echo "::error title=Untrusted image::${repo}:${tag} exists without trusted provenance: $1. Investigate, then bump -xs.n."
  exit 1
}

cosign verify \
  --certificate-identity "$CERT_IDENTITY" \
  --certificate-oidc-issuer "$CERT_OIDC_ISSUER" \
  "${repo}@${digest}" > /dev/null || untrusted "no valid signature from ${CERT_IDENTITY}"

gh attestation verify "oci://${repo}@${digest}" \
  --repo "$GITHUB_REPOSITORY" \
  --cert-identity "$CERT_IDENTITY" \
  --cert-oidc-issuer "$CERT_OIDC_ISSUER" > /dev/null || untrusted "no valid provenance attestation"

"${here}/xs-check-revision.sh" "$repo" "$tag" "$digest" "$commit" "$out"
echo "${repo}:${tag} at ${digest} is trusted"
