#!/bin/bash
set -euo pipefail

release_die() {
  printf 'dsx release: %s\n' "$*" >&2
  exit 1
}

require_tool() {
  command -v "$1" >/dev/null 2>&1 || release_die "required tool is unavailable: $1"
}

require_value() {
  local name="$1"
  local value="${!name:-}"
  [[ -n "$value" ]] || release_die "required release input is missing: $name"
  case "$value" in
    unknown|UNKNOWN|unset|UNSET|placeholder|PLACEHOLDER|-) release_die "$name is not a real configured value" ;;
  esac
}

require_sha256() {
  local name="$1"
  local value="${!name:-}"
  [[ "$value" =~ ^[0-9a-f]{64}$ ]] || release_die "$name must be 64 lowercase hexadecimal SHA-256 characters"
}

require_image_pin() {
  local name="$1"
  local value="${!name:-}"
  require_value "$name"
  [[ "$value" =~ ^[a-z0-9.-]+(:[0-9]+)?/[^[:space:]@]+@sha256:[0-9a-f]{64}$ ]] || release_die "$name must be a published immutable registry reference ending in @sha256:<64 lowercase hex>"
  [[ "$value" != localhost/* && "$value" != dsx.local/* ]] || release_die "$name must reference a published registry, not a local image name"
}

sha256_file() {
  shasum -a 256 "$1" | cut -d ' ' -f 1
}

rfc3339_from_epoch() {
  python3 - "$1" <<'PY'
import datetime as dt
import sys
try:
    epoch = int(sys.argv[1])
except ValueError:
    raise SystemExit("SOURCE_DATE_EPOCH must be an integer")
print(dt.datetime.fromtimestamp(epoch, dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"))
PY
}
