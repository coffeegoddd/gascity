package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// writeSpyScript creates a shell script that logs operations to a file and
// recreates .beads/ on init (simulating bd init wiping hooks). Returns the
// script path.
func writeSpyScript(t *testing.T, logFile string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "spy-beads.sh")

	// The spy logs "op arg1 arg2 ..." to logFile, one line per call.
	// For "init" operations, it also creates .beads/ in the target dir
	// (simulating bd init creating the directory, which wipes hooks).
	content := `#!/bin/sh
echo "$@" >> "` + logFile + `"
case "$1" in
  init)
    # Simulate bd init: create .beads/ (may wipe existing hooks)
    mkdir -p "$2/.beads"
    ;;
  create)
    cat >/dev/null
    printf '{"id":"spy-1","title":"spy bead","status":"open","type":"task"}\n'
    ;;
esac
exit 0
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// readOpLog reads the spy script's operation log and returns the lines.
func readOpLog(t *testing.T, logFile string) []string {
	t.Helper()
	data, err := os.ReadFile(logFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	raw := strings.TrimSpace(string(data))
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

// assertOpSubsequence verifies that ops contains entries with the given
// prefixes in order. The lifecycle tests care about sequencing of the
// current operation, not unrelated trailing health checks from background
// activity elsewhere in the process.
func assertOpSubsequence(t *testing.T, ops []string, want ...string) {
	t.Helper()
	if len(want) == 0 {
		t.Fatalf("assertOpSubsequence requires at least one prefix")
	}
	if len(ops) == 0 {
		t.Fatalf("expected ops containing %v, got none", want)
	}
	index := 0
	for _, op := range ops {
		if strings.HasPrefix(op, want[index]) {
			index++
			if index == len(want) {
				return
			}
		}
	}
	t.Fatalf("expected op subsequence %v in %v", want, ops)
}

// assertSingleStopWithBenignNoise verifies a single stop call while tolerating
// unrelated background health/probe checks from other goroutines in the test
// process.
func assertSingleStopWithBenignNoise(t *testing.T, ops []string) {
	t.Helper()
	if len(ops) == 0 {
		t.Fatalf("expected stop op, got none")
	}

	stopCount := 0
	for _, op := range ops {
		switch {
		case strings.HasPrefix(op, "stop"):
			stopCount++
		case strings.HasPrefix(op, "health"), strings.HasPrefix(op, "probe"):
			continue
		default:
			t.Fatalf("unexpected lifecycle op in stop sequence: %v", ops)
		}
	}
	if stopCount != 1 {
		t.Fatalf("expected exactly one stop op with optional health/probe noise, got %v", ops)
	}
}

// assertHooksAbsent checks that gc-installed bead event hooks are absent at dir.
// installBeadHooks now removes these hooks; they must not exist after any gc operation.
func assertHooksAbsent(t *testing.T, dir, context string) {
	t.Helper()
	for _, hook := range []string{"on_create", "on_close", "on_update"} {
		path := filepath.Join(dir, ".beads", "hooks", hook)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("hook %s must be absent at %s (%s): stat err=%v", hook, dir, context, err)
		}
	}
}

// testCityConfig creates a minimal config.City with the given rigs.
func testCityConfig(cityName string, rigs []config.Rig) *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: cityName},
		Rigs:      rigs,
	}
}

// TestLifecycleCoordination_InitRigAddStart exercises the consolidated
// lifecycle functions using GC_BEADS=exec:<spy> to verify ordering and
// hook survival across gc init → gc rig add → gc start.
func TestLifecycleCoordination_InitRigAddStart(t *testing.T) {
	cityPath := t.TempDir()
	cityName := "testcity"
	rigPath := filepath.Join(cityPath, "rigs", "myrig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \""+cityName+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(t.TempDir(), "ops.log")
	script := writeSpyScript(t, logFile)
	t.Setenv("GC_BEADS", "exec:"+script)

	// Phase 1: gc init — initDirIfReady for city root.
	prefix := "tc"
	deferred, err := initDirIfReady(cityPath, cityPath, prefix)
	if err != nil {
		t.Fatalf("initDirIfReady (city): %v", err)
	}
	if deferred {
		t.Fatal("expected exec: provider not to defer")
	}

	ops := readOpLog(t, logFile)
	assertOpSubsequence(t, ops, "probe", "start", "init "+cityPath)
	cityInitOps := len(ops)
	assertHooksAbsent(t, cityPath, "after city init")

	// Phase 2: gc rig add — initDirIfReady for rig.
	rigPrefix := "mr"
	deferred, err = initDirIfReady(cityPath, rigPath, rigPrefix)
	if err != nil {
		t.Fatalf("initDirIfReady (rig): %v", err)
	}
	if deferred {
		t.Fatal("expected exec: provider not to defer")
	}

	ops = readOpLog(t, logFile)
	if len(ops) <= cityInitOps {
		t.Fatalf("expected rig add to append ops beyond %d entries, got %d: %v", cityInitOps, len(ops), ops)
	}
	assertOpSubsequence(t, ops[cityInitOps:], "probe", "start", "init "+rigPath)
	rigInitOps := len(ops)
	assertHooksAbsent(t, rigPath, "after rig add")

	// Phase 3: gc start — startBeadsLifecycle re-runs provider init and removes
	// any stale gc hooks. No hooks are installed since autoclose runs in-process.
	cfg := testCityConfig(cityName, []config.Rig{
		{Name: "myrig", Path: rigPath, Prefix: rigPrefix},
	})
	if err := startBeadsLifecycle(cityPath, cityName, cfg, io.Discard); err != nil {
		t.Fatalf("startBeadsLifecycle: %v", err)
	}

	ops = readOpLog(t, logFile)
	if len(ops) <= rigInitOps {
		t.Fatalf("expected start to append ops beyond %d entries, got %d: %v", rigInitOps, len(ops), ops)
	}
	assertOpSubsequence(t, ops[rigInitOps:], "start", "init "+cityPath, "init "+rigPath)

	// Verify gc hooks are absent at both paths after start.
	assertHooksAbsent(t, cityPath, "after start")
	assertHooksAbsent(t, rigPath, "after start")
}

// TestLifecycleCoordination_StartOrder verifies that start precedes any
// init call when using startBeadsLifecycle. This catches bugs where init
// runs before the backing service is ready.
func TestLifecycleCoordination_StartOrder(t *testing.T) {
	cityPath := t.TempDir()
	cityName := "ordertest"
	rigPath := filepath.Join(cityPath, "rigs", "myrig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \""+cityName+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(t.TempDir(), "ops.log")
	script := writeSpyScript(t, logFile)
	t.Setenv("GC_BEADS", "exec:"+script)

	cfg := testCityConfig(cityName, []config.Rig{
		{Name: "myrig", Path: rigPath, Prefix: "mr"},
	})
	if err := startBeadsLifecycle(cityPath, cityName, cfg, io.Discard); err != nil {
		t.Fatalf("startBeadsLifecycle: %v", err)
	}

	ops := readOpLog(t, logFile)
	if len(ops) < 2 {
		t.Fatalf("expected at least 2 ops, got %d: %v", len(ops), ops)
	}

	// First op must be start.
	if !strings.HasPrefix(ops[0], "start") {
		t.Fatalf("first op should be start, got: %s", ops[0])
	}

	// All subsequent ops must be init.
	for i := 1; i < len(ops); i++ {
		if !strings.HasPrefix(ops[i], "init ") {
			t.Fatalf("op[%d] should be init, got: %s", i, ops[i])
		}
	}
}

// TestLifecycleCoordination_StopOrder verifies that stop is called
// during gc stop via shutdownBeadsProvider.
func TestLifecycleCoordination_StopOrder(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \"stoptest\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(t.TempDir(), "ops.log")
	script := writeSpyScript(t, logFile)
	t.Setenv("GC_BEADS", "exec:"+script)

	if err := shutdownBeadsProvider(cityPath); err != nil {
		t.Fatalf("shutdownBeadsProvider: %v", err)
	}

	ops := readOpLog(t, logFile)
	assertSingleStopWithBenignNoise(t, ops)
}

// TestLifecycleCoordination_InitDirIfReady_BdDeferred verifies that the bd
// provider returns deferred=true (Dolt isn't running during gc init).
// With the exec: mapping, bd → gc-beads-bd script → probe exits 2 (GC_BEADS_SKIP=1)
// → deferred=true.
func TestLifecycleCoordination_InitDirIfReady_BdDeferred(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	materializeBuiltinPacksForTest(t, dir)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_BEADS_SKIP", "skip")

	deferred, err := initDirIfReady(dir, dir, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !deferred {
		t.Fatal("expected bd provider to defer init")
	}

	// Deferred bd init only ensures the .beads directory exists; bd's
	// proxied-server init (run at gc start) writes metadata.json, config.yaml,
	// client info, and project_id itself. gascity no longer seeds any of that.
	info, err := os.Stat(filepath.Join(dir, ".beads"))
	if err != nil {
		t.Fatalf("stat deferred .beads: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("deferred .beads is not a directory")
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("deferred .beads perm = %o, want 0700", perm)
	}
}

func TestLifecycleCoordination_InitDirIfReadySkipsProviderForPostgresCityAndRig(t *testing.T) {
	cityPath, rigPath, _ := writeInheritedCityPostgresRigFixture(t, "")
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)

	originalEnsure := initDirIfReadyEnsureBeadsProvider
	t.Cleanup(func() {
		initDirIfReadyEnsureBeadsProvider = originalEnsure
	})

	var ensureCalls int
	initDirIfReadyEnsureBeadsProvider = func(string) error {
		ensureCalls++
		return fmt.Errorf("managed Dolt provider start should not run for postgres-backed scopes")
	}

	deferred, err := initDirIfReady(cityPath, cityPath, "gc")
	if err != nil {
		t.Fatalf("initDirIfReady(city) error = %v, want nil", err)
	}
	if deferred {
		t.Fatal("initDirIfReady(city) deferred = true, want false")
	}
	assertHooksAbsent(t, cityPath, "after postgres city init")

	deferred, err = initDirIfReady(cityPath, rigPath, "pg")
	if err != nil {
		t.Fatalf("initDirIfReady(rig) error = %v, want nil", err)
	}
	if deferred {
		t.Fatal("initDirIfReady(rig) deferred = true, want false")
	}
	assertHooksAbsent(t, rigPath, "after postgres rig add")

	if ensureCalls != 0 {
		t.Fatalf("managed Dolt provider start calls = %d, want 0", ensureCalls)
	}
}

func TestLifecycleCoordination_InitDirIfReady_PropagatesManagedDoltInitFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	materializeBuiltinPacksForTest(t, dir)
	t.Setenv("GC_BEADS", "bd")

	originalEnsure := initDirIfReadyEnsureBeadsProvider
	originalInitAndHook := initDirIfReadyInitAndHookDir
	t.Cleanup(func() {
		initDirIfReadyEnsureBeadsProvider = originalEnsure
		initDirIfReadyInitAndHookDir = originalInitAndHook
	})

	var ensureCalls int
	initDirIfReadyEnsureBeadsProvider = func(_ string) error {
		ensureCalls++
		return nil
	}

	var initCalls int
	initDirIfReadyInitAndHookDir = func(_, _, _ string) error {
		initCalls++
		return fmt.Errorf("exec beads init: signal: terminated")
	}

	deferred, err := initDirIfReady(dir, dir, "gc")
	if err == nil {
		t.Fatal("initDirIfReady() error = nil, want propagated initAndHookDir failure")
	}
	if deferred {
		t.Fatal("initDirIfReady() deferred = true, want false")
	}
	if ensureCalls != 1 {
		t.Fatalf("ensureBeadsProvider calls = %d, want 1", ensureCalls)
	}
	if initCalls != 1 {
		t.Fatalf("initAndHookDir calls = %d, want 1 (no retry)", initCalls)
	}
}

func TestLifecycleCoordination_InitDirIfReady_PropagatesManagedDoltSchemaError(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	materializeBuiltinPacksForTest(t, dir)
	t.Setenv("GC_BEADS", "bd")

	originalEnsure := initDirIfReadyEnsureBeadsProvider
	originalInitAndHook := initDirIfReadyInitAndHookDir
	t.Cleanup(func() {
		initDirIfReadyEnsureBeadsProvider = originalEnsure
		initDirIfReadyInitAndHookDir = originalInitAndHook
	})

	initDirIfReadyEnsureBeadsProvider = func(_ string) error { return nil }

	var initCalls int
	initDirIfReadyInitAndHookDir = func(_, _, _ string) error {
		initCalls++
		return fmt.Errorf("bd list: exit status 1: table not found: issues")
	}

	deferred, err := initDirIfReady(dir, dir, "gc")
	if err == nil {
		t.Fatal("initDirIfReady() error = nil, want propagated schema error")
	}
	if deferred {
		t.Fatal("initDirIfReady() deferred = true, want false")
	}
	if initCalls != 1 {
		t.Fatalf("initAndHookDir calls = %d, want 1 (no retry)", initCalls)
	}
}

func TestLifecycleCoordination_InitDirIfReady_DoesNotRetryNonManagedProviderFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(t.TempDir(), "ops.log")
	script := writeSpyScript(t, logFile)
	t.Setenv("GC_BEADS", "exec:"+script)

	originalEnsure := initDirIfReadyEnsureBeadsProvider
	originalInitAndHook := initDirIfReadyInitAndHookDir
	t.Cleanup(func() {
		initDirIfReadyEnsureBeadsProvider = originalEnsure
		initDirIfReadyInitAndHookDir = originalInitAndHook
	})

	var ensureCalls int
	initDirIfReadyEnsureBeadsProvider = func(_ string) error {
		ensureCalls++
		return nil
	}

	var initCalls int
	initDirIfReadyInitAndHookDir = func(_, _, _ string) error {
		initCalls++
		return fmt.Errorf("exec beads init: signal: terminated")
	}

	deferred, err := initDirIfReady(dir, dir, "gc")
	if err == nil {
		t.Fatal("initDirIfReady() error = nil, want non-managed provider failure")
	}
	if deferred {
		t.Fatal("initDirIfReady() deferred = true, want false")
	}
	if ensureCalls != 1 {
		t.Fatalf("ensureBeadsProvider calls = %d, want 1", ensureCalls)
	}
	if initCalls != 1 {
		t.Fatalf("initAndHookDir calls = %d, want 1", initCalls)
	}
}

func TestLifecycleCoordination_InitDirIfReady_BdDeferredPreservesExistingDoltDatabaseWhenCanonicalUnknown(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "metadata.json"), []byte(`{"backend":"dolt","database":"dolt","dolt_mode":"server","dolt_database":"gascity"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	materializeBuiltinPacksForTest(t, dir)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_BEADS_SKIP", "skip")

	deferred, err := initDirIfReady(dir, dir, "gc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !deferred {
		t.Fatal("expected bd provider to defer init")
	}

	metaData, err := os.ReadFile(filepath.Join(dir, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if got := strings.TrimSpace(fmt.Sprint(meta["dolt_database"])); got != "gascity" {
		t.Fatalf("dolt_database = %q, want %q", got, "gascity")
	}
}

func TestSeedDeferredManagedBeadsPreservesExplicitRigConfig(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: fe
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.host: rig-db.example.com
dolt.port: 4406
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	seedDeferredManagedBeads(cityDir, rigDir, "fe", "fe")

	configData, err := os.ReadFile(filepath.Join(rigDir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	cfg := string(configData)
	for _, needle := range []string{
		"gc.endpoint_origin: explicit",
		"gc.endpoint_status: verified",
		"dolt.host: rig-db.example.com",
		"dolt.port: 4406",
	} {
		if !strings.Contains(cfg, needle) {
			t.Fatalf("config missing %q:\n%s", needle, cfg)
		}
	}
}

func TestSeedDeferredManagedBeadsPreservesExistingDoltDatabaseWhenCanonicalUnknown(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "metadata.json"), []byte(`{"backend":"dolt","database":"dolt","dolt_mode":"server","dolt_database":"gascity"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	seedDeferredManagedBeads(dir, dir, "gc", "")

	metaData, err := os.ReadFile(filepath.Join(dir, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if got := strings.TrimSpace(fmt.Sprint(meta["dolt_database"])); got != "gascity" {
		t.Fatalf("dolt_database = %q, want %q", got, "gascity")
	}
}

// TestSeedDeferredManagedBeadsCreatesDirWith0700 asserts that fresh .beads
// directories created during deferred init satisfy bd's recommended 0700
// permission. Wider perms cause bd to emit a warning on every call, which
// spams agent pod output and is treated as a hard failure by the
// controller's collectAssignedWorkBeads stderr-as-error path (hl-39km).
func TestSeedDeferredManagedBeadsCreatesDirWith0700(t *testing.T) {
	dir := t.TempDir()

	seedDeferredManagedBeads(dir, dir, "gc", "test")

	info, err := os.Stat(filepath.Join(dir, ".beads"))
	if err != nil {
		t.Fatalf("stat .beads: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf(".beads perm = %o, want 0700", perm)
	}
}

// TestSeedDeferredManagedBeadsTightensExistingDir asserts that pre-existing
// .beads directories with looser permissions are tightened on next call.
// Required because persistent volumes carry directories created by older
// gascity versions that used 0o755.
func TestSeedDeferredManagedBeadsTightensExistingDir(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Force 0755 explicitly — the test process umask may have reduced it.
	if err := os.Chmod(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	seedDeferredManagedBeads(dir, dir, "gc", "test")

	info, err := os.Stat(beadsDir)
	if err != nil {
		t.Fatalf("stat .beads: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf(".beads perm = %o, want 0700", perm)
	}
}
