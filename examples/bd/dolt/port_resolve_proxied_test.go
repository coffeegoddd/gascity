package dolt_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeStoreMetadata creates <city>/.beads/metadata.json with the given
// dolt_mode, mirroring what `bd init` persists. The proxied port-resolution
// shim reads dolt_mode from here to decide whether bd owns the endpoint.
func writeStoreMetadata(t *testing.T, cityPath, doltMode string) {
	t.Helper()
	beadsDir := filepath.Join(cityPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	content := `{"database":"hq","backend":"dolt","dolt_mode":"` + doltMode + `"}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
}

// sourcePortResolve sources port_resolve.sh (with a no-op managed_runtime_port,
// so no managed port is ever discovered) and then runs body. envExtra is
// appended after the GC_/DOLT_ scrub so GC_CITY_PATH / GC_BEADS_PROXIED survive.
func sourcePortResolve(t *testing.T, body string, envExtra ...string) portResolveResult {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available; skipping shell-function test")
	}
	root := repoRoot(t)
	driver := "managed_runtime_port() { return 0; }\n. " +
		shellQuote(filepath.Join(root, "assets", "scripts", "port_resolve.sh")) + "\n" + body
	cmd := exec.Command("sh", "-c", driver)
	cmd.Env = append(filteredEnv(), envExtra...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		exitErr := &exec.ExitError{}
		if !errors.As(err, &exitErr) {
			t.Fatalf("port_resolve driver failed to run: %v\nstderr:\n%s", err, stderr.String())
		}
		code = exitErr.ExitCode()
	}
	return portResolveResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// TestPortResolveProxiedModeDetectedFromMetadata proves the shim detects
// proxied-server mode from .beads/metadata.json and that
// resolve_dolt_port_or_die then RETURNS (rc 0, no port) instead of exit-78'ing —
// bd owns the endpoint, so there is nothing for gascity to resolve.
func TestPortResolveProxiedModeDetectedFromMetadata(t *testing.T) {
	city := t.TempDir()
	writeStoreMetadata(t, city, "proxied-server")

	detected := sourcePortResolve(t, `printf '%s' "${GC_BEADS_PROXIED:-unset}"`, "GC_CITY_PATH="+city)
	if detected.code != 0 || detected.stdout != "1" {
		t.Fatalf("GC_BEADS_PROXIED = %q (rc=%d), want \"1\"\nstderr:\n%s", detected.stdout, detected.code, detected.stderr)
	}

	// resolve returns (not exits) — the trailing echo must run and print rc=0.
	resolved := sourcePortResolve(t,
		`resolve_dolt_port_or_die "$STATE_FILE" "$PROV" "$DATA" "$CITY"; echo "rc=$?"`,
		"GC_CITY_PATH="+city,
		"STATE_FILE="+filepath.Join(city, "s.json"),
		"PROV="+filepath.Join(city, "p.json"),
		"DATA="+filepath.Join(city, "d"),
		"CITY="+city,
	)
	if resolved.code != 0 {
		t.Fatalf("resolve rc=%d, want 0\nstdout:\n%s\nstderr:\n%s", resolved.code, resolved.stdout, resolved.stderr)
	}
	if resolved.stdout != "rc=0\n" {
		t.Fatalf("resolve stdout=%q, want \"rc=0\\n\" (no port emitted, function returned)", resolved.stdout)
	}
}

// TestPortResolveProxiedGuardHonorsExplicitFlag proves an explicit
// GC_BEADS_PROXIED=1 short-circuits port resolution even with no metadata,
// and that the source-time detection does not clobber a caller-set flag.
func TestPortResolveProxiedGuardHonorsExplicitFlag(t *testing.T) {
	city := t.TempDir() // no metadata.json

	got := sourcePortResolve(t,
		`resolve_dolt_port_or_die "$STATE_FILE" "$PROV" "$DATA" "$CITY"; echo "rc=$?"`,
		"GC_CITY_PATH="+city, "GC_BEADS_PROXIED=1",
		"STATE_FILE="+filepath.Join(city, "s.json"),
		"PROV="+filepath.Join(city, "p.json"),
		"DATA="+filepath.Join(city, "d"),
		"CITY="+city,
	)
	if got.code != 0 || got.stdout != "rc=0\n" {
		t.Fatalf("explicit proxied flag: rc=%d stdout=%q, want rc=0 and \"rc=0\\n\"\nstderr:\n%s", got.code, got.stdout, got.stderr)
	}
}

// TestPortResolveLegacyModeStillExit78 proves a non-proxied (managed "server")
// city with no resolvable port still exit-78s — the detection must not
// misclassify legacy cities as proxied and silently swallow the failure.
func TestPortResolveLegacyModeStillExit78(t *testing.T) {
	city := t.TempDir()
	writeStoreMetadata(t, city, "server")

	got := sourcePortResolve(t,
		`resolve_dolt_port_or_die "$STATE_FILE" "$PROV" "$DATA" "$CITY"; echo should-not-print`,
		"GC_CITY_PATH="+city,
		"STATE_FILE="+filepath.Join(city, "dolt-state.json"),
		"PROV="+filepath.Join(city, "p.json"),
		"DATA="+filepath.Join(city, "d"),
		"CITY="+city,
	)
	if got.code != 78 {
		t.Fatalf("legacy mode rc=%d, want 78\nstdout:\n%s\nstderr:\n%s", got.code, got.stdout, got.stderr)
	}
	if strings.Contains(got.stdout, "should-not-print") {
		t.Fatalf("exit 78 did not terminate the shell; stdout=%q", got.stdout)
	}
}
