#!/usr/bin/env bash
# Prints the digest of an image reference, or nothing when the image does not
# exist. Fails on any other error, so an outage is never read as "absent".
#
#   xs-image-digest.sh ghcr.io/xscaler/tempo:3.1.1-xs.0
#
# For ghcr.io references the GitHub Packages API is asked first: a 404 for the
# package means it has never been pushed, so the tag is absent. That avoids
# guessing at GHCR's error text for a missing package. Only then is the tag
# looked up with crane, where only MANIFEST_UNKNOWN and NAME_UNKNOWN mean
# absent. Needs crane, a prior registry login and, for ghcr.io, GH_TOKEN.
set -uo pipefail

ref="${1:?usage: xs-image-digest.sh <image-ref>}"

case "$ref" in
  ghcr.io/*)
    path="${ref#ghcr.io/}"
    owner="${path%%/*}"
    rest="${path#*/}"
    name="${rest%%[:@]*}"
    enc="${name//\//%2F}"
    if ! err="$(gh api "orgs/${owner}/packages/container/${enc}" 2>&1 >/dev/null)"; then
      if printf '%s' "$err" | grep -q 'HTTP 404'; then
        exit 0
      fi
      echo "::error::could not look up package ${owner}/${name}:" >&2
      printf '%s\n' "$err" >&2
      exit 1
    fi
    ;;
esac

# One lookup. Stdout is the digest on success; stderr is kept apart so
# a log line from crane can never end up in the digest, and a failure is
# classified from the same call without asking the registry again.
errfile="$(mktemp)"
trap 'rm -f "$errfile"' EXIT
if digest="$(crane digest "$ref" 2>"$errfile")"; then
  printf '%s\n' "$digest"
  exit 0
fi

if grep -qE 'MANIFEST_UNKNOWN|NAME_UNKNOWN' "$errfile"; then
  exit 0
fi

echo "::error::could not look up ${ref}:" >&2
cat "$errfile" >&2
exit 1
