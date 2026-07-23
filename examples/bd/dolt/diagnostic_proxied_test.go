package dolt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBdDiag is a stand-in `bd` for the diagnostic-command proxied path. It
// answers SELECT 1 (liveness), SHOW DATABASES (catalog, incl. a system db that
// must be filtered), and logs the positional query it received for `sql`
// forwarding assertions.
const fakeBdDiag = `#!/bin/sh
[ -n "$FAKE_BD_ARGS" ] && printf '%s\n' "$*" >> "$FAKE_BD_ARGS"
while [ $# -gt 0 ]; do
  case "$1" in
    -C) shift 2 ;;
    sql) shift; break ;;
    *) shift ;;
  esac
done
case "${1:-}" in --csv|--json) shift ;; esac
q="${1:-}"
case "$q" in
  *"SELECT 1"*)       printf '1\n1\n' ;;
  *"SHOW DATABASES"*) printf 'Database\nhq\nmysql\n' ;;
  *)                  printf 'ok\n' ;;
esac
`

// runDoltCommandProxied runs commands/<name>/run.sh in proxied mode with a fake
// bd on PATH. argsFile (if non-empty) captures each bd invocation's args.
func runDoltCommandProxied(t *testing.T, name, argsFile string, cmdArgs ...string) (string, int) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	root := repoRoot(t)
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "bd"), fakeBdDiag)
	// No dolt on PATH: proxied diagnostics must never shell out to dolt.
	writeExecutable(t, filepath.Join(binDir, "dolt"), "#!/bin/sh\necho 'dolt must not run in proxied mode' >&2\nexit 97\n")
	city := t.TempDir()
	writeStoreMetadata(t, city, "proxied-server")

	scriptArgs := append([]string{filepath.Join(root, "commands", name, "run.sh")}, cmdArgs...)
	cmd := exec.Command("sh", scriptArgs...)
	env := append(filteredEnv("PATH"),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GC_CITY_PATH="+city,
		"GC_PACK_DIR="+root,
	)
	if argsFile != "" {
		env = append(env, "FAKE_BD_ARGS="+argsFile)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run %s: %v\n%s", name, err, out)
		}
	}
	return string(out), code
}

func TestStatusProxiedReportsReachable(t *testing.T) {
	out, code := runDoltCommandProxied(t, "status", "")
	if code != 0 || !strings.Contains(out, "reachable (bd proxied-server)") {
		t.Fatalf("status: out=%q code=%d, want exit 0 + 'reachable (bd proxied-server)'", out, code)
	}
}

func TestListProxiedEnumeratesCatalogAndFiltersSystemDBs(t *testing.T) {
	out, code := runDoltCommandProxied(t, "list", "")
	if code != 0 {
		t.Fatalf("list exit=%d out=%q", code, out)
	}
	if !strings.Contains(out, "hq") {
		t.Fatalf("list did not show hq: %q", out)
	}
	if strings.Contains(out, "mysql") {
		t.Fatalf("list leaked system db mysql: %q", out)
	}
}

func TestSQLProxiedForwardsQueryToBdPositional(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "bd.args")
	out, code := runDoltCommandProxied(t, "sql", argsFile, "-q", "SELECT 42 FROM `hq`.issues")
	if code != 0 {
		t.Fatalf("sql exit=%d out=%q", code, out)
	}
	logged, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	got := string(logged)
	if strings.Contains(got, "-q") {
		t.Fatalf("bd was passed dolt's -q flag (should be stripped): %q", got)
	}
	if !strings.Contains(got, "SELECT 42 FROM `hq`.issues") {
		t.Fatalf("bd did not receive the positional query: %q", got)
	}
}

func TestRecoverProxiedIsNoOpWhenReachable(t *testing.T) {
	out, code := runDoltCommandProxied(t, "recover", "")
	if code != 0 || !strings.Contains(out, "no recovery needed") {
		t.Fatalf("recover: out=%q code=%d, want exit 0 + 'no recovery needed'", out, code)
	}
}
