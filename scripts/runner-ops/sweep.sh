#!/bin/bash
set -euo pipefail

readonly script_dir="$(CDPATH= cd -- "$(/usr/bin/dirname -- "$0")" && pwd -P)"
# shellcheck source=common.sh
source "$script_dir/common.sh"

[[ $# -eq 1 ]] || fail "usage: sweep.sh EVIDENCE_DIR"
readonly evidence_dir="$1"
[[ "$evidence_dir" = /* ]] || fail "sweeper evidence path must be absolute"
/bin/mkdir -p -m 0700 "$evidence_dir/sweeper"

validate_state_root
validate_github_identity
require_value GH_TOKEN
require_value GITHUB_API_URL
[[ "$GITHUB_API_URL" = "https://api.github.com" ]] || quarantine_host "unexpected GitHub API origin" "$GITHUB_API_URL"
require_program curl
require_program jq

for ledger in "$DSX_CI_STATE_ROOT"/ledgers/*.json; do
  [[ -e "$ledger" ]] || continue
  [[ -f "$ledger" && ! -L "$ledger" ]] || quarantine_host "ledger entry is not a regular file" "$ledger"
  if ! /usr/bin/jq -e --arg schema "$DSX_CI_SCHEMA" '
      .schema == $schema and
      (.status == "running" or .status == "cleanup_failed" or .status == "clean") and
      (.repository | type == "string") and
      (.github_run_id | type == "string" and test("^[0-9]+$")) and
      (.github_run_attempt | type == "string" and test("^[0-9]+$")) and
      (.baseline.resources | type == "object")
    ' "$ledger" >/dev/null; then
    quarantine_host "malformed or unknown runner ledger" "$(sha256_file "$ledger")"
  fi
  [[ "$(/usr/bin/jq -r '.status' "$ledger")" != "clean" ]] || continue

  ledger_repository="$(/usr/bin/jq -r '.repository' "$ledger")"
  ledger_run_id="$(/usr/bin/jq -r '.github_run_id' "$ledger")"
  [[ "$ledger_repository" = "$GITHUB_REPOSITORY" ]] || quarantine_host "stale ledger belongs to another repository" "$ledger_repository"
  response="$evidence_dir/sweeper/run-${ledger_run_id}.json"
  if ! /usr/bin/curl --fail --silent --show-error --proto '=https' --tlsv1.2 \
      -H 'Accept: application/vnd.github+json' \
      -H 'X-GitHub-Api-Version: 2022-11-28' \
      -H "Authorization: Bearer $GH_TOKEN" \
      "$GITHUB_API_URL/repos/$ledger_repository/actions/runs/$ledger_run_id" >"$response"; then
    quarantine_host "GitHub run state could not be established" "$ledger_run_id"
  fi
  /bin/chmod 0600 "$response"
  if ! /usr/bin/jq -e --argjson run_id "$ledger_run_id" '.id == $run_id and .status == "completed"' "$response" >/dev/null; then
    quarantine_host "stale ledger GitHub run is not terminal" "$ledger_run_id"
  fi

  recovery_dir="$evidence_dir/sweeper/recovered-${ledger_run_id}-$(/usr/bin/jq -r '.github_run_attempt' "$ledger")"
  /bin/mkdir -m 0700 "$recovery_dir"
  if ! "$script_dir/cleanup-owned.sh" "$ledger" "$recovery_dir/first-clean"; then
    quarantine_host "terminal-run sweep could not prove exact cleanup" "$(sha256_file "$ledger")"
  fi
  if ! "$script_dir/cleanup-owned.sh" "$ledger" "$recovery_dir/repeated-clean"; then
    quarantine_host "terminal-run sweep could not prove repeated exact cleanup" "$(sha256_file "$ledger")"
  fi
done
