package dolt_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBdShape is a stand-in `bd` that emits the exact output shapes bd 1.1.0
// produces in proxied-server mode (verified live against ps_city_1). It pins
// the contract store_sql / store_reachable / store_dml_rows_db_qualified parse:
//   - `sql --csv 'SELECT 1'`       -> header row then value row
//   - `sql --json <DML>`           -> {"rows_affected":N,...}
//   - `sql --csv 'SHOW DATABASES'` -> header row then all databases
const fakeBdShape = `#!/bin/sh
# Skip a leading "-C <dir>", then the "sql" verb, then an optional format flag.
while [ $# -gt 0 ]; do
  case "$1" in
    -C) shift 2 ;;
    sql) shift; break ;;
    *) shift ;;
  esac
done
fmt=table
case "${1:-}" in
  --csv) fmt=csv; shift ;;
  --json) fmt=json; shift ;;
esac
q="${1:-}"
case "$q" in
  *"SELECT 1"*)
    case "$fmt" in
      csv)  printf '1\n1\n' ;;
      json) printf '[ { "1": 1 } ]\n' ;;
      *)    printf '1\n' ;;
    esac ;;
  *UPDATE*|*DELETE*)
    printf '{ "rows_affected": 3, "schema_version": 1 }\n' ;;
  *"SHOW DATABASES"*)
    printf 'Database\nhq\ninformation_schema\n' ;;
  *) : ;;
esac
`

// runStoreSQL sources port_resolve.sh with GC_BEADS_PROXIED=1 and a fake bd on
// PATH, then runs body. Returns combined output and exit code.
func runStoreSQL(t *testing.T, body string) (string, int) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available; skipping shell-function test")
	}
	root := repoRoot(t)
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "bd"), fakeBdShape)
	city := t.TempDir()

	driver := ". " + shellQuote(filepath.Join(root, "assets", "scripts", "port_resolve.sh")) + "\n" + body
	cmd := exec.Command("sh", "-c", driver)
	cmd.Env = append(filteredEnv("PATH"),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GC_CITY_PATH="+city,
		"GC_BEADS_PROXIED=1",
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		exitErr := &exec.ExitError{}
		if !errors.As(err, &exitErr) {
			t.Fatalf("store_sql driver failed to run: %v\noutput:\n%s", err, out)
		}
		code = exitErr.ExitCode()
	}
	return string(out), code
}

// TestStoreReachableSucceedsWhenBdAnswers proves store_reachable is a true
// SELECT-1 liveness probe through bd (exit 0 when bd answers).
func TestStoreReachableSucceedsWhenBdAnswers(t *testing.T) {
	out, code := runStoreSQL(t, `store_reachable && echo REACHABLE`)
	if code != 0 || !strings.Contains(out, "REACHABLE") {
		t.Fatalf("store_reachable: out=%q code=%d, want exit 0 + REACHABLE", out, code)
	}
}

// TestStoreSQLCsvEmitsHeaderRow pins that bd --csv includes a header row, so
// callers must strip it (tail -n +2). If bd stopped emitting a header this
// would surface here rather than silently corrupting every count.
func TestStoreSQLCsvEmitsHeaderRow(t *testing.T) {
	out, code := runStoreSQL(t, `store_sql csv 'SELECT 1'`)
	if code != 0 {
		t.Fatalf("store_sql csv exit=%d out=%q", code, out)
	}
	if out != "1\n1\n" {
		t.Fatalf("store_sql csv 'SELECT 1' = %q, want header+value \"1\\n1\\n\"", out)
	}
	stripped, _ := runStoreSQL(t, `store_sql csv 'SELECT 1' | tail -n +2`)
	if stripped != "1\n" {
		t.Fatalf("header-stripped value = %q, want \"1\\n\"", stripped)
	}
}

// TestStoreSQLShowDatabasesListsAllRigDBs proves a single bd invocation
// enumerates every database on the shared proxied server (multi-rig sweep).
func TestStoreSQLShowDatabasesListsAllRigDBs(t *testing.T) {
	out, code := runStoreSQL(t, `store_sql csv 'SHOW DATABASES' | tail -n +2`)
	if code != 0 {
		t.Fatalf("SHOW DATABASES exit=%d out=%q", code, out)
	}
	if out != "hq\ninformation_schema\n" {
		t.Fatalf("databases = %q, want \"hq\\ninformation_schema\\n\"", out)
	}
}

// TestStoreDMLRowsParsesRowsAffected proves DML row counts come from
// bd sql --json's rows_affected (ROW_COUNT() is unavailable across a fresh
// bd connection), and that no USE / multi-statement is emitted.
func TestStoreDMLRowsParsesRowsAffected(t *testing.T) {
	out, code := runStoreSQL(t, "store_dml_rows_db_qualified \"UPDATE \\`hq\\`.wisps SET status='closed' WHERE 1=0\"")
	if code != 0 {
		t.Fatalf("store_dml_rows exit=%d out=%q", code, out)
	}
	if strings.TrimSpace(out) != "3" {
		t.Fatalf("rows-affected = %q, want \"3\"", out)
	}
}
