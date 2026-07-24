package gastown_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// extractShellFn returns the `name() { ... }` block from a shell script source,
// matching the closing brace at column 0 (the conventional shape these scripts
// use). Mirrors cmd/gc's extractShellFunction so the core maintenance-script
// functions can be unit-tested in isolation.
func extractShellFn(t *testing.T, script, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?ms)^` + regexp.QuoteMeta(name) + `\(\)\s*\{.*?\n\}`)
	loc := re.FindStringIndex(script)
	if loc == nil {
		t.Fatalf("could not find shell function %q", name)
	}
	return script[loc[0]:loc[1]]
}

func readCoreScript(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(coreScriptPath(name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// TestReaperRunSQLChangeProxiedUsesBdRowsAffected proves run_sql_change, in
// proxied mode, gets its affected-row count from store_dml_rows_db_qualified
// (bd rows_affected) and never emits a USE / multi-statement / dolt call.
func TestReaperRunSQLChangeProxiedUsesBdRowsAffected(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	fn := extractShellFn(t, readCoreScript(t, "reaper.sh"), "run_sql_change")
	argsFile := filepath.Join(t.TempDir(), "dml.args")

	// SQL kept single-quoted in the shell so the backtick-quoted identifiers
	// are literal (no command substitution) and needs no embedded quotes.
	driver := "GC_BEADS_PROXIED=1\n" +
		"store_dml_rows_db_qualified() { printf '%s\\n' \"$1\" >> '" + argsFile + "'; printf '5\\n'; }\n" +
		"record_anomaly() { printf 'ANOMALY: %s\\n' \"$*\" >&2; }\n" +
		"sanitize_output() { cat; }\n" +
		"dolt_sql() { printf 'DOLT_SQL_CALLED %s\\n' \"$*\" >&2; exit 88; }\n" +
		fn + "\n" +
		"q='UPDATE `hq`.wisps SET status = status WHERE 1=1'\n" +
		"run_sql_change hq 'closing stale wisps' \"$q\"\n" +
		"printf 'RESULT=%s\\n' \"$SQL_CHANGE_ROWS_RESULT\"\n"

	out, err := exec.Command("bash", "-c", driver).CombinedOutput()
	if err != nil {
		t.Fatalf("run_sql_change driver failed: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "RESULT=5") {
		t.Fatalf("SQL_CHANGE_ROWS_RESULT not 5: %q", got)
	}
	if strings.Contains(got, "DOLT_SQL_CALLED") {
		t.Fatalf("run_sql_change shelled out to dolt in proxied mode: %q", got)
	}
	logged, _ := os.ReadFile(argsFile)
	if strings.Contains(string(logged), "USE ") {
		t.Fatalf("proxied DML emitted a USE statement: %q", string(logged))
	}
	if !strings.Contains(string(logged), "UPDATE `hq`.wisps") {
		t.Fatalf("proxied DML lost its qualified query: %q", string(logged))
	}
}

// TestReaperSessionPruneBatchesCascadeWithLabeledCommit proves the session-prune
// cascade and its commit run as ONE store_sql_db batch against the city
// database: batching is what produces a single labeled Dolt commit (each
// separate call is its own unit of work and auto-commits, so a commit issued
// afterwards would find nothing to commit). The database is selected by
// store_sql_db, never by a USE statement, which cannot be relied on to carry
// context across the proxied wire.
func TestReaperSessionPruneBatchesCascadeWithLabeledCommit(t *testing.T) {
	src := readCoreScript(t, "reaper.sh")
	start := strings.Index(src, `store_sql_db "$CITY_DB"`)
	if start < 0 {
		t.Fatal("session-prune store_sql_db batch not found")
	}
	end := strings.Index(src[start:], "TOTAL=$((TOTAL + BATCH_COUNT))")
	if end < 0 {
		t.Fatal("session-prune batch terminator not found")
	}
	batch := src[start : start+end]

	for _, tbl := range []string{"DELETE FROM labels", "DELETE FROM dependencies", "DELETE FROM issues"} {
		if !strings.Contains(batch, tbl) {
			t.Fatalf("session-prune batch missing %q\n%s", tbl, batch)
		}
	}
	if !strings.Contains(batch, "CALL DOLT_COMMIT") {
		t.Fatalf("session-prune batch must carry its labeled CALL DOLT_COMMIT\n%s", batch)
	}
	if !strings.Contains(batch, "session_beads_pruned=${BATCH_COUNT}") {
		t.Fatalf("session-prune commit lost its labeled message\n%s", batch)
	}
	if strings.Contains(batch, "USE ") {
		t.Fatalf("session-prune batch must select the database via store_sql_db, not USE\n%s", batch)
	}
}

// TestReaperPerDatabaseCommitUsesDatabaseContext proves the per-database summary
// commit runs through store_sql_db (which selects the database via --database /
// --use-db) rather than a USE statement, and is no longer gated off in proxied
// mode — CALL DOLT_COMMIT cannot be db.-qualified, so database context is the
// only way to target it.
func TestReaperPerDatabaseCommitUsesDatabaseContext(t *testing.T) {
	src := readCoreScript(t, "reaper.sh")
	idx := strings.Index(src, "reaper: stale_wisps=")
	if idx < 0 {
		t.Fatal("per-database summary commit not found")
	}
	// Look back far enough to cover the guard and the call itself.
	windowStart := idx - 1200
	if windowStart < 0 {
		windowStart = 0
	}
	window := src[windowStart : idx+200]

	if !strings.Contains(window, `store_sql_db "$DB"`) {
		t.Fatalf("summary commit does not route through store_sql_db\n%s", window)
	}
	if strings.Contains(window, `GC_BEADS_PROXIED:-0}" != 1`) {
		t.Fatalf("summary commit is still gated off in proxied mode\n%s", window)
	}
	if strings.Contains(window, "USE \\`$DB\\`") {
		t.Fatalf("summary commit still uses a USE statement\n%s", window)
	}
}

// TestJsonlExportEnvelopeWrapsBareArray proves export_json_rows wraps bd's
// bare-array JSON into the {"rows":[...]} envelope the downstream jq consumers
// require, in proxied mode.
func TestJsonlExportEnvelopeWrapsBareArray(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available")
	}
	fn := extractShellFn(t, readCoreScript(t, "jsonl-export.sh"), "export_json_rows")
	outFile := filepath.Join(t.TempDir(), "issues.jsonl")

	driver := "GC_BEADS_PROXIED=1\n" +
		"dolt_sql() { printf '[{\"id\":\"a\"},{\"id\":\"b\"}]\\n'; }\n" +
		fn + "\n" +
		"export_json_rows 'SELECT 1' '" + outFile + "'\n"

	if out, err := exec.Command("bash", "-c", driver).CombinedOutput(); err != nil {
		t.Fatalf("export_json_rows driver failed: %v\n%s", err, out)
	}
	body, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	count := exec.Command("jq", "-r", ".rows | length")
	count.Stdin = strings.NewReader(string(body))
	cout, err := count.Output()
	if err != nil {
		t.Fatalf("jq on export failed (not a {rows:[]} envelope?): %v\nbody=%s", err, body)
	}
	if strings.TrimSpace(string(cout)) != "2" {
		t.Fatalf(".rows length = %q, want 2\nbody=%s", strings.TrimSpace(string(cout)), body)
	}
}
