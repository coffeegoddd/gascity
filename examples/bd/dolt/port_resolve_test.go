package dolt_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestPortResolveOrDieEnvOverride(t *testing.T) {
	result := runPortResolveOrDie(t, portResolveCase{
		stateFile: filepath.Join(t.TempDir(), "missing-state.json"),
		dataDir:   filepath.Join(t.TempDir(), "data"),
		cityPath:  t.TempDir(),
		env:       []string{"GC_DOLT_PORT=4242"},
	})

	assertPortResolveResult(t, result, 0, "4242\n", "")
}

func TestPortResolveOrDieDiscoverySuccess(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "dolt-state.json")
	dataDir := filepath.Join(t.TempDir(), "d")
	cityPath := t.TempDir()
	if err := os.WriteFile(stateFile, []byte(fmt.Sprintf(
		`{"running":true,"pid":%d,"port":47823,"data_dir":%q}`,
		os.Getpid(),
		dataDir,
	)), 0o644); err != nil {
		t.Fatalf("write state fixture: %v", err)
	}

	result := runPortResolveOrDie(t, portResolveCase{
		stateFile:   stateFile,
		dataDir:     dataDir,
		cityPath:    cityPath,
		managedPort: "47823",
	})

	assertPortResolveResult(t, result, 0, "47823\n", "")
}

func TestPortResolveOrDieProviderDiscoverySuccess(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "dolt-state.json")
	providerStateFile := filepath.Join(t.TempDir(), "dolt-provider-state.json")
	dataDir := filepath.Join(t.TempDir(), "d")
	cityPath := t.TempDir()
	if err := os.WriteFile(stateFile, []byte(`{"running":false}`), 0o644); err != nil {
		t.Fatalf("write state fixture: %v", err)
	}
	if err := os.WriteFile(providerStateFile, []byte(fmt.Sprintf(
		`{"running":true,"pid":%d,"port":47824,"data_dir":%q}`,
		os.Getpid(),
		dataDir,
	)), 0o644); err != nil {
		t.Fatalf("write provider state fixture: %v", err)
	}

	result := runPortResolveOrDie(t, portResolveCase{
		stateFile:           stateFile,
		providerStateFile:   providerStateFile,
		dataDir:             dataDir,
		cityPath:            cityPath,
		providerManagedPort: "47824",
	})

	assertPortResolveResult(t, result, 0, "47824\n", "")
}

func TestPortResolveOrDieExplicitStateSkipsProviderFallback(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "dolt-state.json")
	providerStateFile := filepath.Join(t.TempDir(), "dolt-provider-state.json")
	cityPath := t.TempDir()

	result := runPortResolveOrDie(t, portResolveCase{
		stateFile:           stateFile,
		providerStateFile:   providerStateFile,
		dataDir:             filepath.Join(t.TempDir(), "data"),
		cityPath:            cityPath,
		providerManagedPort: "47824",
		env:                 []string{"GC_DOLT_STATE_FILE=" + stateFile},
	})

	assertPortResolveResult(t, result, 78, "", expectedPortResolveError(stateFile, cityPath, "missing"))
}

func TestPortResolveOrDieMissingState(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "dolt-state.json")
	cityPath := t.TempDir()

	result := runPortResolveOrDie(t, portResolveCase{
		stateFile: stateFile,
		dataDir:   filepath.Join(t.TempDir(), "data"),
		cityPath:  cityPath,
	})

	assertPortResolveResult(t, result, 78, "", expectedPortResolveError(stateFile, cityPath, "missing"))
}

func TestPortResolveOrDieStatePresentNotRunning(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "dolt-state.json")
	cityPath := t.TempDir()
	if err := os.WriteFile(stateFile, []byte(`{"running":false}`), 0o644); err != nil {
		t.Fatalf("write state fixture: %v", err)
	}

	result := runPortResolveOrDie(t, portResolveCase{
		stateFile: stateFile,
		dataDir:   filepath.Join(t.TempDir(), "data"),
		cityPath:  cityPath,
	})

	assertPortResolveResult(t, result, 78, "", expectedPortResolveError(stateFile, cityPath, "present but not running"))
}

func TestPortResolveOrDieExit78OnEmptyState(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "dolt-state.json")
	dataDir := filepath.Join(t.TempDir(), "d")
	cityPath := t.TempDir()
	if err := os.WriteFile(stateFile, []byte(fmt.Sprintf(
		`{"running":true,"pid":99999999,"port":47823,"data_dir":%q}`,
		dataDir,
	)), 0o644); err != nil {
		t.Fatalf("write state fixture: %v", err)
	}

	result := runPortResolveOrDie(t, portResolveCase{
		stateFile: stateFile,
		dataDir:   dataDir,
		cityPath:  cityPath,
	})

	assertPortResolveResult(t, result, 78, "", expectedPortResolveError(stateFile, cityPath, "present but not running"))
}

func TestRuntimeShUsesPortResolve(t *testing.T) {
	root := repoRoot(t)
	assertScriptSourcesPortResolveOnce(t, filepath.Join(root, "assets", "scripts", "runtime.sh"))
}

func TestDoltTargetShUsesPortResolve(t *testing.T) {
	root := repoRoot(t)
	assertScriptSourcesPortResolveOnce(t, filepath.Join(root, "..", "..", "..", "internal", "bootstrap", "packs", "core", "assets", "scripts", "dolt-target.sh"))
}

type portResolveCase struct {
	stateFile           string
	providerStateFile   string
	dataDir             string
	cityPath            string
	managedPort         string
	providerManagedPort string
	env                 []string
}

type portResolveResult struct {
	code   int
	stdout string
	stderr string
}

func runPortResolveOrDie(t *testing.T, tc portResolveCase) portResolveResult {
	t.Helper()
	root := repoRoot(t)
	driver := fmt.Sprintf(`
managed_runtime_port() {
    if [ "$1" = "$STATE_FILE" ] && [ -n "${TEST_MANAGED_PORT:-}" ]; then
        printf '%%s\n' "$TEST_MANAGED_PORT"
        return 0
    fi
    if [ -n "${PROVIDER_STATE_FILE:-}" ] && [ "$1" = "$PROVIDER_STATE_FILE" ] && [ -n "${TEST_PROVIDER_MANAGED_PORT:-}" ]; then
        printf '%%s\n' "$TEST_PROVIDER_MANAGED_PORT"
        return 0
    fi
    return 0
}
. %s
if [ -n "${PROVIDER_STATE_FILE:-}" ]; then
    resolve_dolt_port_or_die "$STATE_FILE" "$PROVIDER_STATE_FILE" "$DATA_DIR" "$CITY_PATH"
else
    resolve_dolt_port_or_die "$STATE_FILE" "$DATA_DIR" "$CITY_PATH"
fi
`, shellQuote(filepath.Join(root, "assets", "scripts", "port_resolve.sh")))

	cmd := exec.Command("sh", "-c", driver)
	cmd.Env = filteredEnv("GC_DOLT_PORT", "GC_DOLT_STATE_FILE", "STATE_FILE", "PROVIDER_STATE_FILE", "DATA_DIR", "CITY_PATH", "TEST_MANAGED_PORT", "TEST_PROVIDER_MANAGED_PORT")
	cmd.Env = append(cmd.Env,
		"STATE_FILE="+tc.stateFile,
		"PROVIDER_STATE_FILE="+tc.providerStateFile,
		"DATA_DIR="+tc.dataDir,
		"CITY_PATH="+tc.cityPath,
		"TEST_MANAGED_PORT="+tc.managedPort,
		"TEST_PROVIDER_MANAGED_PORT="+tc.providerManagedPort,
	)
	cmd.Env = append(cmd.Env, tc.env...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		exitErr := &exec.ExitError{}
		ok := errors.As(err, &exitErr)
		if !ok {
			t.Fatalf("port_resolve driver failed to run: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		code = exitErr.ExitCode()
	}
	return portResolveResult{
		code:   code,
		stdout: stdout.String(),
		stderr: stderr.String(),
	}
}

func assertPortResolveResult(t *testing.T, got portResolveResult, wantCode int, wantStdout, wantStderr string) {
	t.Helper()
	if got.code != wantCode {
		t.Fatalf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", got.code, wantCode, got.stdout, got.stderr)
	}
	if got.stdout != wantStdout {
		t.Fatalf("stdout = %q, want %q\nstderr:\n%s", got.stdout, wantStdout, got.stderr)
	}
	if got.stderr != wantStderr {
		t.Fatalf("stderr = %q, want %q", got.stderr, wantStderr)
	}
}

func expectedPortResolveError(stateFile, cityPath, stateStatus string) string {
	return fmt.Sprintf(`gc dolt: cannot resolve runtime port
  state_file: %s (%s)
  city_path:  %s
  consulted:  GC_DOLT_PORT (unset), GC_DOLT_STATE_FILE
  remediation: run `+"`"+`gc start`+"`"+` to bring up the city, or set
               GC_DOLT_PORT explicitly to an already-running
               server.
`, stateFile, stateStatus, cityPath)
}

func expectedPortResolveErrorWithProvider(stateFile, cityPath, stateStatus string) string {
	return fmt.Sprintf(`gc dolt: cannot resolve runtime port
  state_file: %s (%s)
  city_path:  %s
  consulted:  GC_DOLT_PORT (unset), GC_DOLT_STATE_FILE, dolt-provider-state.json
  remediation: run `+"`"+`gc start`+"`"+` to bring up the city, or set
               GC_DOLT_PORT explicitly to an already-running
               server.
`, stateFile, stateStatus, cityPath)
}

func assertScriptSourcesPortResolveOnce(t *testing.T, scriptPath string) {
	t.Helper()
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", scriptPath, err)
	}
	re := regexp.MustCompile(`(?m)^\.\s+.*port_resolve\.sh`)
	matches := re.FindAllString(string(data), -1)
	if len(matches) != 1 {
		t.Fatalf("%s port_resolve.sh source count = %d, want 1\nmatches: %s", scriptPath, len(matches), strings.Join(matches, "\n"))
	}
}

// ── bd proxied-server routing ──────────────────────────────────────────

// runPortResolveScript sources port_resolve.sh then runs the given shell
// body, returning its captured stdout/stderr/exit code. env is appended
// on top of a GC_/DOLT_-scrubbed base environment (see filteredEnv).
func runPortResolveScript(t *testing.T, body string, env ...string) portResolveResult {
	t.Helper()
	root := repoRoot(t)
	driver := fmt.Sprintf(". %s\n%s\n", shellQuote(filepath.Join(root, "assets", "scripts", "port_resolve.sh")), body)

	cmd := exec.Command("sh", "-c", driver)
	cmd.Env = append(filteredEnv(), env...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		exitErr := &exec.ExitError{}
		if !errors.As(err, &exitErr) {
			t.Fatalf("port_resolve script failed to run: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		code = exitErr.ExitCode()
	}
	return portResolveResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func TestResolveDoltPortOrDieReturnsEmptyWhenProxied(t *testing.T) {
	result := runPortResolveScript(t,
		`resolve_dolt_port_or_die "/nonexistent/state.json" "/nonexistent/data" "/nonexistent/city"`,
		"GC_BEADS_PROXIED=1",
	)
	assertPortResolveResult(t, result, 0, "", "")
}

func TestBeadsDoltModeReadsMetadataJSON(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// No trailing newline: metadata.json (like the fixture above, and real
	// output from json.Marshal) commonly has none, and sed's pattern-space
	// print does not append one when the input's last line lacks it.
	result := runPortResolveScript(t, `beads_dolt_mode`, "GC_CITY_PATH="+cityPath)
	assertPortResolveResult(t, result, 0, "proxied-server", "")
}

func TestBeadsDoltModeMissingMetadataFails(t *testing.T) {
	cityPath := t.TempDir()
	result := runPortResolveScript(t, `beads_dolt_mode`, "GC_CITY_PATH="+cityPath)
	if result.code == 0 {
		t.Fatalf("beads_dolt_mode exit = 0, want non-zero for missing metadata.json")
	}
	if result.stdout != "" {
		t.Fatalf("beads_dolt_mode stdout = %q, want empty", result.stdout)
	}
}

func TestGCBeadsProxiedDetectedFromCityMetadata(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	result := runPortResolveScript(t, `printf '%s\n' "$GC_BEADS_PROXIED"`, "GC_CITY_PATH="+cityPath)
	assertPortResolveResult(t, result, 0, "1\n", "")
}

func TestGCBeadsProxiedDefaultsToZeroForServerMode(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	result := runPortResolveScript(t, `printf '%s\n' "$GC_BEADS_PROXIED"`, "GC_CITY_PATH="+cityPath)
	assertPortResolveResult(t, result, 0, "0\n", "")
}

// writeFakeBinOnPath creates an executable at binDir/name that appends its
// invocation ("$*") to callLog and exits 0, then returns a PATH prepending
// binDir to the current process's PATH.
func writeFakeBinOnPath(t *testing.T, binDir, name, callLog string) string {
	t.Helper()
	script := "#!/bin/sh\necho \"$*\" >> " + shellQuote(callLog) + "\necho '[{\"a\":1}]'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binDir + string(os.PathListSeparator) + os.Getenv("PATH")
}

func TestStoreSqlRoutesThroughBdInProxiedMode(t *testing.T) {
	cityPath := t.TempDir()
	binDir := t.TempDir()
	callLog := filepath.Join(t.TempDir(), "bd-calls.log")
	path := writeFakeBinOnPath(t, binDir, "bd", callLog)

	result := runPortResolveScript(t, `store_sql csv "SELECT 1"`,
		"GC_CITY_PATH="+cityPath,
		"GC_BEADS_PROXIED=1",
		"PATH="+path,
	)
	if result.code != 0 {
		t.Fatalf("store_sql exit = %d, want 0; stderr=%s", result.code, result.stderr)
	}
	data, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("bd was not invoked: %v", err)
	}
	got := strings.TrimSpace(string(data))
	want := "-C " + cityPath + " sql --csv SELECT 1"
	if got != want {
		t.Fatalf("bd invoked with %q, want %q", got, want)
	}
}

func TestStoreSqlRoutesThroughDoltInLegacyMode(t *testing.T) {
	cityPath := t.TempDir()
	binDir := t.TempDir()
	callLog := filepath.Join(t.TempDir(), "dolt-calls.log")
	path := writeFakeBinOnPath(t, binDir, "dolt", callLog)

	result := runPortResolveScript(t, `store_sql csv "SELECT 1"`,
		"GC_CITY_PATH="+cityPath,
		"GC_BEADS_PROXIED=0",
		"GC_DOLT_PORT=4406",
		"PATH="+path,
	)
	if result.code != 0 {
		t.Fatalf("store_sql exit = %d, want 0; stderr=%s", result.code, result.stderr)
	}
	data, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("dolt was not invoked: %v", err)
	}
	got := strings.TrimSpace(string(data))
	want := "--host 127.0.0.1 --port 4406 --user root --no-tls sql --result-format csv -q SELECT 1"
	if got != want {
		t.Fatalf("dolt invoked with %q, want %q", got, want)
	}
}

func TestStoreDmlRowsDbQualifiedParsesRowsAffectedInProxiedMode(t *testing.T) {
	cityPath := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte("#!/bin/sh\necho '{\"rows_affected\":3}'\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := binDir + string(os.PathListSeparator) + os.Getenv("PATH")

	result := runPortResolveScript(t, `store_dml_rows_db_qualified "DELETE FROM db.issues WHERE 1=0"`,
		"GC_CITY_PATH="+cityPath,
		"GC_BEADS_PROXIED=1",
		"PATH="+path,
	)
	assertPortResolveResult(t, result, 0, "3\n", "")
}
