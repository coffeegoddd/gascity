#!/bin/sh
# port_resolve.sh — shared GC_BEADS_PORT discovery helper.
#
# Sourced by:
#   .gc/system/packs/dolt/assets/scripts/runtime.sh
#   .gc/system/packs/core/assets/scripts/dolt-target.sh
#   .gc/system/packs/bd/dolt/assets/scripts/runtime.sh
#
# Defines:
#   resolve_dolt_port_or_die  state_file  [provider_state_file]  data_dir  city_path
#
# Preconditions (caller responsibilities):
#   - managed_runtime_port is in scope (either from a sourced runtime.sh
#     above this file in the source chain, or inlined in the caller, as
#     dolt-target.sh currently inlines it).
#   - GC_CITY_PATH is set; the caller enforces this at file head.
#
# POSIX /bin/sh only. No bash-isms.

# resolve_dolt_port_or_die — print the discovered Dolt port to stdout, or
# exit 78 with a structured stderr error if no port can be resolved.
#
# Arguments:
#   $1  state_file           Absolute path to .gc/runtime/packs/dolt/dolt-state.json
#                            (or the equivalent for the caller's pack layout).
#   $2  provider_state_file  Optional provider fallback state file.
#   $3  data_dir             Absolute path the caller expects the running server
#                            to be serving from.
#   $4  city_path            Absolute path of the city root, used only in the
#                            error message so the operator knows which city failed.
#
# The legacy three-argument form remains supported:
#   resolve_dolt_port_or_die  state_file  data_dir  city_path
#
# Behavior:
#   1. It calls managed_runtime_port "$state_file" "$data_dir" and, on a
#      non-empty result, echoes the port and returns 0.
#   2. Otherwise, if provider_state_file was provided and GC_BEADS_STATE_FILE
#      did not force a specific state file, it tries the provider state.
#   3. Otherwise, if GC_BEADS_PORT is non-empty in the caller's environment,
#      the function echoes that value and returns 0 as an operator seed.
#   4. Otherwise, it writes the §3 error template to stderr and exits 78.
#
# Preconditions (caller must arrange):
#   - managed_runtime_port is in scope (sourced from runtime.sh, or inlined
#     in dolt-target.sh per its existing layout).
#   - GC_CITY_PATH is set (the helper does NOT default it; callers already
#     enforce ': "${GC_CITY_PATH:?...}"' at file head).
#
# Output contract:
#   - On success: exactly one line on stdout (the port, e.g. "47823").
#                  Nothing on stderr. Exit 0.
#   - On failure: nothing on stdout. Multi-line structured error on stderr
#                  (§3 verbatim). Exit 78 (whole shell exits, not return).
#
# In bd proxied-server mode (GC_BEADS_PROXIED=1) there is no port to resolve —
# bd owns the endpoint and exposes none — so the function returns 0 with no
# output and callers route SQL through store_sql (below) instead.
resolve_dolt_port_or_die() {
    if [ "${GC_BEADS_PROXIED:-0}" = 1 ]; then
        return 0
    fi
    _rdp_state_file="$1"
    _rdp_provider_state_file=""
    if [ "$#" -ge 4 ]; then
        _rdp_provider_state_file="$2"
        _rdp_data_dir="$3"
        _rdp_city_path="$4"
    else
        _rdp_data_dir="$2"
        _rdp_city_path="$3"
    fi

    _rdp_resolved=$(managed_runtime_port "$_rdp_state_file" "$_rdp_data_dir" 2>/dev/null)
    if [ -n "$_rdp_resolved" ]; then
        printf '%s\n' "$_rdp_resolved"
        return 0
    fi

    _rdp_consulted="GC_BEADS_PORT (unset), GC_BEADS_STATE_FILE"
    if [ -n "$_rdp_provider_state_file" ] && [ -z "${GC_BEADS_STATE_FILE:-}" ]; then
        _rdp_consulted="$_rdp_consulted, dolt-provider-state.json"
        _rdp_resolved=$(managed_runtime_port "$_rdp_provider_state_file" "$_rdp_data_dir" 2>/dev/null)
        if [ -n "$_rdp_resolved" ]; then
            printf '%s\n' "$_rdp_resolved"
            return 0
        fi
    fi

    if [ -n "${GC_BEADS_PORT:-}" ]; then
        printf '%s\n' "$GC_BEADS_PORT"
        return 0
    fi

    _rdp_state_status="missing"
    if [ -f "$_rdp_state_file" ]; then
        _rdp_state_status="present but not running"
    fi

    printf 'gc dolt: cannot resolve runtime port\n' >&2
    printf '  state_file: %s (%s)\n' "$_rdp_state_file" "$_rdp_state_status" >&2
    printf '  city_path:  %s\n' "$_rdp_city_path" >&2
    printf '  consulted:  %s\n' "$_rdp_consulted" >&2
    printf '  remediation: run `gc start` to bring up the city, or set\n' >&2
    printf '               GC_BEADS_PORT explicitly to an already-running\n' >&2
    printf '               server.\n' >&2
    exit 78
}

# ---------------------------------------------------------------------------
# bd proxied-server routing
#
# Under bd proxied-server mode bd owns the Dolt endpoint (port selection,
# spawn, idle-shutdown) and exposes no host/port. gascity therefore reaches
# the store only through the `bd` CLI, never a direct `dolt --host --port`
# connection. The helpers below are the shell-layer equivalent of the Go
# BdStore.QueryJSON path; they mirror gc-beads-bd.sh's run_bd_pinned idiom.
# ---------------------------------------------------------------------------

# beads_dolt_mode — echo the current city's dolt_mode ("proxied-server",
# "server", ...) or nothing. Cheapest source first: .beads/metadata.json (no
# subprocess); falls back to `bd context --json`. Requires GC_CITY_PATH.
beads_dolt_mode() {
    [ -n "${GC_CITY_PATH:-}" ] || return 1
    _bdm_meta="$GC_CITY_PATH/.beads/metadata.json"
    if [ -f "$_bdm_meta" ]; then
        _bdm=$(sed -n 's/.*"dolt_mode"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$_bdm_meta" 2>/dev/null | head -1)
        if [ -n "$_bdm" ]; then
            printf '%s\n' "$_bdm"
            return 0
        fi
    fi
    command -v bd >/dev/null 2>&1 || return 1
    bd -C "$GC_CITY_PATH" context --json 2>/dev/null \
        | sed -n 's/.*"dolt_mode"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1
}

# GC_BEADS_PROXIED is 1 when this city's store is in bd proxied-server mode,
# else 0. Detected once at source time; a caller may pre-set it (e.g. tests).
if [ -z "${GC_BEADS_PROXIED:-}" ]; then
    case "$(beads_dolt_mode 2>/dev/null)" in
        proxied-server) GC_BEADS_PROXIED=1 ;;
        *)              GC_BEADS_PROXIED=0 ;;
    esac
fi

# _store_bounded TIMEOUT CMD... — run CMD with a wall-clock bound. Prefers the
# run_bounded helper from runtime.sh when present (pack commands source it),
# falls back to timeout/gtimeout, then unbounded (dolt-target.sh has neither).
_store_bounded() {
    _sb_t="$1"; shift
    if command -v run_bounded >/dev/null 2>&1; then
        run_bounded "$_sb_t" "$@"
    elif command -v timeout >/dev/null 2>&1; then
        timeout "$_sb_t" "$@"
    elif command -v gtimeout >/dev/null 2>&1; then
        gtimeout "$_sb_t" "$@"
    else
        "$@"
    fi
}

# store_sql FORMAT QUERY — run one SQL statement against the store.
# FORMAT is csv|json|table. Proxied mode routes through `bd sql`; otherwise a
# direct dolt client (unchanged legacy behavior).
#
# bd proxied constraints the caller MUST respect: a single, fully
# `db.table`-qualified statement — NO `USE`, NO `;`-joined statements (bd
# surfaces only the first) — and csv/json output carries a header row.
store_sql() {
    _ss_fmt="$1"; _ss_q="$2"; _ss_t="${STORE_SQL_TIMEOUT:-5}"
    if [ "${GC_BEADS_PROXIED:-0}" = 1 ]; then
        case "$_ss_fmt" in
            csv)  _store_bounded "$_ss_t" bd -C "$GC_CITY_PATH" sql --csv  "$_ss_q" ;;
            json) _store_bounded "$_ss_t" bd -C "$GC_CITY_PATH" sql --json "$_ss_q" ;;
            *)    _store_bounded "$_ss_t" bd -C "$GC_CITY_PATH" sql        "$_ss_q" ;;
        esac
    else
        export DOLT_CLI_PASSWORD="${GC_BEADS_PASSWORD:-}"
        _ss_host="${GC_BEADS_HOST:-127.0.0.1}"
        _ss_user="${GC_BEADS_USER:-root}"
        case "$_ss_fmt" in
            csv)  _store_bounded "$_ss_t" dolt --host "$_ss_host" --port "$GC_BEADS_PORT" --user "$_ss_user" --no-tls sql --result-format csv  -q "$_ss_q" ;;
            json) _store_bounded "$_ss_t" dolt --host "$_ss_host" --port "$GC_BEADS_PORT" --user "$_ss_user" --no-tls sql --result-format json -q "$_ss_q" ;;
            *)    _store_bounded "$_ss_t" dolt --host "$_ss_host" --port "$GC_BEADS_PORT" --user "$_ss_user" --no-tls sql -q "$_ss_q" ;;
        esac
    fi
}

# store_reachable — store liveness. In proxied mode this SELECT 1 also respawns
# an idled proxy. Exit 0 when reachable, non-zero otherwise.
store_reachable() {
    store_sql csv 'SELECT 1' >/dev/null 2>&1
}

# store_dml_rows_db_qualified QUERY — run ONE fully db.table-qualified DML
# statement and echo the affected-row count. Proxied: parse rows_affected from
# `bd sql --json` (ROW_COUNT() is not recoverable across a fresh connection).
# Legacy: append a trailing SELECT ROW_COUNT() (dolt returns it) and read it.
store_dml_rows_db_qualified() {
    _sd_q="$1"
    if [ "${GC_BEADS_PROXIED:-0}" = 1 ]; then
        store_sql json "$_sd_q" \
            | sed -n 's/.*"rows_affected"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' \
            | head -1
    else
        store_sql csv "$_sd_q; SELECT ROW_COUNT() AS gc_rowcount" \
            | grep -E '^[0-9]+$' | tail -1
    fi
}
