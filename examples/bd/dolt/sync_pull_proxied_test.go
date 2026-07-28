package dolt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBdSyncPull is a stand-in `bd` for the proxied sync/pull path. It answers
// SHOW DATABASES (catalog), dolt_remotes / active_branch() reads, and
// CALL DOLT_FETCH/PUSH/PULL, all driven by FAKE_* env vars so each test case
// can shape a scenario without a bespoke script. It logs every invocation's
// query (minus the leading -C/sql/--database/--csv plumbing) to FAKE_BD_ARGS
// when set, so tests can assert push/fetch/pull did or didn't happen.
const fakeBdSyncPull = `#!/bin/sh
db=""
while [ $# -gt 0 ]; do
  case "$1" in
    -C) shift 2 ;;
    sql) shift; break ;;
    *) shift ;;
  esac
done
case "${1:-}" in --csv) shift ;; esac
if [ "${1:-}" = "--database" ]; then db="$2"; shift 2; fi
case "${1:-}" in --csv) shift ;; esac
q="${1:-}"
[ -n "$FAKE_BD_ARGS" ] && printf '%s\t%s\n' "$db" "$q" >> "$FAKE_BD_ARGS"
case "$q" in
  *"SHOW DATABASES"*)
    printf 'Database\n%s\n' "${FAKE_DB_NAME:-hq}" ;;
  *"SELECT name, url FROM dolt_remotes"*)
    if [ -n "$FAKE_REMOTE_NAME" ]; then
      printf 'name,url\n%s,%s\n' "$FAKE_REMOTE_NAME" "${FAKE_REMOTE_URL:-file:///remote}"
    else
      printf 'name,url\n'
    fi ;;
  *"SELECT active_branch()"*)
    printf 'active_branch()\n%s\n' "${FAKE_ACTIVE_BRANCH:-main}" ;;
  *"CALL DOLT_FETCH"*)
    if [ "${FAKE_FETCH_RC:-0}" != 0 ]; then
      printf '%s\n' "${FAKE_FETCH_ERR:-fetch failed}" >&2
      exit "${FAKE_FETCH_RC}"
    fi
    printf 'ok\n' ;;
  *"dolt_log('remotes/"*)
    printf 'n\n%s\n' "${FAKE_AHEAD:-0}" ;;
  *"dolt_log("*)
    printf 'n\n%s\n' "${FAKE_BEHIND:-0}" ;;
  *"CALL DOLT_PUSH"*)
    if [ "${FAKE_PUSH_RC:-0}" != 0 ]; then
      printf '%s\n' "${FAKE_PUSH_ERR:-push failed}" >&2
      exit "${FAKE_PUSH_RC}"
    fi
    printf 'ok\n' ;;
  *"CALL DOLT_PULL"*)
    if [ "${FAKE_PULL_RC:-0}" != 0 ]; then
      printf '%s\n' "${FAKE_PULL_ERR:-pull failed}" >&2
      exit "${FAKE_PULL_RC}"
    fi
    printf 'ok\n' ;;
  *) printf 'ok\n' ;;
esac
`

// syncPullProxiedFixture holds the env vars driving fakeBdSyncPull's canned
// responses for one scenario. Zero values mean "use the fake's own default".
type syncPullProxiedFixture struct {
	remoteName    string
	remoteURL     string
	activeBranch  string
	fetchRC       string
	fetchErr      string
	ahead         string
	behind        string
	pushRC        string
	pushErr       string
	pullRC        string
	pullErr       string
	extraFakeEnv  []string
	extraCmdArgs  []string
	writeNoSync   bool
	skipRouteFile bool
}

// runSyncOrPullProxied runs commands/<name>/run.sh (name is "sync" or "pull")
// in bd proxied-server mode against fakeBdSyncPull, shaped by fx. Returns
// stdout+stderr, exit code, and the recorded "<db>\t<query>" invocation log.
func runSyncOrPullProxied(t *testing.T, name string, fx syncPullProxiedFixture) (string, int, string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	root := repoRoot(t)
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "bd"), fakeBdSyncPull)
	// No dolt on PATH: proxied sync/pull must never shell out to dolt.
	writeExecutable(t, filepath.Join(binDir, "dolt"), "#!/bin/sh\necho 'dolt must not run in proxied mode' >&2\nexit 97\n")

	city := t.TempDir()
	writeStoreMetadata(t, city, "proxied-server")
	if !fx.skipRouteFile {
		if err := os.WriteFile(filepath.Join(city, ".beads", "routes.jsonl"), []byte(`{"database":"hq"}`+"\n"), 0o644); err != nil {
			t.Fatalf("write routes.jsonl: %v", err)
		}
	}
	if fx.writeNoSync {
		if err := os.WriteFile(filepath.Join(city, ".beads", ".no-sync"), []byte("skip\n"), 0o644); err != nil {
			t.Fatalf("write .no-sync marker: %v", err)
		}
	}

	argsFile := filepath.Join(t.TempDir(), "bd.args")
	scriptArgs := append([]string{filepath.Join(root, "commands", name, "run.sh")}, fx.extraCmdArgs...)
	cmd := exec.Command("sh", scriptArgs...)
	env := append(filteredEnv("PATH"),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GC_CITY_PATH="+city,
		"GC_PACK_DIR="+root,
		"FAKE_BD_ARGS="+argsFile,
	)
	if fx.remoteName != "" {
		env = append(env, "FAKE_REMOTE_NAME="+fx.remoteName)
	}
	if fx.remoteURL != "" {
		env = append(env, "FAKE_REMOTE_URL="+fx.remoteURL)
	}
	if fx.activeBranch != "" {
		env = append(env, "FAKE_ACTIVE_BRANCH="+fx.activeBranch)
	}
	if fx.fetchRC != "" {
		env = append(env, "FAKE_FETCH_RC="+fx.fetchRC)
	}
	if fx.fetchErr != "" {
		env = append(env, "FAKE_FETCH_ERR="+fx.fetchErr)
	}
	if fx.ahead != "" {
		env = append(env, "FAKE_AHEAD="+fx.ahead)
	}
	if fx.behind != "" {
		env = append(env, "FAKE_BEHIND="+fx.behind)
	}
	if fx.pushRC != "" {
		env = append(env, "FAKE_PUSH_RC="+fx.pushRC)
	}
	if fx.pushErr != "" {
		env = append(env, "FAKE_PUSH_ERR="+fx.pushErr)
	}
	if fx.pullRC != "" {
		env = append(env, "FAKE_PULL_RC="+fx.pullRC)
	}
	if fx.pullErr != "" {
		env = append(env, "FAKE_PULL_ERR="+fx.pullErr)
	}
	env = append(env, fx.extraFakeEnv...)
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
	logged := ""
	if data, readErr := os.ReadFile(argsFile); readErr == nil {
		logged = string(data)
	}
	return string(out), code, logged
}

func TestSyncProxiedAheadOnlyPushes(t *testing.T) {
	out, code, logged := runSyncOrPullProxied(t, "sync", syncPullProxiedFixture{
		remoteName: "origin",
		remoteURL:  "file:///remote/hq",
		ahead:      "1",
		behind:     "0",
	})
	if code != 0 {
		t.Fatalf("sync exit=%d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "hq: pushed main -> origin:main") {
		t.Fatalf("sync did not report a push:\n%s", out)
	}
	if !strings.Contains(logged, "CALL DOLT_PUSH") {
		t.Fatalf("bd never received CALL DOLT_PUSH:\n%s", logged)
	}
	if strings.Contains(out, "dolt must not run") {
		t.Fatalf("sync shelled out to dolt in proxied mode:\n%s", out)
	}
}

func TestSyncProxiedUpToDateSkips(t *testing.T) {
	out, code, logged := runSyncOrPullProxied(t, "sync", syncPullProxiedFixture{
		remoteName: "origin",
		ahead:      "0",
		behind:     "0",
	})
	if code != 0 {
		t.Fatalf("sync exit=%d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "hq: up-to-date with origin:main") {
		t.Fatalf("sync did not report up-to-date:\n%s", out)
	}
	if strings.Contains(logged, "CALL DOLT_PUSH") {
		t.Fatalf("sync pushed an up-to-date database:\n%s", logged)
	}
}

func TestSyncProxiedDivergedRefuses(t *testing.T) {
	out, code, logged := runSyncOrPullProxied(t, "sync", syncPullProxiedFixture{
		remoteName: "origin",
		ahead:      "1",
		behind:     "1",
	})
	if code != 1 {
		t.Fatalf("sync exit=%d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "diverged (1 ahead / 1 behind)") {
		t.Fatalf("sync did not report divergence:\n%s", out)
	}
	if strings.Contains(logged, "CALL DOLT_PUSH") {
		t.Fatalf("sync pushed a diverged database:\n%s", logged)
	}
}

func TestSyncProxiedForcePushesWhenDiverged(t *testing.T) {
	out, code, logged := runSyncOrPullProxied(t, "sync", syncPullProxiedFixture{
		remoteName:   "origin",
		ahead:        "1",
		behind:       "1",
		extraCmdArgs: []string{"--force"},
	})
	if code != 0 {
		t.Fatalf("sync --force exit=%d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "hq: pushed main -> origin:main") {
		t.Fatalf("sync --force did not report a push:\n%s", out)
	}
	if !strings.Contains(logged, "CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')") {
		t.Fatalf("bd did not receive a force-push with --set-upstream:\n%s", logged)
	}
}

func TestSyncProxiedFirstPushWhenRemoteBranchAbsent(t *testing.T) {
	out, code, logged := runSyncOrPullProxied(t, "sync", syncPullProxiedFixture{
		remoteName: "origin",
		fetchRC:    "1",
		fetchErr:   "no branches found in remote",
	})
	if code != 0 {
		t.Fatalf("sync exit=%d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "hq: pushed main -> origin:main") {
		t.Fatalf("sync did not push on first-push detection:\n%s", out)
	}
	if !strings.Contains(logged, "CALL DOLT_PUSH") {
		t.Fatalf("bd never received CALL DOLT_PUSH on first push:\n%s", logged)
	}
}

func TestSyncProxiedNoRemoteSkips(t *testing.T) {
	out, code, logged := runSyncOrPullProxied(t, "sync", syncPullProxiedFixture{})
	if code != 0 {
		t.Fatalf("sync exit=%d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "hq: skipped (no remote)") {
		t.Fatalf("sync did not skip a remote-less database:\n%s", out)
	}
	if strings.Contains(logged, "CALL DOLT_PUSH") || strings.Contains(logged, "CALL DOLT_FETCH") {
		t.Fatalf("sync touched a database with no remote:\n%s", logged)
	}
}

func TestSyncProxiedNoSyncMarkerSkips(t *testing.T) {
	out, code, logged := runSyncOrPullProxied(t, "sync", syncPullProxiedFixture{
		remoteName:  "origin",
		writeNoSync: true,
	})
	if code != 0 {
		t.Fatalf("sync exit=%d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "hq: skipped (.no-sync)") {
		t.Fatalf("sync did not honor the .no-sync marker:\n%s", out)
	}
	if strings.Contains(logged, "CALL DOLT_PUSH") || strings.Contains(logged, "CALL DOLT_FETCH") {
		t.Fatalf("sync touched a .no-sync database:\n%s", logged)
	}
}

func TestSyncProxiedRefspecEnvOverride(t *testing.T) {
	out, code, logged := runSyncOrPullProxied(t, "sync", syncPullProxiedFixture{
		remoteName: "origin",
		ahead:      "1",
		extraFakeEnv: []string{
			"GC_BEADS_REFSPEC_HQ=dev:staging",
		},
	})
	if code != 0 {
		t.Fatalf("sync exit=%d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "hq: pushed dev -> origin:staging") {
		t.Fatalf("sync did not honor the refspec override:\n%s", out)
	}
	if !strings.Contains(logged, "CALL DOLT_PUSH('origin', 'dev:staging')") {
		t.Fatalf("bd did not receive the overridden refspec:\n%s", logged)
	}
}

func TestPullProxiedSucceeds(t *testing.T) {
	out, code, logged := runSyncOrPullProxied(t, "pull", syncPullProxiedFixture{
		remoteName: "origin",
		remoteURL:  "file:///remote/hq",
	})
	if code != 0 {
		t.Fatalf("pull exit=%d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "hq: pulled from file:///remote/hq") {
		t.Fatalf("pull did not report success:\n%s", out)
	}
	if !strings.Contains(logged, "CALL DOLT_PULL('origin', 'main')") {
		t.Fatalf("bd never received CALL DOLT_PULL:\n%s", logged)
	}
	if strings.Contains(out, "dolt must not run") {
		t.Fatalf("pull shelled out to dolt in proxied mode:\n%s", out)
	}
}

func TestPullProxiedReportsFailure(t *testing.T) {
	out, code, _ := runSyncOrPullProxied(t, "pull", syncPullProxiedFixture{
		remoteName: "origin",
		pullRC:     "1",
		pullErr:    "conflict",
	})
	if code != 1 {
		t.Fatalf("pull exit=%d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "hq: ERROR: pull failed") {
		t.Fatalf("pull did not surface the failure:\n%s", out)
	}
}

func TestPullProxiedNoSyncMarkerSkips(t *testing.T) {
	out, code, logged := runSyncOrPullProxied(t, "pull", syncPullProxiedFixture{
		remoteName:  "origin",
		writeNoSync: true,
	})
	if code != 0 {
		t.Fatalf("pull exit=%d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "hq: skipped (.no-sync)") {
		t.Fatalf("pull did not honor the .no-sync marker:\n%s", out)
	}
	if strings.Contains(logged, "CALL DOLT_PULL") {
		t.Fatalf("pull touched a .no-sync database:\n%s", logged)
	}
}
