#!/usr/bin/env bash
# Installs release tools pinned by version and sha256.
#
#   xs-install-tools.sh crane cosign syft
#
# Binaries go to $RUNNER_TEMP/xs-bin, which is added to GITHUB_PATH. A download
# whose sha256 does not match the pin fails the job. To bump a tool, change the
# version and both checksums here, taken from the project's release checksums
# file.
set -euo pipefail

CRANE_VERSION=0.22.1
COSIGN_VERSION=3.1.4
SYFT_VERSION=1.55.0

case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

declare -A SUMS=(
  [crane-amd64]=0ab7a1d6932a213aed964ce97666c3077fe691c8606413674a8b3e0b9ec4cda0
  [crane-arm64]=898c0cff975f898a33e8c4580bdafb0e7c02c7faa33374e946762f97c4ab7110
  [cosign-amd64]=424e2c97d0b45fd7a6b608a87f090f35633f4c23ece5f092d48bba11aaf4bbfa
  [cosign-arm64]=ade83b7aac62e131cacfe96d958948b2150cc9efec46a570a2649b8e27dedad2
  [syft-amd64]=bb65249542db5fedeeb4d3f9b018b75e7264dbfbbb10486f044581a92f90490d
  [syft-arm64]=929bd163db2d7cd2255d3bcb8967208079813d7b46b0c952f7b794d8e2c7d304
)

bin="${RUNNER_TEMP:?}/xs-bin"
work="$(mktemp -d)"
mkdir -p "$bin"

fetch() { # <url> <file> <sha256>
  curl -fsSL --retry 3 -o "$2" "$1"
  echo "$3  $2" | sha256sum -c - >/dev/null || {
    echo "::error::sha256 mismatch for $1" >&2
    exit 1
  }
}

for tool in "$@"; do
  sum="${SUMS[${tool}-${arch}]:?no pinned checksum for ${tool} ${arch}}"
  case "$tool" in
    crane)
      case "$arch" in amd64) a=x86_64 ;; arm64) a=arm64 ;; esac
      fetch "https://github.com/google/go-containerregistry/releases/download/v${CRANE_VERSION}/go-containerregistry_Linux_${a}.tar.gz" "$work/crane.tgz" "$sum"
      tar -xzf "$work/crane.tgz" -C "$bin" crane
      ;;
    cosign)
      fetch "https://github.com/sigstore/cosign/releases/download/v${COSIGN_VERSION}/cosign-linux-${arch}" "$bin/cosign" "$sum"
      chmod +x "$bin/cosign"
      ;;
    syft)
      fetch "https://github.com/anchore/syft/releases/download/v${SYFT_VERSION}/syft_${SYFT_VERSION}_linux_${arch}.tar.gz" "$work/syft.tgz" "$sum"
      tar -xzf "$work/syft.tgz" -C "$bin" syft
      ;;
    *)
      echo "unknown tool ${tool}" >&2
      exit 1
      ;;
  esac
  echo "installed ${tool}"
done

echo "$bin" >> "${GITHUB_PATH:?}"
