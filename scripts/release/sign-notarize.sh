#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd -P)"
# shellcheck source=common.sh
source "$SCRIPT_DIR/common.sh"
# shellcheck source=../../release/toolchain.env
source "$REPO_ROOT/release/toolchain.env"

UNSIGNED_DIR="${1:-}"
[[ -n "$UNSIGNED_DIR" && -d "$UNSIGNED_DIR/package" ]] || release_die "usage: sign-notarize.sh UNSIGNED_DRY_RUN_DIRECTORY"
for name in SIGNING_IDENTITY NOTARY_KEYCHAIN_PROFILE; do require_value "$name"; done
[[ "$SIGNING_IDENTITY" == "Developer ID Application: "* ]] || release_die "SIGNING_IDENTITY must name a real Developer ID Application identity"
[[ "$SIGNING_IDENTITY" != *$'\n'* && "$NOTARY_KEYCHAIN_PROFILE" != *$'\n'* ]] || release_die "credential selectors must be single-line values"

for tool in security codesign xcrun spctl python3 shasum cut mktemp; do require_tool "$tool"; done
SYFT_BIN="${SYFT_BIN:-$REPO_ROOT/.release-tools/syft}"
[[ -x "$SYFT_BIN" ]] || release_die "pinned Syft is unavailable"
observed_syft="$($SYFT_BIN version -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["version"])')"
[[ "$observed_syft" == "$SYFT_VERSION" ]] || release_die "release requires Syft $SYFT_VERSION, observed $observed_syft"
identities="$(security find-identity -v -p codesigning)"
[[ "$identities" == *"\"$SIGNING_IDENTITY\""* ]] || release_die "configured Developer ID identity is not present in the keychain"
xcrun notarytool history --keychain-profile "$NOTARY_KEYCHAIN_PROFILE" --output-format json >/dev/null || \
  release_die "configured notarization profile is unavailable or invalid"

manifest="$UNSIGNED_DIR/package/release-manifest.json"
[[ -f "$manifest" ]] || release_die "unsigned release manifest is missing"
unsigned_metadata="$(mktemp "${TMPDIR:-/tmp}/dsx-unsigned-version.XXXXXX")"
trap 'rm -f "$unsigned_metadata"' EXIT
"$UNSIGNED_DIR/package/bin/dsx" --version --json > "$unsigned_metadata"
"$SCRIPT_DIR/artifacts.py" verify --root "$UNSIGNED_DIR/package" --manifest "$manifest" --host-metadata "$unsigned_metadata"
rm -f "$unsigned_metadata"
trap - EXIT
metadata_json="$(python3 - "$manifest" <<'PY'
import json, sys
m = json.load(open(sys.argv[1], encoding="utf-8"))
print(json.dumps({"version":m["build"]["version"],"commit":m["build"]["commit"],"built_at":m["build"]["built_at"],"agent":m["images"]["agent"],"browser":m["images"]["browser"]}, separators=(",",":")))
PY
)"
VERSION="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["version"])' "$metadata_json")"
COMMIT="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["commit"])' "$metadata_json")"
BUILT_AT="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["built_at"])' "$metadata_json")"
DSX_AGENT_IMAGE="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["agent"])' "$metadata_json")"
DSX_BROWSER_IMAGE="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["browser"])' "$metadata_json")"
SIGNED_OUT="${SIGNED_OUT:-$REPO_ROOT/dist/dsx-${VERSION}-signed}"
[[ ! -e "$SIGNED_OUT" ]] || release_die "signed output already exists (refusing to overwrite): $SIGNED_OUT"
work="$(mktemp -d "${TMPDIR:-/tmp}/dsx-sign.XXXXXX")"
trap 'rm -rf "$work"' EXIT
cp -R "$UNSIGNED_DIR/package" "$work/package"

codesign --force --options runtime --timestamp --sign "$SIGNING_IDENTITY" "$work/package/bin/dsx"
codesign --verify --strict --verbose=4 "$work/package/bin/dsx"
authority="$(codesign --display --verbose=4 "$work/package/bin/dsx" 2>&1)"
[[ "$authority" == *"Authority=$SIGNING_IDENTITY"* ]] || release_die "signed candidate authority does not match configured identity"

# Signing changes the host bytes. Regenerate the canonical SBOM and all digests.
rm -f "$work/package/dsx.spdx.json" "$work/package/release-manifest.json"
env TZ=UTC LC_ALL=C "$SYFT_BIN" scan "dir:$work/package" -o "spdx-json=$work/package/dsx.spdx.json"
"$SCRIPT_DIR/artifacts.py" normalize-sbom --path "$work/package/dsx.spdx.json" --version "$VERSION" --built-at "$BUILT_AT"
guest_sha256="$(sha256_file "$work/package/bin/dsx-guest")"
"$SCRIPT_DIR/artifacts.py" manifest --root "$work/package" --output "$work/package/release-manifest.json" \
  --version "$VERSION" --commit "$COMMIT" --built-at "$BUILT_AT" --guest-sha256 "$guest_sha256" \
  --agent-image "$DSX_AGENT_IMAGE" --browser-image "$DSX_BROWSER_IMAGE" --syft-version "$SYFT_VERSION"
"$SCRIPT_DIR/artifacts.py" archive --root "$work/package" --output "$work/dsx-${VERSION}-darwin-arm64.zip"

xcrun notarytool submit "$work/dsx-${VERSION}-darwin-arm64.zip" \
  --keychain-profile "$NOTARY_KEYCHAIN_PROFILE" --wait --output-format json > "$work/notarization.json"
python3 - "$work/notarization.json" <<'PY'
import json, sys
value = json.load(open(sys.argv[1], encoding="utf-8"))
if value.get("status") != "Accepted" or not value.get("id"):
    raise SystemExit("dsx release: Apple notarization did not return Accepted with a submission ID")
PY
spctl --assess --type execute --verbose=4 "$work/package/bin/dsx"
EXPECTED_VERSION="$VERSION" EXPECTED_COMMIT="$COMMIT" \
  "$SCRIPT_DIR/verify.sh" --package "$work/package" --notarization-result "$work/notarization.json"

(
  cd "$work"
  for path in "dsx-${VERSION}-darwin-arm64.zip" notarization.json package/release-manifest.json package/dsx.spdx.json; do
    printf '%s  %s\n' "$(sha256_file "$path")" "${path#package/}"
  done > checksums.txt
)
mkdir -p "$(dirname "$SIGNED_OUT")"
mv "$work" "$SIGNED_OUT"
trap - EXIT
printf 'signed and notarized candidate verified at %s\n' "$SIGNED_OUT"
printf 'No artifact was published.\n'
