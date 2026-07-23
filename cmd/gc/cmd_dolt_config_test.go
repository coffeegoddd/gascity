package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"gopkg.in/yaml.v3"
)

func TestDoltConfigWriteManagedCmd(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "packs", "dolt", "dolt-config.yaml")
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"dolt-config", "write-managed",
		"--file", configPath,
		"--host", "127.0.0.1",
		"--port", "3311",
		"--data-dir", "/tmp/city/.beads/dolt",
		"--log-level", "warning",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", configPath, err)
	}
	text := string(data)
	for _, want := range []string{
		"log_level: warning",
		"port: 3311",
		"host: 127.0.0.1",
		`data_dir: "/tmp/city/.beads/dolt"`,
		"archive_level: 0",
		"enable: true",
		"back_log: 50",
		"max_connections_timeout_millis: 5000",
		`dolt_auto_gc_enabled: "ON"`,
		`dolt_stats_enabled: "OFF"`,
		`dolt_stats_gc_enabled: "OFF"`,
		`dolt_stats_memory_only: "ON"`,
		`dolt_stats_paused: "ON"`,
		`wait_timeout: "30"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing %q:\n%s", want, text)
		}
	}
}

func TestDoltConfigWriterIncludesDoctorExpectedCoreValues(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "packs", "dolt", "dolt-config.yaml")
	if err := writeManagedDoltConfigFile(configPath, "127.0.0.1", "3311", "/tmp/city/.beads/dolt", "warning", config.DoltConfig{}); err != nil {
		t.Fatalf("writeManagedDoltConfigFile: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", configPath, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("Unmarshal config: %v", err)
	}

	for _, exp := range doctor.DoltConfigExpectedValues() {
		got, ok := lookupTestYAMLPath(doc, exp.Path)
		if !ok {
			t.Fatalf("managed config missing doctor-expected core path %q:\n%s", exp.Path, data)
		}
		if !testYAMLValueEqual(got, exp.Value) {
			t.Fatalf("managed config %s = %v (%T), want %v (%T)", exp.Path, got, got, exp.Value, exp.Value)
		}
	}
}

func TestWriteManagedDoltConfigFile_UsesCityDoltListenerOverrides(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "packs", "dolt", "dolt-config.yaml")
	if err := writeManagedDoltConfigFile(configPath, "127.0.0.1", "3311", "/tmp/dolt-data", "warning", config.DoltConfig{
		ReadTimeoutMillis:  300000,
		WriteTimeoutMillis: 600000,
		MaxConnections:     1024,
	}); err != nil {
		t.Fatalf("writeManagedDoltConfigFile: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"read_timeout_millis: 300000",
		"write_timeout_millis: 600000",
		"max_connections: 1024",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing %q:\n%s", want, text)
		}
	}
}

func lookupTestYAMLPath(doc map[string]any, dotted string) (any, bool) {
	parts := strings.Split(dotted, ".")
	var cur any = doc
	for _, part := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func testYAMLValueEqual(got, want any) bool {
	switch want := want.(type) {
	case int:
		switch got := got.(type) {
		case int:
			return got == want
		case int64:
			return got == int64(want)
		case uint64:
			return got == uint64(want)
		case float64:
			return got == float64(want)
		}
	case bool:
		gotBool, ok := got.(bool)
		return ok && gotBool == want
	default:
		return got == want
	}
	return false
}

func TestDoltConfigWriteManagedCmd_ExplicitArchiveLevel(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "packs", "dolt", "dolt-config.yaml")
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"dolt-config", "write-managed",
		"--file", configPath,
		"--host", "127.0.0.1",
		"--port", "3311",
		"--data-dir", "/tmp/city/.beads/dolt",
		"--archive-level", "1",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", configPath, err)
	}
	if !strings.Contains(string(data), "archive_level: 1") {
		t.Fatalf("config missing archive_level: 1:\n%s", data)
	}
}

func TestDoltConfigWriteManagedCmd_AutoGCCanBeDisabled(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "packs", "dolt", "dolt-config.yaml")
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"dolt-config", "write-managed",
		"--file", configPath,
		"--host", "127.0.0.1",
		"--port", "3311",
		"--data-dir", "/tmp/city/.beads/dolt",
		"--auto-gc-enabled=false",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", configPath, err)
	}
	text := string(data)
	for _, want := range []string{
		"enable: false",
		`dolt_auto_gc_enabled: "OFF"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing %q:\n%s", want, text)
		}
	}
}

func TestWriteManagedDoltConfigFile_AutoGCDefaultsOn(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "packs", "dolt", "dolt-config.yaml")
	if err := writeManagedDoltConfigFile(configPath, "127.0.0.1", "3311", "/tmp/dolt-data", "warning", config.DoltConfig{}); err != nil {
		t.Fatalf("writeManagedDoltConfigFile: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"enable: true",
		`dolt_auto_gc_enabled: "ON"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing %q:\n%s", want, text)
		}
	}
}

func TestWriteManagedDoltConfigFile_DefaultLogLevel(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "packs", "dolt", "dolt-config.yaml")
	if err := writeManagedDoltConfigFile(configPath, "127.0.0.1", "3311", "/tmp/dolt-data", "", config.DoltConfig{}); err != nil {
		t.Fatalf("writeManagedDoltConfigFile: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "log_level: warning") {
		t.Fatalf("empty logLevel should default to warning, got:\n%s", text)
	}
}

func TestWriteManagedDoltConfigFile_WaitTimeoutCanBeDisabled(t *testing.T) {
	t.Setenv("GC_BEADS_WAIT_TIMEOUT", "-1")
	configPath := filepath.Join(t.TempDir(), "packs", "dolt", "dolt-config.yaml")
	if err := writeManagedDoltConfigFile(configPath, "127.0.0.1", "3311", "/tmp/dolt-data", "", config.DoltConfig{}); err != nil {
		t.Fatalf("writeManagedDoltConfigFile: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(data), "wait_timeout") {
		t.Fatalf("negative GC_BEADS_WAIT_TIMEOUT should disable wait_timeout override:\n%s", data)
	}
}

// TestDoltliteReindexCheckMatchesBuildCapability pins the ga-7hei capability
// probe the maintenance shell gate depends on: `gc dolt-config
// doltlite-reindex --check` must exit 0 exactly when this build can reindex in
// process, and non-zero otherwise. The shell's doltlite_reindex_supported uses
// that exit code to skip the stale-index-producing flatten/gc on a build that
// cannot heal the result, so a mis-wired flag would either reintroduce the
// unhealable-corruption bug or block maintenance on a capable build. The test
// runs in both build tags and asserts consistency with doltliteReindexSupported.
func TestDoltliteReindexCheckMatchesBuildCapability(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-config", "doltlite-reindex", "--dir", dir, "--check"}, &stdout, &stderr)
	if doltliteReindexSupported() {
		if code != 0 {
			t.Fatalf("--check on a reindex-capable build = %d, want 0; stderr=%s", code, stderr.String())
		}
		return
	}
	if code == 0 {
		t.Fatalf("--check on a non-capable build exited 0; the shell gate relies on a non-zero exit to skip " +
			"the stale-index-producing flatten/gc (ga-7hei)")
	}
	if !strings.Contains(stderr.String(), "not supported") {
		t.Fatalf("--check failure should state that in-process reindex is not supported, got stderr=%s", stderr.String())
	}
}
