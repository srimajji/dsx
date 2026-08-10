#!/bin/bash
set -euo pipefail

readonly script_dir="$(CDPATH= cd -- "$(/usr/bin/dirname -- "$0")" && pwd -P)"
# shellcheck source=common.sh
source "$script_dir/common.sh"

[[ $# -eq 1 ]] || fail "usage: inventory.sh OUTPUT.json"
readonly output="$1"
[[ "$output" = /* ]] || fail "inventory output must be absolute"

require_program jq
require_program container
readonly container_cli="$(command -v container)"
[[ "$container_cli" = /* ]] || fail "container executable must resolve to an absolute path"

readonly work="${output}.work.$$"
/bin/mkdir -m 0700 "$work"
cleanup_work() {
  /bin/rm -f -- \
    "$work/containers.raw.json" "$work/networks.raw.json" "$work/volumes.raw.json" \
    "$work/builder.raw.json" "$work/version.raw.json" "$work/status.raw.json" \
    "$work/containers.json" "$work/networks.json" "$work/volumes.json" \
    "$work/builder.json" "$work/version.json" "$work/status.json"
  /bin/rmdir "$work"
}
trap cleanup_work EXIT

"$container_cli" list --all --format json >"$work/containers.raw.json"
"$container_cli" network list --format json >"$work/networks.raw.json"
"$container_cli" volume list --format json >"$work/volumes.raw.json"
"$container_cli" builder status --format json >"$work/builder.raw.json"
"$container_cli" system version --format json >"$work/version.raw.json"
"$container_cli" system status --format json >"$work/status.raw.json"

/usr/bin/jq -e -S '
  if type != "array" then error("container inventory is not an array") else . end
  | map({
      id: (.configuration.id // .id // ""),
      name: (.configuration.id // .id // ""),
      state: (.status.state // ""),
      labels: (.configuration.labels // {}),
      snapshot: .
    })
  | if any(.[]; (.id | type != "string" or length == 0)) then error("container inventory has an empty identity") else . end
  | if (map(.id) | unique | length) != length then error("container inventory has duplicate identities") else . end
  | sort_by(.id)
' "$work/containers.raw.json" >"$work/containers.json"

for kind in networks volumes; do
  /usr/bin/jq -e -S --arg kind "${kind%s}" '
    if type != "array" then error($kind + " inventory is not an array") else . end
    | map({
        id: (.id // ""),
        name: (.configuration.name // .name // .id // ""),
        state: (.status.state // "created"),
        labels: (.configuration.labels // .labels // {}),
        snapshot: .
      })
    | if any(.[]; (.id | type != "string" or length == 0) or (.name | type != "string" or length == 0)) then error($kind + " inventory has an empty identity") else . end
    | if (map(.id) | unique | length) != length then error($kind + " inventory has duplicate identities") else . end
    | sort_by(.id)
  ' "$work/${kind}.raw.json" >"$work/${kind}.json"
done

/usr/bin/jq -e -S 'if type == "array" and length > 0 then sort_by(.configuration.id // .id // "") else error("builder status is absent") end' "$work/builder.raw.json" >"$work/builder.json"
/usr/bin/jq -e -S 'if type == "array" and length == 2 and all(.[]; .appName and .version) then sort_by(.appName) else error("system version pair is invalid") end' "$work/version.raw.json" >"$work/version.json"
/usr/bin/jq -e -S 'if type == "object" then . else error("system status is not an object") end' "$work/status.raw.json" >"$work/status.json"

/usr/bin/jq -n -S \
  --arg schema "$DSX_CI_SCHEMA" \
  --arg captured_at "$(/bin/date -u '+%Y-%m-%dT%H:%M:%SZ')" \
  --arg os_version "$(/usr/bin/sw_vers -productVersion)" \
  --arg arch "$(/usr/bin/uname -m)" \
  --slurpfile containers "$work/containers.json" \
  --slurpfile networks "$work/networks.json" \
  --slurpfile volumes "$work/volumes.json" \
  --slurpfile builder "$work/builder.json" \
  --slurpfile runtime_version "$work/version.json" \
  --slurpfile runtime_status "$work/status.json" \
  '{schema:$schema,captured_at:$captured_at,host:{os_version:$os_version,arch:$arch},runtime:{version:$runtime_version[0],status:$runtime_status[0],builder:$builder[0]},resources:{containers:$containers[0],networks:$networks[0],volumes:$volumes[0]}}' \
  | atomic_json "$output"
