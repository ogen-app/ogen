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
#             build, deploy, volume mounts, variables) with
#             environmentStageChanges(merge: true). Same project only, since
#             config.json is keyed by service and volume ids. Services and
#             volumes that no longer exist are dropped; the stale `groups`
#             map and service groupIds are never restored. Networking
#             (domains, TCP proxies) is left out: Railway applies it at once
#             instead of staging it, so it is listed for manual restore. The
#             changes stay staged for review in the dashboard unless --commit
#             is given.
#
# Sealed variables (listed in each environment's sealed.json) are never
# written in either mode: the backup has no value for them, so they are kept
# as they are.
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
#                     Variables listed in the backup's sealed.json are kept, but
#                     sealed variables created after the backup are deleted.
#                     Deleting a variable may trigger a deploy.
#   --deploy          trigger deploys for changed services (default: skip deploys)
#   --commit          --config: commit the staged changes right away. Refused
#                     when the environment already has staged changes.
#
#   AGE_IDENTITY      age identity file used to decrypt a .tar.gz.age backup
#
# Secrets (the token, variable values, the config patch) are passed through
# files and stdin, never as process arguments.
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

umask 077
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

auth="$tmp/auth"
printf 'Authorization: Bearer %s\n' "$RAILWAY_API_TOKEN" >"$auth"

if [[ -f $backup && $backup == *.age ]]; then
  [[ -n ${AGE_IDENTITY:-} ]] || die "AGE_IDENTITY is required to decrypt $backup"
  command -v age >/dev/null || die "age is not installed"
  mkdir "$tmp/backup"
  age -d -i "$AGE_IDENTITY" "$backup" | tar -xzf - -C "$tmp/backup"
  backup=$(find "$tmp/backup" -mindepth 1 -maxdepth 1 -type d | head -1)
fi
[[ -f $backup/project.json ]] || die "$backup is not a railway-backup.sh backup (no project.json)"

# gql QUERY < VARIABLES_JSON -> prints .data, fails on GraphQL errors.
gql() {
  local query=$1 resp
  resp=$(jq -c --arg q "$query" '{query: $q, variables: .}' \
    | curl -sS --fail-with-body "$API" -H @"$auth" \
      -H 'Content-Type: application/json' --data-binary @-) \
    || die "request failed: $resp"
  if jq -e '.errors | length > 0' >/dev/null <<<"$resp"; then
    die "GraphQL error: $(jq -c '.errors | map(.message)' <<<"$resp")"
  fi
  jq '.data' <<<"$resp"
}

safe_name() { printf '%s' "$1" | tr -c 'A-Za-z0-9._-' '_'; }

# entry NAME ID -> path component, as written by railway-backup.sh.
entry() { printf '%s--%s' "$(safe_name "$1")" "$2"; }

backup_project=$(jq -r '.id' "$backup/project.json")
target_project=${target_project:-$backup_project}
if [[ $mode == config && $target_project != "$backup_project" ]]; then
  die "--config restores into the backed-up project only ($backup_project); use --vars for another project"
fi

jq -n --arg id "$target_project" '{id: $id}' | gql '
  query($id: String!) {
    project(id: $id) {
      id
      name
      environments { edges { node { id name } } }
      services { edges { node { id name } } }
      volumes { edges { node { id name } } }
    }
  }' >"$tmp/target.json"

# Environments to restore: those with a directory in the backup, optionally
# narrowed to the names given on the command line.
selected=()
while IFS=$'\t' read -r env_id env_name; do
  if [[ ! -d $backup/$(entry "$env_name" "$env_id") ]]; then
    continue
  fi
  if [[ ${#only_envs[@]} -gt 0 ]] && ! printf '%s\n' "${only_envs[@]}" | grep -qxF "$env_name"; then
    continue
  fi
  selected+=("$env_id"$'\t'"$env_name")
done < <(jq -r '.environments[] | [.id, .name] | @tsv' "$backup/project.json")

if [[ ${#only_envs[@]} -gt 0 ]]; then
  for name in "${only_envs[@]}"; do
    if [[ ${#selected[@]} -eq 0 ]] || ! printf '%s\n' "${selected[@]}" | cut -f2 | grep -qxF "$name"; then
      die "environment '$name' is not in the backup"
    fi
  done
fi
[[ ${#selected[@]} -gt 0 ]] || die "no environments in the backup"
if [[ -n $to_env && ${#selected[@]} -ne 1 ]]; then
  die "--to-env needs exactly one source environment"
fi

$apply && action=Restoring || action="Dry run: restoring"
echo "$action ($mode) $backup -> project '$(jq -r '.project.name' "$tmp/target.json")'"

# diff_vars BACKUP_FILE CURRENT_FILE SEALED_JSON -> key-level plan, never values.
diff_vars() {
  jq -r --slurpfile cur "$2" --argjson sealed "$3" --argjson replace "$replace" '
    with_entries(select(.key as $k | $sealed | index($k) | not)) as $b | $cur[0] as $c
    | $b
    | ([keys[] | select(. as $k | $c | has($k) | not)] | map("      + \(.)")),
      ([keys[] | select(. as $k | ($c | has($k)) and $c[$k] != $b[$k])] | map("      ~ \(.)")),
      ([$c | keys[] | select(. as $k | $b | has($k) | not)]
        | map(. as $k
          | if ($sealed | index($k)) then "      s \(.) (sealed, kept)"
            elif $replace then "      - \(.)"
            else "      ! \(.) (not in backup, kept)" end)),
      ([keys[] | select(. as $k | ($c | has($k)) and $c[$k] == $b[$k])] | length
        | if . > 0 then ["      = \(.) unchanged"] else [] end)
    | .[]' "$1"
}

# restore_vars ENV_ID LABEL FILE SEALED_JSON [SERVICE_ID]
restore_vars() {
  local env_id=$1 label=$2 file=$3 sealed=$4 svc_id=${5:-} name
  echo "    $label"
  jq -n --arg p "$target_project" --arg e "$env_id" --arg s "$svc_id" \
    '{p: $p, e: $e, s: (if $s == "" then null else $s end)}' | gql '
    query($p: String!, $e: String!, $s: String) {
      variables(projectId: $p, environmentId: $e, serviceId: $s, unrendered: true)
    }' | jq '.variables' >"$tmp/current.json"
  diff_vars "$file" "$tmp/current.json" "$sealed"

  $apply || return 0
  jq --argjson sealed "$sealed" 'with_entries(select(.key as $k | $sealed | index($k) | not))' \
    "$file" >"$tmp/restore.json"
  if [[ $(jq 'length' "$tmp/restore.json") -gt 0 ]]; then
    jq -n --arg p "$target_project" --arg e "$env_id" --arg s "$svc_id" \
      --argjson skip "$skip_deploys" --slurpfile v "$tmp/restore.json" \
      '{input: ({projectId: $p, environmentId: $e, variables: $v[0], skipDeploys: $skip}
                + (if $s == "" then {} else {serviceId: $s} end))}' | gql '
      mutation($input: VariableCollectionUpsertInput!) {
        variableCollectionUpsert(input: $input)
      }' >/dev/null
  fi
  $replace || return 0
  # Delete one by one rather than upserting with replace: true, so sealed
  # variables, whose values the backup lacks, survive.
  while IFS= read -r name; do
    jq -n --arg p "$target_project" --arg e "$env_id" --arg s "$svc_id" --arg n "$name" \
      '{input: ({projectId: $p, environmentId: $e, name: $n}
                + (if $s == "" then {} else {serviceId: $s} end))}' | gql '
      mutation($input: VariableDeleteInput!) {
        variableDelete(input: $input)
      }' >/dev/null
  done < <(jq -r --slurpfile b "$file" --argjson sealed "$sealed" \
    'keys[] | select(. as $k | ($b[0] | has($k) | not) and ($sealed | index($k) | not))' "$tmp/current.json")
}

restore_env_vars() {
  local env_id=$1 env_dir=$2 svc_id svc_name file target_id
  restore_vars "$env_id" "(shared variables)" "$env_dir/variables/_shared.json" \
    "$(jq -c '.shared' "$env_dir/sealed.json")"
  while IFS=$'\t' read -r svc_id svc_name; do
    file="$env_dir/variables/$(entry "$svc_name" "$svc_id").json"
    [[ -f $file ]] || continue
    target_id=$(jq -r --arg n "$svc_name" \
      'first(.project.services.edges[].node | select(.name == $n) | .id) // empty' "$tmp/target.json")
    if [[ -z $target_id ]]; then
      echo "    $svc_name: no service with this name in the target project, skipped"
      continue
    fi
    restore_vars "$env_id" "$svc_name" "$file" \
      "$(jq -c --arg id "$svc_id" '.services[$id] // []' "$env_dir/sealed.json")" "$target_id"
  done < <(jq -r 'to_entries[] | [.key, .value] | @tsv' "$env_dir/services.json")
}

restore_env_config() {
  local env_id=$1 env_dir=$2 staged
  jq --slurpfile t "$tmp/target.json" '
    ([$t[0].project.services.edges[].node.id]) as $svcs
    | ([$t[0].project.volumes.edges[].node.id]) as $vols
    | def unsealed: with_entries(select(.value.isSealed != true and .value.value != null));
    del(.groups)
    | if .sharedVariables then .sharedVariables |= unsealed else . end
    | .services |= ((. // {}) | with_entries(
        select(.key as $k | $svcs | index($k))
        | .value |= (del(.groupId, .networking)
            | if .variables then .variables |= unsealed else . end
            | if .volumeMounts then .volumeMounts |= with_entries(select(.key as $k | $vols | index($k))) else . end)))
    | if .volumes then .volumes |= with_entries(select(.key as $k | $vols | index($k))) else . end
  ' "$env_dir/config.json" >"$tmp/patch.json"

  jq -r --slurpfile names "$env_dir/services.json" --slurpfile p "$tmp/patch.json" '
    (.services // {} | keys) as $all
    | ($p[0].services | keys) as $kept
    | ($all[] | . as $id | "    \(if ($kept | index($id)) then "stage" else "drop (service no longer exists)" end): \($names[0][$id] // $id)"),
      "    volumes: \($p[0].volumes // {} | length) of \(.volumes // {} | length)",
      "    shared variables: \($p[0].sharedVariables // {} | length)",
      (.services // {} | to_entries[] | select(.value.networking != null) | .key as $id | .value.networking
        | ([(.serviceDomains, .customDomains, .tcpProxies) // {} | keys[]]) as $n
        | select($n | length > 0)
        | "    networking not restored, re-create by hand: \($names[0][$id] // $id): \($n | join(", "))")
  ' "$env_dir/config.json"

  staged=$(jq -n --arg e "$env_id" '{e: $e}' | gql '
    query($e: String!) {
      environmentStagedChanges(environmentId: $e) { patch }
    }' | jq '.environmentStagedChanges.patch // {} | with_entries(select(.value != null and .value != {})) | length')
  if [[ $staged -gt 0 ]]; then
    $commit && die "the environment already has staged changes; apply or discard them before --commit"
    echo "    note: the environment already has staged changes; the restore is merged into them"
  fi

  $apply || return 0
  jq -n --arg e "$env_id" --slurpfile input "$tmp/patch.json" '{e: $e, input: $input[0]}' | gql '
    mutation($e: String!, $input: EnvironmentConfig!) {
      environmentStageChanges(environmentId: $e, input: $input, merge: true) { id }
    }' >/dev/null
  if $commit; then
    jq -n --arg e "$env_id" --argjson skip "$skip_deploys" '{e: $e, skip: $skip}' | gql '
      mutation($e: String!, $skip: Boolean) {
        environmentPatchCommitStaged(environmentId: $e, skipDeploys: $skip,
          commitMessage: "Restore from railway-backup.sh")
      }' >/dev/null
    echo "    committed"
  else
    echo "    staged: review and deploy the changes in the Railway dashboard"
  fi
}

for env in "${selected[@]}"; do
  env_id=${env%%$'\t'*}
  env_name=${env#*$'\t'}
  dest_env=${to_env:-$env_name}
  dest_id=$(jq -r --arg n "$dest_env" \
    'first(.project.environments.edges[].node | select(.name == $n) | .id) // empty' "$tmp/target.json")
  [[ -n $dest_id ]] || die "environment '$dest_env' does not exist in the target project; create it first"
  echo "  environment: $env_name -> $dest_env"

  env_dir="$backup/$(entry "$env_name" "$env_id")"
  case $mode in
    vars) restore_env_vars "$dest_id" "$env_dir" ;;
    config) restore_env_config "$dest_id" "$env_dir" ;;
  esac
done

$apply || echo "Nothing written. Re-run with --apply to restore."
