package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestProxiedServerInitOptionsEnabled(t *testing.T) {
	if (proxiedServerInitOptions{}).enabled() {
		t.Fatal("zero value must not be enabled")
	}
	if !(proxiedServerInitOptions{Enabled: true}).enabled() {
		t.Fatal("Enabled: true must be enabled")
	}
}

func TestProxiedServerInitOptionsValidate(t *testing.T) {
	if err := (proxiedServerInitOptions{}).validate(hostedDoltInitOptions{Host: "gateway.example.com"}); err != nil {
		t.Fatalf("disabled proxiedServer must not validate hostedDolt: %v", err)
	}
	if err := (proxiedServerInitOptions{Enabled: true}).validate(hostedDoltInitOptions{}); err != nil {
		t.Fatalf("enabled proxiedServer alone must validate: %v", err)
	}
	err := (proxiedServerInitOptions{Enabled: true}).validate(hostedDoltInitOptions{Host: "gateway.example.com"})
	if err == nil || !strings.Contains(err.Error(), "--dolt-host") {
		t.Fatalf("validate() = %v, want a --dolt-host conflict error", err)
	}
}

func TestProxiedServerInitOptionsApplyToCityConfig(t *testing.T) {
	cfg := config.City{}
	(proxiedServerInitOptions{}).applyToCityConfig(&cfg)
	if cfg.Beads.Backend != "" {
		t.Fatalf("disabled applyToCityConfig set Backend = %q, want unchanged", cfg.Beads.Backend)
	}
	(proxiedServerInitOptions{Enabled: true}).applyToCityConfig(&cfg)
	if cfg.Beads.Backend != "proxied-server" {
		t.Fatalf("Backend = %q, want proxied-server", cfg.Beads.Backend)
	}
}

func TestInitWizardConfigFromFlagsCapturesProxiedServer(t *testing.T) {
	cmd := newInitCmd(io.Discard, io.Discard)
	if err := cmd.Flags().Set("template", "custom"); err != nil {
		t.Fatalf("set template: %v", err)
	}
	wiz, _, err := initWizardConfigFromFlags(cmd, "", "", nil, "custom", "", hostedDoltInitOptions{}, proxiedServerInitOptions{Enabled: true}, false)
	if err != nil {
		t.Fatalf("initWizardConfigFromFlags: %v", err)
	}
	if !wiz.proxiedServer.enabled() {
		t.Fatal("expected wiz.proxiedServer to be enabled")
	}
}

func TestInitWizardConfigFromFlagsRejectsProxiedServerWithHostedDolt(t *testing.T) {
	cmd := newInitCmd(io.Discard, io.Discard)
	if err := cmd.Flags().Set("template", "custom"); err != nil {
		t.Fatalf("set template: %v", err)
	}
	hosted := hostedDoltInitOptions{Host: "gateway.example.com", Port: "4406", Database: "bd_prj_x", ProjectID: "prj_x"}
	_, _, err := initWizardConfigFromFlags(cmd, "", "", nil, "custom", "", hosted, proxiedServerInitOptions{Enabled: true}, false)
	if err == nil || !strings.Contains(err.Error(), "--dolt-host") {
		t.Fatalf("initWizardConfigFromFlags = %v, want --dolt-host conflict error", err)
	}
}

// doInit with --proxied-server pins [beads] backend = "proxied-server" into
// city.toml -- the same city.toml-backed dispatch seam
// cityUsesDoltliteBeadsBackend already uses for "doltlite" -- so
// cityUsesProxiedServerMode resolves true from the moment city.toml is
// written. Does not yet change gc start's runtime dispatch (increment 4).
func TestDoInitWritesProxiedServerBackend(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "proxied-city")
	wiz := wizardConfig{
		configName:      "gascity",
		defaultProvider: "claude",
		provider:        "claude",
		providers:       []string{"claude"},
		proxiedServer:   proxiedServerInitOptions{Enabled: true},
	}
	var stdout, stderr bytes.Buffer
	if code := doInit(fsys.OSFS{}, cityPath, wiz, "proxied-city", &stdout, &stderr, false); code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr=%s", code, stderr.String())
	}

	cityData, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("read city.toml: %v", err)
	}
	if !strings.Contains(string(cityData), `backend = "proxied-server"`) {
		t.Fatalf("city.toml missing [beads] backend = \"proxied-server\":\n%s", cityData)
	}
	if !cityUsesProxiedServerMode(cityPath) {
		t.Fatal("cityUsesProxiedServerMode = false, want true after doInit")
	}
}

func TestDoInitRejectsProxiedServerWithHostedDolt(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "conflicting-city")
	wiz := wizardConfig{
		configName:      "gascity",
		defaultProvider: "claude",
		provider:        "claude",
		providers:       []string{"claude"},
		proxiedServer:   proxiedServerInitOptions{Enabled: true},
		hostedDolt: hostedDoltInitOptions{
			Host:      "gateway.example.com",
			Port:      "4406",
			Database:  "bd_prj_x",
			ProjectID: "prj_x",
		},
	}
	var stdout, stderr bytes.Buffer
	code := doInit(fsys.OSFS{}, cityPath, wiz, "conflicting-city", &stdout, &stderr, false)
	if code == 0 {
		t.Fatalf("doInit = 0, want failure for --proxied-server + --dolt-host; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "--dolt-host") {
		t.Fatalf("stderr = %q, want a --dolt-host conflict error", stderr.String())
	}
}

// Command-level: --proxied-server and --dolt-host are cobra-mutually-exclusive
// flags, so the real `gc init` command rejects the combination before RunE
// even resolves the wizard config.
func TestGcInitCommandRejectsProxiedServerWithDoltHost(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "cmd-conflicting-city")
	var stdout, stderr bytes.Buffer
	cmd := newInitCmd(&stdout, &stderr)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"--template", "gascity",
		"--default-provider", "claude",
		"--skip-provider-readiness",
		"--no-start",
		"--proxied-server",
		"--dolt-host", "gateway.example.com",
		"--dolt-port", "4406",
		"--dolt-database", "bd_prj_x",
		"--dolt-project-id", "prj_x",
		cityPath,
	})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("gc init --proxied-server --dolt-host = nil error, want failure; stderr=%s", stderr.String())
	}
}

func TestGcInitCommandProxiedServer(t *testing.T) {
	t.Setenv("GC_DOLT", "")
	cityPath := filepath.Join(t.TempDir(), "cmd-proxied-city")
	var stdout, stderr bytes.Buffer
	cmd := newInitCmd(&stdout, &stderr)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"--template", "gascity",
		"--default-provider", "claude",
		"--skip-provider-readiness",
		"--no-start",
		"--proxied-server",
		cityPath,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc init --proxied-server = %v, want success; stderr=%s", err, stderr.String())
	}
	if !cityUsesProxiedServerMode(cityPath) {
		t.Fatal("cityUsesProxiedServerMode = false, want true after gc init --proxied-server")
	}
}
