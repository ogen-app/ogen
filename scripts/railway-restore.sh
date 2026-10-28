#!/usr/bin/env bash
# Restore a Railway project's configuration from a scripts/railway-backup.sh
# backup through the public GraphQL API. Dry-run unless --apply is given.
#
# Modes:
#   --vars    (default) upsert service and shared variables from
#             variables/*.json. Services are matched by name, so this also
#             restores into another project or a recreated service.
#             ${{Service.VAR}} references are restored as references.
#   --config  stage the full environment config from config.json (source,
#             build, deploy, networking, volume mounts, variables) with
#             environmentStageChanges(merge: true). Same project only, since
#             config.json is keyed by service and volume ids. Services and
#             volumes that no longer exist are dropped; the stale `groups`
#             map and service groupIds are never restored. The changes stay
#             staged for review in the dashboard unless --commit is given.
#
# Usage:
#   RAILWAY_API_TOKEN=... scripts/railway-restore.sh [options] <backup> [environment ...]
#
#   <backup>          backup directory, or a .tar.gz.age archive (needs AGE_IDENTITY)
#   [environment ...] environments to restore (default: all in the backup)
#
# Options:
#   --apply           write changes (default: print the plan only)
#   --project ID      target project (default: the backup's project)
#   --to-env NAME     restore a single backed-up environment into NAME
#   --replace         --vars: delete variables that are not in the backup.
#                     Sealed variables are never in a backup, so this deletes them.
#   --deploy          trigger deploys for changed services (default: skip deploys)
#   --commit          --config: commit the staged changes right away
#
#   AGE_IDENTITY      age identity file used to decrypt a .tar.gz.age backup
set -euo pipefail

API=https://backboard.railway.com/graphql/v2

die() { echo "error: $*" >&2; exit 1; }

usage() { sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit "${1:-0}"; }

mode=vars apply=false replace=false skip_deploys=true commit=false
target_project= to_env=
while [[ $# -gt 0 ]]; do
  case $1 in
    --vars) mode=vars ;;
    --config) mode=config ;;
    --apply) apply=true ;;
    --replace) replace=true ;;
    --deploy) skip_deploys=false ;;
    --commit) commit=true ;;
    --project) target_project=${2:?--project needs a value}; shift ;;
    --to-env) to_env=${2:?--to-env needs a value}; shift ;;
    -h|--help) usage ;;
    -*) die "unknown option $1" ;;
    *) break ;;
  esac
  shift
done

[[ $# -ge 1 ]] || usage 1
[[ -n ${RAILWAY_API_TOKEN:-} ]] || die "RAILWAY_API_TOKEN is not set"
command -v jq >/dev/null || die "jq is required"
command -v curl >/dev/null || die "curl is required"

backup=$1
shift
only_envs=("$@")

if [[ -f $backup && $backup == *.age ]]; then
  [[ -n ${AGE_IDENTITY:-} ]] || die "AGE_IDENTITY is required to decrypt $backup"
  command -v age >/dev/null || die "age is not installed"
  umask 077
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  age -d -i "$AGE_IDENTITY" "$backup" | tar -xzf - -C "$tmp"
  backup=$(find "$tmp" -mindepth 1 -maxdepth 1 -type d | head -1)
fi
[[ -f $backup/project.json ]] || die "$backup is not a railway-backup.sh backup (no project.json)"

# gql QUERY [VARIABLES_JSON] -> prints .data, fails on GraphQL errors.
gql() {
  local query=$1 vars=${2:-'{}'} resp
  resp=$(curl -sS --fail-with-body "$API" \
    -H "Authorization: Bearer $RAILWAY_API_TOKEN" \
    -H 'Content-Type: application/json' \
    -d "$(jq -n --arg q "$query" --argjson v "$vars" '{query: $q, variables: $v}')") \
    || die "request failed: $resp"
  if jq -e '.errors | length > 0' >/dev/null <<<"$resp"; then
    die "GraphQL error: $(jq -c '.errors | map(.message)' <<<"$resp")"
  fi
  jq '.data' <<<"$resp"
}

safe_name() { printf '%s' "$1" | tr -c 'A-Za-z0-9._-' '_'; }

backup_project=$(jq -r '.id' "$backup/project.json")
target_project=${target_project:-$backup_project}
if [[ $mode == config && $target_project != "$backup_project" ]]; then
  die "--config restores into the backed-up project only ($backup_project); use --vars for another project"
fi

target=$(gql '
  query($id: String!) {
    project(id: $id) {
      id
      name
      environments { edges { node { id name } } }
      services { edges { node { id name } } }
      volumes { edges { node { id name } } }
    }
  }' "$(jq -n --arg id "$target_project" '{id: $id}')")

# Environments to restore: every environment directory in the backup.
backup_envs=$(jq -r '.environments[].name' "$backup/project.json" | while read -r name; do
  [[ -d $backup/$(safe_name "$name") ]] && echo "$name"
done)
if [[ ${#only_envs[@]} -gt 0 ]]; then
  backup_envs=$(grep -xF -f <(printf '%s\n' "${only_envs[@]}") <<<"$backup_envs" || true)
fi
[[ -n $backup_envs ]] || die "no matching environments in the backup"
if [[ -n $to_env && $(wc -l <<<"$backup_envs") -ne 1 ]]; then
  die "--to-env needs exactly one source environment"
fi

$apply && action=Restoring || action="Dry run: restoring"
echo "$action ($mode) $backup -> project '$(jq -r '.project.name' <<<"$target")'"

# diff_vars BACKUP_JSON CURRENT_JSON -> key-level plan, never values.
diff_vars() {
  jq -r --argjson cur "$2" --argjson replace "$replace" '
    . as $b
    | ([keys[] | select(. as $k | $cur | has($k) | not)] | map("      + \(.)")),
      ([keys[] | select(. as $k | ($cur | has($k)) and $cur[$k] != $b[$k])] | map("      ~ \(.)")),
      ([$cur | keys[] | select(. as $k | $b | has($k) | not)]
        | map(if $replace then "      - \(.)" else "      ! \(.) (not in backup, kept)" end)),
      ([keys[] | select(. as $k | ($cur | has($k)) and $cur[$k] == $b[$k])] | length
        | if . > 0 then ["      = \(.) unchanged"] else [] end)
    | .[]' <<<"$1"
}

# restore_vars ENV_ID LABEL FILE [SERVICE_ID]
restore_vars() {
  local env_id=$1 label=$2 file=$3 svc_id=${4:-} current
  echo "    $label"
  current=$(gql '
    query($p: String!, $e: String!, $s: String) {
      variables(projectId: $p, environmentId: $e, serviceId: $s, unrendered: true)
    }' "$(jq -n --arg p "$target_project" --arg e "$env_id" --arg s "$svc_id" \
      '{p: $p, e: $e, s: (if $s == "" then null else $s end)}')" | jq -c '.variables')
  diff_vars "$(cat "$file")" "$current"

  $apply || return 0
  if [[ $(jq 'length' "$file") -eq 0 ]] && ! $replace; then
    return 0
  fi
  gql '
    mutation($input: VariableCollectionUpsertInput!) {
      variableCollectionUpsert(input: $input)
    }' "$(jq -n --arg p "$target_project" --arg e "$env_id" --arg s "$svc_id" \
      --argjson replace "$replace" --argjson skip "$skip_deploys" --slurpfile v "$file" \
      '{input: ({projectId: $p, environmentId: $e, variables: $v[0],
                 replace: $replace, skipDeploys: $skip}
                + (if $s == "" then {} else {serviceId: $s} end))}')" >/dev/null
}

restore_env_vars() {
  local env_id=$1 env_dir=$2 svc_name svc_id file
  restore_vars "$env_id" "(shared variables)" "$env_dir/variables/_shared.json"
  while IFS= read -r svc_name; do
    file="$env_dir/variables/$(safe_name "$svc_name").json"
    [[ -f $file ]] || continue
    svc_id=$(jq -r --arg n "$svc_name" \
      'first(.project.services.edges[].node | select(.name == $n) | .id) // empty' <<<"$target")
    if [[ -z $svc_id ]]; then
      echo "    $svc_name: no service with this name in the target project, skipped"
      continue
    fi
    restore_vars "$env_id" "$svc_name" "$file" "$svc_id"
  done < <(jq -r '.[]' "$env_dir/services.json")
}

restore_env_config() {
  local env_id=$1 env_dir=$2 patch
  patch=$(jq -c --argjson t "$target" '
    ([$t.project.services.edges[].node.id]) as $svcs
    | ([$t.project.volumes.edges[].node.id]) as $vols
    | del(.groups)
    | .services |= ((. // {}) | with_entries(
        select(.key as $k | $svcs | index($k))
        | .value |= (del(.groupId)
            | if .volumeMounts then .volumeMounts |= with_entries(select(.key as $k | $vols | index($k))) else . end)))
    | if .volumes then .volumes |= with_entries(select(.key as $k | $vols | index($k))) else . end
  ' "$env_dir/config.json")

  jq -r --slurpfile names "$env_dir/services.json" --argjson p "$patch" '
    (.services // {} | keys) as $all
    | ($p.services | keys) as $kept
    | ($all[] | . as $id | "    \(if ($kept | index($id)) then "stage" else "drop (service no longer exists)" end): \($names[0][.] // .)"),
      "    volumes: \($p.volumes // {} | length) of \(.volumes // {} | length)",
      "    shared variables: \($p.sharedVariables // {} | length)"
  ' "$env_dir/config.json"

  $apply || return 0
  gql '
    mutation($e: String!, $input: EnvironmentConfig!) {
      environmentStageChanges(environmentId: $e, input: $input, merge: true) { id }
    }' "$(jq -n --arg e "$env_id" --argjson input "$patch" '{e: $e, input: $input}')" >/dev/null
  if $commit; then
    gql '
      mutation($e: String!, $skip: Boolean) {
        environmentPatchCommitStaged(environmentId: $e, skipDeploys: $skip,
          commitMessage: "Restore from railway-backup.sh")
      }' "$(jq -n --arg e "$env_id" --argjson skip "$skip_deploys" '{e: $e, skip: $skip}')" >/dev/null
    echo "    committed"
  else
    echo "    staged: review and deploy the changes in the Railway dashboard"
  fi
}

while IFS= read -r env_name; do
  dest_env=${to_env:-$env_name}
  env_id=$(jq -r --arg n "$dest_env" \
    'first(.project.environments.edges[].node | select(.name == $n) | .id) // empty' <<<"$target")
  [[ -n $env_id ]] || die "environment '$dest_env' does not exist in the target project; create it first"
  echo "  environment: $env_name -> $dest_env"

  env_dir="$backup/$(safe_name "$env_name")"
  case $mode in
    vars) restore_env_vars "$env_id" "$env_dir" ;;
    config) restore_env_config "$env_id" "$env_dir" ;;
  esac
done <<<"$backup_envs"

$apply || echo "Nothing written. Re-run with --apply to restore."
