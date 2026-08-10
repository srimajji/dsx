#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=common.sh
source "$SCRIPT_DIR/common.sh"

package="${1:-}"
notarization_result="${2:-}"
[[ -n "$package" && -d "$package" ]] || release_die "usage: smoke-install.sh PACKAGE_DIRECTORY NOTARIZATION_RESULT [PREFIX]"
[[ -n "$notarization_result" && -f "$notarization_result" ]] || release_die "clean-install smoke requires Accepted notarization evidence"
"$SCRIPT_DIR/verify.sh" --package "$package" --notarization-result "$notarization_result"

owned_prefix=0
if [[ -n "${3:-}" ]]; then
  prefix="$3"
  shopt -s nullglob dotglob
  entries=("$prefix"/*)
  shopt -u nullglob dotglob
  [[ "${#entries[@]}" -eq 0 ]] || release_die "explicit smoke prefix must be empty"
else
  prefix="$(mktemp -d "${TMPDIR:-/tmp}/dsx-clean-install.XXXXXX")"
  owned_prefix=1
fi
cleanup() {
  if [[ "$owned_prefix" -eq 1 ]]; then
    rm -f "$prefix/bin/dsx" "$prefix/bin/dsx-guest" "$prefix/release-manifest.json" "$prefix/dsx.spdx.json"
    rmdir "$prefix/bin" "$prefix" 2>/dev/null || true
  fi
}
trap cleanup EXIT
mkdir -p "$prefix/bin"
install -m 0755 "$package/bin/dsx" "$prefix/bin/dsx"
install -m 0755 "$package/bin/dsx-guest" "$prefix/bin/dsx-guest"
install -m 0644 "$package/release-manifest.json" "$prefix/release-manifest.json"
install -m 0644 "$package/dsx.spdx.json" "$prefix/dsx.spdx.json"

metadata="$(mktemp "${TMPDIR:-/tmp}/dsx-installed-version.XXXXXX")"
trap 'rm -f "$metadata"; cleanup' EXIT
"$prefix/bin/dsx" --version --json > "$metadata"
"$SCRIPT_DIR/artifacts.py" verify --root "$prefix" --manifest "$prefix/release-manifest.json" --host-metadata "$metadata"
# Doctor is deliberately read-only. A clean release smoke must fail if the
# supported Apple runtime is not installed, attested, and healthy.
"$prefix/bin/dsx" doctor --format=json
printf 'clean-install release smoke verified at %s\n' "$prefix"
