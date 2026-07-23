package contract

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestMain(m *testing.M) {
	_ = os.Unsetenv(ManagedCityHostEnv)
	os.Exit(m.Run())
}

// A local city (no dolt coords) resolves to a bd-owned target: no host/port,
// External=false, identity only.
func TestResolveDoltConnectionTargetLocalCity(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{IssuePrefix: "gc"})
	writeCanonicalMetadata(t, fs, city, "hq")

	target, err := ResolveDoltConnectionTarget(fs, city, city)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if target.External {
		t.Fatalf("local city target.External = true, want false")
	}
	if target.Host != "" || target.Port != "" {
		t.Fatalf("local city host/port = %q/%q, want empty (bd owns the endpoint)", target.Host, target.Port)
	}
	if target.Database != "hq" {
		t.Fatalf("target.Database = %q, want hq", target.Database)
	}
}

// A city with dolt coords resolves external, with canonicalized host/port.
func TestResolveDoltConnectionTargetExternalCity(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{IssuePrefix: "gc", DoltHost: "db.example.com", DoltPort: "4406", DoltUser: "agent"})
	writeCanonicalMetadata(t, fs, city, "hq")

	target, err := ResolveDoltConnectionTarget(fs, city, city)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if !target.External || target.Host != "db.example.com" || target.Port != "4406" {
		t.Fatalf("external city target = %+v", target)
	}
	if target.User != "agent" {
		t.Fatalf("target.User = %q, want agent", target.User)
	}
}

// A port-only external endpoint defaults its host to loopback.
func TestResolveDoltConnectionTargetPortOnlyUsesLoopback(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{IssuePrefix: "gc", DoltPort: "4406"})
	writeCanonicalMetadata(t, fs, city, "hq")

	target, err := ResolveDoltConnectionTarget(fs, city, city)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if !target.External || target.Host != "127.0.0.1" || target.Port != "4406" {
		t.Fatalf("port-only target = %+v", target)
	}
}

// A rig with its OWN coords is external and does not inherit the city.
func TestResolveDoltConnectionTargetRigOwnsExternalEndpoint(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, city, ConfigState{IssuePrefix: "gc", DoltHost: "city.example.com", DoltPort: "4406"})
	writeCanonicalConfig(t, fs, rig, ConfigState{IssuePrefix: "fe", DoltHost: "rig.example.com", DoltPort: "5507"})
	writeCanonicalMetadata(t, fs, rig, "fe")

	target, err := ResolveDoltConnectionTarget(fs, city, rig)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if !target.External || target.Host != "rig.example.com" || target.Port != "5507" || target.Database != "fe" {
		t.Fatalf("rig external target = %+v", target)
	}
}

// A rig with NO coords is local — there is no cross-scope endpoint inheritance,
// even when the city is external.
func TestResolveDoltConnectionTargetRigWithoutCoordsIsLocal(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")
	writeCanonicalConfig(t, fs, city, ConfigState{IssuePrefix: "gc", DoltHost: "city.example.com", DoltPort: "4406"})
	writeCanonicalConfig(t, fs, rig, ConfigState{IssuePrefix: "fe"})
	writeCanonicalMetadata(t, fs, rig, "fe")

	target, err := ResolveDoltConnectionTarget(fs, city, rig)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if target.External || target.Host != "" || target.Port != "" {
		t.Fatalf("no-coords rig should be local, got %+v", target)
	}
	if target.Database != "fe" {
		t.Fatalf("target.Database = %q, want fe", target.Database)
	}
}

func TestValidateConnectionConfigStateRejectsWildcardHost(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	if err := ValidateConnectionConfigState(fs, city, city, ConfigState{DoltHost: "0.0.0.0", DoltPort: "4406"}); err == nil {
		t.Fatal("expected wildcard host rejection")
	}
	if err := ValidateConnectionConfigState(fs, city, city, ConfigState{DoltHost: "::", DoltPort: "4406"}); err == nil {
		t.Fatal("expected wildcard host rejection for ::")
	}
	// A local (no-coords) scope is always valid.
	if err := ValidateConnectionConfigState(fs, city, city, ConfigState{IssuePrefix: "gc"}); err != nil {
		t.Fatalf("local scope validation error = %v", err)
	}
}

func TestResolveScopeConfigStateKinds(t *testing.T) {
	fs := fsys.OSFS{}
	// Missing config.yaml.
	missing := t.TempDir()
	if r, err := ResolveScopeConfigState(fs, missing, missing, "gc"); err != nil || r.Kind != ScopeConfigMissing {
		t.Fatalf("missing: kind=%v err=%v", r.Kind, err)
	}
	// Bare local config → legacy-minimal (no coords).
	local := t.TempDir()
	writeCanonicalConfig(t, fs, local, ConfigState{IssuePrefix: "gc"})
	if r, err := ResolveScopeConfigState(fs, local, local, "gc"); err != nil || r.Kind != ScopeConfigLegacyMinimal {
		t.Fatalf("local: kind=%v err=%v", r.Kind, err)
	}
	// External config → authoritative.
	ext := t.TempDir()
	writeCanonicalConfig(t, fs, ext, ConfigState{IssuePrefix: "gc", DoltHost: "db.example.com", DoltPort: "4406"})
	if r, err := ResolveScopeConfigState(fs, ext, ext, "gc"); err != nil || r.Kind != ScopeConfigAuthoritative {
		t.Fatalf("external: kind=%v err=%v", r.Kind, err)
	}
}

func TestConfigHasEndpointAuthorityAndScopeOwnsExternal(t *testing.T) {
	if ConfigHasEndpointAuthority(ConfigState{IssuePrefix: "gc"}) {
		t.Error("local config should not have endpoint authority")
	}
	if !ConfigHasEndpointAuthority(ConfigState{DoltPort: "4406"}) {
		t.Error("coords config should have endpoint authority")
	}
}

func writeCanonicalConfig(t *testing.T, fs fsys.FS, dir string, state ConfigState) {
	t.Helper()
	if err := fs.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCanonicalConfig(fs, filepath.Join(dir, ".beads", "config.yaml"), state); err != nil {
		t.Fatal(err)
	}
}

//nolint:unparam // helper keeps FS explicit in tests
func writeCanonicalMetadata(t *testing.T, fs fsys.FS, dir, db string) {
	t.Helper()
	if _, err := EnsureCanonicalMetadata(fs, filepath.Join(dir, ".beads", "metadata.json"), MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: db,
	}); err != nil {
		t.Fatal(err)
	}
}
