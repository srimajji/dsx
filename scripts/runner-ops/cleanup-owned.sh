#!/bin/bash
set -euo pipefail

readonly script_dir="$(CDPATH= cd -- "$(/usr/bin/dirname -- "$0")" && pwd -P)"
# shellcheck source=common.sh
source "$script_dir/common.sh"

[[ $# -eq 2 ]] || fail "usage: cleanup-owned.sh LEDGER.json EVIDENCE_DIR"
readonly ledger="$1"
readonly evidence_dir="$2"
[[ "$ledger" = /* && "$evidence_dir" = /* ]] || fail "cleanup paths must be absolute"
[[ -f "$ledger" && ! -L "$ledger" ]] || fail "cleanup ledger is absent or a symlink"
readonly ledger_status="$(/usr/bin/jq -r '.status // ""' "$ledger")"
/bin/mkdir -p -m 0700 "$evidence_dir/cleanup"
require_program jq
require_program container
if ! /usr/bin/jq -e --arg schema "$DSX_CI_SCHEMA" '
    .schema == $schema and
    (.baseline.resources.containers | type == "array") and
    (.baseline.resources.networks | type == "array") and
    (.baseline.resources.volumes | type == "array") and
    (.sentinels | type == "array" and length <= 3) and
    ((.sentinels | map(.name) | unique | length) == (.sentinels | length)) and
    all(.sentinels[];
      (.kind == "container" or .kind == "volume" or .kind == "network") and
      (.name | type == "string" and test("^dsx-sentinel-[0-9]+-[0-9]+-[0-9a-f]{8}-[cvn]$")) and
      ((.kind == "container" and (.name | endswith("-c"))) or
       (.kind == "volume" and (.name | endswith("-v"))) or
       (.kind == "network" and (.name | endswith("-n")))) and
      .intent_written == true and
      (.created == false or (.created == true and (.digest | type == "string" and test("^[0-9a-f]{64}$"))))
    )
  ' "$ledger" >/dev/null; then
  quarantine_host "cleanup ledger ownership scope is invalid" "$(sha256_file "$ledger")"
fi
readonly container_cli="$(command -v container)"
readonly current="$evidence_dir/cleanup/current-$$.json"
readonly plan="$evidence_dir/cleanup/plan-$$.json"

"$script_dir/inventory.sh" "$current"

/usr/bin/jq -n -e -S --slurpfile ledger "$ledger" --slurpfile current "$current" '
  def valid_slug: type == "string" and test("^[a-z0-9]([a-z0-9-]{0,22}[a-z0-9])?$");
  def valid_project: type == "string" and test("^[a-z2-7]{20}$");
  def valid_run: type == "string" and test("^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$");
  def expected_kind($bucket): if $bucket == "containers" then ["workspace","browser"] elif $bucket == "networks" then ["network"] else ["volume"] end;
  def classify($bucket; $baseline_ids; $sentinel_names):
    $current[0].resources[$bucket][]
    | . as $resource
    | select(($baseline_ids | index($resource.id)) == null)
    | select(($sentinel_names | index($resource.name)) == null)
    | ($resource.labels // {}) as $labels
    | ($labels["dev.dsx.project"] // "") as $project
    | ($labels["dev.dsx.sandbox"] // "") as $sandbox
    | ($labels["dev.dsx.run"] // "") as $run
    | ($labels["dev.dsx.kind"] // "") as $kind
    | ($labels["dev.dsx.role"] // "") as $role
    | ($labels | keys | map(select(startswith("dev.dsx.")))) as $dsx_keys
    | {
        bucket:$bucket,id:$resource.id,name:$resource.name,state:$resource.state,labels:$labels,snapshot:$resource.snapshot,kind:$kind,
        valid:(
          ($labels | type == "object" and length == 7) and
          ($dsx_keys | length == 7) and
          $labels["dev.dsx.managed"] == "true" and
          $labels["dev.dsx.contract"] == "dsx.ownership/v1" and
          ($project | valid_project) and ($sandbox | valid_slug) and ($role | valid_slug) and ($run | valid_run) and
          ((expected_kind($bucket) | index($kind)) != null) and
          $resource.id == $resource.name and
          $resource.name == ("dsx-" + $project + "-" + $sandbox + "-" + $role)
        )
      };
  ($ledger[0].baseline.resources // error("ledger baseline is absent")) as $baseline
  | (($ledger[0].sentinels // []) | map(.name)) as $sentinels
  | ["containers","volumes","networks"] as $buckets
  | [ $buckets[] as $bucket | classify($bucket; ($baseline[$bucket] | map(.id)); $sentinels) ] as $resources
  | {schema:"dsx.ci.cleanup-plan/v1",resources:$resources,ambiguous:[$resources[] | select(.valid != true)]}
' | atomic_json "$plan"

if [[ "$(/usr/bin/jq '.ambiguous | length' "$plan")" != "0" ]]; then
  quarantine_host "ambiguous runtime ownership during exact cleanup" "$(sha256_file "$plan")"
fi

verify_planned_identity() {
  local bucket="$1" id="$2" expected_file="$3" observed_file="$4"
  "$script_dir/inventory.sh" "$observed_file"
  /usr/bin/jq -e --arg bucket "$bucket" --arg id "$id" --slurpfile expected "$expected_file" '
    [.resources[$bucket][] | select(.id == $id)] as $matches
    | ($matches | length) == 1
      and $matches[0].id == $expected[0].id
      and $matches[0].name == $expected[0].name
      and $matches[0].labels == $expected[0].labels
      and $matches[0].snapshot == $expected[0].snapshot
  ' "$observed_file" >/dev/null
}

readonly delete_log="$evidence_dir/cleanup/deletions.jsonl"
: >"$delete_log"
run_exact_command() {
  local postcondition="$1"
  shift
  set +e
  "$@"
  local exit_code=$?
  set -e
  /usr/bin/jq -n -c --arg postcondition "$postcondition" --argjson exit "$exit_code" --args \
    '$ARGS.positional as $argv | {argv:$argv,exit:$exit,postcondition:(if $exit == 0 then $postcondition else "command failed before postcondition" end)}' "$@" >>"$delete_log"
  return "$exit_code"
}
while IFS=$'\t' read -r bucket id; do
  [[ -n "$bucket" && -n "$id" ]] || continue
  expected="${current}.expected.$$.json"
  observed="${current}.observed.$$.json"
  /usr/bin/jq -S --arg bucket "$bucket" --arg id "$id" '.resources[] | select(.bucket == $bucket and .id == $id)' "$plan" >"$expected"
  if ! verify_planned_identity "$bucket" "$id" "$expected" "$observed"; then
    quarantine_host "resource identity changed before exact deletion" "$bucket:$id"
  fi
  local_state="$(/usr/bin/jq -r --arg bucket "$bucket" --arg id "$id" '.resources[$bucket][] | select(.id == $id) | .state' "$observed")"
  if [[ "$bucket" = "containers" && "$local_state" != "stopped" ]]; then
    run_exact_command "stopped exact owned container" "$container_cli" stop --time 10 "$id"
  fi
  case "$bucket" in
    containers)
      run_exact_command "exact identity absent" "$container_cli" delete "$id"
      ;;
    volumes)
      run_exact_command "exact identity absent" "$container_cli" volume delete "$id"
      ;;
    networks)
      run_exact_command "exact identity absent" "$container_cli" network delete "$id"
      ;;
    *) quarantine_host "cleanup plan contains an unknown bucket" "$bucket" ;;
  esac
done < <(/usr/bin/jq -r '.resources[] | select(.valid == true) | [.bucket,.id] | @tsv' "$plan")

sentinel_digest() {
  local kind="$1" name="$2" destination="$3"
  case "$kind" in
    container) "$container_cli" inspect "$name" ;;
    volume) "$container_cli" volume inspect "$name" ;;
    network) "$container_cli" network inspect "$name" ;;
    *) return 2 ;;
  esac | /usr/bin/jq -S . >"$destination"
  sha256_file "$destination"
}

while IFS=$'\t' read -r kind name created expected_digest; do
  [[ -n "$kind" && -n "$name" ]] || continue
  sentinel_inventory="${current}.sentinel.$$.json"
  if observed_digest="$(sentinel_digest "$kind" "$name" "$sentinel_inventory" 2>/dev/null)"; then
    if [[ "$created" = "true" && ( -z "$expected_digest" || "$observed_digest" != "$expected_digest" ) ]]; then
      quarantine_host "runner sentinel changed" "$kind:$name"
    fi
    if ! /usr/bin/jq -e '[.. | objects | keys[] | select(startswith("dev.dsx."))] | length == 0' "$sentinel_inventory" >/dev/null; then
      quarantine_host "runner sentinel acquired DSX ownership evidence" "$kind:$name"
    fi
    if [[ "$kind" = "container" ]]; then
      sentinel_state="$(/usr/bin/jq -r '.[0].status.state // ""' "$sentinel_inventory")"
      [[ "$sentinel_state" = "stopped" ]] || quarantine_host "runner container sentinel is not stopped" "$name"
      run_exact_command "exact ledger sentinel absent" "$container_cli" delete "$name"
    elif [[ "$kind" = "volume" ]]; then
      run_exact_command "exact ledger sentinel absent" "$container_cli" volume delete "$name"
    else
      run_exact_command "exact ledger sentinel absent" "$container_cli" network delete "$name"
    fi
  elif [[ "$created" = "true" && "$ledger_status" != "clean" ]]; then
    quarantine_host "recorded runner sentinel is missing before comparison" "$kind:$name"
  fi
done < <(/usr/bin/jq -r '.sentinels[]? | [.kind,.name,(.created|tostring),(.digest // "")] | @tsv' "$ledger")

readonly final="$evidence_dir/cleanup/final.json"
"$script_dir/inventory.sh" "$final"
if ! /usr/bin/jq -e --slurpfile baseline "$ledger" '
  .resources == $baseline[0].baseline.resources and .runtime.builder == $baseline[0].baseline.runtime.builder
' "$final" >/dev/null; then
  quarantine_host "post-cleanup runtime or builder differs from baseline" "$(sha256_file "$final")"
fi

/usr/bin/jq -s -S '.' "$delete_log" >"$evidence_dir/cleanup/deletions.json"
if [[ "$ledger_status" = "clean" ]]; then
  repeated_clean=true
else
  repeated_clean=false
fi
update_ledger "$ledger" \
  --arg finished_at "$(/bin/date -u '+%Y-%m-%dT%H:%M:%SZ')" \
  --arg final_digest "$(sha256_file "$final")" \
  --argjson repeated_clean "$repeated_clean" \
  '.status="clean" | .finished_at=$finished_at | .cleanup={exact:true,repeated_clean:$repeated_clean,final_inventory_digest:$final_digest}'
