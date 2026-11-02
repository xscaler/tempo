#!/usr/bin/env bash
# Checks that every per-arch image of an index was built from the expected
# commit, by its org.opencontainers.image.revision label.
#
#   xs-check-revision.sh <repo> <tag> <index-digest> <commit> <output-file>
#
# Writes amd64= and arm64= lines to <output-file>. A mismatch fails with the
# standard "exists without trusted provenance" error. Part of the trust check
# in xs-trust-check.sh. It talks to the registry only through crane, so it
# also runs against a local registry in the dry run. Needs crane.
set -euo pipefail

repo="${1:?repo}"
tag="${2:?tag}"
digest="${3:?index digest}"
commit="${4:?commit}"
out="${5:?output file}"

untrusted() {
  echo "::error title=Untrusted image::${repo}:${tag} exists without trusted provenance: $1. Investigate, then bump -xs.n."
  exit 1
}

manifest="$(crane manifest "${repo}@${digest}")"
for arch in amd64 arm64; do
  d="$(jq -r --arg a "$arch" '.manifests[] | select(.platform.architecture == $a) | .digest' <<< "$manifest")"
  [ -n "$d" ] || untrusted "no ${arch} image in the index"
  rev="$(crane config "${repo}@${d}" | jq -r '.config.Labels["org.opencontainers.image.revision"] // ""')"
  [ "$rev" = "$commit" ] || untrusted "${arch} revision label is '${rev}', not ${commit}"
  echo "${arch}=${d}" >> "$out"
done
echo "revision label matches ${commit} on both arches"
