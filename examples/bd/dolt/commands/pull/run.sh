#!/bin/sh
# gc dolt pull — Pull Dolt databases from their configured remotes.
#
# Uses the live Dolt SQL server when reachable so pull does not contend with
# active databases. Falls back to CLI mode only when no server is running
# (legacy/external cities only — bd proxied-server mode always routes
# through `bd sql`, since bd auto-starts its own server transparently).
# Pulls the configured remote's `main` branch in both SQL and CLI modes.
#
# Environment: GC_CITY_PATH, GC_DOLT_PORT (legacy mode), GC_DOLT_USER,
#              GC_DOLT_PASSWORD, GC_DOLT_PULL_TIMEOUT_SECS (default 1800)
#
# In bd proxied-server mode (GC_BEADS_PROXIED=1), every query below routes
# through `bd sql --database <name>` (store_sql / store_sql_db in
# port_resolve.sh) instead of a direct `dolt --host --port` connection. bd
# owns the endpoint, the process lifecycle, and per-database session context;
# gascity never dials a port or reads bd's on-disk data directory directly.
set -e

: "${GC_DOLT_USER:=root}"
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

db_filter=""
data_dir="$DOLT_DATA_DIR"

while [ $# -gt 0 ]; do
  case "$1" in
    --db) db_filter="$2"; shift 2 ;;
    -h|--help)
      echo "Usage: gc dolt pull [--db NAME]"
      echo ""
      echo "Pull Dolt databases from their configured remotes."
      echo ""
      echo "Flags:"
      echo "  --db NAME   Pull only the named database"
      exit 0
      ;;
    *) echo "gc dolt pull: unknown flag: $1" >&2; exit 1 ;;
  esac
done

case "$(printf '%s' "$db_filter" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | tr '[:upper:]' '[:lower:]')" in
  information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe)
  echo "gc dolt pull: reserved Dolt database name: $(printf '%s' "$db_filter" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//') (used internally by Dolt or gc)" >&2
  exit 1
  ;;
esac

# Wall-clock bound for the pull transfer (seconds). Defaults to 1800s, mirroring
# gc dolt sync's push bound — a first pull from a large remote can transfer as
# much data as a first push. Validated the same way sync validates its timeout
# vars: reject empty / non-numeric / all-zero (GNU `timeout 0` disables the
# timeout, i.e. unbounded, which is the anti-hang outcome this bound exists to
# prevent).
pull_timeout="${GC_DOLT_PULL_TIMEOUT_SECS-1800}"
case "$pull_timeout" in
  ''|*[!0-9]*) pull_timeout_valid=false ;;
  *[1-9]*)     pull_timeout_valid=true ;;
  *)           pull_timeout_valid=false ;;
esac
if [ "$pull_timeout_valid" != true ]; then
  printf 'gc dolt pull: invalid GC_DOLT_PULL_TIMEOUT_SECS=%s (must be a positive integer)\n' \
    "$pull_timeout" >&2
  exit 2
fi

is_running() {
  managed_runtime_tcp_reachable "$GC_DOLT_PORT"
}

valid_database_name() {
  case "$1" in
    [A-Za-z0-9_]*)
      case "$1" in *[!A-Za-z0-9_-]*) return 1 ;; *) return 0 ;; esac
      ;;
    *) return 1 ;;
  esac
}

valid_remote_name() {
  case "$1" in
    [A-Za-z0-9_.-]*)
      case "$1" in *[!A-Za-z0-9_.-]*) return 1 ;; *) return 0 ;; esac
      ;;
    *) return 1 ;;
  esac
}

# routes_files — emit one routes.jsonl path per line.
# Uses gc rig list --json when available so external rigs are included.
# Falls back to a filesystem glob when gc is absent.
routes_files() {
  printf '%s\n' "$GC_CITY_PATH/.beads/routes.jsonl"

  if command -v gc >/dev/null 2>&1; then
    rig_paths=$(gc rig list --json 2>/dev/null \
      | if command -v jq >/dev/null 2>&1; then
          jq -r '.rigs[].path' 2>/dev/null
        else
          grep '"path"' | sed 's/.*"path": *"//;s/".*//'
        fi) || true
    if [ -n "$rig_paths" ]; then
      printf '%s\n' "$rig_paths" | while IFS= read -r p; do
        [ -n "$p" ] && printf '%s\n' "$p/.beads/routes.jsonl"
      done
      return
    fi
  fi

  # Fallback: scan local rigs/ directory only. Cannot discover external rigs
  # when gc is unavailable — acceptable degradation.
  find "$GC_CITY_PATH/rigs" -path '*/.beads/routes.jsonl' 2>/dev/null || true
}

# beads_dir_for_db <name> — emit the .beads directory that owns database
# <name> (the city root for the HQ database, or a rig's .beads/ for a rig
# database), by scanning routes_files() for a reference to <name>. Emits
# nothing if no route file mentions it.
beads_dir_for_db() {
  _bdfd_name="$1"
  while IFS= read -r route_file; do
    [ -f "$route_file" ] || continue
    if grep -q "\"$_bdfd_name\"" "$route_file" 2>/dev/null; then
      dirname "$route_file"
      return 0
    fi
  done <<ROUTES_LIST
$(routes_files)
ROUTES_LIST
  return 1
}

# proxied_database_names — list user databases via the bd-owned server
# catalog (SHOW DATABASES). bd owns the data dir in proxied mode, so a
# filesystem scan is neither available nor authoritative; the SQL catalog is
# (mirrors gc dolt health/list's external_database_names).
proxied_database_names() {
  _show_csv=$(store_sql csv "SHOW DATABASES" 2>/dev/null || true)
  printf '%s\n' "$_show_csv" | while IFS= read -r _raw; do
    _name=$(printf '%s' "$_raw" | tr -d '\r' | sed 's/^"//; s/"$//')
    [ -n "$_name" ] || continue
    [ "$_name" = "Database" ] && continue
    case "$(printf '%s' "$_name" | tr '[:upper:]' '[:lower:]')" in
      information_schema|mysql|dolt|dolt_cluster|performance_schema|sys|__gc_probe) continue ;;
    esac
    valid_database_name "$_name" || continue
    printf '%s\n' "$_name"
  done
}

# db_sql <db> <query> [timeout_secs] — run one SQL statement with <db> as the
# active database, under a wall-clock bound (default 120s, sized for SHORT
# METADATA QUERIES ONLY). Wraps store_sql_db (port_resolve.sh): bd proxied
# mode routes through `bd sql --database <db>`; legacy mode routes through a
# direct `dolt --host --port --use-db <db>` connection.
db_sql() {
  db_sql_db="$1"
  db_sql_query="$2"
  db_sql_tmo="${3:-120}"
  STORE_SQL_TIMEOUT="$db_sql_tmo" store_sql_db "$db_sql_db" csv "$db_sql_query"
}

find_remote_sql() {
  db="$1"
  remote_csv=$(db_sql "$db" "SELECT name, url FROM dolt_remotes LIMIT 1") || return 1
  printf '%s\n' "$remote_csv" | awk -F, 'NR > 1 && $1 != "" {print $1 "|" $2; exit}'
}

pull_database_sql() {
  name="$1"
  if ! valid_database_name "$name"; then
    echo "  $name: ERROR: invalid database name" >&2
    return 1
  fi

  remote_pair=$(find_remote_sql "$name") || {
    echo "  $name: ERROR: failed to query remotes" >&2
    return 1
  }
  if [ -z "$remote_pair" ]; then
    echo "  $name: skipped (no remote)"
    return 0
  fi
  remote_name=${remote_pair%%|*}
  remote_url=${remote_pair#*|}
  if ! valid_remote_name "$remote_name"; then
    echo "  $name: ERROR: invalid remote name: $remote_name" >&2
    return 1
  fi

  if db_sql "$name" "CALL DOLT_PULL('$remote_name', 'main')" "$pull_timeout" >/dev/null 2>&1; then
    echo "  $name: pulled from $remote_url"
    return 0
  fi

  echo "  $name: ERROR: pull failed" >&2
  return 1
}

pull_database_cli() {
  d="$1"
  name="$2"

  remote_name=""
  remote_url=""
  if [ -f "$d/.dolt/remotes.json" ]; then
    remote_name=$(grep -o '"name":"[^"]*"' "$d/.dolt/remotes.json" 2>/dev/null | head -1 | sed 's/"name":"//;s/"//' || true)
    remote_url=$(grep -o '"url":"[^"]*"' "$d/.dolt/remotes.json" 2>/dev/null | head -1 | sed 's/"url":"//;s/"//' || true)
  fi
  [ -z "$remote_name" ] && remote_name="origin"

  if [ -z "$remote_url" ]; then
    echo "  $name: skipped (no remote)"
    return 0
  fi
  if ! valid_remote_name "$remote_name"; then
    echo "  $name: ERROR: invalid remote name: $remote_name" >&2
    return 1
  fi

  if (cd "$d" && dolt pull "$remote_name" main 2>&1); then
    echo "  $name: pulled from $remote_url"
    return 0
  fi

  echo "  $name: ERROR: pull failed" >&2
  return 1
}

exit_code=0

if [ "${GC_BEADS_PROXIED:-0}" = 1 ]; then
  # bd proxied-server mode: enumerate databases from the server catalog (bd
  # owns the data dir, so no filesystem scan is available).
  #
  # The database list is captured to a variable and fed via a heredoc (not
  # piped into the while loop) so `exit_code` mutations inside the loop
  # happen in THIS shell, not a subshell that would silently discard them.
  _proxied_dbs=$(proxied_database_names)
  while IFS= read -r name; do
    [ -n "$name" ] || continue
    [ -n "$db_filter" ] && [ "$name" != "$db_filter" ] && continue

    no_sync=false
    if beads_dir=$(beads_dir_for_db "$name") && [ -f "$beads_dir/.no-sync" ]; then
      no_sync=true
    fi
    if [ "$no_sync" = true ]; then
      echo "  $name: skipped (.no-sync)"
      continue
    fi

    pull_database_sql "$name" || exit_code=1
  done <<PROXIED_DBS
$_proxied_dbs
PROXIED_DBS
  exit $exit_code
fi

server_running=false
is_running && server_running=true
if [ -d "$data_dir" ]; then
  for d in "$data_dir"/*/; do
    [ ! -d "$d/.dolt" ] && continue
    name="$(basename "$d")"
    case "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" in information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe) continue ;; esac
    [ -n "$db_filter" ] && [ "$name" != "$db_filter" ] && continue
    if [ -f "$d/.no-sync" ]; then
      echo "  $name: skipped (.no-sync)"
      continue
    fi

    if [ "$server_running" = true ]; then
      pull_database_sql "$name" || exit_code=1
    else
      pull_database_cli "$d" "$name" || exit_code=1
    fi
  done
fi

exit $exit_code
