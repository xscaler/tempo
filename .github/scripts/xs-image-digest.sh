#!/usr/bin/env bash
# Prints the digest of an image reference, or nothing when the image does not
# exist. Fails on any other error, so an outage is never read as "absent".
#
#   xs-image-digest.sh ghcr.io/xscaler/tempo:3.1.1-xs.0
#
# GHCR answers MANIFEST_UNKNOWN for a missing tag and NAME_UNKNOWN for a
# missing repository. For a package that has never been pushed it can also
# answer "denied: requested access to the resource is denied" even with a valid
# token, so that exact text counts as absent too. Needs crane and a prior login.
set -uo pipefail

ref="${1:?usage: xs-image-digest.sh <image-ref>}"

if err="$(crane digest "$ref" 2>&1 >/dev/null)"; then
  crane digest "$ref"
  exit 0
fi

if printf '%s' "$err" | grep -qE 'MANIFEST_UNKNOWN|NAME_UNKNOWN|requested access to the resource is denied'; then
  exit 0
fi

echo "::error::could not look up ${ref}:" >&2
printf '%s\n' "$err" >&2
exit 1
