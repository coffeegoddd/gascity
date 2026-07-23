#!/bin/sh
# gc dolt sql — Open a Dolt SQL shell or run a one-shot query.
#
# Connects to the running Dolt server if available, otherwise opens
# in embedded mode using the first database directory found. Trailing
# arguments are forwarded verbatim to `dolt sql`, so non-interactive
# use is supported via `gc dolt sql -q "QUERY"`.
#
# Environment: GC_CITY_PATH, GC_BEADS_HOST, GC_BEADS_PORT, GC_BEADS_USER,
#              GC_BEADS_PASSWORD (all optional except GC_CITY_PATH)
set -e

: "${GC_BEADS_USER:=root}"
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"
data_dir="$DOLT_DATA_DIR"

if [ "${GC_BEADS_PROXIED:-0}" = 1 ]; then
  # bd owns the endpoint; route through `bd sql`. bd sql is one-shot (no
  # interactive shell) and takes a positional query, not dolt's
  # -q/--result-format flags, so translate the common non-interactive forms
  # and forward --csv/--json through.
  fmt=""
  query=""
  while [ $# -gt 0 ]; do
    case "$1" in
      -q|--query)          query="$2"; shift 2 ;;
      --result-format)     case "$2" in csv) fmt=--csv ;; json) fmt=--json ;; esac; shift 2 ;;
      --result-format=csv) fmt=--csv; shift ;;
      --result-format=json) fmt=--json; shift ;;
      -r)                  case "$2" in csv) fmt=--csv ;; json) fmt=--json ;; esac; shift 2 ;;
      --csv|--json)        fmt="$1"; shift ;;
      *)                   [ -z "$query" ] && query="$1"; shift ;;
    esac
  done
  if [ -z "$query" ]; then
    echo "gc dolt sql: an interactive shell is unavailable in bd proxied-server mode; pass a query (gc dolt sql -q 'SELECT ...')" >&2
    exit 1
  fi
  if [ -n "$fmt" ]; then
    exec bd -C "$GC_CITY_PATH" sql "$fmt" "$query"
  fi
  exec bd -C "$GC_CITY_PATH" sql "$query"
fi

# Check if the server is reachable.
is_running() {
  if [ -n "$GC_BEADS_HOST" ]; then
    # Remote server — TCP probe.
    (echo > /dev/tcp/"$GC_BEADS_HOST"/"$GC_BEADS_PORT") 2>/dev/null && return 0
    # Fallback: nc/ncat.
    command -v nc >/dev/null 2>&1 && nc -z "$GC_BEADS_HOST" "$GC_BEADS_PORT" 2>/dev/null && return 0
    return 1
  fi
  managed_runtime_tcp_reachable "$GC_BEADS_PORT"
}

if is_running; then
  # Build connection args.
  args=""
  if [ -n "$GC_BEADS_HOST" ]; then
    host="$GC_BEADS_HOST"
  else
    host="127.0.0.1"
  fi
  args="--host $host --port $GC_BEADS_PORT --user $GC_BEADS_USER --no-tls"
  # Always export DOLT_CLI_PASSWORD so dolt's credential parser skips
  # the TTY password prompt. When GC_BEADS_PASSWORD is empty (the
  # managed-local default — root has no password), an unset env var
  # causes `dolt sql -q "..."` to fail with "inappropriate ioctl for
  # device" under non-interactive callers (CI, scripts, automation).
  # Exporting empty satisfies dolt without changing auth outcomes.
  export DOLT_CLI_PASSWORD="${GC_BEADS_PASSWORD:-}"
  exec dolt $args sql "$@"
else
  # Embedded mode — find first database directory.
  if [ ! -d "$data_dir" ]; then
    echo "gc dolt sql: no dolt server running and no databases found" >&2
    exit 1
  fi
  first_db=""
  for d in "$data_dir"/*/; do
    [ -d "$d/.dolt" ] && first_db="$d" && break
  done
  if [ -z "$first_db" ]; then
    echo "gc dolt sql: no dolt server running and no databases found" >&2
    exit 1
  fi
  exec dolt --data-dir "$data_dir" sql "$@"
fi
