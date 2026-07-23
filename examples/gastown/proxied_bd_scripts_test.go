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

// TestReaperSessionPruneProxiedSplitsCascadeNoCommit proves the proxied
// session-prune branch issues three separate db.table-qualified DELETEs and no
// CALL DOLT_COMMIT (bd owns committing history; bd sql runs one statement/call).
func TestReaperSessionPruneProxiedSplitsCascadeNoCommit(t *testing.T) {
	src := readCoreScript(t, "reaper.sh")
	marker := "# the bd CLI SQL runs one statement per call and has no session"
	start := strings.Index(src, marker)
	if start < 0 {
		t.Fatal("proxied session-prune branch marker not found")
	}
	elseIdx := strings.Index(src[start:], "\n                    else")
	if elseIdx < 0 {
		t.Fatal("proxied branch terminator not found")
	}
	branch := src[start : start+elseIdx]

	// Three separate qualified DELETEs against the CITY_DB, one per table.
	for _, tbl := range []string{".labels", ".dependencies", ".issues"} {
		want := "${CITY_DB}"
		if !strings.Contains(branch, want) || !strings.Contains(branch, "DELETE FROM") || !strings.Contains(branch, tbl) {
			t.Fatalf("proxied session-prune branch missing qualified DELETE for %q\n%s", tbl, branch)
		}
	}
	if n := strings.Count(branch, "DELETE FROM"); n != 3 {
		t.Fatalf("proxied session-prune branch has %d DELETEs, want 3 (split cascade)\n%s", n, branch)
	}
	if strings.Contains(branch, "CALL DOLT_COMMIT") {
		t.Fatalf("proxied session-prune branch must not CALL DOLT_COMMIT\n%s", branch)
	}
	if strings.Contains(branch, "USE ") {
		t.Fatalf("proxied session-prune branch must not USE a database\n%s", branch)
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
