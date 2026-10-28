#!/usr/bin/env bash
# Back up a Railway project's configuration through the public GraphQL API.
#
# Environment directories and variable files are named <name>--<id>, so
# distinct names that sanitise to the same string never collide. For every
# environment it saves:
#   config.json          full environment config (services: source, build, deploy,
#                        networking/domains, volume mounts, variables; shared
#                        variables; volumes; groups) with variables decrypted
#   variables/<svc>--<id>.json  unrendered service variables (${{ref}} templates
#                        kept), the exact map variableCollectionUpsert takes
#   variables/_shared.json  unrendered shared (environment-level) variables
#   services.json        service id -> name map (config.json is keyed by id)
#   canvas.json          canvasGroupRefs: the environment's canvas group layout
#   sealed.json          names of sealed variables, whose values Railway never
#                        returns: the backup cannot restore them
#
# At project level, project.json holds the canvas groups (Project.groups) and
# each service's groupId, and groups.json maps group name -> service names.
# The `groups` map inside config.json can be stale; the project-level groups
# are what the canvas shows.
#
# Usage:
#   RAILWAY_API_TOKEN=... scripts/railway-backup.sh <project-id> [environment-name ...]
#
#   RAILWAY_API_TOKEN  account or workspace token (railway.com/account/tokens)
#   OUT_DIR            parent directory for backups (default: ~/railway-backups)
#   AGE_RECIPIENT      if set, the backup is packed into an age-encrypted tarball
#                      and the plaintext directory is removed
#
# The output contains plaintext secrets: it is written with mode 0700/0600 and
# should never be committed.
set -euo pipefail

API=https://backboard.railway.com/graphql/v2

die() { echo "error: $*" >&2; exit 1; }

[[ $# -ge 1 ]] || die "usage: $0 <project-id> [environment-name ...]"
[[ -n ${RAILWAY_API_TOKEN:-} ]] || die "RAILWAY_API_TOKEN is not set"
command -v jq >/dev/null || die "jq is required"
command -v curl >/dev/null || die "curl is required"
if [[ -n ${AGE_RECIPIENT:-} ]]; then
  command -v age >/dev/null || die "AGE_RECIPIENT is set but age is not installed"
fi

PROJECT_ID=$1
shift
ONLY_ENVS=("$@")

umask 077
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# The token goes through a header file so it never shows up in process args.
auth="$tmp/auth"
printf 'Authorization: Bearer %s\n' "$RAILWAY_API_TOKEN" >"$auth"

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

# entry NAME ID -> unique, readable path component.
entry() { printf '%s--%s' "$(safe_name "$1")" "$2"; }

project=$(jq -n --arg id "$PROJECT_ID" '{id: $id}' | gql '
  query($id: String!) {
    project(id: $id) {
      id
      name
      environments { edges { node { id name } } }
      services { edges { node { id name groupId } } }
      groups { edges { node { id name groupId color icon isCollapsed } } }
    }
  }')

if [[ ${#ONLY_ENVS[@]} -gt 0 ]]; then
  for name in "${ONLY_ENVS[@]}"; do
    jq -e --arg n "$name" 'any(.project.environments.edges[].node; .name == $n)' \
      >/dev/null <<<"$project" || die "environment '$name' does not exist in the project"
  done
fi

project_name=$(jq -r '.project.name' <<<"$project")
stamp=$(date -u +%Y%m%dT%H%M%SZ)
dest="${OUT_DIR:-$HOME/railway-backups}/$(safe_name "$project_name")-$stamp"
mkdir -p "$dest"

jq '.project | {id, name,
  environments: [.environments.edges[].node],
  services: [.services.edges[].node],
  groups: [.groups.edges[].node]}' <<<"$project" >"$dest/project.json"

jq '(.groups | map({(.id): .name}) | add // {}) as $names
  | .services
  | group_by(.groupId)
  | map({key: ($names[.[0].groupId // ""] // "(ungrouped)"), value: map(.name)})
  | from_entries' "$dest/project.json" >"$dest/groups.json"

services=$(jq -c '[.project.services.edges[].node]' <<<"$project")
sealed_total=0

echo "Backing up project '$project_name' -> $dest"

while IFS=$'\t' read -r env_id env_name; do
  if [[ ${#ONLY_ENVS[@]} -gt 0 ]] && ! printf '%s\n' "${ONLY_ENVS[@]}" | grep -qxF "$env_name"; then
    continue
  fi

  env_dir="$dest/$(entry "$env_name" "$env_id")"
  mkdir -p "$env_dir/variables"
  echo "  environment: $env_name"

  env=$(jq -n --arg id "$env_id" '{id: $id}' | gql '
    query($id: String!) {
      environment(id: $id) { id name config(decryptVariables: true) canvasGroupRefs }
    }')
  jq '.environment.config' <<<"$env" >"$env_dir/config.json"
  jq '.environment.canvasGroupRefs' <<<"$env" >"$env_dir/canvas.json"

  jq 'def sealed: [to_entries[] | select(.value.isSealed == true or .value.value == null) | .key];
    {shared: (.sharedVariables // {} | sealed),
     services: (.services // {} | map_values(.variables // {} | sealed)
       | with_entries(select(.value | length > 0)))}' \
    "$env_dir/config.json" >"$env_dir/sealed.json"
  sealed=$(jq '(.shared | length) + ([.services[] | length] | add // 0)' "$env_dir/sealed.json")
  if [[ $sealed -gt 0 ]]; then
    echo "    warning: $sealed sealed variable(s) have no value in this backup (see sealed.json)"
    sealed_total=$((sealed_total + sealed))
  fi

  # Services present in this environment's config (a service may not be
  # deployed to every environment).
  env_services=$(jq -c --slurpfile cfg "$env_dir/config.json" \
    '[.[] | select(.id as $id | ($cfg[0].services // {}) | has($id))]' <<<"$services")
  jq 'map({(.id): .name}) | add // {}' <<<"$env_services" >"$env_dir/services.json"

  jq -n --arg p "$PROJECT_ID" --arg e "$env_id" '{p: $p, e: $e}' | gql '
    query($p: String!, $e: String!) {
      variables(projectId: $p, environmentId: $e, unrendered: true)
    }' | jq '.variables' >"$env_dir/variables/_shared.json"

  while IFS=$'\t' read -r svc_id svc_name; do
    echo "    service: $svc_name"
    jq -n --arg p "$PROJECT_ID" --arg e "$env_id" --arg s "$svc_id" '{p: $p, e: $e, s: $s}' | gql '
      query($p: String!, $e: String!, $s: String!) {
        variables(projectId: $p, environmentId: $e, serviceId: $s, unrendered: true)
      }' | jq '.variables' >"$env_dir/variables/$(entry "$svc_name" "$svc_id").json"
  done < <(jq -r '.[] | [.id, .name] | @tsv' <<<"$env_services")
done < <(jq -r '.project.environments.edges[].node | [.id, .name] | @tsv' <<<"$project")

if [[ -n ${AGE_RECIPIENT:-} ]]; then
  archive="$dest.tar.gz.age"
  tar -C "$(dirname "$dest")" -czf - "$(basename "$dest")" | age -r "$AGE_RECIPIENT" -o "$archive"
  rm -rf "$dest"
  echo "Encrypted backup: $archive"
else
  echo "Done. Plaintext secrets in $dest - encrypt or move it to a vault."
fi
if [[ $sealed_total -gt 0 ]]; then
  echo "INCOMPLETE: $sealed_total sealed variable(s) could not be backed up. Keep their"
  echo "values elsewhere; railway-restore.sh never deletes them, but cannot recreate them."
fi
