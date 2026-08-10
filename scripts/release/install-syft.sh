#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd -P)"
# shellcheck source=common.sh
source "$SCRIPT_DIR/common.sh"
# shellcheck source=../../release/toolchain.env
source "$REPO_ROOT/release/toolchain.env"

require_tool curl
require_tool tar
require_tool shasum

case "$(uname -s):$(uname -m)" in
  Darwin:arm64) archive_arch=arm64; expected="$SYFT_DARWIN_ARM64_SHA256" ;;
  Darwin:x86_64) archive_arch=amd64; expected="$SYFT_DARWIN_AMD64_SHA256" ;;
  *) release_die "pinned Syft installer supports only Darwin arm64 or amd64" ;;
esac
require_sha256 SYFT_DARWIN_ARM64_SHA256
require_sha256 SYFT_DARWIN_AMD64_SHA256

DESTINATION="${1:-$REPO_ROOT/.release-tools}"
mkdir -p "$DESTINATION"
temporary="$(mktemp -d "${TMPDIR:-/tmp}/dsx-syft.XXXXXX")"
trap 'rm -rf "$temporary"' EXIT
archive="syft_${SYFT_VERSION}_darwin_${archive_arch}.tar.gz"
curl --fail --location --proto '=https' --tlsv1.2 --output "$temporary/$archive" \
  "https://github.com/anchore/syft/releases/download/v${SYFT_VERSION}/${archive}"
observed="$(sha256_file "$temporary/$archive")"
[[ "$observed" == "$expected" ]] || release_die "pinned Syft archive digest mismatch"
tar -xzf "$temporary/$archive" -C "$temporary" syft
install -m 0755 "$temporary/syft" "$DESTINATION/syft"
observed_version="$($DESTINATION/syft version -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["version"])')"
[[ "$observed_version" == "$SYFT_VERSION" ]] || release_die "installed Syft version mismatch: $observed_version"
printf '%s\n' "$DESTINATION/syft"
