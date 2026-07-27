//go:build acceptance_bd_contract

// Beads `bd sql` contract acceptance test — proxied-server mode.
//
// In proxied-server mode bd is the sole interface to the Dolt store: it owns
// the port, the process lifecycle, and the credentials, and it deliberately
// publishes no endpoint. Gas City's shell layer therefore reaches the store
// only by shelling out to `bd sql` and parsing its output. That parsing lives
// in examples/bd/dolt/assets/scripts/port_resolve.sh (store_sql, store_sql_db,
// store_reachable, store_dml_rows_db_qualified) and is consumed by the
// gc dolt command family plus the core maintenance scripts.
//
// Those parsers depend on emergent properties of `bd sql` output — a header
// row, a bare JSON array, a rows_affected object, unconditional auto-commit,
// first-result-only batches — none of which is published API. The unit tests
// for the shell layer stub `bd` on PATH, so they assert those shapes against a
// fake and cannot catch bd changing them. This test is the contract firewall
// for that boundary: it runs the real bd against a real proxied store.
//
// Every assertion here was established by observing live bd behavior, not by
// reading bd source. When one fails, the shell layer is broken in production
// and the fix belongs in port_resolve.sh (or upstream in bd) — never in this
// file's expectations.
package acceptance_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// proxyTeardownTimeout bounds the wait for bd to tear down the proxy and its
// dolt backend after `bd dolt stop`.
const proxyTeardownTimeout = 30 * time.Second

// bdSupportsProxiedServer reports whether the bd on PATH can run
// proxied-server mode, probed the same way gc start and gc doctor probe it
// (cmd/gc/cmd_doctor.go bdProxiedServerCapability).
func bdSupportsProxiedServer(t *testing.T, bdPath string) bool {
	t.Helper()
	out, err := exec.Command(bdPath, "init", "--help").CombinedOutput()
	if err != nil {
		// --help should never fail; treat a broken probe as unsupported
		// rather than asserting against an unknown bd.
		t.Logf("bd init --help failed, treating proxied-server as unsupported: %v\n%s", err, out)
		return false
	}
	return strings.Contains(string(out), "--proxied-server")
}

// initProxiedBeadsDir creates a temp directory holding a proxied-server beads
// store and registers teardown of the proxy topology bd spawns for it.
//
// Teardown routes through `bd dolt stop`, which calls beads' own
// proxy.Shutdown: it advances the store's stop epoch before signalling, so a
// start attempt already in flight aborts terminally instead of racing to
// respawn a child after the test ends. A bare kill would leave that race open
// and leak a proxy plus a dolt sql-server per test.
func initProxiedBeadsDir(t *testing.T) string {
	t.Helper()
	bdPath := helpers.RequireBD(t)
	if !bdSupportsProxiedServer(t, bdPath) {
		t.Skip("bd on PATH does not support --proxied-server")
	}
	dir := t.TempDir()
	requireBD(t, dir, "init", "-p", "cs", "--proxied-server", "--skip-hooks", "-q")
	t.Cleanup(func() {
		// Best-effort: the store is in a temp dir either way, but a surviving
		// proxy would hold a dolt sql-server against a deleted data directory.
		if out, err := runBD(t, dir, "dolt", "stop"); err != nil {
			t.Logf("bd dolt stop during cleanup: %v\n%s", err, out)
		}
	})
	return dir
}

// sqlCSV runs a query through `bd sql --csv` and returns its raw lines with
// trailing blanks dropped. Line 0 is the header row.
func sqlCSV(t *testing.T, dir, query string, args ...string) []string {
	t.Helper()
	full := append([]string{"sql"}, args...)
	full = append(full, "--csv", query)
	out := requireBD(t, dir, full...)
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// scalarCSV returns the single data value of a one-column, one-row query,
// asserting the header row is present and consumed.
func scalarCSV(t *testing.T, dir, query string, args ...string) string {
	t.Helper()
	lines := sqlCSV(t, dir, query, args...)
	if len(lines) != 2 {
		t.Fatalf("query %q: got %d non-empty lines, want 2 (header + value): %q", query, len(lines), lines)
	}
	return strings.Trim(lines[1], `"`)
}

// TestBdSQLContract pins the `bd sql` output shapes and commit semantics that
// Gas City's shell layer parses. Subtests share one store and run in order.
func TestBdSQLContract(t *testing.T) {
	dir := initProxiedBeadsDir(t)

	// store_sql/store_sql_db callers all strip the first line of --csv output.
	// A headerless bd would make them silently drop their first data row.
	t.Run("CSVEmitsHeaderRow", func(t *testing.T) {
		lines := sqlCSV(t, dir, "SELECT 1 AS one, 2 AS two")
		if len(lines) != 2 {
			t.Fatalf("got %d lines, want 2: %q", len(lines), lines)
		}
		if lines[0] != "one,two" {
			t.Errorf("header row = %q, want %q", lines[0], "one,two")
		}
		if lines[1] != "1,2" {
			t.Errorf("data row = %q, want %q", lines[1], "1,2")
		}
	})

	// jsonl-export.sh wraps bd's output with jq '{rows: .}' precisely because
	// bd emits a bare array. If bd ever wrapped it itself, the export would
	// nest as {"rows":{"rows":[...]}} and every consumer would misparse.
	t.Run("JSONReturnsBareArray", func(t *testing.T) {
		out := requireBD(t, dir, "sql", "--json", "SELECT 1 AS one, 2 AS two")
		trimmed := strings.TrimSpace(out)
		if !strings.HasPrefix(trimmed, "[") {
			t.Fatalf("--json output does not start with '[': %q", trimmed)
		}
		var rows []map[string]any
		if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
			t.Fatalf("--json output is not a bare array: %v\nraw: %s", err, trimmed)
		}
		if len(rows) != 1 {
			t.Fatalf("got %d rows, want 1: %v", len(rows), rows)
		}
		if got := rows[0]["one"]; got != float64(1) {
			t.Errorf("rows[0][\"one\"] = %v (%T), want 1", got, got)
		}
	})

	// store_dml_rows_db_qualified seds rows_affected out of this object; it is
	// the only way to recover a row count in proxied mode, because ROW_COUNT()
	// does not survive to a second bd sql call's fresh connection.
	t.Run("DMLJSONReportsRowsAffected", func(t *testing.T) {
		requireBD(t, dir, "sql", "CREATE TABLE affected (id INT PRIMARY KEY, v INT)")
		out := requireBD(t, dir, "sql", "--json", "INSERT INTO affected VALUES (1,10),(2,20)")
		var res struct {
			RowsAffected *int `json:"rows_affected"`
		}
		idx := strings.Index(out, "{")
		if idx < 0 {
			t.Fatalf("DML --json output has no JSON object: %q", out)
		}
		if err := json.Unmarshal([]byte(out[idx:]), &res); err != nil {
			t.Fatalf("parsing DML --json: %v\nraw: %s", err, out)
		}
		if res.RowsAffected == nil {
			t.Fatalf("DML --json has no rows_affected field: %q", out)
		}
		if *res.RowsAffected != 2 {
			t.Errorf("rows_affected = %d, want 2", *res.RowsAffected)
		}
	})

	// Each bd sql call is its own unit of work and commits, even when asked
	// not to. reaper.sh relies on this: a CALL DOLT_COMMIT issued in a LATER
	// call finds nothing to commit, so a labeled commit must batch the DML and
	// the CALL together.
	t.Run("EverySQLCallAutoCommitsEvenWithAutoCommitOff", func(t *testing.T) {
		requireBD(t, dir, "sql", "--dolt-auto-commit", "off",
			"INSERT INTO affected VALUES (3,30)")
		msg := scalarCSV(t, dir, "SELECT message FROM dolt_log ORDER BY date DESC LIMIT 1")
		if !strings.HasPrefix(msg, "bd sql:") {
			t.Errorf("top commit message = %q, want a generated \"bd sql:\" commit", msg)
		}
		clean := scalarCSV(t, dir, "SELECT COUNT(*) AS n FROM dolt_status")
		if clean != "0" {
			t.Errorf("dolt_status rows = %s, want 0 (the write should be committed, not staged)", clean)
		}
	})

	// A ;-joined batch runs every statement but surfaces only the first
	// statement's result. This is why reads must be single statements: a
	// trailing SELECT in a batch is executed and discarded.
	t.Run("BatchExecutesAllStatementsButSurfacesOnlyFirstResult", func(t *testing.T) {
		out := requireBD(t, dir, "sql", "--csv",
			"INSERT INTO affected VALUES (4,40); INSERT INTO affected VALUES (5,50); SELECT 'SENTINEL' AS marker")
		if strings.Contains(out, "SENTINEL") {
			t.Errorf("batch surfaced a trailing SELECT's result; reads-in-batches may now be safe: %q", out)
		}
		n := scalarCSV(t, dir, "SELECT COUNT(*) AS n FROM affected WHERE id IN (4,5)")
		if n != "2" {
			t.Errorf("rows from later batch statements = %s, want 2 (all statements must execute)", n)
		}
	})

	// store_sql_db exists for this: committing a write made in a database
	// other than the session's active one requires --database. Qualifying the
	// procedure call does NOT redirect it — CALL other.DOLT_COMMIT still
	// operates on the active database and reports "nothing to commit".
	t.Run("CrossDatabaseCommitRequiresDatabaseFlag", func(t *testing.T) {
		requireBD(t, dir, "sql", "CREATE DATABASE other")
		requireBD(t, dir, "sql", "CREATE TABLE `other`.`u` (id INT PRIMARY KEY)")

		for _, tc := range []struct {
			name  string
			batch string
		}{
			{"unqualified call", "INSERT INTO `other`.`u` VALUES (1); CALL DOLT_COMMIT('-Am','unqualified')"},
			{"qualified call", "INSERT INTO `other`.`u` VALUES (2); CALL other.DOLT_COMMIT('-Am','qualified')"},
		} {
			out, err := runBD(t, dir, "sql", "--csv", tc.batch)
			if err == nil {
				t.Errorf("%s: batch succeeded; --database may no longer be required to commit cross-database: %q", tc.name, out)
				continue
			}
			if !strings.Contains(out, "nothing to commit") {
				t.Errorf("%s: error = %q, want it to mention \"nothing to commit\"", tc.name, out)
			}
		}

		// A failed CALL DOLT_COMMIT does NOT roll back the batch's DML: the
		// rows stay in the working set, uncommitted and invisible in
		// dolt_log. Callers that ignore a batch's exit status therefore
		// silently produce data-without-a-commit.
		staged := scalarCSV(t, dir, "SELECT COUNT(*) AS n FROM dolt_status", "--database", "other")
		if staged == "0" {
			t.Errorf("dolt_status in other = 0; the failed batches' DML appears to have rolled back, which callers do not expect")
		}
		if n := scalarCSV(t, dir, "SELECT COUNT(*) AS n FROM `other`.`u`"); n != "2" {
			t.Errorf("rows in other.u = %s, want 2 (failed commit must not roll back the DML)", n)
		}

		// --database selects the active database, so the CALL lands there.
		requireBD(t, dir, "sql", "--database", "other", "--csv",
			"INSERT INTO u VALUES (3); CALL DOLT_COMMIT('-Am','labeled by contract test')")
		msg := scalarCSV(t, dir, "SELECT message FROM `other`.dolt_log ORDER BY date DESC LIMIT 1")
		if msg != "labeled by contract test" {
			t.Errorf("top commit in other = %q, want the label supplied via --database", msg)
		}
	})

	// store_reachable's liveness probe. In proxied mode this same SELECT also
	// respawns an idled proxy, so it doubles as the revival path.
	t.Run("SelectOneIsAValidLivenessProbe", func(t *testing.T) {
		if got := scalarCSV(t, dir, "SELECT 1"); got != "1" {
			t.Errorf("SELECT 1 returned %q, want \"1\"", got)
		}
	})
}

// TestBdDoltStopTearsDownProxyTopology asserts the teardown primitive the
// suite's own cleanup depends on actually removes both processes. `bd dolt
// stop` exiting 0 is not proof the tree is down; a leaked proxy holds a dolt
// sql-server against the store for the rest of the run.
func TestBdDoltStopTearsDownProxyTopology(t *testing.T) {
	dir := initProxiedBeadsDir(t)
	root := filepath.Join(dir, ".beads", "dolt")

	// Force the topology up before measuring it.
	requireBD(t, dir, "sql", "--csv", "SELECT 1")
	before := pidsMatching(t, root)
	if len(before) == 0 {
		t.Skipf("no process found advertising %s; cannot observe teardown on this platform", root)
	}

	requireBD(t, dir, "dolt", "stop")

	deadline := time.Now().Add(proxyTeardownTimeout)
	for {
		alive := pidsMatching(t, root)
		if len(alive) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %s, %d process(es) still hold %s: %v (started with %v)",
				proxyTeardownTimeout, len(alive), root, alive, before)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// pidsMatching returns the PIDs of live processes whose command line mentions
// root. It shells to pgrep rather than reading bd's pid files so the assertion
// does not depend on bd-internal filenames. Returns nil when pgrep is
// unavailable or matches nothing.
func pidsMatching(t *testing.T, root string) []int {
	t.Helper()
	pgrep, err := exec.LookPath("pgrep")
	if err != nil {
		return nil
	}
	out, err := exec.Command(pgrep, "-f", root).Output()
	if err != nil {
		// pgrep exits 1 with no output when nothing matches.
		return nil
	}
	self := os.Getpid()
	var pids []int
	for _, line := range strings.Fields(string(out)) {
		pid, convErr := strconv.Atoi(line)
		if convErr != nil || pid == self {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}
