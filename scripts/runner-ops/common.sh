#!/bin/bash
set -euo pipefail

umask 077

readonly DSX_CI_SCHEMA="dsx.ci.runner/v1"
readonly DSX_CI_SENTINEL_IMAGE="docker.io/library/alpine@sha256:2c9d26f410d032d5b1525aa8a873e238b05b90c4ae8618743d4311f0cc827e37"

fail() {
  printf 'dsx-ci: %s\n' "$*" >&2
  return 1
}

require_program() {
  command -v "$1" >/dev/null 2>&1 || fail "required program is unavailable: $1"
}

require_value() {
  local name="$1"
  [[ -n "${!name:-}" ]] || fail "required environment value is empty: $name"
}

sha256_file() {
  /usr/bin/shasum -a 256 "$1" | /usr/bin/cut -d ' ' -f 1
}

atomic_json() {
  local destination="$1"
  local temporary="${destination}.tmp.$$"
  /usr/bin/jq -S . >"$temporary"
  /bin/chmod 0600 "$temporary"
  /bin/mv -f "$temporary" "$destination"
}

validate_state_root() {
  require_value DSX_CI_STATE_ROOT
  [[ "$DSX_CI_STATE_ROOT" = /* ]] || fail "DSX_CI_STATE_ROOT must be absolute"
  [[ -d "$DSX_CI_STATE_ROOT" && ! -L "$DSX_CI_STATE_ROOT" ]] || fail "runner state root is absent or a symlink"
  [[ "$(/usr/bin/stat -f '%u' "$DSX_CI_STATE_ROOT")" = "$(/usr/bin/id -u)" ]] || fail "runner state root is not owned by the runner account"
  local mode
  mode="$(/usr/bin/stat -f '%Lp' "$DSX_CI_STATE_ROOT")"
  (( 8#$mode <= 8#700 )) || fail "runner state root permissions must be 0700 or stricter"
  local required required_mode
  for required in ledgers quarantine; do
    [[ -d "$DSX_CI_STATE_ROOT/$required" && ! -L "$DSX_CI_STATE_ROOT/$required" ]] || fail "runner state layout is incomplete: $required"
    [[ "$(/usr/bin/stat -f '%u' "$DSX_CI_STATE_ROOT/$required")" = "$(/usr/bin/id -u)" ]] || fail "runner state directory is foreign-owned: $required"
    required_mode="$(/usr/bin/stat -f '%Lp' "$DSX_CI_STATE_ROOT/$required")"
    (( 8#$required_mode <= 8#700 )) || fail "runner state directory permissions are too broad: $required"
  done
}

quarantine_host() {
  local reason="$1"
  local detail="${2:-}"
  local marker="$DSX_CI_STATE_ROOT/QUARANTINED.json"
  if [[ -e "$marker" ]]; then
    if [[ -n "${DSX_CI_EVIDENCE_DIR:-}" && "$DSX_CI_EVIDENCE_DIR" = /* && -d "$DSX_CI_EVIDENCE_DIR" && ! -L "$DSX_CI_EVIDENCE_DIR" ]]; then
      /bin/cp -p "$marker" "$DSX_CI_EVIDENCE_DIR/quarantine.json"
    fi
    printf 'dsx-ci: host remains quarantined; original marker preserved: %s\n' "$reason" >&2
    return 1
  fi
  /usr/bin/jq -n -S \
    --arg schema "$DSX_CI_SCHEMA" \
    --arg reason "$reason" \
    --arg detail "$detail" \
    --arg repository "${GITHUB_REPOSITORY:-unknown}" \
    --arg run_id "${GITHUB_RUN_ID:-unknown}" \
    --arg run_attempt "${GITHUB_RUN_ATTEMPT:-unknown}" \
    --arg created_at "$(/bin/date -u '+%Y-%m-%dT%H:%M:%SZ')" \
    '{schema:$schema,status:"quarantined",reason:$reason,detail:$detail,repository:$repository,github_run_id:$run_id,github_run_attempt:$run_attempt,created_at:$created_at}' \
    | atomic_json "$marker"
  printf 'dsx-ci: host quarantined: %s\n' "$reason" >&2
  if [[ -n "${DSX_CI_EVIDENCE_DIR:-}" && "$DSX_CI_EVIDENCE_DIR" = /* && -d "$DSX_CI_EVIDENCE_DIR" && ! -L "$DSX_CI_EVIDENCE_DIR" ]]; then
    /bin/cp -p "$marker" "$DSX_CI_EVIDENCE_DIR/quarantine.json"
  fi
  return 1
}

require_not_quarantined() {
  [[ ! -e "$DSX_CI_STATE_ROOT/QUARANTINED.json" ]] || fail "runner is quarantined; recovery is manual and fail closed"
}

validate_github_identity() {
  require_value GITHUB_REPOSITORY
  require_value GITHUB_RUN_ID
  require_value GITHUB_RUN_ATTEMPT
  [[ "$GITHUB_REPOSITORY" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || fail "invalid GITHUB_REPOSITORY"
  [[ "$GITHUB_RUN_ID" =~ ^[0-9]+$ && "$GITHUB_RUN_ATTEMPT" =~ ^[0-9]+$ ]] || fail "invalid GitHub run identity"
}

ledger_path() {
  local os_major="$1"
  local repository_key
  repository_key="$(printf '%s' "$GITHUB_REPOSITORY" | /usr/bin/tr '/.' '__')"
  printf '%s/ledgers/%s-%s-%s-macos-%s.json' "$DSX_CI_STATE_ROOT" "$repository_key" "$GITHUB_RUN_ID" "$GITHUB_RUN_ATTEMPT" "$os_major"
}

update_ledger() {
  local ledger="$1"
  shift
  local temporary="${ledger}.tmp.$$"
  /usr/bin/jq "$@" "$ledger" | /usr/bin/jq -S . >"$temporary"
  /bin/chmod 0600 "$temporary"
  /bin/mv -f "$temporary" "$ledger"
}
