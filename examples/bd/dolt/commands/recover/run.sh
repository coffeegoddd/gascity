#!/bin/sh
# gc dolt recover — Check for and recover from Dolt read-only state.
#
# Dolt can enter read-only mode after certain failures. This command
# detects the condition and attempts automatic recovery by calling
# the gc-beads-bd recover operation.
#
# Environment: GC_CITY_PATH, GC_BEADS_HOST, GC_BEADS_PORT, GC_BEADS_USER,
#              GC_BEADS_PASSWORD
set -e

: "${GC_BEADS_USER:=root}"
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

beads_bd="$GC_BEADS_BD_SCRIPT"

if [ "${GC_BEADS_PROXIED:-0}" = 1 ]; then
  # bd owns the Dolt process and transparently respawns an idled or failed
  # proxy on next access. There is no gascity-managed read-only recovery to
  # perform; report liveness and exit cleanly.
  if store_reachable; then
    echo "Dolt store reachable (bd proxied-server); no recovery needed."
  else
    echo "Dolt store not reachable; bd will respawn its proxy on next access."
  fi
  exit 0
fi

# Reject remote servers — can't manage remote dolt processes. Local hosts
# include 0.0.0.0, the explicit wildcard opt-out for the managed server's
# bind (the default bind is 127.0.0.1).
if [ -n "$GC_BEADS_HOST" ]; then
  case "$GC_BEADS_HOST" in
    127.0.0.1|0.0.0.0|localhost|"::1"|"[::1]") ;; # local is fine
    *) echo "gc dolt recover: not supported for remote dolt servers" >&2; exit 1 ;;
  esac
fi

# Check read-only state by attempting a write probe.
#
# Always export DOLT_CLI_PASSWORD (even empty) so the client does not
# prompt for a password on stdin; non-TTY agent sessions would otherwise
# fail with "Failed to parse credentials: operation not supported by
# device" and the probe would falsely report writable. The write probe
# is wrapped in run_bounded so an unresponsive server — the very
# failure mode `gc dolt recover` exists to handle — cannot hang the
# script indefinitely. Mirrors the patterns established in health/run.sh.
# This table-only probe intentionally avoids DROP DATABASE; explicit
# managed probe recovery is available through `gc dolt-state reset-probe`.
check_read_only() {
  host="${GC_BEADS_HOST:-127.0.0.1}"
  args="--host $host --port $GC_BEADS_PORT --user $GC_BEADS_USER --no-tls"
  export DOLT_CLI_PASSWORD="${GC_BEADS_PASSWORD:-}"
  result=$(run_bounded 10 dolt $args sql -q "CREATE TABLE __gc_ro_check (id INT); DROP TABLE __gc_ro_check;" 2>&1) || true
  case "$result" in
    *read*only*|*read-only*|*readonly*) return 0 ;; # read-only detected
  esac
  return 1 # writable
}

if ! check_read_only; then
  echo "Dolt server is not in read-only state."
  exit 0
fi

echo "Dolt server is in read-only state. Attempting recovery..."

if [ -x "$beads_bd" ]; then
  "$beads_bd" recover || {
    echo "gc dolt recover: recovery failed" >&2
    exit 1
  }
else
  echo "gc dolt recover: gc-beads-bd script not found at $beads_bd" >&2
  exit 1
fi

echo "Recovery successful."
