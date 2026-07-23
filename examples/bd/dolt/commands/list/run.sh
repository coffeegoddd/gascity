#!/bin/sh
# gc dolt list — List Dolt databases with their filesystem paths.
#
# Shows databases for the HQ (city) and all configured rigs.
#
# Environment: GC_CITY_PATH
set -e

PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"
data_dir="$DOLT_DATA_DIR"

if [ "${GC_BEADS_PROXIED:-0}" = 1 ]; then
  # bd owns the data dir; enumerate databases from the server catalog instead
  # of scanning on-disk .dolt directories.
  dbs=$(store_sql csv 'SHOW DATABASES' | tail -n +2 | while IFS= read -r name; do
    name=$(printf '%s' "$name" | tr -d '\r')
    [ -n "$name" ] || continue
    case "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" in
      information_schema|mysql|dolt|dolt_cluster|performance_schema|sys|__gc_probe) continue ;;
    esac
    printf '%s\t(bd proxied-server)\n' "$name"
  done)
  if [ -n "$dbs" ]; then
    printf '%s\n' "$dbs"
  else
    echo "No databases found."
  fi
  exit 0
fi

if [ ! -d "$data_dir" ]; then
  echo "No databases found."
  exit 0
fi

found=0
for d in "$data_dir"/*/; do
  [ ! -d "$d/.dolt" ] && continue
  name="$(basename "$d")"
  # Skip system databases.
  case "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" in
    information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe) continue ;;
  esac
  printf "%s\t%s\n" "$name" "$d"
  found=$((found + 1))
done

if [ "$found" -eq 0 ]; then
  echo "No databases found."
fi
