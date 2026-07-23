package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestNewBeadsCmdIncludesCityEndpointSubcommands(t *testing.T) {
	cmd := newBeadsCmd(&bytes.Buffer{}, &bytes.Buffer{})
	city, _, err := cmd.Find([]string{"city"})
	if err != nil {
		t.Fatalf("Find(city): %v", err)
	}
	if city == nil || city.Name() != "city" {
		t.Fatalf("city command = %#v", city)
	}
	useExternal, _, err := cmd.Find([]string{"city", "use-external"})
	if err != nil {
		t.Fatalf("Find(city use-external): %v", err)
	}
	if useExternal == nil || useExternal.Name() != "use-external" {
		t.Fatalf("use-external command = %#v", useExternal)
	}
}

func TestDoBeadsCityEndpointRejectsGCBeadsFileOverride(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doBeadsCityEndpoint(fsys.OSFS{}, cityDir, cityEndpointOptions{External: true, Host: "db.example.com", Port: "4406", AdoptUnverified: true}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doBeadsCityEndpoint() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "only supported for bd-backed beads providers") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestValidateCityEndpointOptionsRejectsWildcardExternalHost(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "::"} {
		t.Run(host, func(t *testing.T) {
			err := validateCityEndpointOptions(cityEndpointOptions{External: true, Host: host, Port: "4406"})
			if err == nil || !strings.Contains(err.Error(), "invalid --host") {
				t.Fatalf("validateCityEndpointOptions(%q) error = %v", host, err)
			}
		})
	}
}

func TestDoBeadsCityEndpointSupportsExecGcBeadsBdProvider(t *testing.T) {
	cityDir := t.TempDir()
	inheritDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(inheritDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityEndpointCityConfigWithCompat(t, cityDir, []config.Rig{{Name: "frontend", Path: inheritDir, Prefix: "fe"}})
	writeRigEndpointMetadata(t, cityDir, "hq")
	writeRigEndpointMetadata(t, inheritDir, "fe")
	t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(cityDir))

	var stdout, stderr bytes.Buffer
	code := doBeadsCityEndpoint(fsys.OSFS{}, cityDir, cityEndpointOptions{External: true, Host: "db.example.com", Port: "4406", AdoptUnverified: true, DryRun: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doBeadsCityEndpoint() = %d, stderr = %s", code, stderr.String())
	}
}

func TestDoBeadsCityUseExternalDryRunDoesNotWriteFiles(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	inheritDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(inheritDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityEndpointCityConfigWithCompat(t, cityDir, []config.Rig{{Name: "frontend", Path: inheritDir, Prefix: "fe"}})
	writeRigEndpointMetadata(t, cityDir, "hq")
	writeRigEndpointMetadata(t, inheritDir, "fe")
	writeRigEndpointCanonicalConfig(t, cityDir, contract.ConfigState{IssuePrefix: "gc"})
	beforeCity := mustReadFile(t, filepath.Join(cityDir, ".beads", "config.yaml"))
	beforeMeta := mustReadFile(t, filepath.Join(cityDir, ".beads", "metadata.json"))
	beforeRigMeta := mustReadFile(t, filepath.Join(inheritDir, ".beads", "metadata.json"))

	var stdout, stderr bytes.Buffer
	code := doBeadsCityEndpoint(fsys.OSFS{}, cityDir, cityEndpointOptions{External: true, Host: "db.example.com", Port: "4406", DryRun: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doBeadsCityEndpoint() = %d, stderr = %s", code, stderr.String())
	}
	if got := mustReadFile(t, filepath.Join(cityDir, ".beads", "config.yaml")); string(got) != string(beforeCity) {
		t.Fatalf("city config changed during dry-run:\n%s", got)
	}
	if got := mustReadFile(t, filepath.Join(cityDir, ".beads", "metadata.json")); string(got) != string(beforeMeta) {
		t.Fatalf("city metadata changed during dry-run:\n%s", got)
	}
	if got := mustReadFile(t, filepath.Join(inheritDir, ".beads", "metadata.json")); string(got) != string(beforeRigMeta) {
		t.Fatalf("rig metadata changed during dry-run:\n%s", got)
	}
	if !strings.Contains(stdout.String(), "WOULD UPDATE: city endpoint") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func writeCityEndpointCityConfigWithCompat(t *testing.T, cityDir string, rigs []config.Rig) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	var content strings.Builder
	content.WriteString("[workspace]\nname = \"test-city\"\n")
	for _, rig := range rigs {
		content.WriteString("\n[[rigs]]\n")
		fmt.Fprintf(&content, "name = %q\n", rig.Name)     //nolint:errcheck
		fmt.Fprintf(&content, "path = %q\n", rig.Path)     //nolint:errcheck
		fmt.Fprintf(&content, "prefix = %q\n", rig.Prefix) //nolint:errcheck
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(content.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

//nolint:unused // retained as a focused helper for future city endpoint tests
func writeCityEndpointCityConfig(t *testing.T, cityDir string, rigs []config.Rig) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "[workspace]\nname = \"test-city\"\n"
	for _, rig := range rigs {
		content += fmt.Sprintf("\n[[rigs]]\nname = %q\npath = %q\nprefix = %q\n", rig.Name, rig.Path, rig.Prefix)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Regression test for the ga-lurp5d follow-up: a failed topology change must
// roll back a symlinked city.toml by restoring the link target, not by
// replacing the link with a regular file.
func TestCityTopologyRollbackRestoresThroughCityTomlSymlink(t *testing.T) {
	fs := fsys.OSFS{}
	cityDir, link, target := setupSymlinkedCityToml(t)

	snapshots, err := snapshotCityTopologyFiles(fs, cityDir, nil)
	if err != nil {
		t.Fatalf("snapshotCityTopologyFiles: %v", err)
	}
	if err := os.WriteFile(target, []byte("[workspace]\nname = \"mutated\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := restoreSnapshots(fs, snapshots); err != nil {
		t.Fatalf("restoreSnapshots: %v", err)
	}
	assertCityTomlSymlinkRestored(t, link, target, symlinkedCityTomlOriginal)
}
