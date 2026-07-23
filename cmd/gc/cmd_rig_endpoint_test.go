package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestValidateRigEndpointOptionsRejectsWildcardExternalHost(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "::"} {
		t.Run(host, func(t *testing.T) {
			err := validateRigEndpointOptions(rigEndpointOptions{External: true, Host: host, Port: "4406"})
			if err == nil || !strings.Contains(err.Error(), "invalid --host") {
				t.Fatalf("validateRigEndpointOptions(%q) error = %v", host, err)
			}
		})
	}
}

func TestDoRigSetEndpointCanonicalizesExistingMetadata(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"fe","dolt_host":"stale.example.com","dolt_server_port":"3307"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{
		External: true,
		Host:     "rig-db.example.com",
		Port:     "4406",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSetEndpoint() = %d, stderr = %s", code, stderr.String())
	}

	data, err := os.ReadFile(filepath.Join(rigDir, ".beads", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "dolt_host") || strings.Contains(text, "dolt_server_port") {
		t.Fatalf("metadata retained deprecated endpoint fields: %s", text)
	}
	doltDatabase, ok, err := contract.ReadDoltDatabase(fsys.OSFS{}, filepath.Join(rigDir, ".beads", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !ok || doltDatabase != "fe" {
		t.Fatalf("metadata lost pinned dolt_database: %s", text)
	}
}

func TestDoRigSetEndpointSupportsExecGcBeadsBdProvider(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	writeRigEndpointMetadata(t, cityDir, "hq")
	writeRigEndpointMetadata(t, rigDir, "fe")
	t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(cityDir))

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{External: true, Host: "rig-db.example.com", Port: "4406", AdoptUnverified: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSetEndpoint() = %d, stderr = %s", code, stderr.String())
	}
}

func TestDoRigSetEndpointMetadataFailureDoesNotWriteConfig(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"backend":"dolt"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{
		External: true,
		Host:     "rig-db.example.com",
		Port:     "4406",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigSetEndpoint() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "canonicalizing metadata") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".beads", "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("config.yaml should not be written on metadata failure, stat err = %v", err)
	}
}

func TestDoRigSetEndpointDryRunDoesNotWriteFilesOrValidate(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	writeRigEndpointMetadata(t, rigDir, "fe")
	writeRigEndpointCanonicalConfig(t, rigDir, contract.ConfigState{
		IssuePrefix: "fe",
		DoltHost:    "old-db.example.com",
		DoltPort:    "3307",
		DoltUser:    "old-user",
	})
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "dolt-server.port"), []byte("3307\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeConfig := mustReadFile(t, filepath.Join(rigDir, ".beads", "config.yaml"))
	beforeMeta := mustReadFile(t, filepath.Join(rigDir, ".beads", "metadata.json"))

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{
		External: true,
		Host:     "new-db.example.com",
		Port:     "4406",
		DryRun:   true,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSetEndpoint() = %d, stderr = %s", code, stderr.String())
	}
	if got := mustReadFile(t, filepath.Join(rigDir, ".beads", "config.yaml")); string(got) != string(beforeConfig) {
		t.Fatalf("config.yaml changed during dry-run:\n%s", got)
	}
	if got := mustReadFile(t, filepath.Join(rigDir, ".beads", "metadata.json")); string(got) != string(beforeMeta) {
		t.Fatalf("metadata.json changed during dry-run:\n%s", got)
	}
	if got := strings.TrimSpace(string(mustReadFile(t, filepath.Join(rigDir, ".beads", "dolt-server.port")))); got != "3307" {
		t.Fatalf("port file = %q, want %q", got, "3307")
	}
}

func TestRemoveDoltPortFileStrictClearsThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	targetDir := filepath.Join(t.TempDir(), "ports")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(targetDir, "dolt-server.port")
	if err := os.WriteFile(target, []byte("3311\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(beadsDir, "dolt-server.port")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := removeDoltPortFileStrict(dir); err != nil {
		t.Fatalf("removeDoltPortFileStrict: %v", err)
	}

	// The operator's link must survive; only the resolved target is cleared.
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("dolt-server.port symlink was replaced by a %v entry; cleanup must clear through the link", info.Mode())
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target stat err = %v, want resolved target removed", err)
	}
}

// setupSymlinkedCityToml creates cityDir/city.toml as a symlink into a
// checkout directory holding the original content, mirroring the ga-lurp5d
// production layout where city.toml links into a checked-out repo.
const symlinkedCityTomlOriginal = "[workspace]\nname = \"test-city\"\n"

func setupSymlinkedCityToml(t *testing.T) (cityDir, link, target string) {
	t.Helper()
	dir := t.TempDir()
	checkoutDir := filepath.Join(dir, "checkout")
	if err := os.MkdirAll(checkoutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target = filepath.Join(checkoutDir, "city.toml")
	if err := os.WriteFile(target, []byte(symlinkedCityTomlOriginal), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDir = filepath.Join(dir, "city")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(cityDir, "city.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return cityDir, link, target
}

// assertCityTomlSymlinkRestored fails the test unless link is still a symlink
// and target holds the original content after a rollback restore.
func assertCityTomlSymlinkRestored(t *testing.T, link, target, original string) {
	t.Helper()
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("city.toml symlink was replaced by a %v entry; rollback must restore through the link", info.Mode())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(data) != original {
		t.Fatalf("target content = %q, want restored original %q", data, original)
	}
}

// Regression test for the ga-lurp5d follow-up: a failed endpoint change must
// roll back a symlinked city.toml by restoring the link target, not by
// replacing the link with a regular file.
func TestRigEndpointRollbackRestoresThroughCityTomlSymlink(t *testing.T) {
	fs := fsys.OSFS{}
	cityDir, link, target := setupSymlinkedCityToml(t)
	scopeRoot := filepath.Join(cityDir, "rig")
	if err := os.MkdirAll(scopeRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	snapshots, err := snapshotRigEndpointFiles(fs, cityDir, scopeRoot)
	if err != nil {
		t.Fatalf("snapshotRigEndpointFiles: %v", err)
	}
	if err := os.WriteFile(target, []byte("[workspace]\nname = \"mutated\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := restoreSnapshots(fs, snapshots); err != nil {
		t.Fatalf("restoreSnapshots: %v", err)
	}
	assertCityTomlSymlinkRestored(t, link, target, symlinkedCityTomlOriginal)
}

// Regression for the attempt-3 review's consistency finding: cmd-side
// rollback snapshots must resolve every captured file the way the API-side
// capture does, not just city.toml — restoring a symlinked .gc/site.toml
// must write the link target, not replace the link with a regular file.
func TestRigEndpointRollbackRestoresThroughSiteTomlSymlink(t *testing.T) {
	fs := fsys.OSFS{}
	original := "name = \"bound-site\"\n"
	cityDir, _, _ := setupSymlinkedCityToml(t)
	checkout := filepath.Join(cityDir, "site-checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(checkout, "site.toml")
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	link := config.SiteBindingPath(cityDir)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	scopeRoot := filepath.Join(cityDir, "rig")
	if err := os.MkdirAll(scopeRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	snapshots, err := snapshotRigEndpointFiles(fs, cityDir, scopeRoot)
	if err != nil {
		t.Fatalf("snapshotRigEndpointFiles: %v", err)
	}
	if err := os.WriteFile(target, []byte("name = \"mutated\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := restoreSnapshots(fs, snapshots); err != nil {
		t.Fatalf("restoreSnapshots: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("site.toml symlink was replaced by a %v entry; rollback must restore through the link", info.Mode())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(data) != original {
		t.Fatalf("target content = %q, want restored original %q", data, original)
	}
}

func TestRigEndpointRollbackRestoresAfterSiteBindingForwardWriteThroughSymlink(t *testing.T) {
	fs := fsys.OSFS{}
	original := "workspace_name = \"bound-site\"\nworkspace_prefix = \"bs\"\n"
	cityDir, _, _ := setupSymlinkedCityToml(t)
	checkout := filepath.Join(cityDir, "site-checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(checkout, "site.toml")
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	link := config.SiteBindingPath(cityDir)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	scopeRoot := filepath.Join(cityDir, "rig")
	if err := os.MkdirAll(scopeRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	snapshots, err := snapshotRigEndpointFiles(fs, cityDir, scopeRoot)
	if err != nil {
		t.Fatalf("snapshotRigEndpointFiles: %v", err)
	}
	if err := config.PersistWorkspaceSiteBinding(fs, cityDir, "mutated-site", "ms"); err != nil {
		t.Fatalf("PersistWorkspaceSiteBinding: %v", err)
	}
	if err := restoreSnapshots(fs, snapshots); err != nil {
		t.Fatalf("restoreSnapshots: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("site.toml symlink was replaced by a %v entry; rollback must restore the effective linked state", info.Mode())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(data) != original {
		t.Fatalf("target content = %q, want restored original %q", data, original)
	}
}

func TestDoRigSetEndpointExternalPreservesExistingUserWhenUserFlagOmitted(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	writeRigEndpointMetadata(t, rigDir, "fe")
	writeRigEndpointCanonicalConfig(t, rigDir, contract.ConfigState{
		IssuePrefix: "fe",
		DoltHost:    "old-db.example.com",
		DoltPort:    "3307",
		DoltUser:    "rig-user",
	})

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{
		External:        true,
		Host:            "new-db.example.com",
		Port:            "4406",
		AdoptUnverified: true,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSetEndpoint() = %d, stderr = %s", code, stderr.String())
	}
	if got := readRigEndpointConfigState(t, rigDir).DoltUser; got != "rig-user" {
		t.Fatalf("DoltUser = %q, want %q", got, "rig-user")
	}
}

func TestDoRigSetEndpointRequiresCanonicalMetadata(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{
		External:        true,
		Host:            "rig-db.example.com",
		Port:            "4406",
		AdoptUnverified: true,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigSetEndpoint() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "missing canonical metadata") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".beads", "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("config.yaml should not be written without metadata, stat err = %v", err)
	}
}

func TestDoRigSetEndpointConfigFailureRollsBackMetadata(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	metadataPath := filepath.Join(rigDir, ".beads", "metadata.json")
	if err := os.WriteFile(metadataPath, []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"fe","dolt_host":"stale.example.com"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(rigDir, ".beads", "config.yaml")
	writeRigEndpointCanonicalConfig(t, rigDir, contract.ConfigState{
		IssuePrefix: "fe",
		DoltHost:    "old-db.example.com",
		DoltPort:    "3307",
		DoltUser:    "old-user",
	})
	beforeMeta := mustReadFile(t, metadataPath)
	beforeConfig := mustReadFile(t, configPath)
	if err := os.Chmod(configPath, 0o444); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(configPath, 0o644) }()

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{
		External: true,
		Host:     "new-db.example.com",
		Port:     "4406",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigSetEndpoint() = %d, want 1", code)
	}
	if got := mustReadFile(t, metadataPath); string(got) != string(beforeMeta) {
		t.Fatalf("metadata rollback failed:\n%s", got)
	}
	if got := mustReadFile(t, configPath); string(got) != string(beforeConfig) {
		t.Fatalf("config rollback failed:\n%s", got)
	}
}

func writeRigEndpointCityConfig(t *testing.T, cityDir, rigDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"test-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := config.PersistWorkspaceSiteBinding(fsys.OSFS{}, cityDir, "test-city", ""); err != nil {
		t.Fatalf("PersistWorkspaceSiteBinding: %v", err)
	}
	if err := config.PersistRigSiteBindings(fsys.OSFS{}, cityDir, []config.Rig{{Name: "frontend", Path: rigDir}}); err != nil {
		t.Fatalf("PersistRigSiteBindings: %v", err)
	}
	content := "[workspace]\n\n[[rigs]]\nname = \"frontend\"\nprefix = \"fe\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeRigEndpointCanonicalConfig(t *testing.T, dir string, state contract.ConfigState) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalConfig(fsys.OSFS{}, filepath.Join(dir, ".beads", "config.yaml"), state); err != nil {
		t.Fatal(err)
	}
}

func writeRigEndpointMetadata(t *testing.T, dir, doltDatabase string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(dir, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: doltDatabase,
	}); err != nil {
		t.Fatal(err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readRigEndpointConfigState(t *testing.T, dir string) contract.ConfigState {
	t.Helper()
	state, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(dir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("config state missing")
	}
	return state
}

// Regression for PR #3428 review: the gc rig set-endpoint outer rollback
// snapshots city.toml at its symlink-resolved path, so restoring after a later
// step fails rewrites the real target and leaves the live symlink intact
// instead of replacing it with a regular file.
func TestSnapshotRigEndpointFilesRestoresSymlinkedCityToml(t *testing.T) {
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	cityDir := filepath.Join(dir, "city")
	scopeRoot := filepath.Join(dir, "rig")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(scopeRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	repoCityPath := filepath.Join(repoDir, "city.toml")
	liveCityPath := filepath.Join(cityDir, "city.toml")
	original := []byte("[workspace]\nname = \"test-city\"\n")
	if err := os.WriteFile(repoCityPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "repo", "city.toml"), liveCityPath); err != nil {
		t.Fatal(err)
	}

	fs := fsys.OSFS{}
	snapshots, err := snapshotRigEndpointFiles(fs, cityDir, scopeRoot)
	if err != nil {
		t.Fatalf("snapshotRigEndpointFiles: %v", err)
	}

	// Simulate the endpoint config rewrite mutating the resolved target before
	// a later step fails and triggers rollback.
	mutated := []byte("[workspace]\nname = \"mutated\"\n")
	if err := os.WriteFile(repoCityPath, mutated, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := restoreSnapshots(fs, snapshots); err != nil {
		t.Fatalf("restoreSnapshots: %v", err)
	}

	info, err := os.Lstat(liveCityPath)
	if err != nil {
		t.Fatalf("Lstat(live city.toml): %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("rollback replaced the live city.toml symlink with a regular file")
	}
	restored, err := os.ReadFile(repoCityPath)
	if err != nil {
		t.Fatalf("read repo city.toml: %v", err)
	}
	if !bytes.Equal(restored, original) {
		t.Fatalf("repo city.toml = %q, want restored original %q", restored, original)
	}
}
