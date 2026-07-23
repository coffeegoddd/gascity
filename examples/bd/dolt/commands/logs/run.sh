#!/bin/sh
# gc dolt logs — Tail the Dolt server log file.
#
# Usage: gc dolt logs [-n LINES] [-f]
#
# Environment: GC_CITY_PATH (set by gc pack command infrastructure)
set -e

lines=50
follow=false
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

while [ $# -gt 0 ]; do
  case "$1" in
    -n|--lines) lines="$2"; shift 2 ;;
    -n*)        lines="${1#-n}"; shift ;;
    -f|--follow) follow=true; shift ;;
    -h|--help)
      echo "Usage: gc dolt logs [-n LINES] [-f]"
      echo ""
      echo "Tail the Dolt server log file."
      echo ""
      echo "Flags:"
      echo "  -n, --lines N   Number of lines to show (default: 50)"
      echo "  -f, --follow    Follow the log in real time"
      exit 0
      ;;
    *) echo "gc dolt logs: unknown flag: $1" >&2; exit 1 ;;
  esac
done

log_file="$DOLT_LOG_FILE"
host="${GC_BEADS_HOST:-127.0.0.1}"

if [ "${GC_BEADS_PROXIED:-0}" = 1 ]; then
  # bd writes the proxied Dolt server's log to the path it records in
  # proxied_server_client_info.json; prefer that over the managed-layout default.
  _ci="$GC_CITY_PATH/.beads/proxied_server_client_info.json"
  if [ -f "$_ci" ]; then
    _lp=$(sed -n 's/.*"log_path"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$_ci" | head -1)
    [ -n "$_lp" ] && log_file="$_lp"
  fi
  if [ ! -f "$log_file" ]; then
    echo "gc dolt logs: no dolt log yet (bd starts the proxied server lazily; it appears after the first access)." >&2
    exit 0
  fi
fi

if [ ! -f "$log_file" ]; then
  if ! is_local_dolt_host "$host"; then
    # Configured external Dolt endpoint: the server log lives on the remote
    # host, not in this city's local pack state. A missing local log is an
    # expected limitation of pointing at an external endpoint, not a failure —
    # do not hard-fail the way a missing managed-server log would (su-deol8).
    echo "gc dolt logs: external Dolt endpoint $host:$GC_BEADS_PORT — server logs live on the remote host and are not available locally." >&2
    exit 0
  fi
  echo "gc dolt logs: log file not found: $log_file" >&2
  exit 1
fi

args="-n${lines}"
if [ "$follow" = true ]; then
  args="$args -f"
fi

exec tail $args "$log_file"
