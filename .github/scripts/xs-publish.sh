#!/usr/bin/env bash
# Pushes the per-arch OCI layouts by digest and creates the multi-arch index.
# One script serves the real publish and the pull request dry run against a
# local registry.
#
#   xs-publish.sh <repo> <tag> <images-dir> <output-file>
#
# <images-dir> holds image-<arch>/oci-<arch> as downloaded from the build job.
# The tag must not exist yet. The index digest comes from the create command,
# never from reading the tag back. Writes digest=, amd64= and arm64= lines to
# <output-file>. Needs crane, docker buildx, jq and a prior registry login.
set -euo pipefail

repo="${1:?repo}"
tag="${2:?tag}"
images="${3:?images dir}"
out="${4:?output file}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
meta="$(mktemp)"

declare -A arch_digest
for arch in amd64 arm64; do
  dir="${images}/image-${arch}/oci-${arch}"
  digest="$(jq -r '.manifests[0].digest' "${dir}/index.json")"
  crane push "$dir" "${repo}@${digest}"
  arch_digest[$arch]="$digest"
done

current="$("${here}/xs-image-digest.sh" "${repo}:${tag}")"
if [ -n "$current" ]; then
  echo "::error::${repo}:${tag} appeared during this run at ${current}"
  exit 1
fi

docker buildx imagetools create \
  --metadata-file "$meta" \
  --tag "${repo}:${tag}" \
  "${repo}@${arch_digest[amd64]}" "${repo}@${arch_digest[arm64]}"

digest="$(jq -r '."containerimage.descriptor".digest' "$meta")"
case "$digest" in
  sha256:*) ;;
  *) echo "::error::no index digest in the create output" >&2; exit 1 ;;
esac

manifest="$(crane manifest "${repo}@${digest}")"
amd64="$(jq -r '.manifests[] | select(.platform.architecture == "amd64") | .digest' <<< "$manifest")"
arm64="$(jq -r '.manifests[] | select(.platform.architecture == "arm64") | .digest' <<< "$manifest")"
if [ "$amd64" != "${arch_digest[amd64]}" ] || [ "$arm64" != "${arch_digest[arm64]}" ]; then
  echo "::error::index ${digest} does not hold the images that were pushed" >&2
  exit 1
fi

{
  echo "digest=${digest}"
  echo "amd64=${amd64}"
  echo "arm64=${arm64}"
} >> "$out"
echo "${repo}:${tag} is ${digest} (amd64 ${amd64}, arm64 ${arm64})"
