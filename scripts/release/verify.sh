#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=common.sh
source "$SCRIPT_DIR/common.sh"

package=""
notarization_result=""
allow_unsigned=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --package) [[ $# -ge 2 ]] || release_die "--package requires a path"; package="$2"; shift 2 ;;
    --notarization-result) [[ $# -ge 2 ]] || release_die "--notarization-result requires a path"; notarization_result="$2"; shift 2 ;;
    --allow-unsigned) allow_unsigned=1; shift ;;
    *) release_die "unknown verification argument: $1" ;;
  esac
done
[[ -n "$package" && -d "$package" ]] || release_die "--package must name an extracted release package"
[[ "$(uname -s)" == "Darwin" ]] || release_die "release verification requires macOS"
require_tool python3
require_tool codesign

metadata="$(mktemp "${TMPDIR:-/tmp}/dsx-version.XXXXXX")"
signature_details="${metadata}.codesign"
trap 'rm -f "$metadata" "$signature_details"' EXIT
verify_args=(verify --root "$package" --manifest "$package/release-manifest.json")
[[ -z "${EXPECTED_VERSION:-}" ]] || verify_args+=(--expected-version "$EXPECTED_VERSION")
[[ -z "${EXPECTED_COMMIT:-}" ]] || verify_args+=(--expected-commit "$EXPECTED_COMMIT")
"$SCRIPT_DIR/artifacts.py" "${verify_args[@]}"

if [[ "$allow_unsigned" -eq 1 ]]; then
  [[ -z "$notarization_result" ]] || release_die "unsigned dry-run cannot claim notarization evidence"
  if codesign --verify --strict "$package/bin/dsx" >/dev/null 2>&1; then
    release_die "--allow-unsigned was used for a signed candidate; run strict verification"
  fi
  "$package/bin/dsx" --version --json > "$metadata"
  "$SCRIPT_DIR/artifacts.py" "${verify_args[@]}" --host-metadata "$metadata"
  printf 'unsigned artifact integrity verified\n'
  printf 'BLOCKED: Developer ID signature and Accepted Apple notarization evidence are absent.\n'
  exit 0
fi

require_tool spctl
codesign --verify --strict --verbose=4 "$package/bin/dsx"
codesign --display --verbose=4 "$package/bin/dsx" >"$signature_details" 2>&1
[[ -n "$notarization_result" && -f "$notarization_result" ]] || release_die "Accepted Apple notarization result is required"
"$SCRIPT_DIR/artifacts.py" verify-security --codesign-details "$signature_details" --notarization-result "$notarization_result"
spctl --assess --type execute --verbose=4 "$package/bin/dsx"
"$package/bin/dsx" --version --json > "$metadata"
"$SCRIPT_DIR/artifacts.py" "${verify_args[@]}" --host-metadata "$metadata"
printf 'signed and notarized release candidate verified\n'
