#!/bin/sh
# gc-beads-bd — exec: beads provider for Dolt-backed beads (bd).
#
# Implements the exec beads lifecycle protocol:
#   gc-beads-bd <operation> [args...]
#
# Operations: start, ensure-ready, stop, shutdown, init, health, recover, probe
# Exit codes: 0 = success, 1 = error, 2 = not needed / not running
#
# Environment:
#   GC_CITY_PATH  — city root directory (required for all operations)
#   GC_CITY_RUNTIME_DIR — canonical hidden runtime root (optional)
#   GC_PACK_STATE_DIR — canonical pack runtime root for dolt (optional)
#   GC_BEADS_SKIP — set truthy (1/true/skip) to no-op all operations (exit 2)
#   GC_BEADS_BACKEND — "dolt" (default) or "doltlite"
#   GC_BEADS_HOST  — dolt server host (empty = managed local server bound to
#                   127.0.0.1; 0.0.0.0 = managed local server exposed on all
#                   interfaces; anything else = remote server GC won't manage)
#   GC_BEADS_PORT  — dolt server port (default: ephemeral, hashed from city path)
#   GC_BEADS_USER  — dolt user (default: root)
#   GC_BEADS_PASSWORD — dolt password (default: empty)
#   GC_BEADS_CONCURRENT_START_READY_TIMEOUT_MS — concurrent-start wait budget in
#       milliseconds (default: 75000 + 2× the lock-release window = 195000 at
#       defaults, covering the start-flock winner's worst-case stop — 30s
#       SIGTERM grace + SIGKILL lock gate + post-exit lock wait — plus the
#       legacy 45s ready allowance)
#   GC_BEADS_LOCK_RELEASE_TIMEOUT_MS — wait budget for dolt's on-disk exclusive
#       store locks (<data_dir>/.dolt/noms/LOCK and
#       <data_dir>/<db>/.dolt/noms/LOCK) to be released before start/stop
#       fail closed, in milliseconds (default: 60000). gc projects
#       [dolt].dolt_lock_release_timeout from city.toml into this variable.

set -e

# --- Configuration ---

# DOLT_PORT is set after derived paths are resolved (see allocate_port below).
DOLT_HOST="${GC_BEADS_HOST:-127.0.0.1}"
DOLT_USER="${GC_BEADS_USER:-root}"
DOLT_PASSWORD="${GC_BEADS_PASSWORD:-}"
DOLT_LOGLEVEL="${GC_BEADS_LOGLEVEL:-warning}"
LSOF_TIMEOUT_SECONDS="${GC_LSOF_TIMEOUT_SECONDS:-2}"
CONCURRENT_START_READY_TIMEOUT_MS="${GC_BEADS_CONCURRENT_START_READY_TIMEOUT_MS:-}"
LOCK_RELEASE_TIMEOUT_MS="${GC_BEADS_LOCK_RELEASE_TIMEOUT_MS:-60000}"
BEADS_BACKEND="${GC_BEADS_BACKEND:-${BEADS_BACKEND:-dolt}}"

# Probed once in the parent shell — dolt_data_lock_holder runs in $(...)
# subshells, so a lazily-set memo there would never persist. Without flock
# the dolt store lock guard (gastownhall/gascity#3174) cannot probe and
# falls back to the legacy fail-open behavior; warn once so the disabled
# guard is visible — but only for operations that reach the guard.
# Status-style ops (health, probe, init, store bridge) never probe the
# lock and would emit the warning on every invocation.
FLOCK_AVAILABLE=true
if ! command -v flock >/dev/null 2>&1; then
    FLOCK_AVAILABLE=false
    case "${1:-}" in
        start|ensure-ready|stop|shutdown|recover)
            echo "warning: flock unavailable; dolt store lock guard disabled (gastownhall/gascity#3174)" >&2
            ;;
    esac
fi

# Derived paths (set after GC_CITY_PATH validation).
GC_DIR=""
PACK_STATE_DIR=""
DATA_DIR=""
LOG_FILE=""
STATE_FILE=""
PID_FILE=""
LOCK_FILE=""
CONFIG_FILE=""

# --- Helpers ---

die() {
    echo "$@" >&2
    exit 1
}

resolve_gc_helper_bin() {
    if [ -n "${GC_BIN:-}" ]; then
        printf '%s\n' "$GC_BIN"
    fi
    return 0
}

is_doltlite_backend() {
    [ "$BEADS_BACKEND" = "doltlite" ]
}

resolve_gc_bin() {
    if [ -n "${GC_BIN:-}" ]; then
        printf '%s\n' "$GC_BIN"
        return 0
    fi
    command -v gc 2>/dev/null || true
}

# is_remote returns 0 (true) when GC_BEADS_HOST explicitly names a remote
# target. Empty, 127.0.0.1 (the default bind), and 0.0.0.0 (the explicit
# wildcard opt-out for multi-host deployments) all mean GC owns a local
# managed server.
is_remote() {
    case "${GC_BEADS_HOST:-}" in
        ''|127.0.0.1|0.0.0.0|localhost|"::1"|"[::1]") return 1 ;;
    esac
    return 0
}

# connect_host returns the host to connect to (loopback IPv4 for local servers).
# Using 127.0.0.1 avoids localhost -> ::1 resolution mismatches when the
# managed Dolt server is only listening on IPv4.
connect_host() {
    if is_remote; then
        echo "$GC_BEADS_HOST"
    else
        echo "127.0.0.1"
    fi
}

trim_space() {
    printf '%s' "$1" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//'
}

lower_dolt_database_name() {
    trim_space "$1" | tr '[:upper:]' '[:lower:]'
}

is_system_dolt_database_name() {
    case "$(lower_dolt_database_name "$1")" in
        information_schema|mysql|dolt|dolt_cluster|performance_schema|sys|__gc_probe) return 0 ;;
        *) return 1 ;;
    esac
}

csv_unquote_single_field() {
    local value
    value="$1"
    case "$value" in
        \"*\")
            case "$value" in
                *\") ;;
                *) return 1 ;;
            esac
            value=${value#\"}
            value=${value%\"}
            printf '%s\n' "$value" | sed 's/""/"/g'
            ;;
        *)
            printf '%s\n' "$value"
            ;;
    esac
}

first_user_database_from_show_databases_csv() {
    local line name
    while IFS= read -r line || [ -n "$line" ]; do
        name=$(csv_unquote_single_field "$line") || return 1
        name=$(trim_space "$name")
        [ -n "$name" ] || continue
        [ "$(lower_dolt_database_name "$name")" = "database" ] && continue
        if is_system_dolt_database_name "$name"; then
            continue
        fi
        printf '%s\n' "$name"
        return 0
    done <<GC_SHOW_DATABASES_CSV
$1
GC_SHOW_DATABASES_CSV
    return 0
}

quote_dolt_identifier() {
    local escaped
    escaped=$(printf '%s' "$1" | sed 's/`/``/g')
    printf '`%s`' "$escaped"
}

# tcp_check_port returns 0 if the given port is reachable.
tcp_check_port() {
    local port="$1"
    local host
    host=$(connect_host)
    if command -v nc >/dev/null 2>&1; then
        nc -z -w 2 "$host" "$port" 2>/dev/null
    elif command -v bash >/dev/null 2>&1; then
        bash -c "echo >/dev/tcp/$host/$port" 2>/dev/null
    else
        return 1
    fi
}

# tcp_check returns 0 if the dolt port is reachable.
tcp_check() {
    tcp_check_port "$DOLT_PORT"
}

run_with_timeout() {
    local timeout_seconds="$1"
    shift
    "$@" &
    local cmd_pid=$!
    (
        sleep "$timeout_seconds" 2>/dev/null || sleep 1
        kill "$cmd_pid" 2>/dev/null || true
    ) </dev/null >/dev/null 2>&1 &
    local watchdog_pid=$!
    local status=0
    wait "$cmd_pid" || status=$?
    kill "$watchdog_pid" 2>/dev/null || true
    wait "$watchdog_pid" 2>/dev/null || true
    return "$status"
}

run_lsof() {
    command -v lsof >/dev/null 2>&1 || return 127
    run_with_timeout "$LSOF_TIMEOUT_SECONDS" lsof "$@"
}

lsof_reports_open() {
    local status
    run_lsof "$@" >/dev/null 2>&1
    status=$?
    case "$status" in
        0) return 0 ;;
        1) return 1 ;;
        *) return 2 ;;
    esac
}

canonical_dir() {
    local dir="$1"
    (cd "$dir" 2>/dev/null && pwd -P) || printf '%s\n' "$dir"
}

same_dir_path() {
    local left="$1" right="$2" abs_left abs_right
    [ "$left" = "$right" ] && return 0
    abs_left=$(canonical_dir "$left")
    abs_right=$(canonical_dir "$right")
    [ "$abs_left" = "$abs_right" ]
}

# do_query_probe runs a read-only information_schema query against the dolt server.
do_query_probe() {
    local host gc_bin
    host=$(connect_host)
    gc_bin=$(resolve_gc_helper_bin)
    if [ -n "$gc_bin" ]; then
        "$gc_bin" dolt-state query-probe --host "$host" --port "$DOLT_PORT" --user "$DOLT_USER" >/dev/null 2>&1
        return $?
    fi
    dolt --host "$host" --port "$DOLT_PORT" --user "$DOLT_USER" --password "${DOLT_PASSWORD:-}" --no-tls         sql -r csv -q "SELECT COUNT(*) AS cnt FROM information_schema.SCHEMATA" >/dev/null 2>&1
}

# server_sql runs a SQL query against the running dolt server.
# Returns 0 on success, 1 on failure. Stdout contains query output,
# stderr contains error messages (callers must redirect as needed).
server_sql() {
    local host
    host=$(connect_host)
    dolt --host "$host" --port "$DOLT_PORT" --user "$DOLT_USER" --password "${DOLT_PASSWORD:-}" --no-tls \
        sql -q "$1"
}

# is_retryable_error checks if an error message is a transient Dolt failure worth retrying.
# Matches the 7 patterns from upstream isDoltRetryableError().
is_retryable_error() {
    case "$1" in
        *"database is read only"*) return 0 ;;
        *"cannot update manifest"*) return 0 ;;
        *"optimistic lock"*) return 0 ;;
        *"serialization failure"*) return 0 ;;
        *"lock wait timeout"*) return 0 ;;
        *"try restarting transaction"*) return 0 ;;
        *"Unknown database"*) return 0 ;;
    esac
    return 1
}

sleep_ms() {
    local ms="$1"
    local seconds remainder
    seconds=$((ms / 1000))
    remainder=$((ms % 1000))
    if [ "$remainder" -eq 0 ]; then
        sleep "$seconds"
    else
        sleep "$seconds.$(printf '%03d' "$remainder")"
    fi
}

# server_sql_retry wraps server_sql with exponential backoff on transient errors.
# 5 attempts, backoff 500ms→1s→2s→4s→8s (capped at 15s).
server_sql_retry() {
    local query="$1"
    local attempt=1
    local max_attempts=5
    local backoff_ms=500
    local max_backoff_ms=15000
    local output

    while [ "$attempt" -le "$max_attempts" ]; do
        output=$(server_sql "$query" 2>&1) && return 0

        if ! is_retryable_error "$output"; then
            echo "$output" >&2
            return 1
        fi

        if [ "$attempt" -lt "$max_attempts" ]; then
            sleep_ms "$backoff_ms" 2>/dev/null || sleep 1
            backoff_ms=$((backoff_ms * 2))
            if [ "$backoff_ms" -gt "$max_backoff_ms" ]; then
                backoff_ms=$max_backoff_ms
            fi
        fi
        attempt=$((attempt + 1))
    done

    echo "after $max_attempts retries: $output" >&2
    return 1
}

# ensure_database_registered creates the database on the running server if
# it doesn't already exist. Dolt's CREATE DATABASE both creates the on-disk
# directory and registers it in the server's in-memory catalog. If the
# directory already exists (from bd init), CREATE DATABASE IF NOT EXISTS
# adopts it. Without this, databases created by bd init on disk are
# invisible to the running server.
#
# After CREATE DATABASE, polls with USE <db> to wait for catalog propagation
# (CREATE DATABASE returns before the catalog is fully updated).
ensure_database_registered() {
    local db="$1"

    # Validate database name before SQL interpolation (upstream 38f7b380).
    if ! valid_sql_name "$db"; then
        echo "error: invalid database name: $db" >&2
        return 1
    fi

    # Check if already visible.
    if server_sql "USE \`$db\`" >/dev/null 2>&1; then
        return 0
    fi

    # Register with the server (use retry for lock contention).
    local reg_err
    if ! reg_err=$(server_sql_retry "CREATE DATABASE IF NOT EXISTS \`$db\`" 2>&1 >/dev/null); then
        echo "warning: CREATE DATABASE $db failed: $reg_err" >&2
        return 1
    fi

    # Wait for catalog propagation (exponential backoff: 100ms → 200ms → 400ms → 800ms → 1.6s).
    local attempt backoff_ms
    backoff_ms=100
    for attempt in 1 2 3 4 5; do
        if server_sql "USE \`$db\`" >/dev/null 2>&1; then
            return 0
        fi
        sleep_ms "$backoff_ms" 2>/dev/null || sleep 1
        backoff_ms=$((backoff_ms * 2))
    done

    echo "warning: database $db not visible after 5 catalog probes" >&2
    return 1
}


read_metadata_string_field() {
    local meta_file="$1" key="$2"
    [ -f "$meta_file" ] || return 0

    if command -v jq >/dev/null 2>&1; then
        jq -r --arg key "$key" '.[$key] // empty' "$meta_file" 2>/dev/null || true
        return 0
    fi

    grep -o "\"$key\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" "$meta_file" 2>/dev/null |
        sed "s/.*\"$key\"[[:space:]]*:[[:space:]]*\"//;s/\"//" || true
}

read_existing_dolt_database() {
    local meta_file="$1"
    [ -f "$meta_file" ] || return 0

    if command -v jq >/dev/null 2>&1; then
        jq -r '.dolt_database // empty' "$meta_file" 2>/dev/null || true
        return 0
    fi

    grep -o '"dolt_database"[[:space:]]*:[[:space:]]*"[^"]*"' "$meta_file" 2>/dev/null | \
        sed 's/.*"dolt_database"[[:space:]]*:[[:space:]]*"//;s/"//' || true
}

identity_toml_present() {
    local dir="$1"
    [ -f "$dir/.beads/identity.toml" ]
}

ensure_project_identity() {
    local dir="$1" meta_file gc_bin dolt_database host
    meta_file="$dir/.beads/metadata.json"
    gc_bin=$(resolve_gc_helper_bin)
    if [ -z "$gc_bin" ]; then
        return 0
    fi
    dolt_database=$(read_existing_dolt_database "$meta_file")
    if [ -z "$dolt_database" ]; then
        return 0
    fi
    host=$(connect_host)
    "$gc_bin" dolt-state ensure-project-id \
        --city "$GC_CITY_PATH" \
        --metadata "$meta_file" \
        --host "$host" \
        --port "$DOLT_PORT" \
        --user "$DOLT_USER" \
        --database "$dolt_database" >/dev/null \
        || die "failed to ensure project identity for $dir"
}

write_doltlite_metadata() {
    local dir="$1" database="$2" metadata_path tmp project_id
    metadata_path="$dir/.beads/metadata.json"
    mkdir -p "$dir/.beads"
    project_id=$(read_metadata_string_field "$metadata_path" project_id)
    if [ -z "$project_id" ]; then
        project_id="$(basename "$dir")"
    fi
    tmp="$metadata_path.tmp.$$"
    cat > "$tmp" <<EOF
{
  "backend": "doltlite",
  "database": "doltlite",
  "dolt_database": "$database",
  "project_id": "$project_id"
}
EOF
    chmod 600 "$tmp"
    mv "$tmp" "$metadata_path"
}

valid_custom_types_value() {
    local types="$1" old_ifs typ
    [ -n "$types" ] || return 1
    old_ifs=$IFS
    IFS=','
    for typ in $types; do
        [ -n "$typ" ] || { IFS=$old_ifs; return 1; }
        valid_sql_name "$typ" || { IFS=$old_ifs; return 1; }
    done
    IFS=$old_ifs
    return 0
}

validate_bd_runtime_config_value() {
    local key="$1"
    local value="$2"
    [ -n "$value" ] || return 0
    case "$key" in
        issue_prefix)
            valid_sql_name "$value" || die "invalid beads prefix: $value"
            ;;
        types.custom)
            valid_custom_types_value "$value" || die "invalid custom bead types: $value"
            ;;
        *)
            die "unsupported bd runtime config key: $key"
            ;;
    esac
}

# ensure_doltlite_runtime_config_value persists a beads runtime config value
# (issue_prefix, types.custom) directly into the embedded DoltLite store's
# SQLite config table. DoltLite is a surviving, server-less backend: it has no
# managed dolt sql-server to route SQL through, so config is written straight
# into the physical .db. The key and value are bound as SQLite named parameters
# (never string-interpolated into the SQL text) so config values cannot inject
# SQL.
ensure_doltlite_runtime_config_value() {
    local db_path="$1"
    local key="$2"
    local value="$3"
    local key_sql value_sql
    [ -n "$db_path" ] || return 0
    [ -n "$value" ] || return 0
    [ -f "$db_path" ] || die "missing doltlite database: $db_path"
    command -v sqlite3 >/dev/null 2>&1 || die "sqlite3 is required to configure doltlite runtime state"
    validate_bd_runtime_config_value "$key" "$value"

    key_sql=$(printf '%s' "$key" | sed "s/'/''/g")
    value_sql=$(printf '%s' "$value" | sed "s/'/''/g")
    sqlite3 "$db_path" <<SQL ||
.parameter init
.parameter set @gc_config_key '$key_sql'
.parameter set @gc_config_value '$value_sql'
REPLACE INTO config ("key", value) VALUES (@gc_config_key, @gc_config_value);
SQL
        die "failed to set doltlite runtime $key for $db_path"
}

bd_runtime_schema_ready() {
    local db="$1"
    [ -n "$db" ] || return 1
    valid_sql_name "$db" || return 1
    server_sql "USE \`$db\`; SELECT 1 FROM config LIMIT 1" >/dev/null 2>&1
}

# server_reachable reports whether the managed Dolt server answers a
# trivial query. Used to distinguish a transient connection failure
# (port drift, an exclusive lock held by a stale dolt, a slow server
# start) from a genuinely missing schema/registration before deciding to
# force a destructive reinit. server_sql carries the connect target, so
# this stays in lockstep with bd_runtime_schema_ready / ensure_database_registered.
server_reachable() {
    server_sql "SELECT 1" >/dev/null 2>&1
}

# --- Robustness Helpers ---

# save_state writes the private provider runtime state atomically (no jq dependency).
save_state() {
    local pid="$1" running="$2" gc_bin
    gc_bin=$(resolve_gc_helper_bin)
    if [ -n "$gc_bin" ]; then
        "$gc_bin" dolt-state write-provider \
            --file "$STATE_FILE" \
            --pid "$pid" \
            --running "$running" \
            --port "$DOLT_PORT" \
            --data-dir "$DATA_DIR" || die "failed to write provider state via gc helper $gc_bin"
        return 0
    fi
    mkdir -p "$(dirname "$STATE_FILE")"
    local tmp
    tmp=$(mktemp "$STATE_FILE.tmp.XXXXXX")
    printf '{"running":%s,"pid":%s,"port":%s,"data_dir":"%s","started_at":"%s"}\n' \
        "$running" "$pid" "$DOLT_PORT" "$DATA_DIR" \
        "$(date -u +"%Y-%m-%dT%H:%M:%SZ")" > "$tmp"
    mv "$tmp" "$STATE_FILE"
}

# load_state_field extracts a field from the private provider runtime state (no jq dependency).
load_state_field() {
    [ -f "$STATE_FILE" ] || return 0
    local gc_bin
    gc_bin=$(resolve_gc_helper_bin)
    if [ -n "$gc_bin" ]; then
        "$gc_bin" dolt-state read-provider --file "$STATE_FILE" --field "$1" 2>/dev/null || true
        return 0
    fi
    sed -n 's/.*"'"$1"'"[[:space:]]*:[[:space:]]*"\{0,1\}\([^",}]*\)"\{0,1\}.*/\1/p' "$STATE_FILE" | head -1
}
load_runtime_layout_from_gc() {
    local gc_bin output key value
    gc_bin=$(resolve_gc_helper_bin)
    [ -n "$gc_bin" ] || return 1
    output=$("$gc_bin" dolt-state runtime-layout --city "$GC_CITY_PATH" </dev/null 2>/dev/null) || return 1
    while IFS="$(printf '	')" read -r key value; do
        case "$key" in
            GC_PACK_STATE_DIR) PACK_STATE_DIR="$value" ;;
            GC_BEADS_DATA_DIR) DATA_DIR="$value" ;;
            GC_BEADS_LOG_FILE) LOG_FILE="$value" ;;
            GC_BEADS_STATE_FILE) STATE_FILE="$value" ;;
            GC_BEADS_PID_FILE) PID_FILE="$value" ;;
            GC_BEADS_LOCK_FILE) LOCK_FILE="$value" ;;
            GC_BEADS_CONFIG_FILE) CONFIG_FILE="$value" ;;
        esac
    done <<EOF
$output
EOF
    [ -n "$PACK_STATE_DIR" ] && [ -n "$DATA_DIR" ] && [ -n "$LOG_FILE" ] && [ -n "$STATE_FILE" ] && [ -n "$PID_FILE" ] && [ -n "$LOCK_FILE" ] && [ -n "$CONFIG_FILE" ]
}

load_managed_process_inspection_from_gc() {
    local gc_bin output key value
    gc_bin=$(resolve_gc_helper_bin)
    [ -n "$gc_bin" ] || return 1
    output=$("$gc_bin" dolt-state inspect-managed --city "$GC_CITY_PATH" --port "$DOLT_PORT" </dev/null 2>/dev/null) || return 1
    GC_MANAGED_PID=""
    GC_MANAGED_OWNED="false"
    GC_MANAGED_DELETED="false"
    GC_PORT_HOLDER_PID=""
    GC_PORT_HOLDER_OWNED="false"
    GC_PORT_HOLDER_DELETED="false"
    while IFS="$(printf '	')" read -r key value; do
        case "$key" in
            managed_pid)
                [ "$value" != "0" ] && GC_MANAGED_PID="$value"
                ;;
            managed_owned)
                GC_MANAGED_OWNED="$value"
                ;;
            managed_deleted_inodes)
                GC_MANAGED_DELETED="$value"
                ;;
            port_holder_pid)
                [ "$value" != "0" ] && GC_PORT_HOLDER_PID="$value"
                ;;
            port_holder_owned)
                GC_PORT_HOLDER_OWNED="$value"
                ;;
            port_holder_deleted_inodes)
                GC_PORT_HOLDER_DELETED="$value"
                ;;
        esac
    done <<EOF
$output
EOF
    return 0
}

load_probe_managed_from_gc() {
    local gc_bin host output key value status parsed=false
    host=$(connect_host)
    gc_bin=$(resolve_gc_helper_bin)
    GC_PROBE_USED="false"
    GC_PROBE_RUNNING="false"
    GC_PROBE_PORT_HOLDER_PID=""
    GC_PROBE_PORT_HOLDER_OWNED="false"
    GC_PROBE_PORT_HOLDER_DELETED="false"
    GC_PROBE_TCP_REACHABLE="false"
    [ -n "$gc_bin" ] || return 1
    GC_PROBE_USED="true"
    output=$("$gc_bin" dolt-state probe-managed --city "$GC_CITY_PATH" --host "$host" --port "$DOLT_PORT" </dev/null 2>/dev/null)
    status=$?
    while IFS="$(printf '	')" read -r key value; do
        case "$key" in
            running)
                GC_PROBE_RUNNING="$value"
                parsed=true
                ;;
            port_holder_pid)
                [ "$value" != "0" ] && GC_PROBE_PORT_HOLDER_PID="$value"
                parsed=true
                ;;
            port_holder_owned)
                GC_PROBE_PORT_HOLDER_OWNED="$value"
                parsed=true
                ;;
            port_holder_deleted_inodes)
                GC_PROBE_PORT_HOLDER_DELETED="$value"
                parsed=true
                ;;
            tcp_reachable)
                GC_PROBE_TCP_REACHABLE="$value"
                parsed=true
                ;;
        esac
    done <<EOF
$output
EOF
    if [ "$status" -ne 0 ] && [ "$parsed" != "true" ]; then
        GC_PROBE_USED="false"
        return 1
    fi
    [ "$status" -eq 0 ]
}

current_time_ms() {
    local gc_bin now
    gc_bin=$(resolve_gc_helper_bin)
    if [ -n "$gc_bin" ]; then
        now=$("$gc_bin" dolt-state now-ms </dev/null 2>/dev/null) || now=""
        case "$now" in
            ''|*[!0-9]*)
                now=""
                ;;
        esac
        if [ -n "$now" ]; then
            printf '%s\n' "$now"
            return 0
        fi
    fi
    now=$(date +%s 2>/dev/null) || return 1
    case "$now" in
        ''|*[!0-9]*)
            return 1
            ;;
    esac
    printf '%s000\n' "$now"
}

# find_port_holder returns the PID of the process listening on DOLT_PORT.

find_port_holder() {
    run_lsof -nP -t -iTCP:"$DOLT_PORT" -sTCP:LISTEN 2>/dev/null | head -1
}

# verify_our_server checks if a PID belongs to our server (matching data-dir).
# Returns 0 if ours, 1 if imposter or unknown.
verify_our_server() {
    local pid="$1"
    [ -n "$pid" ] || return 1

    # Layer 1: State file data-dir comparison.
    local state_dir
    state_dir=$(load_state_field data_dir)
    if [ -n "$state_dir" ] && ! same_dir_path "$state_dir" "$DATA_DIR"; then
        return 1
    fi

    # Layer 2: Process args from ps — check --config or --data-dir.
    local proc_args
    proc_args=$(ps -p "$pid" -o args= 2>/dev/null) || return 1
    case "$proc_args" in
        *"--config $CONFIG_FILE"*|*"--config=$CONFIG_FILE"*)
            return 0
            ;;
        *"--config"*)
            # --config present but doesn't match our CONFIG_FILE — imposter.
            return 1
            ;;
        *"--data-dir"*)
            local proc_dir
            proc_dir=$(echo "$proc_args" | sed -n 's/.*--data-dir[= ]*\([^ ]*\).*/\1/p')
            if [ -n "$proc_dir" ]; then
                if same_dir_path "$proc_dir" "$DATA_DIR"; then
                    return 0
                fi
                return 1
            fi
            ;;
    esac

    # Layer 3: /proc/PID/cwd fallback (Linux only).
    if [ -d "/proc/$pid" ]; then
        local cwd
        cwd=$(readlink "/proc/$pid/cwd" 2>/dev/null) || true
        if [ -n "$cwd" ] && same_dir_path "$cwd" "$DATA_DIR"; then
            return 0
        fi
    fi

    # State file said it's ours (or no state file) and we couldn't disprove it.
    if [ -n "$state_dir" ] && same_dir_path "$state_dir" "$DATA_DIR"; then
        return 0
    fi

    # Cannot verify — treat as unknown (not ours).
    return 1
}

# kill_imposter kills a process that isn't our dolt server.
kill_imposter() {
    local pid="$1"
    [ -n "$pid" ] || return 0

    echo "killing imposter dolt server (PID $pid) on port $DOLT_PORT" >&2
    kill "$pid" 2>/dev/null || return 0

    # Wait up to 5s for graceful shutdown.
    local waited=0
    while [ "$waited" -lt 5 ]; do
        if ! kill -0 "$pid" 2>/dev/null; then
            return 0
        fi
        sleep 1
        waited=$((waited + 1))
    done

    # Force kill.
    kill -9 "$pid" 2>/dev/null || true
    sleep 1
}

# dolt_data_lock_holder prints the path of the first dolt exclusive store
# lock (root-level <data_dir>/.dolt/noms/LOCK or per-database
# <data_dir>/<db>/.dolt/noms/LOCK) held by a live process and returns 0, or
# returns 1 when every lock is free. Dolt holds this flock until its chunk
# journal is flushed and the store is closed — it is the authoritative
# "safe to bind / safe to force-kill" signal (gastownhall/gascity#3174).
# A lock flock cannot probe (exit code other than 0 or 1, e.g. an unreadable
# file) is skipped with a warning — fail open, matching the gc helper's
# probe convention. Without the flock binary no lock state can be probed;
# report free so callers keep the legacy behavior. Unlike the gc helper's
# probe, the per-database glob does not match dot-prefixed database
# directories; managed layouts never create them.
dolt_data_lock_holder() {
    local lock_file probe_err probe_status
    [ "$FLOCK_AVAILABLE" = "true" ] || return 1
    for lock_file in "$DATA_DIR"/.dolt/noms/LOCK "$DATA_DIR"/*/.dolt/noms/LOCK; do
        [ -f "$lock_file" ] || continue
        probe_status=0
        probe_err=$(flock -n "$lock_file" true 2>&1) || probe_status=$?
        case "$probe_status" in
            0) ;;
            1)
                printf '%s\n' "$lock_file"
                return 0
                ;;
            *)
                echo "warning: cannot probe dolt store lock $lock_file: ${probe_err:-flock exit status $probe_status}; treating as free (gastownhall/gascity#3174)" >&2
                ;;
        esac
    done
    return 1
}

# lock_release_timeout_ms prints LOCK_RELEASE_TIMEOUT_MS sanitized to a
# non-negative integer, defaulting to 60000 — matching the gc helper's
# config.DefaultDoltLockReleaseTimeout (1m).
lock_release_timeout_ms() {
    case "$LOCK_RELEASE_TIMEOUT_MS" in
        ''|*[!0-9]*) printf '60000\n' ;;
        *) printf '%s\n' "$LOCK_RELEASE_TIMEOUT_MS" ;;
    esac
}

# wait_dolt_data_lock_free blocks until no live process holds a dolt
# exclusive store lock under DATA_DIR, or LOCK_RELEASE_TIMEOUT_MS elapses.
# Lock release on a clean dolt shutdown happens only after the chunk journal
# is flushed, so success also means the prior instance finished writing.
# Returns 1 (fail closed) when a lock is still held at the deadline.
wait_dolt_data_lock_free() {
    local timeout_ms deadline_ms now_ms holder
    timeout_ms=$(lock_release_timeout_ms)
    holder=$(dolt_data_lock_holder) || return 0
    now_ms=$(current_time_ms) || return 1
    deadline_ms=$((now_ms + timeout_ms))
    while :; do
        now_ms=$(current_time_ms) || return 1
        if [ "$now_ms" -ge "$deadline_ms" ]; then
            echo "dolt exclusive store lock $holder is still held by a live process after ${timeout_ms}ms; a prior dolt sql-server has not released the data dir" >&2
            return 1
        fi
        sleep_ms 250 2>/dev/null || sleep 1
        holder=$(dolt_data_lock_holder) || return 0
    done
}

# graceful_stop_owned_pid stops one of OUR dolt server processes without ever
# SIGKILLing it mid-journal-write: SIGTERM, wait for exit (60 × 500ms = 30s,
# matching the gc helper's default dolt_stop_timeout), then force-kill only if
# the dolt exclusive store lock is free. After exit, blocks until the lock is
# released so a follow-up start cannot bind the data_dir mid-flush. Returns 1
# (fail closed) when the process survives while still holding the lock.
graceful_stop_owned_pid() {
    local pid="$1" waited=0 holder lock_window_ms lock_deadline_ms now_ms
    [ -n "$pid" ] || return 0
    kill "$pid" 2>/dev/null || true
    while [ "$waited" -lt 60 ] && kill -0 "$pid" 2>/dev/null; do
        sleep 0.5 2>/dev/null || sleep 1
        waited=$((waited + 1))
    done
    if kill -0 "$pid" 2>/dev/null; then
        # The process outlived the SIGTERM grace. Extend the wait by the
        # lock-release window while the store lock is held — the holder is
        # mid-flush — then force-kill only once the lock is free.
        lock_window_ms=$(lock_release_timeout_ms)
        now_ms=$(current_time_ms) || now_ms=0
        lock_deadline_ms=$((now_ms + lock_window_ms))
        while kill -0 "$pid" 2>/dev/null && dolt_data_lock_holder >/dev/null; do
            now_ms=$(current_time_ms) || break
            [ "$now_ms" -lt "$lock_deadline_ms" ] || break
            sleep_ms 250 2>/dev/null || sleep 1
        done
        if kill -0 "$pid" 2>/dev/null; then
            if holder=$(dolt_data_lock_holder); then
                echo "PID $pid did not exit within the SIGTERM grace and a live process still holds dolt exclusive store lock $holder; refusing SIGKILL mid-journal-write (gastownhall/gascity#3174)" >&2
                return 1
            fi
            kill -9 "$pid" 2>/dev/null || true
            sleep 1
        fi
    fi
    wait_dolt_data_lock_free
}

# write_config_yaml generates a managed dolt-config.yaml with timeouts and GC settings.
# Overwritten on each server start. Without read/write timeouts, CLOSE_WAIT connections
# accumulate and the server enters unrecoverable read-only mode.
write_config_yaml() {
    local archive_level auto_gc_enabled auto_gc_sysvar gc_bin raw_wait_timeout wait_timeout_line max_connections read_timeout_millis write_timeout_millis
    # Surface the resolved managed-server bind. Since the default flipped from
    # 0.0.0.0 to loopback, an operator who relied on the old wildcard bind would
    # otherwise see a bare connection-refused; this line names the bind host and
    # the override knob.
    printf 'gc-beads-bd: managed dolt server binding %s:%s (override bind with GC_BEADS_HOST=0.0.0.0)\n' "$DOLT_HOST" "$DOLT_PORT" >&2
    archive_level=${GC_BEADS_ARCHIVE_LEVEL:-0}
    case "$archive_level" in
        ''|*[!0-9]*)
            archive_level=0
            ;;
    esac
    # Incremental auto-GC defaults to ON; only explicit false-y overrides
    # disable it. Mirrors parseEnvAutoGCEnabled in cmd/gc/dolt_start_managed.go,
    # including its whitespace trim.
    auto_gc_enabled=true
    auto_gc_sysvar=ON
    case "$(printf '%s' "${GC_BEADS_AUTO_GC_ENABLED:-}" | tr -d '[:space:]')" in
        0|[Ff]|[Ff][Aa][Ll][Ss][Ee]|[Oo][Ff][Ff])
            auto_gc_enabled=false
            auto_gc_sysvar=OFF
            ;;
    esac
    max_connections=${GC_BEADS_MAX_CONNECTIONS:-256}
    case "$max_connections" in
        ''|*[!0-9]*|0)
            max_connections=256
            ;;
    esac
    read_timeout_millis=${GC_BEADS_READ_TIMEOUT_MILLIS:-15000}
    case "$read_timeout_millis" in
        ''|*[!0-9]*|0)
            read_timeout_millis=15000
            ;;
    esac
    write_timeout_millis=${GC_BEADS_WRITE_TIMEOUT_MILLIS:-300000}
    case "$write_timeout_millis" in
        ''|*[!0-9]*|0)
            write_timeout_millis=300000
            ;;
    esac
    gc_bin=$(resolve_gc_helper_bin)
    if [ -n "$gc_bin" ]; then
        "$gc_bin" dolt-config write-managed \
            --file "$CONFIG_FILE" \
            --host "$DOLT_HOST" \
            --port "$DOLT_PORT" \
            --data-dir "$DATA_DIR" \
            --log-level "$DOLT_LOGLEVEL" \
            --archive-level "$archive_level" \
            --auto-gc-enabled="$auto_gc_enabled" \
            --max-connections "$max_connections" \
            --read-timeout-millis "$read_timeout_millis" \
            --write-timeout-millis "$write_timeout_millis" || die "failed to write managed dolt config via gc helper $gc_bin"
        return 0
    fi
    wait_timeout_line='  wait_timeout: "30"'
    raw_wait_timeout=${GC_BEADS_WAIT_TIMEOUT:-}
    case "$raw_wait_timeout" in
        '' ) ;;
        -*)
            case "${raw_wait_timeout#-}" in
                ''|*[!0-9]* ) ;;
                * ) wait_timeout_line="" ;;
            esac
            ;;
        *[!0-9]* ) ;;
        * )
            if [ "$raw_wait_timeout" -gt 0 ] 2>/dev/null; then
                wait_timeout_line="  wait_timeout: \"$raw_wait_timeout\""
            else
                wait_timeout_line=""
            fi
            ;;
    esac
    local tmp
    tmp=$(mktemp "$CONFIG_FILE.tmp.XXXXXX")
    cat > "$tmp" <<YAML
# Dolt SQL server configuration — managed by gc-beads-bd
# Do not edit manually; changes are overwritten on each server start.
# To customize, set environment variables:
#   GC_BEADS_PORT, GC_BEADS_HOST, GC_BEADS_USER, GC_BEADS_PASSWORD, GC_BEADS_LOGLEVEL

log_level: $DOLT_LOGLEVEL

listener:
  port: $DOLT_PORT
  host: $DOLT_HOST
  max_connections: $max_connections
  back_log: 50
  max_connections_timeout_millis: 5000
  read_timeout_millis: $read_timeout_millis
  write_timeout_millis: $write_timeout_millis

data_dir: "$DATA_DIR"

# Incremental auto-GC bounds the noms journal so it never reaches GB scale,
# shrinking both the unclean-stop corruption window and the recovery blast
# radius (#3176). Historically OFF to work around dolt#10944 (load-avg gating
# that never fired); fixed upstream in dolt 2.0.3 and the managed floor is
# 2.1.0+. Scheduled compaction (gc dolt compact) still handles history
# flattening — see #1918, #1200 for that lineage. Override via city.toml
# [dolt] auto_gc_enabled or GC_BEADS_AUTO_GC_ENABLED.
behavior:
  auto_gc_behavior:
    enable: $auto_gc_enabled
    archive_level: $archive_level

# Managed Gas City workloads generate short-lived probe and metadata queries.
# Dolt's persistent stats worker can make those tiny databases grow large
# stats stores and burn CPU, especially on macOS endpoint-managed machines.
# Keep stats disabled for managed servers; use explicit gc dolt maintenance
# commands for storage cleanup instead of background workers.
system_variables:
  dolt_auto_gc_enabled: "$auto_gc_sysvar"
  dolt_stats_enabled: "OFF"
  dolt_stats_gc_enabled: "OFF"
  dolt_stats_memory_only: "ON"
  dolt_stats_paused: "ON"
$wait_timeout_line
YAML
    mv "$tmp" "$CONFIG_FILE"
}

# get_connection_count queries the active connection count from the dolt server.
# Prints the count to stdout. Returns 1 on failure.
get_connection_count() {
    local host output
    host=$(connect_host)
    output=$(dolt --host "$host" --port "$DOLT_PORT" --user "$DOLT_USER" --password "${DOLT_PASSWORD:-}" --no-tls \
        sql -r csv -q "SELECT COUNT(*) AS cnt FROM information_schema.PROCESSLIST" 2>/dev/null) || return 1
    # Parse CSV: "cnt\n5\n" — take last non-empty line.
    echo "$output" | tail -1 | tr -d '[:space:]'
}

# drain_connections_before_stop waits briefly for in-flight SQL work to leave
# before SIGTERM. It is best-effort: an unreachable or wedged server should not
# block explicit stop/recover forever.
drain_connections_before_stop() {
    local count waited
    waited=0
    while [ "$waited" -lt 100 ]; do
        count=$(get_connection_count 2>/dev/null) || return 0
        case "$count" in
            ''|*[!0-9]*) return 0 ;;
        esac
        [ "$count" -le 1 ] && return 0
        sleep 0.1 2>/dev/null || sleep 1
        waited=$((waited + 1))
    done
}

# check_read_only tests if the dolt server is in read-only mode.
# Returns 0 if read-only, 1 if writable, 2 if the write probe is inconclusive.
check_read_only() {
    local host gc_bin db quoted_db probe_table sql output err_file err_text status
    host=$(connect_host)
    gc_bin=$(resolve_gc_helper_bin)
    if [ -n "$gc_bin" ]; then
        err_file=$(mktemp "${TMPDIR:-/tmp}/gc-dolt-read-only-check.XXXXXX") || return 2
        if "$gc_bin" dolt-state read-only-check --host "$host" --port "$DOLT_PORT" --user "$DOLT_USER" >/dev/null 2>"$err_file"; then
            rm -f "$err_file"
            return 0
        fi
        err_text=$(cat "$err_file" 2>/dev/null || true)
        rm -f "$err_file"
        if [ -n "$err_text" ]; then
            echo "$err_text" >&2
            return 2
        fi
        return 1
    fi
    err_file=$(mktemp "${TMPDIR:-/tmp}/gc-dolt-show-databases.XXXXXX") || return 2
    if output=$(dolt --host "$host" --port "$DOLT_PORT" --user "$DOLT_USER" --password "${DOLT_PASSWORD:-}" --no-tls \
        sql -r csv -q "SHOW DATABASES" 2>"$err_file"); then
        status=0
    else
        status=$?
    fi
    err_text=$(cat "$err_file" 2>/dev/null || true)
    rm -f "$err_file"
    if [ "$status" -ne 0 ]; then
        case "$err_text" in
            *"read only"*|*"READ ONLY"*|*"Read-only"*)
                return 0
                ;;
        esac
        [ -n "$err_text" ] && echo "dolt SHOW DATABASES failed: $err_text" >&2
        return 2
    fi
    db=$(first_user_database_from_show_databases_csv "$output") || return 2
    if [ -z "$db" ]; then
        echo "dolt read-only probe inconclusive: no user database available" >&2
        return 2
    fi
    quoted_db=$(quote_dolt_identifier "$db")
    probe_table='`__gc_read_only_probe`'
    sql="CREATE TABLE IF NOT EXISTS ${quoted_db}.${probe_table} (k INT PRIMARY KEY); REPLACE INTO ${quoted_db}.${probe_table} VALUES (1);"
    if output=$(dolt --host "$host" --port "$DOLT_PORT" --user "$DOLT_USER" --password "${DOLT_PASSWORD:-}" --no-tls \
        sql -q "$sql" 2>&1); then
        return 1
    fi
    case "$output" in
        *"read only"*|*"READ ONLY"*|*"Read-only"*)
            return 0  # Is read-only.
            ;;
    esac
    [ -n "$output" ] && echo "dolt write probe failed: $output" >&2
    return 2
}

# find_dolt_pid finds the dolt sql-server process.
# Priority: PID file → lsof port holder → ps grep fallback.
find_dolt_pid() {
    if [ -z "$DATA_DIR" ]; then
        return
    fi

    # 1. PID file (most reliable if we wrote it).
    if [ -f "$PID_FILE" ]; then
        local file_pid
        file_pid=$(cat "$PID_FILE" 2>/dev/null)
        if [ -n "$file_pid" ] && kill -0 "$file_pid" 2>/dev/null; then
            echo "$file_pid"
            return
        fi
        # Stale PID file — clean up.
        rm -f "$PID_FILE"
    fi

    # 2. lsof port holder.
    local holder
    holder=$(find_port_holder)
    if [ -n "$holder" ]; then
        echo "$holder"
        return
    fi

    # 3. ps grep fallback (least reliable) — try --config first, then --data-dir.
    if [ -n "$CONFIG_FILE" ]; then
        local config_pid
        config_pid=$(ps ax -o pid,args 2>/dev/null | grep "dolt sql-server" | grep -- "--config.*$CONFIG_FILE" | grep -v grep | awk '{print $1}' | head -1)
        if [ -n "$config_pid" ]; then
            echo "$config_pid"
            return
        fi
    fi
    ps ax -o pid,args 2>/dev/null | grep "dolt sql-server" | grep -- "--data-dir.*$(basename "$DATA_DIR")" | grep -v grep | awk '{print $1}' | head -1
}

# allocate_port determines the dolt server port.
# Resolution order:
#   1. State file has a reachable port + PID is alive → reuse it
#   2. GC_BEADS_PORT env var (initial/operator override) → use it
#   3. Hash GC_CITY_PATH into range 10000–60000, probe with lsof, increment until free
allocate_port() {
    local gc_bin helper_port
    gc_bin=$(resolve_gc_helper_bin)
    if [ -n "$gc_bin" ]; then
        helper_port=$("$gc_bin" dolt-state allocate-port --city "$GC_CITY_PATH" --state-file "$STATE_FILE" </dev/null 2>/dev/null || true)
        if [ -n "$helper_port" ]; then
            echo "$helper_port"
            return
        fi
    fi

    # 1. Provider state port with live PID. Long-lived agents can inherit a
    # stale GC_BEADS_PORT after managed Dolt rolls to a new port, so validated
    # state wins over the inherited environment.
    if [ -f "$STATE_FILE" ]; then
        local state_port state_pid
        state_port=$(load_state_field port)
        state_pid=$(load_state_field pid)
        if [ -n "$state_port" ] && [ -n "$state_pid" ] && kill -0 "$state_pid" 2>/dev/null && tcp_check_port "$state_port"; then
            echo "$state_port"
            return
        fi
    fi

    # 2. Explicit override when no live provider state is available.
    if [ -n "$GC_BEADS_PORT" ]; then
        echo "$GC_BEADS_PORT"
        return
    fi

    # 3. Deterministic hash of city path, probe until free.
    local hash_val
    hash_val=$(printf '%s' "$GC_CITY_PATH" | cksum | awk '{print $1 % 50000 + 10000}')
    local port="$hash_val"
    local attempts=0
    while [ "$attempts" -lt 100 ]; do
        if ! run_lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
            echo "$port"
            return
        fi
        port=$((port + 1))
        if [ "$port" -gt 60000 ]; then
            port=10000
        fi
        attempts=$((attempts + 1))
    done

    # Exhausted probes — fall back to the hash value and hope for the best.
    echo "$hash_val"
}

# --- Operations ---

# valid_sql_name checks that a name is safe for backtick-quoted SQL identifiers.
# Allows alphanumeric, hyphens, underscores. Rejects empty names and names
# containing backticks, quotes, or other special characters (upstream 38f7b380).
valid_sql_name() {
    case "$1" in
        "") return 1 ;;
        *[!a-zA-Z0-9_-]*) return 1 ;;
    esac
    return 0
}

# clean_stale_sockets removes stale Unix domain sockets left by a crashed
# dolt server. Without cleanup, "unix socket set up failed: file already in
# use" prevents clean restarts (upstream 2e058fa1).
clean_stale_sockets() {
    local sock
    for sock in /tmp/dolt*.sock; do
        [ -S "$sock" ] || continue
        local open_status
        set +e
        lsof_reports_open "$sock"
        open_status=$?
        set -e
        case "$open_status" in
            0)
                ;;
            1)
                echo "removing stale socket: $sock" >&2
                rm -f "$sock"
                ;;
            *)
                echo "preserving socket with unknown open-file state: $sock" >&2
                ;;
        esac
    done
}

# ensure_beads_role ensures beads.role is set in global git config.
# bd exits non-zero with "beads.role not configured" (gastownhall/beads#2950)
# when this key is absent. That non-zero exit causes the `run_bd_pinned … ||
# true` calls in op_init to fail silently, leaving issue_prefix and
# types.custom unset in the Dolt database and making every subsequent
# bd-create call fail with "database not initialized". Defaulting to
# "maintainer" matches the role that gc-managed agents use to create beads.
ensure_beads_role() {
    if git config --global beads.role >/dev/null 2>&1; then
        return 0
    fi
    echo "gc-beads-bd: setting git config --global beads.role maintainer" >&2
    git config --global beads.role maintainer || die "failed to set git config beads.role"
}

# ensure_dolt_identity ensures dolt has user.name and user.email configured.
#
# Resolution order per field, in order of precedence:
#   1. dolt config --global (returned as-is if already set)
#   2. git config --global  (copied into dolt config)
#
# If a field is missing from BOTH dolt and git, fail with an error that
# names the specific field(s) the user must set — never instruct the user
# to set a value they have already configured. Historically this function
# would report "user.name not available" whenever EITHER field was missing
# (because the dolt-side guard required both), which left users running
# `dolt config --add user.name` over and over while the real culprit was
# user.email.
ensure_dolt_identity() {
    # Use dolt's exit code as the canonical "is this field configured?"
    # signal. Real dolt returns 0 with the value on stdout when the field
    # is set, non-zero otherwise; some tests stub dolt to return 0 with
    # empty stdout for any `config` invocation, and we treat that as
    # configured too (matches historical behavior of this helper).
    local dolt_has_name=0 dolt_has_email=0
    local dolt_name="" dolt_email="" git_name git_email
    if dolt config --global --get user.name >/dev/null 2>&1; then
        dolt_has_name=1
        dolt_name=$(dolt config --global --get user.name 2>/dev/null || true)
    fi
    if dolt config --global --get user.email >/dev/null 2>&1; then
        dolt_has_email=1
        dolt_email=$(dolt config --global --get user.email 2>/dev/null || true)
    fi
    if [ "$dolt_has_name" -eq 1 ] && [ "$dolt_has_email" -eq 1 ]; then
        return 0
    fi

    git_name=$(git config --global user.name 2>/dev/null || true)
    git_email=$(git config --global user.email 2>/dev/null || true)

    # Accumulate missing-field hints in a semicolon-joined string rather
    # than a bash array so this stays runnable under POSIX /bin/sh
    # (matches the script's shebang). Each branch reports only the field
    # that is truly missing from BOTH dolt and git — never instruct the
    # user to set a value they have already configured.
    local missing=""
    if [ "$dolt_has_name" -ne 1 ] && [ -z "$git_name" ]; then
        missing='dolt config --global --add user.name "Your Name"'
    fi
    if [ "$dolt_has_email" -ne 1 ] && [ -z "$git_email" ]; then
        if [ -n "$missing" ]; then
            missing="$missing; "
        fi
        missing="${missing}dolt config --global --add user.email \"you@example.com\""
    fi
    if [ -n "$missing" ]; then
        die "dolt identity incomplete; run: $missing"
    fi

    # Backfill missing dolt fields from git.
    if [ "$dolt_has_name" -ne 1 ]; then
        dolt config --global --add user.name "$git_name" || die "failed to set dolt user.name"
    fi
    if [ "$dolt_has_email" -ne 1 ]; then
        dolt config --global --add user.email "$git_email" || die "failed to set dolt user.email"
    fi
}

# journal_corruption_signature filters stdin for the dolt startup errors that
# indicate a corrupted noms journal ("possible data loss detected in journal
# file at offset N: corrupted journal", "journal index is malformed"). Used on
# captured startup output, the managed log tail, and per-database offline
# probe output.
journal_corruption_signature() {
    grep -qiE 'corrupted journal|journal index is malformed|possible data loss detected in journal file'
}

# log_tail_has_journal_corruption reports whether the recent managed dolt log
# contains a journal-corruption startup error. Bounded to the log tail so a
# huge log cannot stall start; stale matches from earlier incidents are
# harmless because recovery re-verifies each database with an offline probe
# before touching anything.
log_tail_has_journal_corruption() {
    [ -f "$LOG_FILE" ] || return 1
    tail -c 65536 "$LOG_FILE" 2>/dev/null | journal_corruption_signature
}

# database_journal_corrupt probes one database directory offline and reports
# whether dolt refuses to load it with a journal-corruption error. Only safe
# while the managed server is down — offline dolt commands contend with a
# running server's file locks. Probe output is spooled to a temp file so stdout
# and stderr can be scanned together without retaining the diagnostic stream
# in a shell variable.
database_journal_corrupt() {
    local probe_db_dir="$1" probe_out probe_hit=1
    probe_out=$(mktemp) || {
        echo "gc-beads-bd: probe tempfile unavailable; treating $probe_db_dir as not corrupt" >&2
        return 1
    }
    (cd "$probe_db_dir" && run_with_timeout 30 dolt status) > "$probe_out" 2>&1 || true
    if journal_corruption_signature < "$probe_out"; then
        probe_hit=0
    fi
    rm -f "$probe_out"
    return "$probe_hit"
}

# backup_remote_url_for_recovery prints the <db>-backup remote URL recorded in
# a database's repo_state.json. The file is plain JSON, so the URL is readable
# even when the noms store itself can no longer be opened. Handles both the
# object form ("backups": {"db-backup": {"url": "..."}}) and the legacy plain
# string form.
backup_remote_url_for_recovery() {
    local recovery_db="$1" recovery_db_dir="$2" repo_state url
    repo_state="$recovery_db_dir/.dolt/repo_state.json"
    [ -f "$repo_state" ] || return 1
    if command -v jq >/dev/null 2>&1; then
        url=$(jq -r --arg name "${recovery_db}-backup" '.backups[$name].url? // .backups[$name] // empty' "$repo_state" 2>/dev/null)
    else
        url=$(tr -d '\n' < "$repo_state" | sed -n "s/.*\"${recovery_db}-backup\"[^}]*\"url\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p")
        if [ -z "$url" ]; then
            url=$(tr -d '\n' < "$repo_state" | sed -n "s/.*\"${recovery_db}-backup\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p")
        fi
    fi
    [ -n "$url" ] || return 1
    printf '%s\n' "$url"
}

# backup_restore_source_usable reports whether url points at a local backup
# that actually has content to restore from. Only file:// remotes qualify:
# remote backups cannot be cheaply verified, and restoring from an unverified
# source is exactly the kind of silent data movement auto-recovery must not do.
backup_restore_source_usable() {
    local usable_url="$1" usable_path
    case "$usable_url" in
        file://*) usable_path="${usable_url#file://}" ;;
        *) return 1 ;;
    esac
    [ -d "$usable_path" ] || return 1
    [ -n "$(ls -A "$usable_path" 2>/dev/null)" ]
}

# attempt_journal_corruption_recovery scans the data dir for databases whose
# noms journal dolt refuses to load, preserves each corrupt store under
# $PACK_STATE_DIR/corrupt-aside/ (never deleted), and restores the database
# from its local <db>-backup remote (#3176). Fail-closed: when any corrupt
# database has no usable backup or the restore fails, its store is moved back
# so the server cannot come up silently missing a database, and the function
# returns 1. Returns 0 only when at least one database was restored and none
# were left unrecoverable. Everything is logged loudly — restored copies are
# missing all writes since the last backup sync, and operators must know that.
attempt_journal_corruption_recovery() {
    local aside_root="$PACK_STATE_DIR/corrupt-aside"
    local ts db_dir db aside url recovered=0
    ts=$(date -u +%Y%m%dT%H%M%SZ 2>/dev/null || date +%s)
    echo "gc-beads-bd: journal corruption reported at startup; probing databases in $DATA_DIR" >&2
    for db_dir in "$DATA_DIR"/*/; do
        [ -d "$db_dir/.dolt" ] || continue
        db=$(basename "$db_dir")
        database_journal_corrupt "$db_dir" || continue
        echo "gc-beads-bd: journal corruption confirmed in database '$db'" >&2
        url=$(backup_remote_url_for_recovery "$db" "$db_dir") || url=""
        if [ -z "$url" ] || ! backup_restore_source_usable "$url"; then
            echo "gc-beads-bd: NOT auto-recovering '$db': no usable local backup (remote url: ${url:-none})" >&2
            echo "gc-beads-bd: manual recovery required: move $DATA_DIR/$db aside, then run 'dolt backup restore <url> $db' from $DATA_DIR" >&2
            return 1
        fi
        mkdir -p "$aside_root" || return 1
        aside="$aside_root/$db.$ts"
        if ! mv "$DATA_DIR/$db" "$aside"; then
            echo "gc-beads-bd: could not move corrupt store $DATA_DIR/$db aside to $aside; aborting recovery" >&2
            return 1
        fi
        echo "gc-beads-bd: preserved corrupt store at $aside" >&2
        if (cd "$DATA_DIR" && run_with_timeout 600 dolt backup restore "$url" "$db") >> "$LOG_FILE" 2>&1; then
            recovered=$((recovered + 1))
            echo "gc-beads-bd: RESTORED '$db' from backup $url — writes since the last backup sync are NOT in the restored copy; pre-corruption store kept at $aside" >&2
        else
            # Fail closed: put the corrupt store back so the server cannot
            # start without this database and silently drop it from the
            # control plane. The partial restore output (if any) is fresh
            # data written by the failed restore, not original state.
            rm -rf "${DATA_DIR:?}/${db:?}" 2>/dev/null || true
            if ! mv "$aside" "$DATA_DIR/$db"; then
                echo "gc-beads-bd: CRITICAL: restore failed AND corrupt store could not be moved back; original data is at $aside" >&2
            fi
            echo "gc-beads-bd: backup restore failed for '$db' (see $LOG_FILE); not retrying" >&2
            return 1
        fi
    done
    if [ "$recovered" -eq 0 ]; then
        echo "gc-beads-bd: no corrupt database confirmed by offline probe; not recovering" >&2
        return 1
    fi
    return 0
}

# op_start starts the dolt server if not already running.
op_start() {
    # bd's proxied-server mode owns the dolt sql-server lifecycle: op_init runs
    # `bd init --proxied-server`, and bd lazily starts the proxy + child dolt on
    # the first store command and idles them out on its own. gascity no longer
    # spawns or supervises a server, so start is a no-op.
    exit 0
}

# op_ensure_ready is a legacy alias for start.
op_ensure_ready() {
    # bd's proxied-server provider starts the proxy + child dolt lazily on the
    # first store command; there is nothing to pre-start.
    exit 0
}

# run_bd_pinned executes bd against the already-selected Dolt backend for this
# provider operation. Without these exports, plain bd commands can rediscover
# or auto-start a different local server mid-init.
run_bd_pinned() {
    local dir="$1"
    shift
    local beads_dir="$dir/.beads"
    local host
    host=$(connect_host)
    (
        cd "$dir" || exit 1
        export BEADS_DIR="$beads_dir"
        export GC_BEADS_HOST="$host"
        export BEADS_DOLT_SERVER_HOST="$host"
        export GC_BEADS_PORT="$DOLT_PORT"
        export BEADS_DOLT_SERVER_PORT="$DOLT_PORT"
        export GC_BEADS_USER="$DOLT_USER"
        export GC_BEADS_PASSWORD="$DOLT_PASSWORD"
        export BEADS_DOLT_SERVER_USER="$DOLT_USER"
        export BEADS_DOLT_PASSWORD="$DOLT_PASSWORD"
        bd "$@"
    )
}

run_bd_doltlite() {
    local dir="$1"
    shift
    (
        cd "$dir" || exit 1
        export BEADS_DIR="$dir/.beads"
        export BEADS_BACKEND="doltlite"
        export GC_BEADS_BACKEND="doltlite"
        unset GC_BEADS_HOST GC_BEADS_PORT GC_BEADS_USER GC_BEADS_PASSWORD GC_DOLT
        unset BEADS_DOLT_DATABASE BEADS_DOLT_PORT
        unset BEADS_DOLT_SERVER_DATABASE BEADS_DOLT_SERVER_HOST BEADS_DOLT_SERVER_MODE BEADS_DOLT_SERVER_PORT BEADS_DOLT_SERVER_USER BEADS_DOLT_PASSWORD
        export BEADS_DOLT_AUTO_START=0
        "${BD_BIN:-bd}" "$@"
    )
}

doltlite_bd_issue_prefix() {
    local dir="$1"
    run_bd_doltlite "$dir" config get issue_prefix 2>/dev/null | sed 's/[[:space:]]*$//' || true
}

doltlite_bd_schema_ready() {
    local dir="$1" prefix="$2"
    doltlite_bd_issue_prefix "$dir" | grep -Fx "$prefix" >/dev/null 2>&1
}

run_bd_doltlite_init() {
    local dir="$1" prefix="$2" database="$3" reinit="${4:-false}"
    if [ "$reinit" = true ]; then
        run_bd_doltlite "$dir" init --reinit-local --quiet -p "$prefix" --database "$database" --skip-hooks --skip-agents || die "bd doltlite init failed for $dir"
        return 0
    fi
    run_bd_doltlite "$dir" init --quiet -p "$prefix" --database "$database" --skip-hooks --skip-agents || die "bd doltlite init failed for $dir"
}

ensure_doltlite_bd_schema() {
    local dir="$1" prefix="$2" database="$3" reinit=false
    if doltlite_bd_schema_ready "$dir" "$prefix"; then
        return 0
    fi
    if [ -d "$dir/.beads/embeddeddolt/$database/.dolt" ]; then
        reinit=true
    fi
    run_bd_doltlite_init "$dir" "$prefix" "$database" "$reinit"
}

doltlite_maintenance_due() {
    local dir="$1"
    local stamp="$dir/.beads/doltlite/.gc-maintenance.stamp"
    local interval="${GC_DOLTLITE_MAINTENANCE_INTERVAL_SECONDS:-86400}"
    local now last
    [ "$interval" -gt 0 ] 2>/dev/null || return 0
    [ -f "$stamp" ] || return 0
    now=$(date +%s 2>/dev/null || echo 0)
    last=$(stat -c %Y "$stamp" 2>/dev/null || stat -f %m "$stamp" 2>/dev/null || echo 0)
    [ $((now - last)) -ge "$interval" ]
}

# run_doltlite_reindex rebuilds the DoltLite store's SQLite secondary indexes.
# `bd flatten`/`bd gc` rewrite the store (like a clone/pull) and leave the
# secondary indexes stale, so index-path reads (count/status/list) silently
# return wrong results until a REINDEX (ga-7hei). REINDEX is SQLite-specific
# DDL, so it must run against the physical .beads/doltlite/<db>.db file through
# gc's in-process SQLite driver (gc dolt-config doltlite-reindex, which resolves
# the same .db the read path opens from metadata.json). It cannot go through
# `bd sql`: that surface speaks Dolt/MySQL and rejects REINDEX, and it is
# refused outright in the embedded mode run_bd_doltlite forces. Best-effort and
# non-fatal: the caller warns on non-zero exit.
run_doltlite_reindex() {
    local dir="$1"
    local gc_bin
    gc_bin=$(resolve_gc_helper_bin)
    if [ -z "$gc_bin" ]; then
        return 1
    fi
    "$gc_bin" dolt-config doltlite-reindex --dir "$dir"
}

# doltlite_reindex_supported reports whether the resolved gc helper can rebuild
# the DoltLite SQLite indexes in process. Only a gc built with the native beads
# SQLite driver can (gc dolt-config doltlite-reindex --check); a default build
# returns non-zero. The maintenance path probes this BEFORE the stale-index-
# producing flatten/gc so it never creates index corruption it cannot heal
# (ga-7hei).
doltlite_reindex_supported() {
    local dir="$1"
    local gc_bin
    gc_bin=$(resolve_gc_helper_bin)
    if [ -z "$gc_bin" ]; then
        return 1
    fi
    "$gc_bin" dolt-config doltlite-reindex --dir "$dir" --check >/dev/null 2>&1
}

run_doltlite_existing_db_maintenance() {
    local dir="$1"
    local stamp="$dir/.beads/doltlite/.gc-maintenance.stamp"
    if ! doltlite_maintenance_due "$dir"; then
        return 0
    fi
    # flatten/gc rewrite the store and leave its SQLite secondary indexes stale;
    # only a reindex-capable gc build can heal that (ga-7hei). gc reaches the
    # store through the bd subprocess and links no in-process reindex helper, so
    # do NOT run the stale-index-producing flatten/gc at all: creating index
    # corruption we cannot heal and then latching the maintenance stamp "done" is
    # worse than skipping compaction. Leave the stamp untouched so a later
    # reindex-capable binary still runs maintenance.
    if ! doltlite_reindex_supported "$dir"; then
        echo "warning: skipping doltlite maintenance for $dir: in-process reindex is not supported by this gc build; leaving the store un-flattened to avoid stale indexes (ga-7hei)" >&2
        return 0
    fi
    echo "gc-beads-bd: running doltlite maintenance for $dir" >&2
    run_bd_doltlite "$dir" flatten --force --json >/dev/null 2>&1 || echo "warning: bd flatten failed for $dir" >&2
    run_bd_doltlite "$dir" gc --skip-decay --force --json >/dev/null 2>&1 || echo "warning: bd gc failed for $dir" >&2
    # flatten/gc leave the SQLite secondary indexes stale; rebuild them so
    # index-path reads don't silently return wrong data (ga-7hei). Only stamp
    # maintenance complete when the reindex succeeds — a failed reindex (e.g. a
    # transient SQLite lock) must stay visible and retryable on the next cycle,
    # not be suppressed for the whole maintenance interval.
    if ! run_doltlite_reindex "$dir"; then
        echo "warning: doltlite reindex failed for $dir; leaving maintenance stamp unrefreshed so the next run retries (ga-7hei)" >&2
        return 0
    fi
    mkdir -p "$dir/.beads/doltlite" 2>/dev/null || true
    date +%s > "$stamp" 2>/dev/null || true
}

ensure_beads_dir_permissions() {
    local dir="$1"
    local beads_dir="$dir/.beads"
    mkdir -p "$beads_dir" || die "failed to create $beads_dir"
    chmod 700 "$beads_dir" || die "failed to set $beads_dir permissions to 700"
}

# op_init initializes beads in a directory.
# Args: <dir> <prefix> [dolt_database]
op_init() {
    local dir="$1" prefix="$2" dolt_database="${3:-$prefix}"
    if [ -z "$dir" ] || [ -z "$prefix" ]; then
        die "usage: gc-beads-bd init <dir> <prefix> [dolt_database]"
    fi
    if ! valid_sql_name "$prefix"; then
        die "invalid beads prefix: $prefix (must be alphanumeric, hyphens, underscores)"
    fi
    if ! valid_sql_name "$dolt_database"; then
        die "invalid beads database: $dolt_database (must be alphanumeric, hyphens, underscores)"
    fi

    local beads_dir="$dir/.beads"
    unset BEADS_DIR
    export BEADS_DIR="$beads_dir"
    ensure_beads_dir_permissions "$dir"
    ensure_beads_role

    local custom_types="${GC_BEADS_CUSTOM_TYPES:-molecule,convoy,message,event,gate,merge-request,agent,role,rig,session,spec,convergence,step}"

    if is_doltlite_backend; then
        local database already_ready
        database="$dolt_database"
        if [ -z "$database" ]; then
            database="$prefix"
        fi
        if ! valid_sql_name "$database"; then
            die "invalid doltlite database name: $database (must be alphanumeric, hyphens, underscores)"
        fi
        validate_bd_runtime_config_value "types.custom" "$custom_types"
        ensure_beads_dir_permissions "$dir"
        already_ready=false
        if doltlite_bd_schema_ready "$dir" "$prefix"; then
            already_ready=true
        fi
        ensure_doltlite_bd_schema "$dir" "$prefix" "$database"
        write_doltlite_metadata "$dir" "$database"
        if [ "$already_ready" = true ]; then
            run_doltlite_existing_db_maintenance "$dir"
        fi
        exit 0
    fi

    # Proxied-server mode: bd owns the proxy + child dolt and writes every beads
    # state file (metadata.json, config.yaml, proxied_server_client_info.json,
    # project_id). gascity supplies only operational inputs — the tuning config,
    # the root/log/config paths, and the idle timeout. Idempotent: skip when the
    # store is already initialized and serving.
    if [ -f "$dir/.beads/metadata.json" ] && run_bd_pinned "$dir" ready >/dev/null 2>&1; then
        exit 0
    fi

    local idle="${GC_BEADS_PROXIED_IDLE_TIMEOUT:-5m}"

    if is_remote || [ -n "${BEADS_DOLT_CREDENTIAL_COMMAND:-}" ]; then
        # External/hosted backend: bd fronts an operator-managed dolt sql-server.
        run_bd_pinned "$dir" init --quiet --proxied-server \
            -p "$prefix" --database "$dolt_database" --skip-hooks --skip-agents \
            --proxied-server-idle-timeout "$idle" \
            --proxied-server-log-path "$LOG_FILE" \
            --proxied-server-external-host "$GC_BEADS_HOST" \
            --proxied-server-external-port "$DOLT_PORT" \
            "$dir" || die "bd init --proxied-server (external) failed for $dir"
    else
        # Local backend: gascity writes the operational dolt sql-server config
        # (its tuning knobs); bd starts and manages the proxy + child dolt from it.
        DOLT_HOST=127.0.0.1
        write_config_yaml || die "failed to write proxied dolt config for $dir"
        run_bd_pinned "$dir" init --quiet --proxied-server \
            -p "$prefix" --database "$dolt_database" --skip-hooks --skip-agents \
            --proxied-server-idle-timeout "$idle" \
            --proxied-server-root-path "$DATA_DIR" \
            --proxied-server-log-path "$LOG_FILE" \
            --proxied-server-config-path "$CONFIG_FILE" \
            "$dir" || die "bd init --proxied-server failed for $dir"
    fi

    # gascity's custom bead types, injected through bd (proxied SQL) rather than
    # a direct dial. bd init -p already set the issue prefix.
    run_bd_pinned "$dir" sql \
        "INSERT INTO config (\`key\`, value) VALUES ('types.custom', '$custom_types') ON DUPLICATE KEY UPDATE value = VALUES(value)" \
        >/dev/null 2>&1 || true
    # Re-tighten after bd/dolt init: the child dolt sql-server can recreate or
    # relax .beads to a looser mode; bd warns on every command when .beads is
    # group/world-accessible, so restore the 0700 bd expects.
    ensure_beads_dir_permissions "$dir"
    exit 0
}


scope_store_dir() {
    if [ -n "${GC_STORE_ROOT:-}" ]; then
        printf '%s\n' "$GC_STORE_ROOT"
        return
    fi
    printf '%s\n' "$GC_CITY_PATH"
}

op_store_bridge() {
    local scope_dir host gc_bin
    scope_dir=$(scope_store_dir)
    host=$(connect_host)

    gc_bin=$(resolve_gc_bin)
    if [ -z "$gc_bin" ]; then
        die "gc binary not found for exec store operations"
    fi

    GC_BEADS_PASSWORD="$DOLT_PASSWORD"     BEADS_DOLT_PASSWORD="$DOLT_PASSWORD"     "$gc_bin" bd-store-bridge         --dir "$scope_dir"         --host "$host"         --port "$DOLT_PORT"         --user "$DOLT_USER"         "$@"
    return $?
}
op_health() {
    # Liveness through bd, the sole interface to the server: a trivial read
    # proves the proxied server is reachable (bd brings the proxy up on demand).
    if run_bd_pinned "$GC_CITY_PATH" sql "SELECT 1" >/dev/null 2>&1; then
        exit 0
    fi
    die "beads store not reachable via bd"
}

# op_probe checks if the dolt server is available.
# Exit 0 = running, exit 2 = not running.
op_probe() {
    if is_remote; then
        # Remote server — check TCP.
        if tcp_check; then
            exit 0
        else
            exit 2
        fi
    fi

    if load_probe_managed_from_gc; then
        if [ "$GC_PROBE_RUNNING" = "true" ]; then
            exit 0
        fi
        exit 2
    fi

    # Local server — check port holder and verify identity.
    local holder
    if load_managed_process_inspection_from_gc; then
        holder="$GC_PORT_HOLDER_PID"
        if [ -n "$holder" ]; then
            if [ "$GC_PORT_HOLDER_OWNED" = true ] && tcp_check; then
                exit 0
            fi
            exit 2
        fi
    fi
    holder=$(find_port_holder)
    if [ -z "$holder" ]; then
        exit 2
    fi

    # Verify it's our server.
    if verify_our_server "$holder" && tcp_check; then
        exit 0
    fi

    # Imposter or unreachable.
    exit 2
}

enospc_helper="$(CDPATH= cd -- "$(dirname "$0")" && pwd)/dolt-enospc.sh"
if [ -r "$enospc_helper" ]; then
    . "$enospc_helper"
else
    # Some focused shell harnesses execute gc-beads-bd's prelude as a single
    # temporary file without sibling assets. Keep the production helper as the
    # canonical copy, but preserve the same detector behavior for those harnesses.
    recovery_should_skip_due_to_enospc() {
        [ -n "${LOG_FILE:-}" ] && [ -r "$LOG_FILE" ] || return 1
        tail -n 1000 "$LOG_FILE" 2>/dev/null \
            | grep -qE 'no space left on device|copy_file_range:.*no space|ENOSPC' \
            || return 1
        return 0
    }
fi

# op_recover stops the dolt server, restarts it, and verifies health.
op_recover() {
    # bd's proxied-server provider self-recovers: the proxy respawns its child
    # dolt on demand, so there is no gascity-driven recovery.
    exit 0
}

# op_stop_impl is the internal stop implementation (no exit on "not running").
op_stop_impl() {
    # Tear down bd's proxy + child dolt for this scope.
    run_bd_pinned "$GC_CITY_PATH" dolt stop >/dev/null 2>&1 || true
    return 0
}

# op_stop stops the dolt server.
op_stop() {
    op_stop_impl
    exit 0
}

# op_shutdown is a legacy alias for stop.
op_shutdown() {
    op_stop
}

# --- Main ---

# GC_BEADS_SKIP truthy → no-op for all operations.
if [ "$GC_BEADS_SKIP" = "1" ] || [ "$GC_BEADS_SKIP" = "true" ] || [ "$GC_BEADS_SKIP" = "skip" ]; then
    exit 2
fi

op="$1"
shift || true

# Validate GC_CITY_PATH.
if [ -z "$GC_CITY_PATH" ]; then
    die "GC_CITY_PATH not set"
fi

# Set derived paths.
GC_DIR="$GC_CITY_PATH/.gc"
BEADS_DIR_ROOT="$GC_CITY_PATH/.beads"

# Prefer GC-owned runtime layout derivation when the current gc binary is
# available. Fall back to the legacy shell derivation for compatibility.
if ! load_runtime_layout_from_gc; then
    if [ -n "$GC_PACK_STATE_DIR" ]; then
        PACK_STATE_DIR="$GC_PACK_STATE_DIR"
    elif [ -n "$GC_CITY_RUNTIME_DIR" ]; then
        PACK_STATE_DIR="$GC_CITY_RUNTIME_DIR/packs/dolt"
    else
        PACK_STATE_DIR="$GC_DIR/runtime/packs/dolt"
    fi

    # All data lives under .beads/dolt by default. Runtime state (logs, PID,
    # lock) lives under PACK_STATE_DIR. GC may project the fully resolved paths so
    # this backend bridge does not have to own runtime layout policy.
    DATA_DIR="${GC_BEADS_DATA_DIR:-$BEADS_DIR_ROOT/dolt}"
    LOG_FILE="${GC_BEADS_LOG_FILE:-$PACK_STATE_DIR/dolt.log}"
    STATE_FILE="${GC_BEADS_STATE_FILE:-$PACK_STATE_DIR/dolt-provider-state.json}"
    PID_FILE="${GC_BEADS_PID_FILE:-$PACK_STATE_DIR/dolt.pid}"
    LOCK_FILE="${GC_BEADS_LOCK_FILE:-$PACK_STATE_DIR/dolt.lock}"
    CONFIG_FILE="${GC_BEADS_CONFIG_FILE:-$PACK_STATE_DIR/dolt-config.yaml}"
fi
if is_doltlite_backend; then
    mkdir -p "$PACK_STATE_DIR"
else
    mkdir -p "$DATA_DIR" "$PACK_STATE_DIR"
fi

# Resolve DOLT_PORT now that STATE_FILE is set.
DOLT_PORT=$(allocate_port)

case "$op" in
    start)        op_start ;;
    ensure-ready) op_ensure_ready ;;
    init)         op_init "$@" ;;
    create|get|update|close|reopen|list|ready|children|list-by-label|set-metadata|delete|dep-add|dep-remove|dep-list)
                  op_store_bridge "$op" "$@" ;;
    health)       op_health ;;
    probe)        op_probe ;;
    recover)      op_recover ;;
    stop)         op_stop ;;
    shutdown)     op_shutdown ;;
    *)            exit 2 ;;  # Unknown operation — forward compatible.
esac
