#!/bin/bash
set -euo pipefail

readonly script_dir="$(CDPATH= cd -- "$(/usr/bin/dirname -- "$0")" && pwd -P)"
# shellcheck source=common.sh
source "$script_dir/common.sh"

[[ $# -ge 2 ]] || fail "usage: record-command.sh LABEL EXECUTABLE [ARG ...]"
readonly label="$1"
shift
[[ "$label" =~ ^[a-z0-9-]+$ ]] || fail "evidence command label is invalid"
require_value DSX_CI_EVIDENCE_DIR
readonly command_dir="$DSX_CI_EVIDENCE_DIR/commands"
/bin/mkdir -p -m 0700 "$command_dir"
readonly started_at="$(/bin/date -u '+%Y-%m-%dT%H:%M:%SZ')"

set +e
"$@"
readonly exit_code=$?
set -e

/usr/bin/jq -n -S \
  --arg schema "$DSX_CI_SCHEMA" \
  --arg label "$label" \
  --arg started_at "$started_at" \
  --arg finished_at "$(/bin/date -u '+%Y-%m-%dT%H:%M:%SZ')" \
  --argjson exit "$exit_code" \
  --args '$ARGS.positional as $argv | {schema:$schema,label:$label,started_at:$started_at,finished_at:$finished_at,argv:$argv,exit:$exit,postcondition:(if $exit == 0 then "completed" else "failed" end)}' \
  "$@" | atomic_json "$command_dir/$label.json"

exit "$exit_code"
