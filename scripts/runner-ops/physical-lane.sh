#!/bin/bash
set -euo pipefail

readonly script_dir="$(CDPATH= cd -- "$(/usr/bin/dirname -- "$0")" && pwd -P)"
# shellcheck source=common.sh
source "$script_dir/common.sh"

[[ $# -eq 1 && ( "$1" = "begin" || "$1" = "finish" ) ]] || fail "usage: physical-lane.sh begin|finish"
readonly operation="$1"

require_program jq
validate_state_root
validate_github_identity
require_value DSX_CI_EXPECTED_OS_MAJOR
[[ "$DSX_CI_EXPECTED_OS_MAJOR" = "26" || "$DSX_CI_EXPECTED_OS_MAJOR" = "27" ]] || fail "unsupported physical lane OS major"
readonly ledger="$(ledger_path "$DSX_CI_EXPECTED_OS_MAJOR")"
readonly lock="$DSX_CI_STATE_ROOT/host.lock"
require_value DSX_CI_EVIDENCE_DIR
[[ "$DSX_CI_EVIDENCE_DIR" = /* ]] || fail "DSX_CI_EVIDENCE_DIR must be absolute"

attest_host() {
  local inventory="$1"
  local os_major
  os_major="$(/usr/bin/jq -r '.host.os_version | split(".")[0]' "$inventory")"
  [[ "$os_major" = "$DSX_CI_EXPECTED_OS_MAJOR" ]] || quarantine_host "host OS does not match assigned lane" "$os_major"
  [[ "$(/usr/bin/jq -r '.host.arch' "$inventory")" = "arm64" ]] || quarantine_host "physical runner is not arm64" "$(/usr/bin/jq -r '.host.arch' "$inventory")"
  /usr/bin/jq -e '
    ([.runtime.version[] | select(.appName == "container") | .version] == ["1.2.2"]) and
    ([.runtime.version[] | select(.appName == "container-apiserver") | .version | capture("(?<version>[0-9]+\\.[0-9]+\\.[0-9]+)").version] == ["1.2.2"]) and
    (.runtime.status.status == "running") and
    ((.runtime.status.apiServerVersion | capture("(?<version>[0-9]+\\.[0-9]+\\.[0-9]+)").version) == "1.2.2") and
    (.runtime.builder | type == "array" and length > 0)
  ' "$inventory" >/dev/null || quarantine_host "Apple CLI/server 1.2.2 attestation failed" "$(sha256_file "$inventory")"
}

sentinel_inspect() {
  local kind="$1" name="$2" destination="$3"
  case "$kind" in
    container) container inspect "$name" ;;
    volume) container volume inspect "$name" ;;
    network) container network inspect "$name" ;;
    *) return 2 ;;
  esac | /usr/bin/jq -S . >"$destination"
}

create_sentinel() {
  local kind="$1" name="$2"
  update_ledger "$ledger" --arg kind "$kind" --arg name "$name" \
    '.sentinels += [{kind:$kind,name:$name,created:false,intent_written:true}]'
  case "$kind" in
    container)
      "$script_dir/record-command.sh" sentinel-container-create container create --name "$name" --label "dev.dsxci.sentinel=$(/usr/bin/jq -r '.run_label' "$ledger")" --entrypoint /bin/true "$DSX_CI_SENTINEL_IMAGE" >/dev/null
      ;;
    volume)
      "$script_dir/record-command.sh" sentinel-volume-create container volume create --label "dev.dsxci.sentinel=$(/usr/bin/jq -r '.run_label' "$ledger")" "$name" >/dev/null
      ;;
    network)
      "$script_dir/record-command.sh" sentinel-network-create container network create --label "dev.dsxci.sentinel=$(/usr/bin/jq -r '.run_label' "$ledger")" "$name" >/dev/null
      ;;
  esac
  local inspected="$DSX_CI_EVIDENCE_DIR/sentinels/$kind.json"
  sentinel_inspect "$kind" "$name" "$inspected"
  if [[ "$kind" = "container" ]] && ! /usr/bin/jq -e '.[0].status.state == "stopped"' "$inspected" >/dev/null; then
    quarantine_host "runner container sentinel was not created stopped" "$name"
  fi
  local digest
  digest="$(sha256_file "$inspected")"
  update_ledger "$ledger" --arg kind "$kind" --arg name "$name" --arg digest "$digest" \
    '(.sentinels[] | select(.kind == $kind and .name == $name)) |= (.created=true | .digest=$digest)'
}

recover_terminal_lock() {
  [[ -d "$lock" && ! -L "$lock" && -f "$lock/owner.json" ]] || quarantine_host "host-global lock shape is uncertain" "$lock"
  if ! /usr/bin/jq -e --arg repository "$GITHUB_REPOSITORY" '
      .repository == $repository and
      (.github_run_id | type == "string" and test("^[0-9]+$")) and
      (.github_run_attempt | type == "string" and test("^[0-9]+$")) and
      (.os_major == "26" or .os_major == "27")
    ' "$lock/owner.json" >/dev/null; then
    quarantine_host "host-global lock owner is malformed or foreign" "$(sha256_file "$lock/owner.json")"
  fi
  "$script_dir/sweep.sh" "$DSX_CI_EVIDENCE_DIR"
  local old_run old_attempt old_major repository_key old_ledger
  old_run="$(/usr/bin/jq -r '.github_run_id' "$lock/owner.json")"
  old_attempt="$(/usr/bin/jq -r '.github_run_attempt' "$lock/owner.json")"
  old_major="$(/usr/bin/jq -r '.os_major' "$lock/owner.json")"
  repository_key="$(printf '%s' "$GITHUB_REPOSITORY" | /usr/bin/tr '/.' '__')"
  old_ledger="$DSX_CI_STATE_ROOT/ledgers/${repository_key}-${old_run}-${old_attempt}-macos-${old_major}.json"
  [[ -f "$old_ledger" ]] || quarantine_host "stale lock has no exact ledger" "$old_run"
  [[ "$(/usr/bin/jq -r '.status // ""' "$old_ledger")" = "clean" ]] || quarantine_host "stale lock ledger was not reconciled" "$old_run"
  /bin/rm -f -- "$lock/owner.json"
  /bin/rmdir "$lock"
}

begin_lane() {
  [[ "${GITHUB_REF_PROTECTED:-false}" = "true" ]] || fail "destructive runner requires a protected ref"
  [[ "${GITHUB_EVENT_NAME:-}" = "workflow_dispatch" || "${GITHUB_EVENT_NAME:-}" = "push" ]] || fail "destructive runner rejects this event"
  if [[ "${GITHUB_EVENT_NAME:-}" = "push" ]]; then
    [[ "${GITHUB_REF:-}" = "refs/heads/main" || "${GITHUB_REF:-}" =~ ^refs/tags/v[0-9] ]] || fail "push ref is not a trusted release ref"
  fi
  require_not_quarantined
  [[ ! -e "$DSX_CI_EVIDENCE_DIR" ]] || quarantine_host "run evidence directory already exists" "$DSX_CI_EVIDENCE_DIR"
  /bin/mkdir -p -m 0700 "$DSX_CI_EVIDENCE_DIR" "$DSX_CI_EVIDENCE_DIR/sentinels"
  [[ ! -L "$DSX_CI_EVIDENCE_DIR" && "$(/usr/bin/stat -f '%u' "$DSX_CI_EVIDENCE_DIR")" = "$(/usr/bin/id -u)" ]] || quarantine_host "run evidence directory is unsafe" "$DSX_CI_EVIDENCE_DIR"
  if ! /bin/mkdir -m 0700 "$lock" 2>/dev/null; then
    recover_terminal_lock
    /bin/mkdir -m 0700 "$lock" 2>/dev/null || quarantine_host "host-global lock could not be acquired after terminal recovery" "$lock"
  fi
  /usr/bin/jq -n -S --arg repository "$GITHUB_REPOSITORY" --arg run_id "$GITHUB_RUN_ID" --arg run_attempt "$GITHUB_RUN_ATTEMPT" --arg os_major "$DSX_CI_EXPECTED_OS_MAJOR" \
    '{repository:$repository,github_run_id:$run_id,github_run_attempt:$run_attempt,os_major:$os_major}' | atomic_json "$lock/owner.json"

  "$script_dir/sweep.sh" "$DSX_CI_EVIDENCE_DIR"
  readonly baseline="$DSX_CI_EVIDENCE_DIR/baseline.json"
  "$script_dir/inventory.sh" "$baseline"
  attest_host "$baseline"

  local random repository_name run_label suffix
  random="$(/usr/bin/od -An -N4 -tx1 /dev/urandom | /usr/bin/tr -d ' \n')"
  repository_name="$(printf '%s' "${GITHUB_REPOSITORY#*/}" | /usr/bin/tr '[:upper:]_.' '[:lower:]--' | /usr/bin/cut -c1-20)"
  run_label="dsxci-${repository_name}-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}-${random}"
  suffix="$(printf '%s-%s-%s' "$GITHUB_RUN_ID" "$GITHUB_RUN_ATTEMPT" "$random" | /usr/bin/tail -c 30)"

  /usr/bin/jq -n -S \
    --arg schema "$DSX_CI_SCHEMA" --arg status "running" --arg run_label "$run_label" \
    --arg repository "$GITHUB_REPOSITORY" --arg run_id "$GITHUB_RUN_ID" --arg run_attempt "$GITHUB_RUN_ATTEMPT" \
    --arg ref "${GITHUB_REF:-}" --arg sha "${GITHUB_SHA:-}" --arg os_major "$DSX_CI_EXPECTED_OS_MAJOR" \
    --arg started_at "$(/bin/date -u '+%Y-%m-%dT%H:%M:%SZ')" --arg baseline_digest "$(sha256_file "$baseline")" \
    --slurpfile baseline "$baseline" \
    '{schema:$schema,status:$status,run_label:$run_label,repository:$repository,github_run_id:$run_id,github_run_attempt:$run_attempt,ref:$ref,sha:$sha,expected_os_major:$os_major,started_at:$started_at,baseline_digest:$baseline_digest,baseline:$baseline[0],sentinels:[]}' \
    | atomic_json "$ledger"

  create_sentinel container "dsx-sentinel-${suffix}-c"
  create_sentinel volume "dsx-sentinel-${suffix}-v"
  create_sentinel network "dsx-sentinel-${suffix}-n"

  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
    {
      printf 'run_label=%s\n' "$run_label"
      printf 'ledger=%s\n' "$ledger"
    } >>"$GITHUB_OUTPUT"
  fi
}

finish_lane() {
  /bin/mkdir -p -m 0700 "$DSX_CI_EVIDENCE_DIR"
  if [[ ! -f "$ledger" ]]; then
    quarantine_host "physical lane ended without a durable ledger" "$ledger"
  fi
  if [[ ! -f "$lock/owner.json" ]] || ! /usr/bin/jq -e \
      --arg repository "$GITHUB_REPOSITORY" --arg run_id "$GITHUB_RUN_ID" --arg run_attempt "$GITHUB_RUN_ATTEMPT" --arg os_major "$DSX_CI_EXPECTED_OS_MAJOR" \
      '.repository == $repository and .github_run_id == $run_id and .github_run_attempt == $run_attempt and .os_major == $os_major' "$lock/owner.json" >/dev/null; then
    quarantine_host "host-global lock ownership is uncertain at cleanup" "$lock"
  fi

  if ! "$script_dir/cleanup-owned.sh" "$ledger" "$DSX_CI_EVIDENCE_DIR/first-clean"; then
    update_ledger "$ledger" '.status="cleanup_failed"'
    quarantine_host "exact ownership cleanup failed" "$(sha256_file "$ledger")"
  fi
  if ! "$script_dir/cleanup-owned.sh" "$ledger" "$DSX_CI_EVIDENCE_DIR/repeated-clean"; then
    update_ledger "$ledger" '.status="cleanup_failed"'
    quarantine_host "repeated exact cleanup failed" "$(sha256_file "$ledger")"
  fi

  local performance_status="invalid"
  local performance_evidence="$DSX_CI_EVIDENCE_DIR/performance.json"
  if [[ -f "$performance_evidence" && ! -L "$performance_evidence" ]] && /usr/bin/jq -e '
      .schema_version == 1 and
      (.runs | type == "number" and . >= 20) and
      ([.timings[] | .name] | sort == ["clean","inspect","planning","shell","shell_ready","start"]) and
      ([.timings[] | .sample_size] | all(. >= 20)) and
      (.guest_rss.sample_size >= 20) and
      (.guest_rss.max_bytes <= .guest_rss.budget_bytes)
    ' "$performance_evidence" >/dev/null; then
    performance_status="valid"
  fi
  /usr/bin/jq -n -S \
    --arg schema "$DSX_CI_SCHEMA" --arg ledger_digest "$(sha256_file "$ledger")" \
    --arg completed_at "$(/bin/date -u '+%Y-%m-%dT%H:%M:%SZ')" --arg performance_status "$performance_status" \
    --slurpfile ledger "$ledger" \
    --slurpfile commands <(/usr/bin/jq -s -S '.' "$DSX_CI_EVIDENCE_DIR"/commands/*.json 2>/dev/null || printf '[]') \
    '($commands[0] // []) as $command_evidence | {schema:$schema,run_verdict:(if ([$command_evidence[] | select(.label == "canary") | .exit] == [0]) and ([$command_evidence[] | select(.label == "apple-suites") | .exit] == [0]) and $performance_status == "valid" then "passed" else "incomplete_or_failed" end),host_cleanup_status:"clean",performance_evidence:$performance_status,completed_at:$completed_at,ledger_digest:$ledger_digest,baseline_digest:$ledger[0].baseline_digest,attestation:$ledger[0].baseline.host,runtime_attestation:$ledger[0].baseline.runtime,sentinels:$ledger[0].sentinels,cleanup:$ledger[0].cleanup,commands:$command_evidence}' \
    | atomic_json "$DSX_CI_EVIDENCE_DIR/dsx-084-runner-evidence.json"

  /bin/rm -f -- "$lock/owner.json"
  /bin/rmdir "$lock"
  [[ "$performance_status" = "valid" ]] || fail "physical lane performance evidence is absent or invalid"
}

if [[ "$operation" = "begin" ]]; then
  begin_lane
else
  finish_lane
fi
