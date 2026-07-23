package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	bdpack "github.com/gastownhall/gascity/examples/bd"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"gopkg.in/yaml.v3"
)

func freeLoopbackPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() {
		_ = listener.Close()
	}()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr = %T, want *net.TCPAddr", listener.Addr())
	}
	return strconv.Itoa(addr.Port)
}

func setScopedBeadsProviderForTest(t *testing.T, scopeRoot, provider string) {
	t.Helper()
	t.Setenv("GC_BEADS", provider)
	t.Setenv("GC_BEADS_SCOPE_ROOT", scopeRoot)
}

func mustProviderLifecycleProcessEnv(t *testing.T, cityPath, provider string) []string {
	t.Helper()
	env, err := providerLifecycleProcessEnvWithError(cityPath, provider)
	if err != nil {
		t.Fatalf("providerLifecycleProcessEnvWithError: %v", err)
	}
	return env
}

// TestEnsureBeadsProvider_file verifies that file provider is a no-op.
func TestEnsureBeadsProvider_file(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SKIP", "skip")
	if err := ensureBeadsProvider(t.TempDir()); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestEnsureBeadsProvider_exec calls script with ensure-ready, exit 2 = no-op.
func TestEnsureBeadsProvider_exec(t *testing.T) {
	dir := t.TempDir()
	script := writeTestScript(t, "ensure-ready", 2, "")
	setScopedBeadsProviderForTest(t, dir, "exec:"+script)
	if err := ensureBeadsProvider(dir); err != nil {
		t.Fatalf("expected nil for exit 2, got %v", err)
	}
}

func TestProviderLifecycleProcessEnvProjectsCanonicalDoltPaths(t *testing.T) {
	cityPath := t.TempDir()
	wantCityPath := normalizePathForCompare(cityPath)
	t.Setenv("GC_PACK_STATE_DIR", "/tmp/wrong-pack")
	t.Setenv("GC_BEADS_DATA_DIR", "/tmp/wrong-data")
	t.Setenv("GC_BEADS_LOG_FILE", "/tmp/wrong-log")
	t.Setenv("GC_BEADS_STATE_FILE", "/tmp/wrong-state")
	t.Setenv("GC_BEADS_PID_FILE", "/tmp/wrong-pid")
	t.Setenv("GC_BEADS_LOCK_FILE", "/tmp/wrong-lock")
	t.Setenv("GC_BEADS_CONFIG_FILE", "/tmp/wrong-config")

	envEntries := mustProviderLifecycleProcessEnv(t, cityPath, "exec:"+gcBeadsBdScriptPath(cityPath))
	env := map[string]string{}
	for _, entry := range envEntries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}

	packStateDir := citylayout.PackStateDir(wantCityPath, "dolt")
	want := map[string]string{
		"GC_PACK_STATE_DIR":    packStateDir,
		"GC_BEADS_DATA_DIR":    filepath.Join(wantCityPath, ".beads", "dolt"),
		"GC_BEADS_LOG_FILE":    filepath.Join(packStateDir, "dolt.log"),
		"GC_BEADS_STATE_FILE":  filepath.Join(packStateDir, "dolt-provider-state.json"),
		"GC_BEADS_PID_FILE":    filepath.Join(packStateDir, "dolt.pid"),
		"GC_BEADS_LOCK_FILE":   filepath.Join(packStateDir, "dolt.lock"),
		"GC_BEADS_CONFIG_FILE": filepath.Join(packStateDir, "dolt-config.yaml"),
	}
	for key, expected := range want {
		if got := env[key]; got != expected {
			t.Fatalf("providerLifecycleProcessEnv()[%s] = %q, want %q", key, got, expected)
		}
	}
}

func TestProviderLifecycleProcessEnvCanonicalizesSymlinkedCityPath(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	if err := os.MkdirAll(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(root, "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skip("symlinks not supported")
	}
	aliasCity := filepath.Join(aliasParent, "bright-lights")
	realCity := filepath.Join(realParent, "bright-lights")
	if err := os.MkdirAll(realCity, 0o755); err != nil {
		t.Fatal(err)
	}
	wantCityPath := normalizePathForCompare(realCity)

	envEntries := mustProviderLifecycleProcessEnv(t, aliasCity, "exec:"+gcBeadsBdScriptPath(aliasCity))
	env := map[string]string{}
	for _, entry := range envEntries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}

	packStateDir := citylayout.PackStateDir(wantCityPath, "dolt")
	want := map[string]string{
		"GC_CITY":              wantCityPath,
		"GC_CITY_PATH":         wantCityPath,
		"GC_CITY_RUNTIME_DIR":  filepath.Join(wantCityPath, ".gc", "runtime"),
		"GC_PACK_STATE_DIR":    packStateDir,
		"GC_BEADS_DATA_DIR":    filepath.Join(wantCityPath, ".beads", "dolt"),
		"GC_BEADS_STATE_FILE":  filepath.Join(packStateDir, "dolt-provider-state.json"),
		"GC_BEADS_CONFIG_FILE": filepath.Join(packStateDir, "dolt-config.yaml"),
	}
	for key, expected := range want {
		if got := env[key]; got != expected {
			t.Fatalf("providerLifecycleProcessEnv()[%s] = %q, want %q", key, got, expected)
		}
	}
}

func TestProviderLifecycleProcessEnvProjectsResolvedGCBin(t *testing.T) {
	cityPath := t.TempDir()
	t.Setenv("GC_BIN", "/tmp/wrong-gc")
	oldResolve := resolveProviderLifecycleGCBinary
	resolveProviderLifecycleGCBinary = func() string { return "/opt/gc/bin/gc" }
	t.Cleanup(func() { resolveProviderLifecycleGCBinary = oldResolve })

	envEntries := mustProviderLifecycleProcessEnv(t, cityPath, "exec:"+gcBeadsBdScriptPath(cityPath))
	env := map[string]string{}
	for _, entry := range envEntries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	if got := env["GC_BIN"]; got != "/opt/gc/bin/gc" {
		t.Fatalf("providerLifecycleProcessEnv()[GC_BIN] = %q, want %q", got, "/opt/gc/bin/gc")
	}
}

func TestProviderLifecycleProcessEnvPropagatesArchiveLevel(t *testing.T) {
	cityPath := t.TempDir()
	normPath := normalizePathForCompare(cityPath)

	level := 1
	cityDoltConfigs.Store(normPath, config.DoltConfig{ArchiveLevel: &level})
	t.Cleanup(func() { cityDoltConfigs.Delete(normPath) })

	envEntries := mustProviderLifecycleProcessEnv(t, cityPath, "exec:"+gcBeadsBdScriptPath(cityPath))
	env := map[string]string{}
	for _, entry := range envEntries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	if got := env["GC_BEADS_ARCHIVE_LEVEL"]; got != "1" {
		t.Fatalf("GC_BEADS_ARCHIVE_LEVEL = %q, want %q", got, "1")
	}
}

func TestProviderLifecycleProcessEnvPropagatesAutoGCEnabled(t *testing.T) {
	cityPath := t.TempDir()
	normPath := normalizePathForCompare(cityPath)

	autoGC := false
	cityDoltConfigs.Store(normPath, config.DoltConfig{AutoGCEnabled: &autoGC})
	t.Cleanup(func() { cityDoltConfigs.Delete(normPath) })

	envEntries := mustProviderLifecycleProcessEnv(t, cityPath, "exec:"+gcBeadsBdScriptPath(cityPath))
	env := map[string]string{}
	for _, entry := range envEntries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	if got := env["GC_BEADS_AUTO_GC_ENABLED"]; got != "false" {
		t.Fatalf("GC_BEADS_AUTO_GC_ENABLED = %q, want %q", got, "false")
	}
}

func TestProviderLifecycleProcessEnvOmitsAutoGCEnabledWhenNil(t *testing.T) {
	cityPath := t.TempDir()
	normPath := normalizePathForCompare(cityPath)

	cityDoltConfigs.Store(normPath, config.DoltConfig{})
	t.Cleanup(func() { cityDoltConfigs.Delete(normPath) })

	envEntries := mustProviderLifecycleProcessEnv(t, cityPath, "exec:"+gcBeadsBdScriptPath(cityPath))
	for _, entry := range envEntries {
		if strings.HasPrefix(entry, "GC_BEADS_AUTO_GC_ENABLED=") {
			t.Fatalf("GC_BEADS_AUTO_GC_ENABLED should not be set when AutoGCEnabled is nil, got %q", entry)
		}
	}
}

func TestProviderLifecycleProcessEnvPropagatesManagedDoltListenerOverrides(t *testing.T) {
	cityPath := t.TempDir()
	normPath := normalizePathForCompare(cityPath)

	cityDoltConfigs.Store(normPath, config.DoltConfig{
		ReadTimeoutMillis:  300000,
		WriteTimeoutMillis: 600000,
		MaxConnections:     1024,
	})
	t.Cleanup(func() { cityDoltConfigs.Delete(normPath) })

	envEntries := mustProviderLifecycleProcessEnv(t, cityPath, "exec:"+gcBeadsBdScriptPath(cityPath))
	env := map[string]string{}
	for _, entry := range envEntries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	for key, want := range map[string]string{
		"GC_BEADS_READ_TIMEOUT_MILLIS":  "300000",
		"GC_BEADS_WRITE_TIMEOUT_MILLIS": "600000",
		"GC_BEADS_MAX_CONNECTIONS":      "1024",
	} {
		if got := env[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestProviderLifecycleProcessEnvPropagatesDoltLockReleaseTimeout(t *testing.T) {
	cityPath := t.TempDir()
	normPath := normalizePathForCompare(cityPath)

	cityDoltConfigs.Store(normPath, config.DoltConfig{DoltLockReleaseTimeout: "90s"})
	t.Cleanup(func() { cityDoltConfigs.Delete(normPath) })

	envEntries := mustProviderLifecycleProcessEnv(t, cityPath, "exec:"+gcBeadsBdScriptPath(cityPath))
	env := map[string]string{}
	for _, entry := range envEntries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	if got := env["GC_BEADS_LOCK_RELEASE_TIMEOUT_MS"]; got != "90000" {
		t.Fatalf("GC_BEADS_LOCK_RELEASE_TIMEOUT_MS = %q, want %q", got, "90000")
	}
}

func TestProviderLifecycleProcessEnvOmitsDoltLockReleaseTimeoutWhenUnset(t *testing.T) {
	cityPath := t.TempDir()
	normPath := normalizePathForCompare(cityPath)

	cityDoltConfigs.Store(normPath, config.DoltConfig{})
	t.Cleanup(func() { cityDoltConfigs.Delete(normPath) })

	envEntries := mustProviderLifecycleProcessEnv(t, cityPath, "exec:"+gcBeadsBdScriptPath(cityPath))
	for _, entry := range envEntries {
		if strings.HasPrefix(entry, "GC_BEADS_LOCK_RELEASE_TIMEOUT_MS=") {
			t.Fatalf("GC_BEADS_LOCK_RELEASE_TIMEOUT_MS should not be set when DoltLockReleaseTimeout is empty, got %q", entry)
		}
	}
}

func TestCityDoltConfigHasLifecycleFieldsRecognizesDoltLockReleaseTimeout(t *testing.T) {
	// A city that sets only [dolt].dolt_lock_release_timeout must register
	// its dolt config — otherwise startBeadsLifecycle clears the registry
	// entry and the value never reaches providerLifecycleProcessEnv.
	if !cityDoltConfigHasLifecycleFields(config.DoltConfig{DoltLockReleaseTimeout: "90s"}) {
		t.Fatal("cityDoltConfigHasLifecycleFields must recognize DoltLockReleaseTimeout")
	}
	if cityDoltConfigHasLifecycleFields(config.DoltConfig{}) {
		t.Fatal("cityDoltConfigHasLifecycleFields must stay false for an empty config")
	}
}

func TestCityDoltConfigHasLifecycleFieldsRecognizesAutoGCEnabled(t *testing.T) {
	// A city that sets only [dolt].auto_gc_enabled must register its dolt
	// config — otherwise startBeadsLifecycle clears the registry entry and
	// the documented auto_gc_enabled=false rollback never reaches
	// providerLifecycleProcessEnv, so the shell fallback silently re-enables
	// auto-GC. Both an explicit false and an explicit true must register.
	autoGCOff := false
	if !cityDoltConfigHasLifecycleFields(config.DoltConfig{AutoGCEnabled: &autoGCOff}) {
		t.Fatal("cityDoltConfigHasLifecycleFields must recognize AutoGCEnabled=false")
	}
	autoGCOn := true
	if !cityDoltConfigHasLifecycleFields(config.DoltConfig{AutoGCEnabled: &autoGCOn}) {
		t.Fatal("cityDoltConfigHasLifecycleFields must recognize AutoGCEnabled=true")
	}
	if cityDoltConfigHasLifecycleFields(config.DoltConfig{}) {
		t.Fatal("cityDoltConfigHasLifecycleFields must stay false for an empty config")
	}
}

func TestProviderLifecycleProcessEnvOmitsArchiveLevelWhenNil(t *testing.T) {
	cityPath := t.TempDir()
	normPath := normalizePathForCompare(cityPath)

	cityDoltConfigs.Store(normPath, config.DoltConfig{})
	t.Cleanup(func() { cityDoltConfigs.Delete(normPath) })

	envEntries := mustProviderLifecycleProcessEnv(t, cityPath, "exec:"+gcBeadsBdScriptPath(cityPath))
	for _, entry := range envEntries {
		if strings.HasPrefix(entry, "GC_BEADS_ARCHIVE_LEVEL=") {
			t.Fatalf("GC_BEADS_ARCHIVE_LEVEL should not be set when ArchiveLevel is nil, got %q", entry)
		}
	}
}

func TestProviderLifecycleProcessEnvFallsBackToLaunchctlGetenvForLoglevel(t *testing.T) {
	// `gc start` runs in the user's shell, which doesn't see `launchctl
	// setenv` values. Without the fallback, GC_BEADS_LOGLEVEL set only via
	// launchctl is silently dropped between the shell and gc-beads-bd.sh,
	// so the managed dolt config gets written with `log_level: warning`.
	t.Setenv("GC_BEADS_LOGLEVEL", "")
	_ = os.Unsetenv("GC_BEADS_LOGLEVEL")

	prev := providerLifecycleLaunchctlGetenv
	providerLifecycleLaunchctlGetenv = func(key string) string {
		if key == "GC_BEADS_LOGLEVEL" {
			return "debug"
		}
		return ""
	}
	t.Cleanup(func() { providerLifecycleLaunchctlGetenv = prev })

	cityPath := t.TempDir()
	envEntries := mustProviderLifecycleProcessEnv(t, cityPath, "exec:"+gcBeadsBdScriptPath(cityPath))
	got := ""
	for _, entry := range envEntries {
		if strings.HasPrefix(entry, "GC_BEADS_LOGLEVEL=") {
			got = strings.TrimPrefix(entry, "GC_BEADS_LOGLEVEL=")
			break
		}
	}
	if got != "debug" {
		t.Fatalf("GC_BEADS_LOGLEVEL = %q, want %q (launchctl fallback should fire when os.Environ lacks it)", got, "debug")
	}
}

func TestProviderLifecycleProcessEnvPrefersOSEnvOverLaunchctlForLoglevel(t *testing.T) {
	// When a user explicitly exports GC_BEADS_LOGLEVEL in the shell, that
	// value must win over any stale launchctl-domain value.
	t.Setenv("GC_BEADS_LOGLEVEL", "trace")

	prev := providerLifecycleLaunchctlGetenv
	providerLifecycleLaunchctlGetenv = func(key string) string {
		if key == "GC_BEADS_LOGLEVEL" {
			return "debug"
		}
		return ""
	}
	t.Cleanup(func() { providerLifecycleLaunchctlGetenv = prev })

	cityPath := t.TempDir()
	envEntries := mustProviderLifecycleProcessEnv(t, cityPath, "exec:"+gcBeadsBdScriptPath(cityPath))
	got := ""
	for _, entry := range envEntries {
		if strings.HasPrefix(entry, "GC_BEADS_LOGLEVEL=") {
			got = strings.TrimPrefix(entry, "GC_BEADS_LOGLEVEL=")
			break
		}
	}
	if got != "trace" {
		t.Fatalf("GC_BEADS_LOGLEVEL = %q, want %q (os.Environ should win over launchctl)", got, "trace")
	}
}

func TestProviderLifecycleProcessEnvOmitsLoglevelWhenLaunchctlEmpty(t *testing.T) {
	// When neither os.Environ nor launchctl has GC_BEADS_LOGLEVEL, the env
	// must not contain a synthetic empty value (which would override
	// gc-beads-bd.sh's `${GC_BEADS_LOGLEVEL:-warning}` default to empty).
	t.Setenv("GC_BEADS_LOGLEVEL", "")
	_ = os.Unsetenv("GC_BEADS_LOGLEVEL")

	prev := providerLifecycleLaunchctlGetenv
	providerLifecycleLaunchctlGetenv = func(string) string { return "" }
	t.Cleanup(func() { providerLifecycleLaunchctlGetenv = prev })

	cityPath := t.TempDir()
	envEntries := mustProviderLifecycleProcessEnv(t, cityPath, "exec:"+gcBeadsBdScriptPath(cityPath))
	for _, entry := range envEntries {
		if strings.HasPrefix(entry, "GC_BEADS_LOGLEVEL=") {
			t.Fatalf("GC_BEADS_LOGLEVEL should be absent when neither os.Environ nor launchctl has it, got %q", entry)
		}
	}
}

func TestGcBeadsBdShellFallbackSanitizesArchiveLevel(t *testing.T) {
	cityPath := t.TempDir()
	materializeBuiltinPacksForTest(t, cityPath)
	scriptData, err := os.ReadFile(bundledGcBeadsBdScriptForTest(t))
	if err != nil {
		t.Fatalf("ReadFile(gc-beads-bd): %v", err)
	}
	script := string(scriptData)
	for _, forbidden := range []string{
		`--archive-level "${GC_BEADS_ARCHIVE_LEVEL:-0}"`,
		"archive_level: ${GC_BEADS_ARCHIVE_LEVEL:-0}",
	} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("gc-beads-bd shell fallback uses unsanitized archive level pattern %q", forbidden)
		}
	}
	for _, want := range []string{
		"archive_level=${GC_BEADS_ARCHIVE_LEVEL:-0}",
		"*[!0-9]*",
		"--archive-level \"$archive_level\"",
		"archive_level: $archive_level",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("gc-beads-bd shell fallback missing sanitized archive level pattern %q", want)
		}
	}
}

func TestGcBeadsBdReadOnlyFallbackNoUserDatabaseIsDiagnostic(t *testing.T) {
	cityPath := t.TempDir()
	materializeBuiltinPacksForTest(t, cityPath)
	scriptData, err := os.ReadFile(bundledGcBeadsBdScriptForTest(t))
	if err != nil {
		t.Fatalf("ReadFile(gc-beads-bd): %v", err)
	}
	prelude, _, ok := strings.Cut(string(scriptData), "# --- Main ---")
	if !ok {
		t.Fatal("gc-beads-bd script missing main marker")
	}

	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(`#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
case "$*" in
  *"sql -r csv -q SHOW DATABASES"*)
    printf 'Database\ninformation_schema\nmysql\ndolt\ndolt_cluster\nperformance_schema\nsys\n__gc_probe\n'
    exit 0
    ;;
  *"CREATE TABLE IF NOT EXISTS"*"__gc_read_only_probe"*)
    echo "unexpected write probe without a user database" >&2
    exit 2
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 2
    ;;
esac
`), 0o755); err != nil {
		t.Fatalf("WriteFile(dolt): %v", err)
	}

	harness := filepath.Join(t.TempDir(), "read-only-fallback.sh")
	body := prelude + `
GC_BIN=""
GC_BEADS_HOST=""
DOLT_PORT=3311
DOLT_USER=root
set +e
check_read_only
status=$?
set -e
printf 'status=%s\n' "$status"
`
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", harness)
	cmd.Env = append(sanitizedBaseEnv(
		"INVOCATION_FILE="+invocationFile,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	), "GC_BIN=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("check_read_only harness failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "status=2") {
		t.Fatalf("check_read_only output = %s, want diagnostic status 2", out)
	}
	if !strings.Contains(string(out), "no user database") {
		t.Fatalf("check_read_only output = %s, want no-user-database diagnostic", out)
	}
	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	if strings.Contains(string(invocation), "CREATE TABLE IF NOT EXISTS") {
		t.Fatalf("check_read_only ran write probe without user database:\n%s", invocation)
	}
}

func TestGcBeadsBdReadOnlyHelperErrorIsDiagnostic(t *testing.T) {
	cityPath := t.TempDir()
	materializeBuiltinPacksForTest(t, cityPath)
	scriptData, err := os.ReadFile(bundledGcBeadsBdScriptForTest(t))
	if err != nil {
		t.Fatalf("ReadFile(gc-beads-bd): %v", err)
	}
	prelude, _, ok := strings.Cut(string(scriptData), "# --- Main ---")
	if !ok {
		t.Fatal("gc-beads-bd script missing main marker")
	}

	gcBin := filepath.Join(t.TempDir(), "gc")
	if err := os.WriteFile(gcBin, []byte(`#!/bin/sh
set -eu
case "$1 $2" in
  "dolt-state read-only-check")
    echo "gc dolt-state read-only-check: no user database available for managed Dolt read-only probe" >&2
    exit 1
    ;;
  *)
    echo "unexpected gc command: $*" >&2
    exit 66
    ;;
esac
`), 0o755); err != nil {
		t.Fatalf("WriteFile(gc): %v", err)
	}

	harness := filepath.Join(t.TempDir(), "read-only-helper.sh")
	body := prelude + fmt.Sprintf(`
GC_BIN=%q
GC_BEADS_HOST=""
DOLT_PORT=3311
DOLT_USER=root
set +e
check_read_only
status=$?
set -e
printf 'status=%%s\n' "$status"
`, gcBin)
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", harness)
	cmd.Env = sanitizedBaseEnv("PATH=" + os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("check_read_only harness failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "status=2") {
		t.Fatalf("check_read_only output = %s, want diagnostic status 2", out)
	}
	if !strings.Contains(string(out), "no user database") {
		t.Fatalf("check_read_only output = %s, want helper diagnostic", out)
	}
}

// TestEnsureBeadsProvider_bd_skip verifies bd provider is no-op when GC_BEADS_SKIP=1.
func TestEnsureBeadsProvider_bd_skip(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	materializeBuiltinPacksForTest(t, dir)
	setScopedBeadsProviderForTest(t, dir, "bd")
	t.Setenv("GC_BEADS_SKIP", "skip")
	if err := ensureBeadsProvider(dir); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestEnsureBeadsProviderBdDoltliteDoesNotStartManagedDolt(t *testing.T) {
	dir := t.TempDir()
	script := gcBeadsBdScriptPath(dir)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "bd"
backend = "doltlite"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho unexpected managed dolt start >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	setScopedBeadsProviderForTest(t, dir, "bd")

	if cityUsesManagedDoltBeadsLifecycle(dir) {
		t.Fatal("doltlite-backed bd city should not use managed Dolt lifecycle")
	}
	if err := ensureBeadsProvider(dir); err != nil {
		t.Fatalf("ensureBeadsProvider = %v, want nil", err)
	}
}

func TestEnsureBeadsProvider_execDoesNotMaskStartErrorWithHealth(t *testing.T) {
	dir := t.TempDir()
	callLog := filepath.Join(dir, "provider.log")
	marker := filepath.Join(dir, "started")
	script := filepath.Join(dir, "provider.sh")
	content := "#!/bin/sh\n" +
		"set -eu\n" +
		"echo \"$1\" >> \"" + callLog + "\"\n" +
		"case \"${1:-}\" in\n" +
		"  start)\n" +
		"    : > \"" + marker + "\"\n" +
		"    echo 'signal: terminated' >&2\n" +
		"    exit 1\n" +
		"    ;;\n" +
		"  health)\n" +
		"    [ -f \"" + marker + "\" ]\n" +
		"    ;;\n" +
		"  *)\n" +
		"    exit 0\n" +
		"    ;;\n" +
		"esac\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	setScopedBeadsProviderForTest(t, dir, "exec:"+script)

	err := ensureBeadsProvider(dir)
	if err == nil {
		t.Fatal("ensureBeadsProvider = nil, want start error")
	}
	if !strings.Contains(err.Error(), "signal: terminated") {
		t.Fatalf("ensureBeadsProvider error = %v, want start error", err)
	}

	data, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("read call log: %v", readErr)
	}
	got := strings.Fields(string(data))
	want := []string{"start"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("provider calls = %v, want %v", got, want)
	}
}

func TestEnsureBeadsProvider_execDoesNotReclassifyProviderAfterStart(t *testing.T) {
	dir := t.TempDir()
	callLog := filepath.Join(dir, "provider.log")
	marker := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")
	script := filepath.Join(dir, "provider.sh")
	content := "#!/bin/sh\n" +
		"set -eu\n" +
		"echo \"$1\" >> \"" + callLog + "\"\n" +
		"case \"${1:-}\" in\n" +
		"  start)\n" +
		"    : > \"" + marker + "\"\n" +
		"    i=0\n" +
		"    while [ ! -f \"" + release + "\" ]; do\n" +
		"      i=$((i + 1))\n" +
		"      [ \"$i\" -le 1000 ] || exit 42\n" +
		"      sleep 0.01\n" +
		"    done\n" +
		"    echo 'signal: terminated' >&2\n" +
		"    exit 1\n" +
		"    ;;\n" +
		"  health)\n" +
		"    [ -f \"" + marker + "\" ]\n" +
		"    ;;\n" +
		"  *)\n" +
		"    exit 0\n" +
		"    ;;\n" +
		"esac\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	originalProvider, hadProvider := os.LookupEnv("GC_BEADS")
	if err := os.Setenv("GC_BEADS", "exec:"+script); err != nil {
		t.Fatalf("set GC_BEADS: %v", err)
	}
	t.Setenv("GC_BEADS_SCOPE_ROOT", dir)
	t.Cleanup(func() {
		if hadProvider {
			_ = os.Setenv("GC_BEADS", originalProvider)
			return
		}
		_ = os.Unsetenv("GC_BEADS")
	})

	releaseErr := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(marker); err == nil {
				break
			}
			if time.Now().After(deadline) {
				releaseErr <- fmt.Errorf("provider start marker was not written")
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := os.Setenv("GC_BEADS", "bd"); err != nil {
			releaseErr <- err
			return
		}
		releaseErr <- os.WriteFile(release, []byte("ok"), 0o644)
	}()

	err := ensureBeadsProvider(dir)
	if releaseErr := <-releaseErr; releaseErr != nil {
		t.Fatalf("release provider script: %v", releaseErr)
	}
	if err == nil {
		t.Fatal("ensureBeadsProvider = nil, want original start error")
	}
	if !strings.Contains(err.Error(), "signal: terminated") {
		t.Fatalf("ensureBeadsProvider error = %v, want start error", err)
	}

	data, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("read call log: %v", readErr)
	}
	got := strings.Fields(string(data))
	want := []string{"start"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("provider calls = %v, want %v", got, want)
	}
}

// TestShutdownBeadsProvider_file verifies that file provider is a no-op.
func TestShutdownBeadsProvider_file(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SKIP", "skip")
	if err := shutdownBeadsProvider(t.TempDir()); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestShutdownBeadsProvider_exec calls script with shutdown, exit 2 = no-op.
func TestShutdownBeadsProvider_exec(t *testing.T) {
	dir := t.TempDir()
	script := writeTestScript(t, "shutdown", 2, "")
	setScopedBeadsProviderForTest(t, dir, "exec:"+script)
	if err := shutdownBeadsProvider(dir); err != nil {
		t.Fatalf("expected nil for exit 2, got %v", err)
	}
}

// TestShutdownBeadsProvider_bd_skip verifies bd provider is no-op when GC_BEADS_SKIP=1.
func TestShutdownBeadsProvider_bd_skip(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	materializeBuiltinPacksForTest(t, dir)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_BEADS_SCOPE_ROOT", dir)
	t.Setenv("GC_BEADS_SKIP", "skip")
	if err := shutdownBeadsProvider(dir); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestInitBeadsForDir_file verifies that unmarked file cities stay in legacy shared mode.
func TestInitBeadsForDir_file(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SKIP", "skip")
	cityDir := t.TempDir()
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityDir)
	if err := initBeadsForDir(cityDir, cityDir, "test", "test"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if fileStoreUsesScopedRoots(cityDir) {
		t.Fatal("unmarked file city should remain legacy-shared")
	}
	if _, err := os.Stat(filepath.Join(cityDir, ".gc", "beads.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy file city should not create beads.json on init, stat err = %v", err)
	}
	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	list, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty legacy store, got %#v", list)
	}
}

func TestInitBeadsForDir_fileScopedRigCreatesStore(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SKIP", "skip")
	cityDir := t.TempDir()
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityDir)
	rigDir := filepath.Join(t.TempDir(), "rig1")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := initBeadsForDir(cityDir, rigDir, "test", "test"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".gc", "beads.json")); err != nil {
		t.Fatalf("expected scoped rig file store bootstrap, stat err = %v", err)
	}
	store, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	if _, err := store.Create(beads.Bead{Title: "rig bead", Type: "task"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func TestInitBeadsForDir_fileLegacyRigPreservesSharedCityStore(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SKIP", "skip")
	cityDir := t.TempDir()
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityDir)
	rigDir := filepath.Join(t.TempDir(), "rig1")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := openScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("openScopeLocalFileStore(city): %v", err)
	}
	if err := initBeadsForDir(cityDir, rigDir, "test", "test"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".gc", "beads.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy shared file city should not create rig store, stat err = %v", err)
	}
	if fileStoreUsesScopedRoots(cityDir) {
		t.Fatal("legacy shared file city should not be marked scoped")
	}
}

func writeMinimalCityToml(t *testing.T, cityDir string) {
	t.Helper()
	content := `[workspace]
name = "demo"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestInitBeadsForDir_exec calls script with init <dir> <prefix> <dolt_database>.
func TestInitBeadsForDir_exec(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	script := writeTestScript(t, "init", 2, "")
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityDir)
	if err := initBeadsForDir(cityDir, cityDir, "prefix", "prefix"); err != nil {
		t.Fatalf("expected nil for exit 2, got %v", err)
	}
}

func TestInitBeadsForDir_execPassesCanonicalDoltDatabase(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	logFile := filepath.Join(t.TempDir(), "args.log")
	script := filepath.Join(t.TempDir(), "record-args.sh")
	content := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" > %q\nexit 0\n", logFile)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityDir)
	if err := initBeadsForDir(cityDir, cityDir, "gc", "gascity"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	want := "init " + cityDir + " gc gascity"
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("script args = %q, want %q", got, want)
	}
}

// TestInitBeadsForDirExecSetsBEADSDIR exercises the controller-side exec paths
// that invoke bd init directly and asserts BEADS_DIR=<dir>/.beads is present in
// the subprocess env. The k8s scoped path sets BEADS_DIR inside the provider
// script itself; that behavior is covered by internal/runtime/k8s tests.
// Regression for #399.
func TestInitBeadsForDirExecSetsBEADSDIR(t *testing.T) {
	for _, tc := range []struct {
		name       string
		scriptBase string
		// cityToml uses dolt/rig config appropriate for the exec branch.
		cityToml func(rigRel string) string
	}{
		{
			name:       "gc-beads-bd canonical",
			scriptBase: "gc-beads-bd",
			cityToml: func(rigRel string) string {
				return "[workspace]\nname = \"demo\"\n\n[[rigs]]\nname = \"r\"\npath = \"" + rigRel + "\"\nprefix = \"rg\"\n"
			},
		},
		{
			name:       "generic legacy exec",
			scriptBase: "record-env",
			cityToml: func(rigRel string) string {
				return "[workspace]\nname = \"demo\"\n\n[[rigs]]\nname = \"r\"\npath = \"" + rigRel + "\"\nprefix = \"rg\"\n"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			rigDir := filepath.Join(cityDir, "r")
			if err := os.MkdirAll(rigDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(tc.cityToml("r")), 0o644); err != nil {
				t.Fatal(err)
			}
			logFile := filepath.Join(t.TempDir(), "env.log")
			script := filepath.Join(t.TempDir(), tc.scriptBase)
			content := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = init ]; then printf '%%s\\n' \"${BEADS_DIR:-<unset>}\" > %q; fi\nexit 0\n", logFile)
			if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
				t.Fatal(err)
			}

			t.Setenv("GC_BEADS", "exec:"+script)
			if err := initBeadsForDir(cityDir, rigDir, "rg", "rg-db"); err != nil {
				t.Fatalf("initBeadsForDir: %v", err)
			}

			data, err := os.ReadFile(logFile)
			if err != nil {
				t.Fatalf("read env log: %v", err)
			}
			want := filepath.Join(rigDir, ".beads")
			if got := strings.TrimSpace(string(data)); got != want {
				t.Fatalf("BEADS_DIR = %q, want %q (bd init without BEADS_DIR creates .git as a side effect)", got, want)
			}
		})
	}
}

func TestInitBeadsForDirCanonicalRigIgnoresUnresolvableCityPostgres(t *testing.T) {
	clearAmbientPostgresEnv(t)

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "canonical-dolt")
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[[rigs]]
name = "canonical-dolt"
path = "rigs/canonical-dolt"
prefix = "cd"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "metadata.json"), []byte(`{"database":"beads","backend":"postgres","postgres_host":"db.example.test","postgres_port":"5432","postgres_user":"bd","postgres_database":"beads_pg"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: cd
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: rig-db.example.test
dolt.port: 4407
`), 0o644); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(t.TempDir(), "env.log")
	script := filepath.Join(t.TempDir(), "gc-beads-bd")
	content := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = init ]; then printf '%%s|%%s|%%s|%%s|%%s\\n' \"${BEADS_DIR:-}\" \"${GC_BEADS_HOST:-}\" \"${GC_BEADS_PORT:-}\" \"${BEADS_POSTGRES_HOST:-}\" \"${BEADS_DOLT_AUTO_START:-}\" > %q; fi\nexit 0\n", logFile)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityDir)

	if err := initBeadsForDir(cityDir, rigDir, "cd", "cd"); err != nil {
		t.Fatalf("initBeadsForDir: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read env log: %v", err)
	}
	parts := strings.Split(strings.TrimSpace(string(data)), "|")
	if len(parts) != 5 {
		t.Fatalf("env log = %q, want beads_dir|host|port|postgres_host|auto_start", string(data))
	}
	if got, want := parts[0], filepath.Join(rigDir, ".beads"); got != want {
		t.Fatalf("BEADS_DIR = %q, want %q", got, want)
	}
	if got := parts[1]; got != "rig-db.example.test" {
		t.Fatalf("GC_BEADS_HOST = %q, want rig-db.example.test", got)
	}
	if got := parts[2]; got != "4407" {
		t.Fatalf("GC_BEADS_PORT = %q, want 4407", got)
	}
	if got := parts[3]; got != "" {
		t.Fatalf("BEADS_POSTGRES_HOST = %q, want empty for independent Dolt rig init", got)
	}
	if got := parts[4]; got != "0" {
		t.Fatalf("BEADS_DOLT_AUTO_START = %q, want 0", got)
	}
}

func TestInitBeadsForDirCanonicalRigClearsResolvableCityPostgres(t *testing.T) {
	clearAmbientPostgresEnv(t)

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "canonical-dolt")
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[[rigs]]
name = "canonical-dolt"
path = "rigs/canonical-dolt"
prefix = "cd"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "metadata.json"), []byte(`{"database":"beads","backend":"postgres","postgres_host":"db.example.test","postgres_port":"5432","postgres_user":"bd","postgres_database":"beads_pg"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", ".env"), []byte("BEADS_POSTGRES_PASSWORD=citypw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: cd
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: rig-db.example.test
dolt.port: 4407
`), 0o644); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(t.TempDir(), "env.log")
	script := filepath.Join(t.TempDir(), "gc-beads-bd")
	content := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = init ]; then printf '%%s|%%s|%%s|%%s|%%s|%%s\\n' \"${BEADS_DIR:-}\" \"${GC_BEADS_HOST:-}\" \"${GC_BEADS_PORT:-}\" \"${BEADS_DOLT_SERVER_HOST:-}\" \"${BEADS_POSTGRES_HOST:-}\" \"${BEADS_POSTGRES_PASSWORD:-}\" > %q; fi\nexit 0\n", logFile)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityDir)

	if err := initBeadsForDir(cityDir, rigDir, "cd", "cd"); err != nil {
		t.Fatalf("initBeadsForDir: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read env log: %v", err)
	}
	parts := strings.Split(strings.TrimSpace(string(data)), "|")
	if len(parts) != 6 {
		t.Fatalf("env log = %q, want beads_dir|host|port|beads_host|postgres_host|postgres_password", string(data))
	}
	if got, want := parts[0], filepath.Join(rigDir, ".beads"); got != want {
		t.Fatalf("BEADS_DIR = %q, want %q", got, want)
	}
	if got := parts[1]; got != "rig-db.example.test" {
		t.Fatalf("GC_BEADS_HOST = %q, want rig-db.example.test", got)
	}
	if got := parts[2]; got != "4407" {
		t.Fatalf("GC_BEADS_PORT = %q, want 4407", got)
	}
	if got := parts[3]; got != "rig-db.example.test" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want rig-db.example.test", got)
	}
	if got := parts[4]; got != "" {
		t.Fatalf("BEADS_POSTGRES_HOST = %q, want empty for independent Dolt rig init", got)
	}
	if got := parts[5]; got != "" {
		t.Fatalf("BEADS_POSTGRES_PASSWORD = %q, want empty for independent Dolt rig init", got)
	}
}

func TestInitBeadsForDirExecWithoutCityPathPreservesAmbientEnv(t *testing.T) {
	rigDir := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "env.log")
	script := filepath.Join(t.TempDir(), "record-env")
	content := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = init ]; then printf '%%s|%%s\\n' \"${GC_BEADS_HOST:-}\" \"${BEADS_DIR:-<unset>}\" > %q; fi\nexit 0\n", logFile)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_HOST", "ambient-dolt")
	if err := initBeadsForDir("", rigDir, "rg", ""); err != nil {
		t.Fatalf("initBeadsForDir: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read env log: %v", err)
	}
	parts := strings.Split(strings.TrimSpace(string(data)), "|")
	if len(parts) != 2 {
		t.Fatalf("env log = %q, want host|beads_dir", string(data))
	}
	if got := parts[0]; got != "ambient-dolt" {
		t.Fatalf("GC_BEADS_HOST = %q, want ambient-dolt", got)
	}
	if got, want := parts[1], filepath.Join(rigDir, ".beads"); got != want {
		t.Fatalf("BEADS_DIR = %q, want %q", got, want)
	}
}

func TestInitBeadsForDirExecPreventsStrayGitInit(t *testing.T) {
	script := filepath.Join(t.TempDir(), "bd-like-provider.sh")
	content := `#!/bin/sh
set -eu
op="$1"
shift
case "$op" in
  init)
    dir="$1"
    mkdir -p "$dir/.beads"
    if [ -z "${BEADS_DIR:-}" ]; then
      mkdir -p "$dir/.git"
    fi
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	rawDir := t.TempDir()
	rawCmd := exec.Command(script, "init", rawDir, "raw")
	rawCmd.Env = sanitizedBaseEnv()
	rawOut, err := rawCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("direct provider init failed: %v\n%s", err, rawOut)
	}
	if _, err := os.Stat(filepath.Join(rawDir, ".beads")); err != nil {
		t.Fatalf("direct provider init did not create .beads: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rawDir, ".git")); err != nil {
		t.Fatalf("direct provider init did not emulate stray .git creation: %v", err)
	}

	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+script)
	if err := initBeadsForDir(cityDir, rigDir, "fe", "frontend-db"); err != nil {
		t.Fatalf("initBeadsForDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".beads")); err != nil {
		t.Fatalf("initBeadsForDir did not create .beads: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("initBeadsForDir should prevent stray .git creation, stat err = %v", err)
	}
}

func TestRunProviderOpStripsAmbientGCDoltSkip(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	logFile := filepath.Join(t.TempDir(), "env.log")
	script := filepath.Join(t.TempDir(), "record-env.sh")
	content := fmt.Sprintf(`#!/bin/sh
printf '%%s|%%s|%%s|%%s
' "${GC_BEADS_SKIP:-}" "${GC_BEADS_HOST:-}" "${GC_BEADS_PORT:-}" "${GC_CITY_PATH:-}" > %q
exit 0
`, logFile)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityDir)
	t.Setenv("GC_BEADS_SKIP", "skip")

	if err := runProviderOp(script, cityDir, "init", cityDir, "gc", "hq"); err != nil {
		t.Fatalf("runProviderOp: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read env log: %v", err)
	}
	parts := strings.Split(strings.TrimSpace(string(data)), "|")
	if len(parts) != 4 {
		t.Fatalf("captured env = %q, want 4 fields", strings.TrimSpace(string(data)))
	}
	if parts[0] != "" {
		t.Fatalf("GC_BEADS_SKIP leaked into provider env: %q", parts[0])
	}
	if parts[3] != cityDir {
		t.Fatalf("GC_CITY_PATH = %q, want %q", parts[3], cityDir)
	}
}

func TestInitBeadsForDirBuildsCanonicalBdInitProviderOp(t *testing.T) {
	tests := []struct {
		name       string
		provider   func(string) string
		wantScript func(string) string
	}{
		{
			name:       "logical bd uses the stable city wrapper",
			provider:   func(string) string { return "bd" },
			wantScript: gcBeadsBdScriptPath,
		},
		{
			name: "explicit canonical wrapper keeps its configured path",
			provider: func(cityDir string) string {
				return "exec:" + filepath.Join(cityDir, "custom", "gc-beads-bd")
			},
			wantScript: func(cityDir string) string {
				return filepath.Join(cityDir, "custom", "gc-beads-bd")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityDir := t.TempDir()
			cityConfig := fmt.Sprintf(`[workspace]
name = "demo"

[beads]
provider = %q
`, tt.provider(cityDir))
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityConfig), 0o644); err != nil {
				t.Fatal(err)
			}

			stopAfterCapture := errors.New("stop after capturing provider op")
			var calls int
			var gotScript string
			var gotEnv, gotArgs []string
			execute := func(script string, environ []string, args ...string) error {
				calls++
				gotScript = script
				gotEnv = append([]string(nil), environ...)
				gotArgs = append([]string(nil), args...)
				return stopAfterCapture
			}

			err := initBeadsForDirWithExecutor(cityDir, cityDir, "gc", "hq", execute)
			if !errors.Is(err, stopAfterCapture) {
				t.Fatalf("initBeadsForDirWithExecutor error = %v, want %v", err, stopAfterCapture)
			}
			if calls != 1 {
				t.Fatalf("provider calls = %d, want 1", calls)
			}
			if got, want := gotScript, tt.wantScript(cityDir); got != want {
				t.Fatalf("script = %q, want %q", got, want)
			}
			if want := []string{"init", cityDir, "gc", "hq"}; !reflect.DeepEqual(gotArgs, want) {
				t.Fatalf("args = %#v, want %#v", gotArgs, want)
			}

			env := runtimeEnvEntriesToMap(gotEnv)
			for key, want := range map[string]string{
				"GC_CITY_PATH":        cityDir,
				"GC_CITY_RUNTIME_DIR": filepath.Join(cityDir, ".gc", "runtime"),
				"GC_PACK_STATE_DIR":   citylayout.PackStateDir(cityDir, "dolt"),
				"GC_BEADS_DATA_DIR":   filepath.Join(cityDir, ".beads", "dolt"),
				"BEADS_DIR":           filepath.Join(cityDir, ".beads"),
			} {
				if got := env[key]; got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
		})
	}
}

func TestInitBeadsForDir_execOmitsCanonicalDoltDatabaseWhenUnknown(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	logFile := filepath.Join(t.TempDir(), "args.log")
	script := filepath.Join(t.TempDir(), "record-args.sh")
	content := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" > %q\nexit 0\n", logFile)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityDir)
	if err := initBeadsForDir(cityDir, cityDir, "gc", ""); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	want := "init " + cityDir + " gc"
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("script args = %q, want %q", got, want)
	}
}

// TestInitBeadsForDir_bd_skip verifies bd provider is no-op when GC_BEADS_SKIP=1.
func TestInitBeadsForDirExecGcBeadsBdPassesComputedCanonicalDoltDatabase(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	logFile := filepath.Join(t.TempDir(), "args.log")
	script := filepath.Join(t.TempDir(), "gc-beads-bd")
	content := fmt.Sprintf(`#!/bin/sh
set -eu
op="$1"
shift || true
case "$op" in
  init)
    printf '%%s\n' "$*" > %q
    exit 0
    ;;
  list)
    printf '[]\n'
    exit 0
    ;;
  *)
    exit 2
    ;;
esac
`, logFile)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityDir)
	if err := initBeadsForDir(cityDir, cityDir, "gc", ""); err != nil {
		t.Fatalf("initBeadsForDir: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	want := cityDir + " gc hq"
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("script args = %q, want %q", got, want)
	}
}

func TestInitBeadsForDir_bd_skip(t *testing.T) {
	dir := t.TempDir()
	writeMinimalCityToml(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	materializeBuiltinPacksForTest(t, dir)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_BEADS_SCOPE_ROOT", dir)
	t.Setenv("GC_BEADS_SKIP", "skip")
	if err := initBeadsForDir(dir, dir, "test", "test"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestRunProviderOp_exit2 verifies exit 2 is treated as success (not needed).
func TestRunProviderOp_exit2(t *testing.T) {
	script := writeTestScript(t, "", 2, "")
	if err := runProviderOp(script, "", "ensure-ready"); err != nil {
		t.Fatalf("expected nil for exit 2, got %v", err)
	}
}

// TestRunProviderOp_exit0 verifies exit 0 is success.
func TestRunProviderOp_exit0(t *testing.T) {
	script := writeTestScript(t, "", 0, "")
	if err := runProviderOp(script, "", "ensure-ready"); err != nil {
		t.Fatalf("expected nil for exit 0, got %v", err)
	}
}

// TestRunProviderOp_error verifies exit 1 propagates the error with stderr.
func TestRunProviderOp_error(t *testing.T) {
	script := writeTestScript(t, "", 1, "server crashed")
	err := runProviderOp(script, "", "ensure-ready")
	if err == nil {
		t.Fatal("expected error for exit 1")
	}
	if got := err.Error(); got != "exec beads ensure-ready: server crashed" {
		t.Fatalf("unexpected error message: %s", got)
	}
}

// TestRunProviderOp_errorNoStderr verifies exit 1 with no stderr uses exec error.
func TestRunProviderOp_errorNoStderr(t *testing.T) {
	script := writeTestScript(t, "", 1, "")
	err := runProviderOp(script, "", "shutdown")
	if err == nil {
		t.Fatal("expected error for exit 1")
	}
	if got := err.Error(); got == "" {
		t.Fatal("expected non-empty error")
	}
}

// TestRunProviderOp_setsCityRuntimeEnv verifies city runtime env vars are set in the script env.
func TestRunProviderOp_setsCityRuntimeEnv(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "check-env.sh")
	content := "#!/bin/sh\nif [ \"$GC_CITY\" = \"" + dir + "\" ] && [ \"$GC_CITY_PATH\" = \"" + dir + "\" ] && [ \"$GC_CITY_RUNTIME_DIR\" = \"" + filepath.Join(dir, ".gc", "runtime") + "\" ]; then exit 0; else exit 1; fi\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runProviderOp(script, dir, "health"); err != nil {
		t.Fatalf("expected city runtime env to be set, got %v", err)
	}
}

func TestRunProviderOpSanitizesInheritedRuntimeEnv(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "sanitize-env.sh")
	content := "#!/bin/sh\n" +
		"test \"$GC_CITY\" = \"" + dir + "\" || exit 1\n" +
		"test \"$GC_CITY_PATH\" = \"" + dir + "\" || exit 1\n" +
		"test \"$GC_CITY_RUNTIME_DIR\" = \"" + filepath.Join(dir, ".gc", "runtime") + "\" || exit 1\n" +
		"test -z \"$GC_PACK_STATE_DIR\" || exit 1\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", "/wrong")
	t.Setenv("GC_CITY_PATH", "/wrong")
	t.Setenv("GC_CITY_ROOT", "/wrong")
	t.Setenv("GC_CITY_RUNTIME_DIR", "/wrong/.gc/runtime")
	t.Setenv("GC_PACK_STATE_DIR", "/wrong/.gc/runtime/packs/dolt")
	if err := runProviderOp(script, dir, "health"); err != nil {
		t.Fatalf("expected sanitized runtime env, got %v", err)
	}
}

func TestRunProviderOpKillsProcessGroupOnTimeout(t *testing.T) {
	cancelCh := useCancelableProviderLifecycleContext(t)

	dir := t.TempDir()
	childPIDFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "provider-op.sh")
	content := `#!/bin/sh
sh -c 'echo $$ > "$GC_TEST_CHILD_PID"; while :; do sleep 1; done' &
wait
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	// Timeouts and explicit cancellation both drive exec.Cmd.Cancel; cancel only
	// after the child PID is observable so the cleanup assertion cannot race setup.
	resultCh := make(chan error, 1)
	go func() {
		resultCh <- runProviderOpWithEnv(script, append(os.Environ(), "GC_TEST_CHILD_PID="+childPIDFile), "health")
	}()

	cancel := waitForProviderLifecycleCancel(t, cancelCh)
	t.Cleanup(cancel)
	pid := waitForProviderTestChildPID(t, childPIDFile)
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})

	cancel()

	var err error
	select {
	case err = <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("provider op did not return after cancellation")
	}
	if err == nil {
		t.Fatal("expected timeout error")
	}

	waitForProviderTestPIDExit(t, pid, "provider op")
}

func TestRunProviderOpWithEnvContextParentCancellationKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	childPIDFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "provider-op.sh")
	content := `#!/bin/sh
sh -c 'echo $$ > "$GC_TEST_CHILD_PID"; while :; do sleep 1; done' &
wait
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan error, 1)
	go func() {
		resultCh <- runProviderOpWithEnvContext(ctx, script, append(os.Environ(), "GC_TEST_CHILD_PID="+childPIDFile), "health")
	}()

	pid := waitForProviderTestChildPID(t, childPIDFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	cancel()

	select {
	case err := <-resultCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("provider op error = %v, want context canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("provider op did not return after parent cancellation")
	}
	waitForProviderTestPIDExit(t, pid, "provider op with parent context")
}

func TestRunProviderProbeKillsProcessGroupOnTimeout(t *testing.T) {
	cancelCh := useCancelableProviderLifecycleContext(t)

	dir := t.TempDir()
	childPIDFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "provider-probe.sh")
	content := `#!/bin/sh
sh -c 'echo $$ > "$GC_TEST_CHILD_PID"; while :; do sleep 1; done' &
wait
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_TEST_CHILD_PID", childPIDFile)

	// Timeouts and explicit cancellation both drive exec.Cmd.Cancel; cancel only
	// after the child PID is observable so the cleanup assertion cannot race setup.
	resultCh := make(chan bool, 1)
	go func() {
		resultCh <- runProviderProbe(script, "", "")
	}()

	cancel := waitForProviderLifecycleCancel(t, cancelCh)
	t.Cleanup(cancel)
	pid := waitForProviderTestChildPID(t, childPIDFile)
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})

	cancel()

	var ok bool
	select {
	case ok = <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("provider probe did not return after cancellation")
	}
	if ok {
		t.Fatal("expected timeout probe to return false")
	}

	waitForProviderTestPIDExit(t, pid, "provider probe")
}

func useCancelableProviderLifecycleContext(t *testing.T) <-chan context.CancelFunc {
	t.Helper()
	oldProviderLifecycleContext := providerLifecycleContext
	cancelCh := make(chan context.CancelFunc, 1)
	providerLifecycleContext = func(parent context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		select {
		case cancelCh <- cancel:
		default:
		}
		return ctx, cancel
	}
	t.Cleanup(func() {
		providerLifecycleContext = oldProviderLifecycleContext
	})
	return cancelCh
}

func waitForProviderLifecycleCancel(t *testing.T, cancelCh <-chan context.CancelFunc) context.CancelFunc {
	t.Helper()
	select {
	case cancel := <-cancelCh:
		return cancel
	case <-time.After(2 * time.Second):
		t.Fatal("provider lifecycle context was not created")
		return nil
	}
}

func waitForProviderTestChildPID(t *testing.T, path string) int {
	t.Helper()
	pidText := waitForProviderTestNonEmptyFile(t, path, 5*time.Second)
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		t.Fatalf("parse child pid: %v", err)
	}
	return pid
}

func waitForProviderTestNonEmptyFile(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pidBytes, err := os.ReadFile(path)
		if err == nil {
			pid := strings.TrimSpace(string(pidBytes))
			if pid != "" {
				return pid
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read child pid: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child pid was not written within %s", timeout)
	return ""
}

func waitForProviderTestPIDExit(t *testing.T, pid int, label string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("provider child pid %d survived %s cancellation", pid, label)
}

func TestStartBeadsLifecycleDoesNotMutateProcessDoltEnv(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SKIP", "skip")
	t.Setenv("GC_BEADS_PORT", "")
	_ = os.Unsetenv("GC_BEADS_PORT")
	_ = os.Unsetenv("BEADS_DOLT_SERVER_PORT")
	_ = os.Unsetenv("BEADS_DOLT_SERVER_HOST")

	cityPath := t.TempDir()
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeDoltState(cityPath, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      4406,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	if err := startBeadsLifecycle(cityPath, "test-city", cfg, io.Discard); err != nil {
		t.Fatalf("startBeadsLifecycle: %v", err)
	}
	if got := os.Getenv("GC_BEADS_PORT"); got != "" {
		t.Fatalf("GC_BEADS_PORT = %q, want empty", got)
	}
	if got := os.Getenv("BEADS_DOLT_SERVER_PORT"); got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want empty", got)
	}
	if got := os.Getenv("BEADS_DOLT_SERVER_HOST"); got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want empty", got)
	}
}

func TestGcBeadsBdStartUsesRootBeadsDataDir(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage")
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	materializeBuiltinPacksForTest(t, cityPath)
	script := gcBeadsBdScriptPath(cityPath)

	homeDir := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitConfig := filepath.Join(homeDir, ".gitconfig")
	if err := os.WriteFile(gitConfig, []byte("[user]\n\tname = Test User\n\temail = test@example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	poisonRuntimeDir := filepath.Join(t.TempDir(), "poison-runtime")
	poisonPackStateDir := filepath.Join(poisonRuntimeDir, "packs", "dolt")
	poisonStateFile := filepath.Join(poisonPackStateDir, "dolt-provider-state.json")
	t.Setenv("GC_CITY_RUNTIME_DIR", poisonRuntimeDir)
	t.Setenv("GC_PACK_STATE_DIR", poisonPackStateDir)
	t.Setenv("GC_BEADS_STATE_FILE", poisonStateFile)

	scriptEnv := sanitizedBaseEnv(
		"HOME="+homeDir,
		"GIT_CONFIG_GLOBAL="+gitConfig,
		"GC_CITY_PATH="+cityPath,
		"PATH="+strings.Join([]string{
			filepath.Dir(doltPath),
			os.Getenv("PATH"),
		}, string(os.PathListSeparator)),
	)

	runScript := func(args ...string) {
		t.Helper()
		cmd := exec.Command(script, args...)
		cmd.Env = scriptEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	t.Cleanup(func() {
		cmd := exec.Command(script, "stop")
		cmd.Env = scriptEnv
		_ = cmd.Run()
	})

	runScript("start")

	stateFile := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-provider-state.json")
	state, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("read provider state file: %v", err)
	}
	if !strings.Contains(string(state), filepath.Join(cityPath, ".beads", "dolt")) {
		t.Fatalf("provider state file should point at .beads/dolt, got:\n%s", state)
	}

	if _, err := os.Stat(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-state.json")); !os.IsNotExist(err) {
		t.Fatalf("canonical dolt-state.json should not be shell-owned, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".beads", "dolt-server.port")); !os.IsNotExist(err) {
		t.Fatalf("dolt-server.port should not be written by shell start, stat err = %v", err)
	}
	if _, err := os.Stat(poisonStateFile); !os.IsNotExist(err) {
		t.Fatalf("start leaked ambient GC_* state to %q, stat err = %v", poisonStateFile, err)
	}
}

type rootStoreVerificationRetryStore struct {
	*beads.MemStore
	failuresRemaining int
	listQueries       []beads.ListQuery
}

func writeGcBeadsBdInitEnvCaptureScript(t *testing.T, captureFile string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "gc-beads-bd")
	body := fmt.Sprintf(`#!/bin/sh
set -eu
op="$1"
shift
case "$op" in
  init)
    printf '%%s|%%s|%%s|%%s|%%s|%%s|%%s|%%s|%%s
' "${GC_BEADS_HOST:-}" "${GC_BEADS_PORT:-}" "${GC_BEADS_USER:-}" "${GC_BEADS_PASSWORD:-}" "${BEADS_DOLT_SERVER_HOST:-}" "${BEADS_DOLT_SERVER_PORT:-}" "${BEADS_DOLT_SERVER_USER:-}" "${BEADS_DOLT_PASSWORD:-}" "${GC_PACK_STATE_DIR:-}" > %q
    exit 0
    ;;
  *)
    exit 2
    ;;
esac
`, captureFile)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestInitAndHookDirExecGcBeadsBdProjectsCanonicalExternalCityEnv(t *testing.T) {
	cityPath := t.TempDir()
	cityToml := `[workspace]
name = "demo"

[dolt]
host = "city-db.example.com"
port = 3307
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.com
dolt.port: 3307
dolt.user: city-user
`
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	captureFile := filepath.Join(t.TempDir(), "init-env-city")
	script := writeGcBeadsBdInitEnvCaptureScript(t, captureFile)
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	t.Setenv("GC_BEADS_HOST", "ambient.invalid")
	t.Setenv("GC_BEADS_PORT", "9999")
	t.Setenv("GC_PACK_STATE_DIR", "/wrong/.gc/runtime/packs/dolt")
	if err := initAndHookDir(cityPath, cityPath, "gc"); err != nil {
		t.Fatalf("initAndHookDir(city external): %v", err)
	}
	data, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	got := strings.TrimSpace(string(data))
	want := strings.Join([]string{"city-db.example.com", "3307", "city-user", "city-pass", "city-db.example.com", "3307", "city-user", "city-pass", citylayout.PackStateDir(cityPath, "dolt")}, "|")
	if got != want {
		t.Fatalf("captured external city init env = %q, want %q", got, want)
	}
}

func TestInitAndHookDirExecGcBeadsBdProjectsCanonicalExplicitRigEnv(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "demo"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
dolt_host = "rig-db.example.com"
dolt_port = "4407"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	rigCfg := `issue_prefix: fe
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: rig-db.example.com
dolt.port: 4407
dolt.user: rig-user
`
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte(rigCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=rig-pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	captureFile := filepath.Join(t.TempDir(), "init-env-rig")
	script := writeGcBeadsBdInitEnvCaptureScript(t, captureFile)
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	t.Setenv("GC_BEADS_HOST", "ambient.invalid")
	t.Setenv("GC_BEADS_PORT", "9999")
	t.Setenv("GC_PACK_STATE_DIR", "/wrong/.gc/runtime/packs/dolt")
	if err := initAndHookDir(cityPath, rigPath, "fe"); err != nil {
		t.Fatalf("initAndHookDir(explicit rig): %v", err)
	}
	data, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	got := strings.TrimSpace(string(data))
	want := strings.Join([]string{"rig-db.example.com", "4407", "rig-user", "rig-pass", "rig-db.example.com", "4407", "rig-user", "rig-pass", citylayout.PackStateDir(cityPath, "dolt")}, "|")
	if got != want {
		t.Fatalf("captured explicit rig init env = %q, want %q", got, want)
	}
}

func TestInitAndHookDirExecGcBeadsBdProjectsInheritedExternalRigEnv(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "demo"

[dolt]
host = "city-db.example.com"
port = 3307

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	cityCfg := `issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.com
dolt.port: 3307
dolt.user: city-user
`
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(cityCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	rigCfg := `issue_prefix: fe
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.com
dolt.port: 3307
dolt.user: city-user
`
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte(rigCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=rig-pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	captureFile := filepath.Join(t.TempDir(), "init-env-inherited-rig")
	script := writeGcBeadsBdInitEnvCaptureScript(t, captureFile)
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	t.Setenv("GC_BEADS_HOST", "ambient.invalid")
	t.Setenv("GC_BEADS_PORT", "9999")
	if err := initAndHookDir(cityPath, rigPath, "fe"); err != nil {
		t.Fatalf("initAndHookDir(inherited rig): %v", err)
	}
	data, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	got := strings.TrimSpace(string(data))
	want := strings.Join([]string{"city-db.example.com", "3307", "city-user", "city-pass", "city-db.example.com", "3307", "city-user", "city-pass", citylayout.PackStateDir(cityPath, "dolt")}, "|")
	if got != want {
		t.Fatalf("captured inherited rig init env = %q, want %q", got, want)
	}
}

func TestHealthBeadsProviderExecGcBeadsBdProjectsCanonicalExternalCityEnv(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.com
dolt.port: 3307
dolt.user: city-user
`
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	captureFile := filepath.Join(t.TempDir(), "health-env-city")
	script := filepath.Join(t.TempDir(), "gc-beads-bd")
	scriptBody := fmt.Sprintf(`#!/bin/sh
set -eu
op="$1"
shift
case "$op" in
  health)
    printf '%%s|%%s|%%s|%%s|%%s|%%s|%%s|%%s|%%s
' "${GC_BEADS_HOST:-}" "${GC_BEADS_PORT:-}" "${GC_BEADS_USER:-}" "${GC_BEADS_PASSWORD:-}" "${BEADS_DOLT_SERVER_HOST:-}" "${BEADS_DOLT_SERVER_PORT:-}" "${BEADS_DOLT_SERVER_USER:-}" "${BEADS_DOLT_PASSWORD:-}" "${GC_PACK_STATE_DIR:-}" > %q
    exit 0
    ;;
  *)
    exit 2
    ;;
esac
`, captureFile)
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	t.Setenv("GC_BEADS_HOST", "ambient.invalid")
	t.Setenv("GC_BEADS_PORT", "9999")
	t.Setenv("GC_PACK_STATE_DIR", "/wrong/.gc/runtime/packs/dolt")
	if err := healthBeadsProvider(cityPath); err != nil {
		t.Fatalf("healthBeadsProvider: %v", err)
	}
	data, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	got := strings.TrimSpace(string(data))
	want := strings.Join([]string{"city-db.example.com", "3307", "city-user", "city-pass", "city-db.example.com", "3307", "city-user", "city-pass", citylayout.PackStateDir(cityPath, "dolt")}, "|")
	if got != want {
		t.Fatalf("captured health env = %q, want %q", got, want)
	}
}

func TestEnsureBeadsProviderExecGcBeadsBdProjectsCanonicalPackStateDir(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.com
dolt.port: 3307
`
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	captureFile := filepath.Join(t.TempDir(), "start-env")
	script := filepath.Join(t.TempDir(), "gc-beads-bd")
	scriptBody := fmt.Sprintf(`#!/bin/sh
set -eu
op="$1"
shift
case "$op" in
  start)
    printf '%%s
' "${GC_PACK_STATE_DIR:-}" > %q
    exit 2
    ;;
  *)
    exit 2
    ;;
esac
`, captureFile)
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	t.Setenv("GC_PACK_STATE_DIR", "/wrong/.gc/runtime/packs/dolt")
	if err := ensureBeadsProvider(cityPath); err != nil {
		t.Fatalf("ensureBeadsProvider: %v", err)
	}
	data, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	if got, want := strings.TrimSpace(string(data)), citylayout.PackStateDir(cityPath, "dolt"); got != want {
		t.Fatalf("captured start GC_PACK_STATE_DIR = %q, want %q", got, want)
	}
}

func TestInitAndHookDirPreservesPostgresMetadataAndSkipsDoltInit(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: gc\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(cityPath, ".beads", "metadata.json")
	if err := os.WriteFile(metadataPath, []byte(`{"database":"beads","backend":"postgres","postgres_host":"db.example.test","postgres_port":"5432","postgres_user":"bd","postgres_database":"beads_pg"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	callsFile := filepath.Join(t.TempDir(), "provider-calls.log")
	script := filepath.Join(t.TempDir(), "gc-beads-bd")
	scriptBody := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s
' "$*" >> %q
exit 99
`, callsFile)
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)

	if err := initAndHookDir(cityPath, cityPath, "gc"); err != nil {
		t.Fatalf("initAndHookDir: %v", err)
	}
	if data, err := os.ReadFile(callsFile); err == nil {
		t.Fatalf("provider init should not run for postgres metadata; calls:\n%s", data)
	} else if !os.IsNotExist(err) {
		t.Fatalf("read provider calls: %v", err)
	}
	state, ok, err := contract.LoadMetadataState(fsys.OSFS{}, metadataPath)
	if err != nil {
		t.Fatalf("LoadMetadataState: %v", err)
	}
	if !ok || state.Backend != "postgres" || state.PostgresDatabase != "beads_pg" {
		t.Fatalf("metadata state = %+v, ok=%v; want postgres metadata preserved", state, ok)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".beads", "hooks", "on_create")); !os.IsNotExist(err) {
		t.Fatalf("gc must not install bd event hooks for postgres scope (stat err=%v)", err)
	}
}

func writeInheritedCityPostgresRigFixture(t *testing.T, rigMetadata string) (string, string, string) {
	t.Helper()
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "pg")
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: gc\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"database":"beads","backend":"postgres","postgres_host":"db.example.test","postgres_port":"5432","postgres_user":"bd","postgres_database":"beads_pg"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_POSTGRES_PASSWORD=citypw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte("issue_prefix: pg\ngc.endpoint_origin: inherited_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(rigPath, ".beads", "metadata.json")
	if rigMetadata != "" {
		if err := os.WriteFile(metadataPath, []byte(rigMetadata), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return cityPath, rigPath, metadataPath
}

func TestInitAndHookDirSkipsDoltInitForInheritedCityPostgresRig(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "pg")
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: gc\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"database":"beads","backend":"postgres","postgres_host":"db.example.test","postgres_port":"5432","postgres_user":"bd","postgres_database":"beads_pg"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_POSTGRES_PASSWORD=citypw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte("issue_prefix: pg\ngc.endpoint_origin: inherited_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	callsFile := filepath.Join(t.TempDir(), "provider-calls.log")
	script := filepath.Join(t.TempDir(), "gc-beads-bd")
	scriptBody := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s
' "$*" >> %q
exit 99
`, callsFile)
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)

	if err := initAndHookDir(cityPath, rigPath, "pg"); err != nil {
		t.Fatalf("initAndHookDir: %v", err)
	}
	if data, err := os.ReadFile(callsFile); err == nil {
		t.Fatalf("provider init should not run for inherited postgres rig; calls:\n%s", data)
	} else if !os.IsNotExist(err) {
		t.Fatalf("read provider calls: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rigPath, ".beads", "metadata.json")); !os.IsNotExist(err) {
		t.Fatalf("inherited postgres rig should not be pinned with local metadata, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(rigPath, ".beads", "hooks", "on_create")); !os.IsNotExist(err) {
		t.Fatalf("gc must not install bd event hooks for inherited postgres rig (stat err=%v)", err)
	}
}

func TestInitAndHookDirSkipsDoltInitForInheritedCityPostgresRigWithEmptyMetadata(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata string
	}{
		{name: "empty_object", metadata: `{}`},
		{name: "database_only", metadata: `{"database":"beads"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, rigPath, metadataPath := writeInheritedCityPostgresRigFixture(t, tc.metadata)
			callsFile := filepath.Join(t.TempDir(), "provider-calls.log")
			script := filepath.Join(t.TempDir(), "gc-beads-bd")
			scriptBody := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s
' "$*" >> %q
exit 99
`, callsFile)
			if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GC_BEADS", "exec:"+script)
			t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)

			if err := initAndHookDir(cityPath, rigPath, "pg"); err != nil {
				t.Fatalf("initAndHookDir: %v", err)
			}
			if data, err := os.ReadFile(callsFile); err == nil {
				t.Fatalf("provider init should not run for inherited postgres rig; calls:\n%s", data)
			} else if !os.IsNotExist(err) {
				t.Fatalf("read provider calls: %v", err)
			}
			data, err := os.ReadFile(metadataPath)
			if err != nil {
				t.Fatalf("read metadata: %v", err)
			}
			if string(data) != tc.metadata {
				t.Fatalf("metadata = %s, want preserved %s", data, tc.metadata)
			}
			if _, err := os.Stat(filepath.Join(rigPath, ".beads", "hooks", "on_create")); !os.IsNotExist(err) {
				t.Fatalf("gc must not install bd event hooks for inherited postgres rig (stat err=%v)", err)
			}
		})
	}
}

func TestInitAndHookDirAdoptsAlreadyInitializedDefaultRigBdStore(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "tincan")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// doltlite city triggers shouldInitDefaultRigBdStore for the rig without
	// setting a GC_BEADS scope-wide override that would shadow the rig's own
	// metadata-based provider detection.
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"tincan-city\"\n\n[beads]\nprovider = \"doltlite\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"tincan"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	initArgsFile := filepath.Join(t.TempDir(), "bd-init-args")
	fakeBd := filepath.Join(binDir, "bd")
	fakeBdScript := fmt.Sprintf(`#!/bin/sh
set -eu
case "${1:-}" in
  init)
    printf '%%s\n' "$@" > %q
    echo "Found existing Dolt database 'tincan' for this workspace. This workspace is already initialized; just run bd commands normally. Aborting." >&2
    exit 1
    ;;
  list)
    printf '[]\n'
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, initArgsFile)
	if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	if err := initAndHookDir(cityPath, rigPath, "tc"); err != nil {
		t.Fatalf("initAndHookDir should adopt existing initialized rig store: %v", err)
	}
	if _, err := os.Stat(initArgsFile); err != nil {
		t.Fatalf("expected bd init attempt, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(rigPath, ".beads", "hooks", "on_create")); !os.IsNotExist(err) {
		t.Fatalf("gc must not install bd event hooks for adopted rig (stat err=%v)", err)
	}
}

func TestInitAndHookDirAdoptsAlreadyInitializedCanonicalExecBdStore(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "tincan")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"tincan"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeBd := filepath.Join(binDir, "bd")
	if err := os.WriteFile(fakeBd, []byte("#!/bin/sh\nif [ \"${1:-}\" = list ]; then printf '[]\\n'; fi\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	providerArgsFile := filepath.Join(t.TempDir(), "provider-init-args")
	providerScript := filepath.Join(t.TempDir(), "gc-beads-bd")
	providerBody := fmt.Sprintf(`#!/bin/sh
set -eu
case "${1:-}" in
  init)
    printf '%%s\n' "$@" > %q
    echo "Found existing Dolt database 'tincan' for this workspace. This workspace is already initialized; just run bd commands normally. Aborting." >&2
    exit 1
    ;;
  *)
    exit 0
    ;;
esac
`, providerArgsFile)
	if err := os.WriteFile(providerScript, []byte(providerBody), 0o755); err != nil {
		t.Fatal(err)
	}

	setScopedBeadsProviderForTest(t, cityPath, "exec:"+providerScript)
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	if err := initAndHookDir(cityPath, rigPath, "tc"); err != nil {
		t.Fatalf("initAndHookDir should adopt existing initialized canonical exec rig store: %v", err)
	}
	if _, err := os.Stat(providerArgsFile); err != nil {
		t.Fatalf("expected provider init attempt, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(rigPath, ".beads", "hooks", "on_create")); !os.IsNotExist(err) {
		t.Fatalf("gc must not install bd event hooks for adopted canonical exec rig (stat err=%v)", err)
	}
}

func TestSeedDeferredManagedBeadsSkipsDoltMetadataForInheritedCityPostgresRigWithEmptyMetadata(t *testing.T) {
	cityPath, rigPath, metadataPath := writeInheritedCityPostgresRigFixture(t, `{"database":"beads"}`)

	if err := seedDeferredManagedBeadsErr(cityPath, rigPath, "pg", ""); err != nil {
		t.Fatalf("seedDeferredManagedBeadsErr: %v", err)
	}
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if got, want := string(data), `{"database":"beads"}`; got != want {
		t.Fatalf("metadata = %s, want preserved %s", got, want)
	}
}

func TestGcBeadsBdInitTightensBeadsDirPermissions(t *testing.T) {
	tests := []struct {
		name             string
		preexistingDir   bool
		existingMetadata bool
		wantInitPerm     string
	}{
		{name: "fresh_init", preexistingDir: false, existingMetadata: false, wantInitPerm: "700"},
		{name: "preexisting_dir_without_metadata", preexistingDir: true, existingMetadata: false, wantInitPerm: "700"},
		{name: "existing_metadata", preexistingDir: true, existingMetadata: true, wantInitPerm: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
				t.Fatal(err)
			}

			if tc.preexistingDir {
				if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o775); err != nil {
					t.Fatal(err)
				}
			}
			if tc.existingMetadata {
				if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"gascity"}`), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			materializeBuiltinPacksForTest(t, cityPath)
			script := gcBeadsBdScriptPath(cityPath)

			binDir := filepath.Join(t.TempDir(), "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}

			initPermFile := filepath.Join(t.TempDir(), "bd-init-perm")
			fakeBd := filepath.Join(binDir, "bd")
			fakeBdScript := `#!/bin/sh
set -eu
perm_file="` + initPermFile + `"
case "${1:-}" in
  init)
    last=""
    for arg in "$@"; do
      last="$arg"
    done
    if [ -d "$last/.beads" ]; then
      if stat -c %a "$last/.beads" >/dev/null 2>&1; then
        stat -c %a "$last/.beads" > "$perm_file"
      else
        stat -f %Lp "$last/.beads" > "$perm_file"
      fi
    else
      printf 'missing
' > "$perm_file"
    fi
    mkdir -p "$last/.beads"
    chmod 775 "$last/.beads"
    cat > "$last/.beads/metadata.json" <<'JSON'
{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"gascity"}
JSON
    exit 0
    ;;
  config|migrate|list)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`
			if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
				t.Fatal(err)
			}

			fakeDolt := filepath.Join(binDir, "dolt")
			if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(script, "init", cityPath, "gc", "gascity")
			cmd.Env = sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
				"GC_CITY_PATH="+cityPath,
				"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
			)...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("gc-beads-bd init failed: %v\n%s", err, out)
			}

			if tc.wantInitPerm == "" {
				if _, err := os.Stat(initPermFile); !os.IsNotExist(err) {
					t.Fatalf("bd init should not run for existing metadata, stat err=%v", err)
				}
			} else {
				data, err := os.ReadFile(initPermFile)
				if err != nil {
					t.Fatalf("read init perm: %v", err)
				}
				got := strings.TrimSpace(string(data))
				if len(got) > 3 {
					got = got[len(got)-3:]
				}
				if got != tc.wantInitPerm {
					t.Fatalf("bd init saw .beads perm %q, want effective bits %q", strings.TrimSpace(string(data)), tc.wantInitPerm)
				}
			}

			info, err := os.Stat(filepath.Join(cityPath, ".beads"))
			if err != nil {
				t.Fatalf("stat .beads: %v", err)
			}
			if got := info.Mode().Perm(); got != beadsDirPerm {
				t.Fatalf(".beads perm = %o, want %o", got, beadsDirPerm)
			}
		})
	}
}

func TestGcBeadsBdInitFailsWhenBeadsDirPermissionsCannotBeTightened(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o775); err != nil {
		t.Fatal(err)
	}

	materializeBuiltinPacksForTest(t, cityPath)
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	realChmod, err := exec.LookPath("chmod")
	if err != nil {
		t.Fatalf("LookPath(chmod): %v", err)
	}
	fakeChmod := filepath.Join(binDir, "chmod")
	fakeChmodScript := fmt.Sprintf(`#!/bin/sh
set -eu
if [ "$#" -ge 2 ] && [ "$1" = "700" ] && [ "$2" = %q ]; then
  echo "chmod blocked" >&2
  exit 1
fi
exec %q "$@"
`, filepath.Join(cityPath, ".beads"), realChmod)
	if err := os.WriteFile(fakeChmod, []byte(fakeChmodScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeBd := filepath.Join(binDir, "bd")
	if err := os.WriteFile(fakeBd, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script, "init", cityPath, "gc", "gascity")
	cmd.Env = sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("gc-beads-bd init unexpectedly succeeded\n%s", out)
	}
	if !strings.Contains(string(out), "failed to set "+filepath.Join(cityPath, ".beads")+" permissions to 700") {
		t.Fatalf("init error = %q, want chmod failure", string(out))
	}
}

func TestGcBeadsBdInitEnsuresProjectIdentityWhenMetadataExistsWithoutProjectID(t *testing.T) {
	skipSlowCmdGCTest(t, "runs the materialized gc-beads-bd init script with GC_BIN helper; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"gascity"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	materializeBuiltinPacksForTest(t, cityPath)
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	captureDir := t.TempDir()
	fakeBd := filepath.Join(binDir, "bd")
	fakeBdScript := fmt.Sprintf(`#!/bin/sh
set -eu
capture_dir=%q
cmd="${1:-}"
	case "$cmd" in
  init)
    : > "$capture_dir/init.called"
    exit 0
    ;;
  migrate)
    : > "$capture_dir/migrate.called"
    exit 0
    ;;
  config|list)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, captureDir)
	if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeGC := filepath.Join(binDir, "gc-helper")
	fakeGCScript := fmt.Sprintf(`#!/bin/sh
set -eu
capture_dir=%q
cmd="$1 $2"
shift 2
case "$cmd" in
  'dolt-state ensure-project-id')
    metadata=''
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --metadata)
          metadata="$2"
          shift 2
          ;;
        --city|--host|--port|--user|--database)
          shift 2
          ;;
        *)
          exit 64
          ;;
      esac
    done
    : > "$capture_dir/helper.called"
    python3 - <<'PY' "$metadata"
import json, pathlib, sys
path = pathlib.Path(sys.argv[1])
data = json.loads(path.read_text())
data['project_id'] = 'helper-project-id'
path.write_text(json.dumps(data, indent=2) + '\n')
PY
    ;;
  'dolt-config normalize-scope')
    exit 0
    ;;
  *)
    echo "unexpected gc helper args: $cmd $*" >&2
    exit 64
    ;;
esac
`, captureDir)
	if err := os.WriteFile(fakeGC, []byte(fakeGCScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script, "init", cityPath, "gc", "gascity")
	cmd.Env = sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
		"GC_CITY_PATH="+cityPath,
		"GC_BIN="+fakeGC,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd init failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(captureDir, "migrate.called")); !os.IsNotExist(err) {
		t.Fatalf("migrate should not run on metadata fast path, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(captureDir, "helper.called")); err != nil {
		t.Fatalf("expected project-id helper call, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(captureDir, "init.called")); !os.IsNotExist(err) {
		t.Fatalf("bd init should be skipped on metadata fast path, stat err = %v", err)
	}
	metaData, err := os.ReadFile(filepath.Join(cityPath, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("ReadFile(metadata.json): %v", err)
	}
	if !strings.Contains(string(metaData), `"project_id": "helper-project-id"`) {
		t.Fatalf("metadata.json missing helper project_id:\n%s", metaData)
	}
}

func TestGcBeadsBdInitDoltliteInitializesDelegatedBdWrites(t *testing.T) {
	bdPath, err := exec.LookPath("bd")
	if err != nil {
		t.Skip("bd CLI required for DoltLite wrapper init smoke test")
	}

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	materializeBuiltinPacksForTest(t, cityPath)
	script := gcBeadsBdScriptPath(cityPath)

	cmd := exec.Command(script, "init", cityPath, "gc", "hq")
	cmd.Env = sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
		"GC_CITY_PATH="+cityPath,
		"GC_BEADS_BACKEND=doltlite",
		"BEADS_BACKEND=doltlite",
		"BD_NON_INTERACTIVE=1",
		"BD_BIN="+bdPath,
		"PATH="+os.Getenv("PATH"),
	)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd doltlite init failed: %v\n%s", err, out)
	}

	metaData, err := os.ReadFile(filepath.Join(cityPath, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	metaText := string(metaData)
	for _, want := range []string{`"backend": "doltlite"`, `"database": "doltlite"`, `"dolt_database": "hq"`} {
		if !strings.Contains(metaText, want) {
			t.Fatalf("metadata missing %q:\n%s", want, metaText)
		}
	}

	create := exec.Command(bdPath, "create", "--json", "probe task")
	create.Dir = cityPath
	create.Env = sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
		"BEADS_DIR="+filepath.Join(cityPath, ".beads"),
		"GC_BEADS_BACKEND=doltlite",
		"BEADS_BACKEND=doltlite",
		"BD_NON_INTERACTIVE=1",
		"PATH="+os.Getenv("PATH"),
	)...)
	created, err := create.CombinedOutput()
	if err != nil {
		t.Fatalf("bd create after doltlite init failed: %v\n%s", err, created)
	}
	var createdIssue struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(created, &createdIssue); err != nil {
		t.Fatalf("parse bd create output: %v\n%s", err, created)
	}
	if !strings.HasPrefix(createdIssue.ID, "gc-") {
		t.Fatalf("created issue ID = %q, want gc-*", createdIssue.ID)
	}
	if createdIssue.Title != "probe task" {
		t.Fatalf("created issue title = %q, want probe task", createdIssue.Title)
	}
}

func TestGcBeadsBdInitDoltliteRejectsUnsafeCustomTypes(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repoRootForLint(t), "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	cmd := exec.Command(script, "init", cityPath, "gc", "hq")
	cmd.Env = sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
		"GC_CITY_PATH="+cityPath,
		"GC_BEADS_BACKEND=doltlite",
		"BEADS_BACKEND=doltlite",
		"GC_BEADS_CUSTOM_TYPES=task,bad'type",
	)...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("gc-beads-bd doltlite init succeeded with unsafe custom type:\n%s", out)
	}
	if !strings.Contains(string(out), "invalid custom bead types") {
		t.Fatalf("gc-beads-bd doltlite init error = %q, want invalid custom bead types", out)
	}

	if _, err := os.Stat(filepath.Join(cityPath, ".beads", "embeddeddolt")); err == nil {
		t.Fatal("rejected doltlite init created delegated bd storage")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat delegated bd storage: %v", err)
	}
}

// ── isExternalDolt tests ──────────────────────────────────────────────

// ── per-city dolt config registration tests ─────────────────────────

func writeFakeManagedConfigWriterGC(t *testing.T, binDir, invocationFile string) string {
	t.Helper()
	fakeGC := filepath.Join(binDir, "gc")
	fakeGCScript := fmt.Sprintf(`#!/bin/sh
set -eu
invocation_file=%q
subcmd="$1 $2"
shift 2
case "$subcmd" in
  "dolt-config write-managed")
    config_file=""
    host=""
    port=""
    data_dir=""
    log_level="warning"
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --file)
          config_file="$2"
          shift 2
          ;;
        --host)
          host="$2"
          shift 2
          ;;
        --port)
          port="$2"
          shift 2
          ;;
        --data-dir)
          data_dir="$2"
          shift 2
          ;;
        --log-level)
          log_level="$2"
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 65
          ;;
      esac
    done
    printf 'gc dolt-config write-managed\n' >> "$invocation_file"
    mkdir -p "$(dirname "$config_file")"
    cat > "$config_file" <<EOF
# rendered by fake gc
log_level: $log_level
listener:
  port: $port
  host: $host
  max_connections: 256
  read_timeout_millis: 15000
  write_timeout_millis: 300000

data_dir: "$data_dir"

behavior:
  auto_gc_behavior:
    enable: true
    archive_level: 0

system_variables:
  dolt_auto_gc_enabled: "ON"
  dolt_stats_enabled: "OFF"
  dolt_stats_gc_enabled: "OFF"
  dolt_stats_memory_only: "ON"
  dolt_stats_paused: "ON"
EOF
    ;;
  "dolt-state allocate-port")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--state-file)
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state allocate-port\n' >> "$invocation_file"
    printf '3311\n'
    ;;
  "dolt-state inspect-managed")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--port)
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state inspect-managed\n' >> "$invocation_file"
    printf 'managed_pid\t0\n'
    printf 'managed_source\t\n'
    printf 'managed_owned\tfalse\n'
    printf 'managed_deleted_inodes\tfalse\n'
    printf 'port_holder_pid\t0\n'
    printf 'port_holder_owned\tfalse\n'
    printf 'port_holder_deleted_inodes\tfalse\n'
    ;;
  "dolt-state existing-managed")
    city=""
    port=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city)
          city="$2"
          shift 2
          ;;
        --port)
          port="$2"
          shift 2
          ;;
        --host|--user|--timeout-ms)
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state existing-managed\n' >> "$invocation_file"
    pack_dir="$city/.gc/runtime/packs/dolt-from-gc"
    pid_file="$pack_dir/dolt.pid"
    state_file="$pack_dir/dolt-provider-state.json"
    if [ -s "$pid_file" ] && [ -f "$state_file" ]; then
      managed_pid=$(cat "$pid_file")
      printf 'managed_pid\t%%s\n' "$managed_pid"
      printf 'managed_owned\ttrue\n'
      printf 'deleted_inodes\tfalse\n'
      printf 'state_port\t%%s\n' "$port"
      printf 'ready\ttrue\n'
      printf 'reusable\ttrue\n'
      exit 0
    fi
    printf 'managed_pid\t0\n'
    printf 'managed_owned\tfalse\n'
    printf 'deleted_inodes\tfalse\n'
    printf 'state_port\t0\n'
    printf 'ready\tfalse\n'
    printf 'reusable\tfalse\n'
    ;;
  "dolt-state probe-managed")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--host|--port)
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state probe-managed\n' >> "$invocation_file"
    printf 'running\tfalse\n'
    printf 'port_holder_pid\t0\n'
    printf 'port_holder_owned\tfalse\n'
    printf 'port_holder_deleted_inodes\tfalse\n'
    printf 'tcp_reachable\tfalse\n'
    ;;
  "dolt-state wait-ready")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--host|--port|--user|--pid|--timeout-ms)
          shift 2
          ;;
        --check-deleted)
          shift 1
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state wait-ready\n' >> "$invocation_file"
    printf 'ready\ttrue\n'
    printf 'pid_alive\ttrue\n'
    printf 'deleted_inodes\tfalse\n'
    ;;
  "dolt-state stop-managed")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--port)
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state stop-managed\n' >> "$invocation_file"
    printf 'had_pid\ttrue\n'
    printf 'pid\t123\n'
    printf 'forced\tfalse\n'
    ;;
  "dolt-state recover-managed")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--host|--port|--user|--log-level|--timeout-ms)
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state recover-managed\n' >> "$invocation_file"
    printf 'diagnosed_read_only\t%%s\n' "${GC_FAKE_RECOVER_DIAGNOSED_READ_ONLY:-false}"
    printf 'had_pid\ttrue\n'
    printf 'forced\tfalse\n'
    printf 'ready\ttrue\n'
    printf 'pid\t12345\n'
    printf 'port\t3311\n'
    printf 'healthy\ttrue\n'
    ;;
  "dolt-state query-probe")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --host|--port|--user)
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state query-probe\n' >> "$invocation_file"
    ;;
  "dolt-state read-only-check")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --host|--port|--user)
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state read-only-check\n' >> "$invocation_file"
    exit 1
    ;;
  "dolt-state preflight-clean")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city)
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state preflight-clean\n' >> "$invocation_file"
    ;;
  "dolt-state runtime-layout")
    city=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city)
          city="$2"
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    pack_dir="$city/.gc/runtime/packs/dolt-from-gc"
    printf 'gc dolt-state runtime-layout\n' >> "$invocation_file"
    printf 'GC_PACK_STATE_DIR\t%%s\n' "$pack_dir"
    printf 'GC_BEADS_DATA_DIR\t%%s\n' "$city/.beads/dolt"
    printf 'GC_BEADS_LOG_FILE\t%%s\n' "$pack_dir/dolt.log"
    printf 'GC_BEADS_STATE_FILE\t%%s\n' "$pack_dir/dolt-provider-state.json"
    printf 'GC_BEADS_PID_FILE\t%%s\n' "$pack_dir/dolt.pid"
    printf 'GC_BEADS_LOCK_FILE\t%%s\n' "$pack_dir/dolt.lock"
    printf 'GC_BEADS_CONFIG_FILE\t%%s\n' "$pack_dir/dolt-config.yaml"
    ;;
  "dolt-state start-managed")
    city=""
    host=""
    port=""
    log_level="warning"
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city)
          city="$2"
          shift 2
          ;;
        --host|--port|--user|--log-level|--timeout-ms)
          if [ "$1" = "--host" ]; then host="$2"; fi
          if [ "$1" = "--port" ]; then port="$2"; fi
          if [ "$1" = "--log-level" ]; then log_level="$2"; fi
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    pack_dir="$city/.gc/runtime/packs/dolt-from-gc"
    data_dir="$city/.beads/dolt"
    config_file="$pack_dir/dolt-config.yaml"
    state_file="$pack_dir/dolt-provider-state.json"
    pid_file="$pack_dir/dolt.pid"
    printf 'gc dolt-state start-managed\n' >> "$invocation_file"
    if [ -n "${GC_FAKE_FD9_STATUS_FILE:-}" ]; then
      if (: >&9) 2>/dev/null; then
        printf 'open\n' > "$GC_FAKE_FD9_STATUS_FILE"
      else
        printf 'closed\n' > "$GC_FAKE_FD9_STATUS_FILE"
      fi
    fi
    mkdir -p "$pack_dir" "$data_dir"
    cat > "$config_file" <<EOF
# rendered by fake gc
log_level: $log_level
listener:
  port: $port
  host: $host
  max_connections: 256
  read_timeout_millis: 15000
  write_timeout_millis: 300000

data_dir: "$data_dir"

behavior:
  auto_gc_behavior:
    enable: true
    archive_level: 0

system_variables:
  dolt_auto_gc_enabled: "ON"
  dolt_stats_enabled: "OFF"
  dolt_stats_gc_enabled: "OFF"
  dolt_stats_memory_only: "ON"
  dolt_stats_paused: "ON"
EOF
    printf '12345\n' > "$pid_file"
    printf '{"running":true,"pid":12345,"port":%%s,"data_dir":"%%s","started_at":"2026-04-14T00:00:00Z"}\n' "$port" "$data_dir" > "$state_file"
    printf 'ready\ttrue\n'
    printf 'pid\t12345\n'
    printf 'port\t%%s\n' "$port"
    printf 'address_in_use\tfalse\n'
    printf 'attempts\t1\n'
    ;;
  "dolt-state write-provider")
    state_file=""
    pid=""
    running=""
    port=""
    data_dir=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --file)
          state_file="$2"
          shift 2
          ;;
        --pid)
          pid="$2"
          shift 2
          ;;
        --running)
          running="$2"
          shift 2
          ;;
        --port)
          port="$2"
          shift 2
          ;;
        --data-dir)
          data_dir="$2"
          shift 2
          ;;
        --started-at)
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 67
          ;;
      esac
    done
    printf 'gc dolt-state write-provider\n' >> "$invocation_file"
    mkdir -p "$(dirname "$state_file")"
    printf '{"running":%%s,"pid":%%s,"port":%%s,"data_dir":"%%s","started_at":"2026-04-14T00:00:00Z"}\n' "$running" "$pid" "$port" "$data_dir" > "$state_file"
    ;;
  dolt-state\ *cleanup*|dolt-state\ *preflight*|dolt-state\ *quarantine*|dolt-state\ *stale*)
    printf 'gc %%s\n' "$subcmd" >> "$invocation_file"
    ;;
  *)
    echo "unexpected gc args: $subcmd $*" >&2
    exit 64
    ;;
esac
`, invocationFile)
	if err := os.WriteFile(fakeGC, []byte(fakeGCScript), 0o755); err != nil {
		t.Fatal(err)
	}
	return fakeGC
}

func writeFakeManagedConfigWriterDolt(t *testing.T, binDir string) {
	t.Helper()
	fakeDolt := filepath.Join(binDir, "dolt")
	fakeDoltScript := `#!/bin/sh
set -eu
case "${1:-}" in
  config)
    exit 0
    ;;
  sql-server)
    if [ "${GC_FAKE_DOLT_FAIL_SQL_SERVER:-}" = "true" ]; then
      echo "unexpected dolt sql-server invocation" >&2
      exit 97
    fi
    config_file=""
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "--config" ]; then
        config_file="$arg"
        break
      fi
      prev="$arg"
    done
    port=$(awk '/port:/ {print $2; exit}' "$config_file")
    data_dir=$(awk '/data_dir:/ {print $2; exit}' "$config_file" | tr -d '"')
    exec python3 - "$port" "$data_dir" <<'INNERPY'
import os
import signal
import socket
import sys
import time
port = int(sys.argv[1])
data_dir = sys.argv[2]
if data_dir:
    os.makedirs(data_dir, exist_ok=True)
    os.chdir(data_dir)
sock = socket.socket()
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind(("0.0.0.0", port))
sock.listen(128)
sock.settimeout(1.0)
def _stop(*_args):
    raise SystemExit(0)
signal.signal(signal.SIGTERM, _stop)
signal.signal(signal.SIGINT, _stop)
while True:
    try:
        conn, _ = sock.accept()
        conn.close()
    except socket.timeout:
        continue
INNERPY
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(fakeDolt, []byte(fakeDoltScript), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestGcBeadsBdStartDoesNotReplaceLiveLockFileInode(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage")
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock not installed")
	}

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	materializeBuiltinPacksForTest(t, cityPath)
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation")
	fakeDolt := filepath.Join(binDir, "dolt")
	fakeDoltScript := `#!/bin/sh
set -eu
case "${1:-}" in
  config)
    exit 0
    ;;
  sql-server)
    printf 'sql-server\n' >> "$GC_FAKE_DOLT_INVOCATION_FILE"
    config_file=""
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "--config" ]; then
        config_file="$arg"
        break
      fi
      prev="$arg"
    done
    port=$(awk '/port:/ {print $2; exit}' "$config_file")
    data_dir=$(awk '/data_dir:/ {print $2; exit}' "$config_file" | tr -d '"')
    exec python3 - "$port" "$data_dir" <<'INNERPY'
import os
import signal
import socket
import sys
import time
port = int(sys.argv[1])
data_dir = sys.argv[2]
if data_dir:
    os.makedirs(data_dir, exist_ok=True)
    os.chdir(data_dir)
sock = socket.socket()
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind(("0.0.0.0", port))
sock.listen(128)
sock.settimeout(1.0)
def _stop(*_args):
    raise SystemExit(0)
signal.signal(signal.SIGTERM, _stop)
signal.signal(signal.SIGINT, _stop)
while True:
    try:
        conn, _ = sock.accept()
        conn.close()
    except socket.timeout:
        continue
INNERPY
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(fakeDolt, []byte(fakeDoltScript), 0o755); err != nil {
		t.Fatal(err)
	}

	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.LockFile), 0o755); err != nil {
		t.Fatal(err)
	}

	readyFile := filepath.Join(t.TempDir(), "holder-ready")
	releaseFile := filepath.Join(t.TempDir(), "holder-release")
	holder := exec.Command("sh", "-c", `
set -eu
lock_file="$1"
ready_file="$2"
release_file="$3"
: > "$lock_file"
exec 9>"$lock_file"
flock 9
printf 'ready\n' > "$ready_file"
while [ ! -f "$release_file" ]; do
  sleep 0.1
 done
`, "sh", layout.LockFile, readyFile, releaseFile)
	holder.Env = sanitizedBaseEnv("PATH=" + os.Getenv("PATH"))
	if err := holder.Start(); err != nil {
		t.Fatalf("start lock holder: %v", err)
	}
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- holder.Wait()
	}()
	t.Cleanup(func() {
		_ = os.WriteFile(releaseFile, []byte("release\n"), 0o644)
		select {
		case err := <-holderDone:
			if err != nil {
				t.Errorf("lock holder exit: %v", err)
			}
		case <-time.After(5 * time.Second):
			_ = holder.Process.Kill()
			<-holderDone
			t.Errorf("timed out waiting for lock holder to exit")
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for lock holder to acquire flock")
		}
		time.Sleep(25 * time.Millisecond)
	}

	inodeFor := func(path string) uint64 {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%s): %v", path, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("Stat(%s) did not expose syscall.Stat_t", path)
		}
		return stat.Ino
	}
	beforeInode := inodeFor(layout.LockFile)

	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_BEADS_PORT=3311",
		"GC_BEADS_CONCURRENT_START_READY_TIMEOUT_MS=1000",
		"GC_FAKE_DOLT_INVOCATION_FILE="+invocationFile,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	cmd := exec.Command(script, "start")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		exitErr := &exec.ExitError{}
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("gc-beads-bd start unexpected error: %v", err)
		}
	}
	if exitCode == 0 {
		stop := exec.Command(script, "stop")
		stop.Env = env
		_ = stop.Run()
		t.Fatalf("gc-beads-bd start unexpectedly succeeded while another process held the start lock\n%s", out)
	}
	if exitCode != 1 {
		t.Fatalf("gc-beads-bd start exit %d, want 1\n%s", exitCode, out)
	}
	if !strings.Contains(string(out), "could not acquire dolt start lock") {
		t.Fatalf("gc-beads-bd start output = %q, want lock acquisition failure", out)
	}
	afterInode := inodeFor(layout.LockFile)
	if afterInode != beforeInode {
		t.Fatalf("lock inode changed from %d to %d while original holder was still live", beforeInode, afterInode)
	}
	if invocation, err := os.ReadFile(invocationFile); err == nil && strings.TrimSpace(string(invocation)) != "" {
		t.Fatalf("dolt should not have been invoked while the start lock was held:\n%s", string(invocation))
	}
}

func TestGcBeadsBdStartWaitsForSlowConcurrentStarterSuccess(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	materializeBuiltinPacksForTest(t, cityPath)
	script := gcBeadsBdScriptPath(cityPath)
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.LockFile), 0o755); err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	invocationFile := filepath.Join(t.TempDir(), "gc-invocation")
	startedFile := filepath.Join(t.TempDir(), "starter-ready")
	nowFile := filepath.Join(t.TempDir(), "gc-now-ms")
	fakeGC := filepath.Join(binDir, "gc")
	fakeGCScript := fmt.Sprintf(`#!/bin/sh
set -eu
invocation_file=%q
started_file=%q
now_file=%q
subcmd="$1 $2"
shift 2
case "$subcmd" in
  "dolt-state now-ms")
    if [ -f "$now_file" ]; then
      now=$(cat "$now_file")
    else
      now=1000000
      printf '%%s\n' "$now" > "$now_file"
    fi
    printf '%%s\n' "$now"
    ;;
  "dolt-state runtime-layout")
    city=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city)
          city="$2"
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state runtime-layout\n' >> "$invocation_file"
    printf 'GC_PACK_STATE_DIR\t%%s\n' %q
    printf 'GC_BEADS_DATA_DIR\t%%s\n' %q
    printf 'GC_BEADS_LOG_FILE\t%%s\n' %q
    printf 'GC_BEADS_STATE_FILE\t%%s\n' %q
    printf 'GC_BEADS_PID_FILE\t%%s\n' %q
    printf 'GC_BEADS_LOCK_FILE\t%%s\n' %q
    printf 'GC_BEADS_CONFIG_FILE\t%%s\n' %q
    ;;
  "dolt-state existing-managed")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--host|--port|--user|--timeout-ms)
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state existing-managed\n' >> "$invocation_file"
    if [ -f "$started_file" ]; then
      printf 'managed_pid\t4242\n'
      printf 'managed_owned\ttrue\n'
      printf 'deleted_inodes\tfalse\n'
      printf 'state_port\t3311\n'
      printf 'ready\ttrue\n'
      printf 'reusable\ttrue\n'
    else
      printf 'managed_pid\t0\n'
      printf 'managed_owned\tfalse\n'
      printf 'deleted_inodes\tfalse\n'
      printf 'state_port\t0\n'
      printf 'ready\tfalse\n'
      printf 'reusable\tfalse\n'
    fi
    ;;
  "dolt-state probe-managed")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--host|--port)
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state probe-managed
' >> "$invocation_file"
    printf 'running	true
'
    printf 'port_holder_pid	4242
'
    printf 'port_holder_owned	true
'
    printf 'port_holder_deleted_inodes	false
'
    printf 'tcp_reachable	true
'
    ;;
  "dolt-state query-probe")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --host|--port|--user)
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state query-probe
' >> "$invocation_file"
    if [ -f "$started_file" ]; then
      exit 0
    fi
    exit 1
    ;;
  "dolt-state write-provider")
    state_file=""
    pid=""
    running=""
    port=""
    data_dir=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --file)
          state_file="$2"
          shift 2
          ;;
        --pid)
          pid="$2"
          shift 2
          ;;
        --running)
          running="$2"
          shift 2
          ;;
        --port)
          port="$2"
          shift 2
          ;;
        --data-dir)
          data_dir="$2"
          shift 2
          ;;
        --started-at)
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state write-provider
' >> "$invocation_file"
    mkdir -p "$(dirname "$state_file")"
    printf '{"running":%%s,"pid":%%s,"port":%%s,"data_dir":"%%s","started_at":"2026-04-14T00:00:00Z"}
' "$running" "$pid" "$port" "$data_dir" > "$state_file"
    ;;
  *)
    echo "unexpected gc args: $subcmd $*" >&2
    exit 64
    ;;
esac
`, invocationFile, startedFile, nowFile, layout.PackStateDir, layout.DataDir, layout.LogFile, layout.StateFile, layout.PIDFile, layout.LockFile, layout.ConfigFile)
	if err := os.WriteFile(fakeGC, []byte(fakeGCScript), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nset -eu\ncase \"${1:-}\" in\n  config)\n    exit 0\n    ;;\n  *)\n    printf 'dolt %s\\n' \"$*\" >> \"$GC_FAKE_DOLT_INVOCATION_FILE\"\n    exit 1\n    ;;\nesac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeSleep := filepath.Join(binDir, "sleep")
	fakeSleepScript := fmt.Sprintf(`#!/bin/sh
set -eu
now_file=%q
started_file=%q
if [ "$#" -ne 1 ]; then
  echo "sleep: expected exactly one duration" >&2
  exit 64
fi
case "$1" in
  0.5|0.500)
    ;;
  *)
    echo "sleep: unexpected duration $1" >&2
    exit 64
    ;;
esac
if [ ! -f "$now_file" ]; then
  exit 0
fi
now=$(cat "$now_file")
case "$now" in
  ''|*[!0-9]*)
    echo "sleep: invalid fake clock $now" >&2
    exit 65
    ;;
esac
now=$((now + 500))
printf '%%s\n' "$now" > "$now_file"
if [ "$now" -ge 1011000 ]; then
  : > "$started_file"
fi
`, nowFile, startedFile)
	if err := os.WriteFile(fakeSleep, []byte(fakeSleepScript), 0o755); err != nil {
		t.Fatal(err)
	}
	invokedDolt := filepath.Join(t.TempDir(), "dolt-invocation")

	readyFile := filepath.Join(t.TempDir(), "holder-ready")
	holder := exec.Command("sh", "-c", `
set -eu
lock_file="$1"
ready_file="$2"
: > "$lock_file"
exec 9>"$lock_file"
flock 9
printf 'ready\n' > "$ready_file"
exec sleep 60
`, "sh", layout.LockFile, readyFile)
	holder.Env = sanitizedBaseEnv("PATH=" + os.Getenv("PATH"))
	if err := holder.Start(); err != nil {
		t.Fatalf("start lock holder: %v", err)
	}
	defer func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for lock holder to acquire flock")
		}
		time.Sleep(25 * time.Millisecond)
	}

	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_BEADS_PORT=3311",
		"GC_BEADS_CONCURRENT_START_READY_TIMEOUT_MS=12000",
		"GC_BIN="+fakeGC,
		"GC_FAKE_DOLT_INVOCATION_FILE="+invokedDolt,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	cmd := exec.Command(script, "start")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd start failed while slow concurrent starter was making progress: %v\n%s", err, out)
	}
	readyAt, err := strconv.Atoi(strings.TrimSpace(string(mustReadFile(t, nowFile))))
	if err != nil {
		t.Fatalf("parse simulated concurrent-ready clock: %v", err)
	}
	if elapsed := readyAt - 1000000; elapsed <= 10000 || elapsed >= 12000 {
		t.Fatalf("concurrent starter became ready after %dms, want more than 10000ms and less than the 12000ms deadline", elapsed)
	}
	if invocation, err := os.ReadFile(invokedDolt); err == nil && strings.TrimSpace(string(invocation)) != "" {
		t.Fatalf("dolt should not have been invoked while concurrent starter won:\n%s", string(invocation))
	}
}

func TestGcBeadsBdStartConcurrentWaitPassesRemainingExistingManagedBudget(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	materializeBuiltinPacksForTest(t, cityPath)
	script := gcBeadsBdScriptPath(cityPath)
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.LockFile), 0o755); err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	invocationFile := filepath.Join(t.TempDir(), "gc-invocation")
	nowFile := filepath.Join(t.TempDir(), "gc-now-ms")
	fakeGC := filepath.Join(binDir, "gc")
	fakeGCScript := fmt.Sprintf(`#!/bin/sh
set -eu
invocation_file=%q
now_file=%q
subcmd="$1 $2"
shift 2
case "$subcmd" in
  "dolt-state now-ms")
    if [ -f "$now_file" ]; then
      step=$(cat "$now_file")
    else
      step=0
    fi
    case "$step" in
      0)
        printf '1000000\n'
        printf '1\n' > "$now_file"
        ;;
      1)
        printf '1000000\n'
        printf '2\n' > "$now_file"
        ;;
      *)
        printf '1001000\n'
        printf '3\n' > "$now_file"
        ;;
    esac
    ;;
  "dolt-state runtime-layout")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city)
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state runtime-layout\n' >> "$invocation_file"
    printf 'GC_PACK_STATE_DIR\t%%s\n' %q
    printf 'GC_BEADS_DATA_DIR\t%%s\n' %q
    printf 'GC_BEADS_LOG_FILE\t%%s\n' %q
    printf 'GC_BEADS_STATE_FILE\t%%s\n' %q
    printf 'GC_BEADS_PID_FILE\t%%s\n' %q
    printf 'GC_BEADS_LOCK_FILE\t%%s\n' %q
    printf 'GC_BEADS_CONFIG_FILE\t%%s\n' %q
    ;;
  "dolt-state existing-managed")
    timeout_ms=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--host|--port|--user)
          shift 2
          ;;
        --timeout-ms)
          timeout_ms="$2"
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state existing-managed timeout=%%s\n' "$timeout_ms" >> "$invocation_file"
    if [ "${timeout_ms:-0}" -gt 1500 ]; then
      sleep 2
    fi
    printf 'managed_pid\t4242\n'
    printf 'managed_owned\ttrue\n'
    printf 'deleted_inodes\tfalse\n'
    printf 'state_port\t3311\n'
    printf 'ready\tfalse\n'
    printf 'reusable\tfalse\n'
    ;;
  "dolt-state probe-managed")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--host|--port)
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state probe-managed\n' >> "$invocation_file"
    printf 'running\tfalse\n'
    printf 'port_holder_pid\t0\n'
    printf 'port_holder_owned\tfalse\n'
    printf 'port_holder_deleted_inodes\tfalse\n'
    printf 'tcp_reachable\tfalse\n'
    ;;
  *)
    echo "unexpected gc args: $subcmd $*" >&2
    exit 64
    ;;
esac
`, invocationFile, nowFile, layout.PackStateDir, layout.DataDir, layout.LogFile, layout.StateFile, layout.PIDFile, layout.LockFile, layout.ConfigFile)
	if err := os.WriteFile(fakeGC, []byte(fakeGCScript), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nset -eu\ncase \"${1:-}\" in\n  config)\n    exit 0\n    ;;\n  *)\n    exit 1\n    ;;\nesac\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	readyFile := filepath.Join(t.TempDir(), "holder-ready")
	holder := exec.Command("sh", "-c", `
set -eu
lock_file="$1"
ready_file="$2"
: > "$lock_file"
exec 9>"$lock_file"
flock 9
printf 'ready\n' > "$ready_file"
sleep 5
`, "sh", layout.LockFile, readyFile)
	holder.Env = sanitizedBaseEnv("PATH=" + os.Getenv("PATH"))
	if err := holder.Start(); err != nil {
		t.Fatalf("start lock holder: %v", err)
	}
	defer func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for lock holder to acquire flock")
		}
		time.Sleep(25 * time.Millisecond)
	}

	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_BEADS_PORT=3311",
		"GC_BEADS_CONCURRENT_START_READY_TIMEOUT_MS=1000",
		"GC_BIN="+fakeGC,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	cmd := exec.Command(script, "start")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		exitErr := &exec.ExitError{}
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("gc-beads-bd start unexpected error: %v", err)
		}
	}
	if exitCode != 1 {
		t.Fatalf("gc-beads-bd start exit %d, want 1\n%s", exitCode, out)
	}
	if !strings.Contains(string(out), "could not acquire dolt start lock") {
		t.Fatalf("gc-beads-bd start output = %q, want lock acquisition failure", out)
	}
	invocation := string(mustReadFile(t, invocationFile))
	if strings.Contains(invocation, "timeout=30000") {
		t.Fatalf("existing-managed should not receive the default 30s timeout inside concurrent wait:\n%s", invocation)
	}
	if !strings.Contains(invocation, "timeout=1000") {
		t.Fatalf("existing-managed should receive the remaining wait budget on the first attempt:\n%s", invocation)
	}
}

func TestManagedDoltConfigGoWriterMatchesShellFallbackSemantics(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the materialized gc-beads-bd shell fallback; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	goConfigPath := filepath.Join(t.TempDir(), "go", "dolt-config.yaml")
	if err := writeManagedDoltConfigFile(goConfigPath, "127.0.0.1", "3311", filepath.Join(cityPath, ".beads", "dolt"), "info", config.DoltConfig{}); err != nil {
		t.Fatalf("writeManagedDoltConfigFile: %v", err)
	}

	materializeBuiltinPacksForTest(t, cityPath)
	script := gcBeadsBdScriptPath(cityPath)
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeDolt := filepath.Join(binDir, "dolt")
	fakeDoltScript := `#!/bin/sh
set -eu
case "${1:-}" in
  config)
    exit 0
    ;;
  sql-server)
    config_file=""
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "--config" ]; then
        config_file="$arg"
        break
      fi
      prev="$arg"
    done
    port=$(awk '/port:/ {print $2; exit}' "$config_file")
    data_dir=$(awk '/data_dir:/ {print $2; exit}' "$config_file" | tr -d '"')
    exec python3 - "$port" "$data_dir" <<'INNERPY'
import os
import signal
import socket
import sys
import time
port = int(sys.argv[1])
data_dir = sys.argv[2]
if data_dir:
    os.makedirs(data_dir, exist_ok=True)
    os.chdir(data_dir)
sock = socket.socket()
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind(("0.0.0.0", port))
sock.listen(128)
sock.settimeout(1.0)
def _stop(*_args):
    raise SystemExit(0)
signal.signal(signal.SIGTERM, _stop)
signal.signal(signal.SIGINT, _stop)
while True:
    try:
        conn, _ = sock.accept()
        conn.close()
    except socket.timeout:
        continue
INNERPY
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(fakeDolt, []byte(fakeDoltScript), 0o755); err != nil {
		t.Fatal(err)
	}
	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_BEADS_PORT=3311",
		"GC_BEADS_LOGLEVEL=info",
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	cmd := exec.Command(script, "start")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd start failed: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		stop := exec.Command(script, "stop")
		stop.Env = env
		_ = stop.Run()
	})
	shellConfigPath := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")
	goConfig := readManagedDoltConfigForTest(t, goConfigPath)
	shellConfig := readManagedDoltConfigForTest(t, shellConfigPath)
	if !reflect.DeepEqual(goConfig, shellConfig) {
		t.Fatalf("managed config mismatch\nGo: %+v\nShell: %+v", goConfig, shellConfig)
	}
}

type managedDoltConfigForTest struct {
	LogLevel string `yaml:"log_level"`
	Listener struct {
		Port               int    `yaml:"port"`
		Host               string `yaml:"host"`
		MaxConnections     int    `yaml:"max_connections"`
		BackLog            int    `yaml:"back_log"`
		MaxConnTimeoutMS   int    `yaml:"max_connections_timeout_millis"`
		ReadTimeoutMillis  int    `yaml:"read_timeout_millis"`
		WriteTimeoutMillis int    `yaml:"write_timeout_millis"`
	} `yaml:"listener"`
	DataDir  string `yaml:"data_dir"`
	Behavior struct {
		AutoGCBehavior struct {
			Enable       bool `yaml:"enable"`
			ArchiveLevel int  `yaml:"archive_level"`
		} `yaml:"auto_gc_behavior"`
	} `yaml:"behavior"`
	SystemVariables map[string]string `yaml:"system_variables"`
}

func readManagedDoltConfigForTest(t *testing.T, path string) managedDoltConfigForTest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var cfg managedDoltConfigForTest
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal(%s): %v\n%s", path, err, data)
	}
	return cfg
}

func readDoltStartCountForTest(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read start count: %v", err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse start count %q: %v", strings.TrimSpace(string(data)), err)
	}
	return count
}

func TestGcBeadsBdStartIsIdempotentWhenAlreadyRunning(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	materializeBuiltinPacksForTest(t, cityPath)
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	invocationFile := filepath.Join(t.TempDir(), "gc-invocation")
	fakeGC := writeFakeManagedConfigWriterGC(t, binDir, invocationFile)
	writeFakeManagedConfigWriterDolt(t, binDir)

	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_BIN="+fakeGC,
		"GC_FAKE_DOLT_FAIL_SQL_SERVER=true",
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)

	runStart := func() {
		t.Helper()
		cmd := exec.Command(script, "start")
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gc-beads-bd start failed: %v\n%s", err, out)
		}
	}

	runStart()
	t.Cleanup(func() {
		stop := exec.Command(script, "stop")
		stop.Env = env
		_ = stop.Run()
	})

	runtimeDir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt-from-gc")
	pidPath := filepath.Join(runtimeDir, "dolt.pid")
	statePath := filepath.Join(runtimeDir, "dolt-provider-state.json")
	firstPIDData, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read first pid file: %v", err)
	}
	firstPID := strings.TrimSpace(string(firstPIDData))
	if firstPID == "" {
		t.Fatal("first pid file is empty")
	}
	firstState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read first state file: %v", err)
	}
	if !strings.Contains(string(firstState), "\"pid\":"+firstPID) {
		t.Fatalf("provider state file should record pid %s, got: %s", firstPID, firstState)
	}

	runStart()

	secondPIDData, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read second pid file: %v", err)
	}
	if !bytes.Equal(secondPIDData, firstPIDData) {
		t.Fatalf("repeated start changed pid file from %q to %q", firstPIDData, secondPIDData)
	}
	secondState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read second state file: %v", err)
	}
	if !bytes.Equal(secondState, firstState) {
		t.Fatalf("repeated start changed provider state:\nfirst:  %s\nsecond: %s", firstState, secondState)
	}

	invocation := string(mustReadFile(t, invocationFile))
	if got := strings.Count(invocation, "gc dolt-state existing-managed\n"); got != 2 {
		t.Fatalf("existing-managed invocation count = %d, want 2:\n%s", got, invocation)
	}
	if got := strings.Count(invocation, "gc dolt-state start-managed\n"); got != 1 {
		t.Fatalf("start-managed invocation count = %d, want 1:\n%s", got, invocation)
	}
}

func TestGcBeadsBdStartRestartsServerHoldingDeletedDataInodes(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	materializeBuiltinPacksForTest(t, cityPath)
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	countFile := filepath.Join(t.TempDir(), "dolt-start-count")
	deletedMarkerFile := filepath.Join(t.TempDir(), "deleted-inode-held")
	fakeDolt := filepath.Join(binDir, "dolt")
	port := freeLoopbackPort(t)
	fakeScript := fmt.Sprintf(`#!/bin/sh
set -eu
count_file=%q
data_dir=%q
case "${1:-}" in
  config)
    exit 0
    ;;
  sql-server)
    count=0
    if [ -f "$count_file" ]; then
      count=$(cat "$count_file")
    fi
    count=$((count + 1))
    printf '%%s\n' "$count" > "$count_file"
    config_file=""
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "--config" ]; then
        config_file="$arg"
        break
      fi
      prev="$arg"
    done
    port=$(awk '/port:/ {print $2; exit}' "$config_file")
    exec python3 - "$port" "$data_dir" %q <<'INNERPY'
import os
import signal
import socket
import sys
import time
port = int(sys.argv[1])
data_dir = sys.argv[2]
marker_path = sys.argv[3]
os.makedirs(data_dir, exist_ok=True)
os.chdir(data_dir)
open_file = None
if not os.path.exists(marker_path):
    with open(marker_path, "w") as marker:
        marker.write("held")
    stale = os.path.join(data_dir, "stale-open.txt")
    open_file = open(stale, "w+")
    open_file.write("stale")
    open_file.flush()
    os.unlink(stale)
sock = socket.socket()
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind(("0.0.0.0", port))
sock.listen(128)
sock.settimeout(1.0)
def _stop(*_args):
    raise SystemExit(0)
signal.signal(signal.SIGTERM, _stop)
signal.signal(signal.SIGINT, _stop)
while True:
    try:
        conn, _ = sock.accept()
        conn.close()
    except socket.timeout:
        continue
INNERPY
    ;;
  *)
    exit 0
    ;;
esac
	`, countFile, filepath.Join(cityPath, ".beads", "dolt"), deletedMarkerFile)
	if err := os.WriteFile(fakeDolt, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_BEADS_PORT="+port,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)

	runStart := func() {
		t.Helper()
		cmd := exec.Command(script, "start")
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gc-beads-bd start failed: %v\n%s", err, out)
		}
	}

	runStart()
	t.Cleanup(func() {
		stop := exec.Command(script, "stop")
		stop.Env = env
		_ = stop.Run()
	})

	firstPIDData, err := os.ReadFile(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt.pid"))
	if err != nil {
		t.Fatalf("read first pid file: %v", err)
	}
	firstPID := strings.TrimSpace(string(firstPIDData))
	if firstPID == "" {
		t.Fatal("first pid file is empty")
	}
	initialStartCount := readDoltStartCountForTest(t, countFile)

	runStart()

	secondPIDData, err := os.ReadFile(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt.pid"))
	if err != nil {
		t.Fatalf("read second pid file: %v", err)
	}
	secondPID := strings.TrimSpace(string(secondPIDData))
	if secondPID == "" {
		t.Fatal("second pid file is empty")
	}

	if got := readDoltStartCountForTest(t, countFile); got <= initialStartCount {
		t.Fatalf("dolt sql-server launch count = %d, want greater than initial %d", got, initialStartCount)
	}
}

func TestStartBeadsLifecycleRegistersArchiveLevelOnlyDoltConfig(t *testing.T) {
	realCity := t.TempDir()
	aliasRoot := t.TempDir()
	aliasCity := filepath.Join(aliasRoot, "city-link")
	if err := os.Symlink(realCity, aliasCity); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", aliasCity)
	t.Setenv("GC_BEADS_SKIP", "skip")

	archiveLevel := 1
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Beads:     config.BeadsConfig{Server: config.DoltConfig{ArchiveLevel: &archiveLevel}},
	}
	if err := startBeadsLifecycle(aliasCity, "test-city", cfg, io.Discard); err != nil {
		t.Fatalf("startBeadsLifecycle: %v", err)
	}
	t.Cleanup(func() { cityDoltConfigs.Delete(normalizePathForCompare(realCity)) })

	envEntries := mustProviderLifecycleProcessEnv(t, realCity, "exec:"+gcBeadsBdScriptPath(realCity))
	env := map[string]string{}
	for _, entry := range envEntries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	if got := env["GC_BEADS_ARCHIVE_LEVEL"]; got != "1" {
		t.Fatalf("GC_BEADS_ARCHIVE_LEVEL = %q, want 1", got)
	}
}

func TestStartBeadsLifecycleRegistersAutoGCOnlyDoltConfig(t *testing.T) {
	// A city.toml whose [dolt] table sets only auto_gc_enabled = false (the
	// documented opt-out and the documented rollback for the auto-GC default
	// flip) must register its dolt config so the override reaches the managed
	// dolt server. Without AutoGCEnabled in cityDoltConfigHasLifecycleFields,
	// startBeadsLifecycle clears the registry entry and
	// providerLifecycleProcessEnvFromBase never projects
	// GC_BEADS_AUTO_GC_ENABLED, so the shell fallback re-enables auto-GC.
	realCity := t.TempDir()
	aliasRoot := t.TempDir()
	aliasCity := filepath.Join(aliasRoot, "city-link")
	if err := os.Symlink(realCity, aliasCity); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", aliasCity)
	t.Setenv("GC_BEADS_SKIP", "skip")

	autoGCOff := false
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Beads:     config.BeadsConfig{Server: config.DoltConfig{AutoGCEnabled: &autoGCOff}},
	}
	if err := startBeadsLifecycle(aliasCity, "test-city", cfg, io.Discard); err != nil {
		t.Fatalf("startBeadsLifecycle: %v", err)
	}
	t.Cleanup(func() { cityDoltConfigs.Delete(normalizePathForCompare(realCity)) })

	envEntries := mustProviderLifecycleProcessEnv(t, realCity, "exec:"+gcBeadsBdScriptPath(realCity))
	env := map[string]string{}
	for _, entry := range envEntries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	if got := env["GC_BEADS_AUTO_GC_ENABLED"]; got != "false" {
		t.Fatalf("GC_BEADS_AUTO_GC_ENABLED = %q, want false", got)
	}
}

func TestHealthBeadsProviderDoesNotRecoverExternalLoopbackTarget(t *testing.T) {
	cityPath := t.TempDir()
	callLog := filepath.Join(cityPath, "op-calls.log")
	script := gcBeadsBdScriptPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	scriptText := "#!/bin/sh\necho \"$1\" >> " + callLog + "\nif [ \"$1\" = \"health\" ]; then\n  echo \"health failed\" >&2\n  exit 1\nfi\nexit 0\n"
	if err := os.WriteFile(script, []byte(scriptText), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.host: 127.0.0.1
dolt.port: "4406"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)

	// Defensively ensure the call log does not pre-exist. t.TempDir()
	// provides a fresh directory, but other test-global resolution paths
	// (e.g., beadsProvider → gcBeadsBdScriptPath) may resolve to the same
	// script location and invoke it before this test reaches the SUT call.
	_ = os.Remove(callLog)

	err := healthBeadsProvider(cityPath)
	if err == nil || !strings.Contains(err.Error(), "exec beads health: health failed") {
		t.Fatalf("healthBeadsProvider() error = %v, want direct external health failure", err)
	}

	data, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("reading call log: %v", readErr)
	}
	ops := strings.TrimSpace(string(data))
	if ops != "health" {
		t.Fatalf("call log = %q, want only health", ops)
	}
}

func TestShutdownBeadsProviderSkipsExternalLoopbackTarget(t *testing.T) {
	cityPath := t.TempDir()
	callLog := filepath.Join(cityPath, "op-calls.log")
	script := gcBeadsBdScriptPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$1\" >> "+callLog+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.host: 127.0.0.1
dolt.port: "4406"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	if err := shutdownBeadsProvider(cityPath); err != nil {
		t.Fatalf("shutdownBeadsProvider() error = %v", err)
	}
	if _, err := os.Stat(callLog); !os.IsNotExist(err) {
		t.Fatalf("shutdownBeadsProvider() should not invoke stop for external loopback target, stat err = %v", err)
	}
}

// ── startBeadsLifecycle skips provider for external ───────────────────

func TestStartBeadsLifecycleSkipsProviderForPostgresCity(t *testing.T) {
	cityPath := t.TempDir()
	callLog := filepath.Join(cityPath, "op-calls.log")
	script := writeManagedBdTestScript(t, "#!/bin/sh\necho \"$1\" >> "+callLog+"\nexit 99\n")
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(cityPath, ".beads", "metadata.json"), contract.MetadataState{
		Database:         "beads",
		Backend:          "postgres",
		PostgresHost:     "db.example.test",
		PostgresPort:     "5432",
		PostgresUser:     "bd",
		PostgresDatabase: "beads_pg",
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	if err := startBeadsLifecycle(cityPath, "test-city", cfg, io.Discard); err != nil {
		t.Fatalf("startBeadsLifecycle() error = %v", err)
	}
	if _, err := os.Stat(callLog); !os.IsNotExist(err) {
		t.Fatalf("startBeadsLifecycle() should not invoke provider for postgres city, stat err = %v", err)
	}
}

func TestGcBeadsBdInitNormalizesScopeAndRemovesLocalServerArtifacts(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "gascity"
prefix = "gc"

[beads]
provider = "bd"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	materializeBuiltinPacksForTest(t, cityPath)
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	bdInitLog := filepath.Join(t.TempDir(), "bd-init.args")
	fakeBd := filepath.Join(binDir, "bd")
	fakeBdScript := `#!/bin/sh
set -eu
case "${1:-}" in
  init)
    last=""
    for arg in "$@"; do
      last="$arg"
    done
    mkdir -p "$last/.beads"
    cat > "$last/.beads/metadata.json" <<'JSON'
{"database":"legacy","backend":"legacy","dolt_mode":"embedded","dolt_database":"wrong-db","dolt_server_host":"127.0.0.1","dolt_server_port":"3307"}
JSON
    cat > "$last/.beads/config.yaml" <<'YAML'
issue-prefix: stale
dolt.auto-start: true
dolt_server_port: 3307
YAML
    : > "$last/.beads/dolt-server.pid"
	: > "$last/.beads/dolt-server.lock"
	: > "$last/.beads/dolt-server.log"
	printf '3307\n' > "$last/.beads/dolt-server.port"
	printf '%s\n' "$*" > "` + bdInitLog + `"
	exit 0
	;;
  *)
	echo "unexpected bd command: $*" >&2
	exit 64
	;;
esac
`
	if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	reexecGC := reexecGCTestBinaryForTests(t)
	gcWrapper := filepath.Join(binDir, "gc-wrapper")
	gcWrapperScript := fmt.Sprintf(`#!/bin/sh
set -eu
real_gc=%q
case "${1:-} ${2:-}" in
	"dolt-state ensure-project-id")
		exit 0
		;;
	"dolt-config normalize-scope")
		exec "$real_gc" "$@"
		;;
	*)
		echo "unexpected gc helper command: $*" >&2
		exit 64
		;;
esac
`, reexecGC)
	if err := os.WriteFile(gcWrapper, []byte(gcWrapperScript), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script, "init", rigPath, "fe", "fe")
	cmd.Env = sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
		"GC_CITY_PATH="+cityPath,
		"GC_BEADS=bd",
		"GC_BIN="+gcWrapper,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd init failed: %v\n%s", err, out)
	}
	bdInitData, err := os.ReadFile(bdInitLog)
	if err != nil {
		t.Fatalf("ReadFile(bd init call): %v", err)
	}
	if args := strings.Fields(string(bdInitData)); len(args) == 0 || args[0] != "init" {
		t.Fatalf("bd init call = %q, want init invocation before normalization", strings.TrimSpace(string(bdInitData)))
	}

	metaData, err := os.ReadFile(filepath.Join(rigPath, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("ReadFile(rig metadata): %v", err)
	}
	var metadata struct {
		DoltDatabase string `json:"dolt_database"`
	}
	if err := json.Unmarshal(metaData, &metadata); err != nil {
		t.Fatalf("Unmarshal(rig metadata): %v", err)
	}
	if metadata.DoltDatabase != "fe" {
		t.Fatalf("rig dolt_database = %q, want fresh-init scope %q", metadata.DoltDatabase, "fe")
	}

	rigCfg, err := os.ReadFile(filepath.Join(rigPath, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("ReadFile(rig config): %v", err)
	}
	cfgText := string(rigCfg)
	for _, want := range []string{"issue_prefix: fe", "gc.endpoint_origin: inherited_city"} {
		if !strings.Contains(cfgText, want) {
			t.Fatalf("rig config missing %q:\n%s", want, cfgText)
		}
	}

	artifact := filepath.Join(rigPath, ".beads", "dolt-server.port")
	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Fatalf("fresh-init local server artifact should be removed, stat err = %v", err)
	}
}

func TestGcBeadsBdWriteConfigYamlFallsBackToShellWhenGCBinUnset(t *testing.T) {
	scriptData, err := bdpack.PackFS.ReadFile("assets/scripts/gc-beads-bd.sh")
	if err != nil {
		t.Fatalf("read embedded gc-beads-bd.sh: %v", err)
	}
	prelude, _, ok := strings.Cut(string(scriptData), "\n# --- Main ---\n")
	if !ok {
		t.Fatal("embedded gc-beads-bd.sh missing main marker")
	}

	testDir := t.TempDir()
	configFile := filepath.Join(testDir, "dolt-config.yaml")
	dataDir := filepath.Join(testDir, "dolt-data")
	const (
		wantHost     = "127.0.0.42"
		wantPort     = 13306
		wantLogLevel = "debug"
	)

	binDir := t.TempDir()
	sentinelGC := filepath.Join(binDir, "gc")
	if err := os.WriteFile(sentinelGC, []byte(`#!/bin/sh
printf '%s\n' "$*" > "${0}.called"
printf 'PATH gc sentinel invoked\n' >&2
exit 97
`), 0o755); err != nil {
		t.Fatalf("write PATH gc sentinel: %v", err)
	}

	harness := prelude + `
if [ "${GC_BIN+x}" = x ]; then
	printf 'GC_BIN must be unset\n' >&2
	exit 96
fi
CONFIG_FILE="$1"
DATA_DIR="$2"
DOLT_HOST="$3"
DOLT_PORT="$4"
DOLT_LOGLEVEL="$5"
write_config_yaml
`
	harnessPath := filepath.Join(testDir, "write-config-harness.sh")
	if err := os.WriteFile(harnessPath, []byte(harness), 0o755); err != nil {
		t.Fatalf("write shell config harness: %v", err)
	}
	cmd := exec.Command("sh", harnessPath, configFile, dataDir, wantHost, strconv.Itoa(wantPort), wantLogLevel)
	cmd.Env = sanitizedBaseEnv(
		"PATH=" + strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	out, runErr := cmd.CombinedOutput()
	if invocation, err := os.ReadFile(sentinelGC + ".called"); err == nil {
		t.Fatalf("PATH gc sentinel was called with %q while GC_BIN was empty\n%s", strings.TrimSpace(string(invocation)), out)
	} else if !os.IsNotExist(err) {
		t.Fatalf("read PATH gc sentinel record: %v", err)
	}
	if runErr != nil {
		t.Fatalf("write_config_yaml shell fallback failed: %v\n%s", runErr, out)
	}

	cfg := readManagedDoltConfigForTest(t, configFile)
	if got := cfg.Listener.Host; got != wantHost {
		t.Fatalf("listener.host = %q, want %q", got, wantHost)
	}
	if got := cfg.Listener.Port; got != wantPort {
		t.Fatalf("listener.port = %d, want %d", got, wantPort)
	}
	if got := cfg.DataDir; got != dataDir {
		t.Fatalf("data_dir = %q, want %q", got, dataDir)
	}
	if got := cfg.LogLevel; got != wantLogLevel {
		t.Fatalf("log_level = %q, want %q", got, wantLogLevel)
	}
}

func TestAcquireProviderSemaphore_SerializesConcurrentOps(t *testing.T) {
	t.Parallel()
	cityPath := t.TempDir()

	// First acquire succeeds immediately.
	release1, err := acquireProviderSemaphore(context.Background(), cityPath)
	if err != nil {
		t.Fatalf("acquireProviderSemaphore first: %v", err)
	}

	// Second acquire should block.
	acquired := make(chan struct{})
	go func() {
		release2, err := acquireProviderSemaphore(context.Background(), cityPath)
		if err != nil {
			return
		}
		close(acquired)
		release2()
	}()

	select {
	case <-acquired:
		t.Fatal("second acquire succeeded while first still held")
	case <-time.After(50 * time.Millisecond):
		// Expected — still blocked.
	}

	// Release first — second should unblock.
	release1()

	select {
	case <-acquired:
		// Expected.
	case <-time.After(2 * time.Second):
		t.Fatal("second acquire did not unblock after release")
	}
}

func TestAcquireProviderSemaphore_IndependentCities(t *testing.T) {
	t.Parallel()
	city1 := t.TempDir()
	city2 := t.TempDir()

	release1, err := acquireProviderSemaphore(context.Background(), city1)
	if err != nil {
		t.Fatalf("acquireProviderSemaphore city1: %v", err)
	}
	defer release1()

	// Different city should not block.
	acquired := make(chan struct{})
	go func() {
		release2, err := acquireProviderSemaphore(context.Background(), city2)
		if err != nil {
			return
		}
		close(acquired)
		release2()
	}()

	select {
	case <-acquired:
		// Expected — different cities are independent.
	case <-time.After(2 * time.Second):
		t.Fatal("acquire for different city blocked unexpectedly")
	}
}

func TestAcquireProviderSemaphoreHonorsContextDeadline(t *testing.T) {
	t.Parallel()
	cityPath := t.TempDir()

	release1, err := acquireProviderSemaphore(context.Background(), cityPath)
	if err != nil {
		t.Fatalf("acquireProviderSemaphore first: %v", err)
	}
	defer release1()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	release2, err := acquireProviderSemaphore(ctx, cityPath)
	if err == nil {
		release2()
		t.Fatal("second acquire succeeded while first still held")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquireProviderSemaphore error = %v, want context deadline", err)
	}
}

func TestEnsureBeadsProviderSerializesConcurrentExecStarts(t *testing.T) {
	cityPath := t.TempDir()
	script := filepath.Join(cityPath, "provider.sh")
	lockDir := filepath.Join(cityPath, "provider.lock")
	callLog := filepath.Join(cityPath, "provider.log")
	scriptBody := fmt.Sprintf(`#!/bin/sh
set -eu
if [ "$1" = "start" ]; then
  if ! mkdir %q 2>/dev/null; then
    echo "overlap" >&2
    exit 1
  fi
  echo "start" >> %q
  sleep 0.1
  rmdir %q
  exit 0
fi
exit 2
`, lockDir, callLog, lockDir)
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)

	errs := make(chan error, 2)
	for range 2 {
		go func() {
			errs <- ensureBeadsProvider(cityPath)
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("ensureBeadsProvider: %v", err)
		}
	}

	data, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	if got := strings.Count(string(data), "start"); got != 2 {
		t.Fatalf("start call count = %d, want 2; log:\n%s", got, data)
	}
}

func TestHealthBeadsProviderSerializesConcurrentExecHealthChecks(t *testing.T) {
	cityPath := t.TempDir()
	script := filepath.Join(cityPath, "provider.sh")
	lockDir := filepath.Join(cityPath, "provider.lock")
	callLog := filepath.Join(cityPath, "provider.log")
	scriptBody := fmt.Sprintf(`#!/bin/sh
set -eu
if [ "$1" = "health" ]; then
  if ! mkdir %q 2>/dev/null; then
    echo "overlap" >&2
    exit 1
  fi
  echo "health" >> %q
  sleep 0.1
  rmdir %q
  exit 0
fi
exit 2
`, lockDir, callLog, lockDir)
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)

	errs := make(chan error, 2)
	for range 2 {
		go func() {
			errs <- healthBeadsProvider(cityPath)
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("healthBeadsProvider: %v", err)
		}
	}

	data, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	if got := strings.Count(string(data), "health"); got != 2 {
		t.Fatalf("health call count = %d, want 2; log:\n%s", got, data)
	}
}

func setupFileProviderCityForTest(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir beads: %v", err)
	}
	return tmp
}

// setupBdContractCityForTest returns a temp city dir that satisfies
// cityUsesBdStoreContract but has no published Dolt port.
func setupBdContractCityForTest(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_BEADS_SCOPE_ROOT", tmp)
	beadsDir := filepath.Join(tmp, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir beads: %v", err)
	}
	// Minimum to make cityUsesBdStoreContract return true: the
	// canonical config file presence is what's checked. Drop the file
	// the function expects.
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("issue_prefix: tc\nissue-prefix: tc\n"), 0o644); err != nil {
		t.Fatalf("seed config.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"backend":"dolt","dolt_database":"hq","dolt_mode":"server"}`), 0o644); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
	return tmp
}

// writeBreakerAwarePreflightFakes sets up a fake gc-beads-bd script whose
// `health` op writes its name to opsFile then fails with healthStderr, and
// whose `recover` op writes its name and exits 0. Returns the ops-log path
// for later assertion.
func writeBreakerAwarePreflightFakes(t *testing.T, cityPath, healthStderr string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "issue_prefix: gc\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\n"
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	opsFile := filepath.Join(t.TempDir(), "provider-ops.log")
	script := gcBeadsBdScriptPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' "$1" >> %q
case "$1" in
  health)
    printf '%%s\n' %q >&2
    exit 1
    ;;
  recover)
    exit 0
    ;;
  *)
    exit 2
    ;;
esac
`, opsFile, healthStderr)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return opsFile
}

func TestHealthBeadsProviderBacksOffSecondRecoverWithinCooldown(t *testing.T) {
	cityPath := t.TempDir()
	writeMinimalCityToml(t, cityPath)
	opsFile := writeBreakerAwarePreflightFakes(t, cityPath, "unhealthy")

	cityKey := normalizePathForCompare(cityPath)
	t.Cleanup(func() { lastBeadsProviderRecover.Delete(cityKey) })

	t0 := time.Unix(1_700_000_000, 0).UTC()
	clock := []time.Time{t0, t0.Add(5 * time.Second)}
	var idx int
	prevNow, prevCD := providerRecoverNow, providerRecoverCooldown
	providerRecoverNow = func() time.Time {
		v := clock[idx]
		if idx < len(clock)-1 {
			idx++
		}
		return v
	}
	providerRecoverCooldown = func() time.Duration { return 30 * time.Second }
	t.Cleanup(func() {
		providerRecoverNow, providerRecoverCooldown = prevNow, prevCD
	})

	// First call: health fails (non-breaker) → records timestamp + invokes
	// recover. Downstream publish/wait may error; we only assert the OPS log.
	_ = healthBeadsProvider(cityPath)
	// Second call (5s later, < 30s cooldown): recover must be skipped.
	_ = healthBeadsProvider(cityPath)

	ops, readErr := os.ReadFile(opsFile)
	if readErr != nil {
		t.Fatalf("read provider ops: %v", readErr)
	}
	got := strings.Fields(strings.TrimSpace(string(ops)))
	if h, r := countOps(got, "health", "recover"); h < 2 || r != 1 {
		t.Fatalf("provider ops = %v; want health>=2 and recover==1 (2nd recover gated by cooldown)", got)
	}
}

func TestHealthBeadsProviderAllowsRecoverAfterCooldown(t *testing.T) {
	cityPath := t.TempDir()
	writeMinimalCityToml(t, cityPath)
	opsFile := writeBreakerAwarePreflightFakes(t, cityPath, "unhealthy")

	cityKey := normalizePathForCompare(cityPath)
	t.Cleanup(func() { lastBeadsProviderRecover.Delete(cityKey) })

	t0 := time.Unix(1_700_000_000, 0).UTC()
	clock := []time.Time{t0, t0.Add(60 * time.Second)}
	var idx int
	prevNow, prevCD := providerRecoverNow, providerRecoverCooldown
	providerRecoverNow = func() time.Time {
		v := clock[idx]
		if idx < len(clock)-1 {
			idx++
		}
		return v
	}
	providerRecoverCooldown = func() time.Duration { return 30 * time.Second }
	t.Cleanup(func() {
		providerRecoverNow, providerRecoverCooldown = prevNow, prevCD
	})

	_ = healthBeadsProvider(cityPath)
	_ = healthBeadsProvider(cityPath)

	ops, readErr := os.ReadFile(opsFile)
	if readErr != nil {
		t.Fatalf("read provider ops: %v", readErr)
	}
	got := strings.Fields(strings.TrimSpace(string(ops)))
	if h, r := countOps(got, "health", "recover"); h < 2 || r != 2 {
		t.Fatalf("provider ops = %v; want health>=2 and recover==2 (2nd recover allowed past cooldown)", got)
	}
}

func countOps(ops []string, names ...string) (int, int) {
	counts := make(map[string]int, len(names))
	for _, op := range ops {
		counts[op]++
	}
	if len(names) != 2 {
		panic("countOps expects two op names")
	}
	return counts[names[0]], counts[names[1]]
}
