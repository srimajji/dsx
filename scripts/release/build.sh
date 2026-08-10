#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd -P)"
# shellcheck source=common.sh
source "$SCRIPT_DIR/common.sh"
# shellcheck source=../../release/toolchain.env
source "$REPO_ROOT/release/toolchain.env"

for name in VERSION COMMIT SOURCE_DATE_EPOCH DSX_AGENT_IMAGE DSX_BROWSER_IMAGE; do require_value "$name"; done
require_image_pin DSX_AGENT_IMAGE
require_image_pin DSX_BROWSER_IMAGE
[[ "$VERSION" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$ ]] || release_die "VERSION must be a concrete SemVer release"
[[ "$COMMIT" =~ ^[0-9a-f]{40}$ ]] || release_die "COMMIT must be the full lowercase 40-hex Git object ID"
[[ "$SOURCE_DATE_EPOCH" =~ ^[0-9]+$ ]] || release_die "SOURCE_DATE_EPOCH must be a non-negative integer"

GO_BIN="${GO_BIN:-go}"
SYFT_BIN="${SYFT_BIN:-$REPO_ROOT/.release-tools/syft}"
require_tool "$GO_BIN"
for tool in python3 shasum cut mktemp; do require_tool "$tool"; done
[[ -x "$SYFT_BIN" ]] || release_die "pinned Syft is unavailable; run scripts/release/install-syft.sh"
observed_go="$($GO_BIN env GOVERSION)"
[[ "$observed_go" == "go1.26.5" ]] || release_die "release requires Go 1.26.5, observed $observed_go"
observed_syft="$($SYFT_BIN version -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["version"])')"
[[ "$observed_syft" == "$SYFT_VERSION" ]] || release_die "release requires Syft $SYFT_VERSION, observed $observed_syft"

BUILT_AT="$(rfc3339_from_epoch "$SOURCE_DATE_EPOCH")"
OUT_DIR="${OUT_DIR:-$REPO_ROOT/dist/dsx-${VERSION}-dry-run}"
[[ ! -e "$OUT_DIR" ]] || release_die "output already exists (refusing to overwrite): $OUT_DIR"
work="$(mktemp -d "${TMPDIR:-/tmp}/dsx-release.XXXXXX")"
trap 'rm -rf "$work"' EXIT
package="$work/package"
mkdir -p "$package/bin"

common_ldflags="-buildid= -s -w -X github.com/srimajji/dsx/internal/buildinfo.Version=$VERSION -X github.com/srimajji/dsx/internal/buildinfo.Commit=$COMMIT -X github.com/srimajji/dsx/internal/buildinfo.BuiltAt=$BUILT_AT"
(
  cd "$REPO_ROOT"
  env CGO_ENABLED=0 GOOS=linux GOARCH=arm64 TZ=UTC LC_ALL=C \
    "$GO_BIN" build -mod=readonly -buildvcs=false -trimpath -ldflags "$common_ldflags" \
    -o "$package/bin/dsx-guest" ./cmd/dsx-guest
)
guest_sha256="$(sha256_file "$package/bin/dsx-guest")"
host_ldflags="$common_ldflags -X github.com/srimajji/dsx/internal/buildinfo.GuestSHA256=$guest_sha256 -X github.com/srimajji/dsx/internal/buildinfo.AgentImage=$DSX_AGENT_IMAGE -X github.com/srimajji/dsx/internal/buildinfo.BrowserImage=$DSX_BROWSER_IMAGE"
(
  cd "$REPO_ROOT"
  env CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 TZ=UTC LC_ALL=C \
    "$GO_BIN" build -mod=readonly -buildvcs=false -trimpath -ldflags "$host_ldflags" \
    -o "$package/bin/dsx" ./cmd/dsx
)
chmod 0755 "$package/bin/dsx" "$package/bin/dsx-guest"

# Syft is pinned and its nondeterministic document fields are canonicalized
# before their digest enters the release manifest.
env SOURCE_DATE_EPOCH="$SOURCE_DATE_EPOCH" TZ=UTC LC_ALL=C \
  "$SYFT_BIN" scan "dir:$package" -o "spdx-json=$package/dsx.spdx.json"
"$SCRIPT_DIR/artifacts.py" normalize-sbom --path "$package/dsx.spdx.json" --version "$VERSION" --built-at "$BUILT_AT"
"$SCRIPT_DIR/artifacts.py" manifest --root "$package" --output "$package/release-manifest.json" \
  --version "$VERSION" --commit "$COMMIT" --built-at "$BUILT_AT" --guest-sha256 "$guest_sha256" \
  --agent-image "$DSX_AGENT_IMAGE" --browser-image "$DSX_BROWSER_IMAGE" --syft-version "$SYFT_VERSION"

host_metadata="$work/host-version.json"
"$package/bin/dsx" --version --json > "$host_metadata"
"$SCRIPT_DIR/artifacts.py" verify --root "$package" --manifest "$package/release-manifest.json" \
  --host-metadata "$host_metadata" --expected-version "$VERSION" --expected-commit "$COMMIT"
"$SCRIPT_DIR/artifacts.py" archive --root "$package" --output "$work/dsx-${VERSION}-darwin-arm64.zip"
(
  cd "$work"
  {
    observed="$(sha256_file "dsx-${VERSION}-darwin-arm64.zip")"
    printf '%s  %s\n' "$observed" "dsx-${VERSION}-darwin-arm64.zip"
    observed="$(sha256_file "package/release-manifest.json")"
    printf '%s  %s\n' "$observed" "release-manifest.json"
    observed="$(sha256_file "package/dsx.spdx.json")"
    printf '%s  %s\n' "$observed" "dsx.spdx.json"
  } > checksums.txt
)
mkdir -p "$(dirname "$OUT_DIR")"
mv "$work" "$OUT_DIR"
trap - EXIT
printf 'unsigned release dry-run created at %s\n' "$OUT_DIR"
printf 'BLOCKED: Developer ID signing and Apple notarization are required before release use.\n'
