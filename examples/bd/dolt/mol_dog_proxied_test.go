package dolt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fakeBdMolDog is a stand-in `bd` for the mol-dog scripts' proxied path. It
// answers the four reads those scripts issue and logs every invocation so tests
// can assert what was asked.
const fakeBdMolDog = `#!/bin/sh
[ -n "$FAKE_BD_ARGS" ] && printf '%s\n' "$*" >> "$FAKE_BD_ARGS"
while [ $# -gt 0 ]; do
  case "$1" in
    -C) shift 2 ;;
    --database) shift 2 ;;
    sql) shift; break ;;
    *) shift ;;
  esac
done
case "${1:-}" in --csv|--json) shift ;; esac
q="${1:-}"
if [ "${FAKE_BD_STORE_DOWN:-0}" = 1 ]; then
  echo "bd: store unreachable" >&2
  exit 1
fi
case "$q" in
  *active_branch*)        printf 'active_branch()\nmain\n' ;;
  *max_connections*)      printf '@@GLOBAL.max_connections\n256\n' ;;
  *PROCESSLIST*)          printf 'COUNT(*)\n1\n' ;;
  *"SHOW DATABASES"*)     printf 'Database\nhq\ndo\n' ;;
  *)                      printf 'ok\n' ;;
esac
`

// runMolDogProxied runs a mol-dog script against a proxied-server city with a
// fake bd on PATH. A fake `dolt` is installed that fails loudly, so any residual
// direct-dolt SQL call is caught. `dolt backup`/`dolt version` are allowed
// because they run against the on-disk repo and need no port.
func runMolDogProxied(t *testing.T, scriptName, argsFile string, extraEnv ...string) (string, int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := repoRoot(t)
	binDir := t.TempDir()
	cityPath := t.TempDir()
	dataDir := t.TempDir()
	writeStoreMetadata(t, cityPath, "proxied-server")
	// The scripts only act on databases that exist on disk, so the fixture must
	// materialize the two the fake bd reports from SHOW DATABASES.
	for _, db := range []string{"hq", "do"} {
		if err := os.MkdirAll(filepath.Join(dataDir, db, ".dolt"), 0o755); err != nil {
			t.Fatalf("mkdir %s/.dolt: %v", db, err)
		}
	}
	writeExecutable(t, filepath.Join(binDir, "bd"), fakeBdMolDog)
	// Only `dolt backup` / `dolt version` are legitimate in proxied mode: they
	// act on the on-disk repo. Any `dolt ... sql` is the bug this test guards.
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
for a in "$@"; do
  if [ "$a" = "sql" ]; then echo "FORBIDDEN: direct dolt sql in proxied mode" >&2; exit 97; fi
done
case "${1:-}" in
  version) echo "dolt version 2.2.1" ;;
  backup)  [ "${2:-}" = "" ] && echo "" || exit 0 ;;
  *) : ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "gc"), "#!/bin/sh\nexit 0\n")

	cmd := exec.Command("bash", filepath.Join(root, "assets", "scripts", scriptName))
	env := append(filteredEnv("PATH"),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_BEADS_DATA_DIR="+dataDir,
	)
	if argsFile != "" {
		env = append(env, "FAKE_BD_ARGS="+argsFile)
	}
	cmd.Env = append(env, extraEnv...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run %s: %v\n%s", scriptName, err, out)
		}
	}
	return string(out), code
}

// TestDoctorProxiedProbesThroughBdWithoutEscalating is the regression guard for
// pc1-rqv: with an empty GC_BEADS_PORT the doctor used to dial `--port ""`, fail
// the active_branch() probe, and file a false CRITICAL every 5 minutes. Routed
// through bd it must succeed and stay quiet.
func TestDoctorProxiedProbesThroughBdWithoutEscalating(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "bd.args")
	out, code := runMolDogProxied(t, "mol-dog-doctor.sh", argsFile)

	if code != 0 {
		t.Fatalf("doctor exit=%d, want 0\n%s", code, out)
	}
	if strings.Contains(out, "FORBIDDEN") {
		t.Fatalf("doctor issued a direct dolt sql in proxied mode:\n%s", out)
	}
	if strings.Contains(out, "unreachable") {
		t.Fatalf("doctor reported the store unreachable while bd was answering:\n%s", out)
	}
	logged, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read bd args: %v", err)
	}
	if !strings.Contains(string(logged), "active_branch") {
		t.Fatalf("doctor did not run its probe through bd; bd saw:\n%s", string(logged))
	}
}

// TestDoctorProxiedEscalationNamesEndpointNotEmptyPort proves that when the
// store genuinely is unreachable, the CRITICAL subject identifies the proxied
// endpoint instead of interpolating an empty $PORT ("unreachable on port  ").
func TestDoctorProxiedEscalationNamesEndpointNotEmptyPort(t *testing.T) {
	escalateLog := filepath.Join(t.TempDir(), "escalate.log")
	escalateDir := t.TempDir()
	writeExecutable(t, filepath.Join(escalateDir, "escalate.sh"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+escalateLog+"'\nexit 0\n")

	out, code := runMolDogProxied(t, "mol-dog-doctor.sh", "",
		"FAKE_BD_STORE_DOWN=1",
		"GC_ESCALATE_SCRIPT="+filepath.Join(escalateDir, "escalate.sh"),
	)
	if code != 0 {
		t.Fatalf("doctor exit=%d, want 0 (it exits 0 after escalating)\n%s", code, out)
	}
	body, _ := os.ReadFile(escalateLog)
	subject := string(body) + out
	if regexp.MustCompile(`on port\s+\[`).MatchString(subject) {
		t.Fatalf("escalation still interpolates an empty port:\n%s", subject)
	}
	if !strings.Contains(subject, "proxied-server") {
		t.Fatalf("escalation does not name the proxied endpoint:\n%s", subject)
	}
}

// TestBackupProxiedDiscoversDatabasesThroughBd is the regression guard for
// pc1-ntz: SHOW DATABASES was the backup script's only SQL, so an empty port
// made it silently find zero databases and register no backups at all.
func TestBackupProxiedDiscoversDatabasesThroughBd(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "bd.args")
	out, code := runMolDogProxied(t, "mol-dog-backup.sh", argsFile)

	if code != 0 {
		t.Fatalf("backup exit=%d, want 0\n%s", code, out)
	}
	if strings.Contains(out, "FORBIDDEN") {
		t.Fatalf("backup issued a direct dolt sql in proxied mode:\n%s", out)
	}
	if strings.Contains(out, "no databases found") {
		t.Fatalf("backup found no databases while bd reported hq/do:\n%s", out)
	}
	logged, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read bd args: %v", err)
	}
	if !strings.Contains(string(logged), "SHOW DATABASES") {
		t.Fatalf("backup did not enumerate databases through bd; bd saw:\n%s", string(logged))
	}
}

// TestCleanupForceRefusesWhenRigDBMatchesBdStalePrefix covers the safety
// regression introduced when `gc dolt cleanup --force` began delegating to
// `bd dolt clean-databases`. bd picks victims by name prefix alone and has no
// registered-rig protection, unlike the Go command it replaced. A registered rig
// database whose name matches one of bd's stale prefixes must abort the
// destructive run rather than be silently dropped.
func TestCleanupForceRefusesWhenRigDBMatchesBdStalePrefix(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	root := repoRoot(t)
	binDir := t.TempDir()
	cityPath := t.TempDir()
	dataDir := t.TempDir()
	writeStoreMetadata(t, cityPath, "proxied-server")

	// A registered rig whose database name collides with bd's "testdb_" prefix.
	rigPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"),
		[]byte(`{"dolt_database":"testdb_live_rig"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(binDir, "gc"), "#!/bin/sh\n"+
		"if [ \"$1\" = \"rig\" ]; then printf '{\"rigs\":[{\"path\":\""+rigPath+"\"}]}\\n'; fi\nexit 0\n")
	bdLog := filepath.Join(t.TempDir(), "bd.args")
	writeExecutable(t, filepath.Join(binDir, "bd"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+bdLog+"'\nexit 0\n")

	cmd := exec.Command("sh", filepath.Join(root, "commands", "cleanup", "run.sh"), "--force")
	cmd.Env = append(filteredEnv("PATH"),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_BEADS_DATA_DIR="+dataDir,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("cleanup --force succeeded; want refusal when a rig DB matches a bd stale prefix\n%s", out)
	}
	if !strings.Contains(string(out), "refusing --force") {
		t.Fatalf("expected an explicit refusal, got:\n%s", out)
	}
	if body, _ := os.ReadFile(bdLog); strings.Contains(string(body), "clean-databases") {
		t.Fatalf("cleanup invoked bd destructively despite the collision: %q", string(body))
	}
}

// TestCleanupForcePurgesToReclaimDisk proves --force asks bd to purge the
// dropped databases' data. Dropping alone frees no disk, which was the entire
// point of the cleanup — the Go command this delegation replaced did drop and
// purge together.
func TestCleanupForcePurgesToReclaimDisk(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	root := repoRoot(t)
	binDir := t.TempDir()
	cityPath := t.TempDir()
	dataDir := t.TempDir()
	writeStoreMetadata(t, cityPath, "proxied-server")

	// A registered rig with a name that does NOT collide with bd's prefixes.
	rigPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"),
		[]byte(`{"dolt_database":"safe_rig"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(binDir, "gc"), "#!/bin/sh\n"+
		"if [ \"$1\" = \"rig\" ]; then printf '{\"rigs\":[{\"path\":\""+rigPath+"\"}]}\\n'; fi\nexit 0\n")
	bdLog := filepath.Join(t.TempDir(), "bd.args")
	writeExecutable(t, filepath.Join(binDir, "bd"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+bdLog+"'\nexit 0\n")

	cmd := exec.Command("sh", filepath.Join(root, "commands", "cleanup", "run.sh"), "--force")
	cmd.Env = append(filteredEnv("PATH"),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_BEADS_DATA_DIR="+dataDir,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleanup --force failed: %v\n%s", err, out)
	}
	body, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read bd args: %v", err)
	}
	if !strings.Contains(string(body), "clean-databases --purge-dropped") {
		t.Fatalf("--force did not ask bd to purge; bd saw: %q", string(body))
	}
	if strings.Contains(string(body), "--dry-run") {
		t.Fatalf("--force delegated a dry run: %q", string(body))
	}
}

// TestPortReadingScriptsAreProxiedAware guards the whole class of bug behind
// pc1-rqv and pc1-ntz. resolve_dolt_port_or_die returns EMPTY in proxied mode
// (rather than exiting 78), so a script that dials $GC_BEADS_PORT without ever
// checking GC_BEADS_PROXIED silently connects to port "" and fails in a
// confusing way — which is exactly how the doctor escalated
// "unreachable on port  [CRITICAL]" every five minutes.
//
// Reading the port is fine; reading it while *unaware* of proxied mode is not.
// The migrated commands/*/run.sh keep their legacy `dolt --host --port` branch
// and are legitimate because they early-return in proxied mode first.
func TestPortReadingScriptsAreProxiedAware(t *testing.T) {
	root := repoRoot(t)
	repo := filepath.Clean(filepath.Join(root, "..", "..", ".."))

	// These define the port/shim contract itself.
	allowed := map[string]bool{
		"port_resolve.sh": true, // defines the resolver + store_sql shim
		"runtime.sh":      true, // assigns GC_BEADS_PORT from the resolver
		"gc-beads-bd.sh":  true, // the provider script; owns the backend
	}
	portRe := regexp.MustCompile(`\$\{?GC_BEADS_(PORT|HOST)\b`)
	// Any of these means the script knows proxied mode exists.
	awareRe := regexp.MustCompile(`GC_BEADS_PROXIED|store_sql|store_reachable|store_sql_db`)

	var offenders []string
	for _, dir := range []string{
		filepath.Join(repo, "examples"),
		filepath.Join(repo, "internal", "bootstrap", "packs"),
	} {
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".sh") {
				return nil //nolint:nilerr // best-effort walk; unreadable entries are skipped
			}
			if allowed[filepath.Base(path)] {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil //nolint:nilerr // unreadable file is not an offender
			}
			body := string(data)
			if !portRe.MatchString(body) || awareRe.MatchString(body) {
				return nil
			}
			rel, _ := filepath.Rel(repo, path)
			offenders = append(offenders, rel)
			return nil
		})
	}
	if len(offenders) > 0 {
		t.Fatalf("these shipped scripts dial GC_BEADS_PORT/HOST but never check GC_BEADS_PROXIED "+
			"and never use the store_sql shim.\nIn proxied-server mode the port is EMPTY, so they "+
			"fail silently or escalate nonsense:\n  %s", strings.Join(offenders, "\n  "))
	}
}
