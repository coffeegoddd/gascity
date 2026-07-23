package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/rig"
	"github.com/spf13/cobra"
)

type rigEndpointOptions struct {
	External        bool
	Host            string
	Port            string
	User            string
	AdoptUnverified bool
	DryRun          bool
}

func newRigSetEndpointCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts rigEndpointOptions
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "set-endpoint <rig>",
		Short: "Pin a rig to an external Dolt endpoint",
		Long: `Pin a rig to its own external Dolt endpoint.

Use --external with --host/--port to point the rig at an operator-managed Dolt
server; bd fronts it via proxied-server-external. A rig with no external
endpoint inherits the city's endpoint by default (bd owns the local server), so
no command is needed for the common case.

This command owns the rig's canonical .beads/config.yaml topology state.`,
		Example: `  gc rig set-endpoint frontend --external --host db.example.com --port 3307
  gc rig set-endpoint frontend --external --host db.example.com --port 3307 --user agent
  gc rig set-endpoint frontend --external --host db.example.com --port 3307 --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if jsonOutput {
				if cmdRigSetEndpoint(args[0], opts, io.Discard, stderr) != 0 {
					return errExit
				}
				return writeManagementActionJSON(stdout, managementActionResult{
					Command:  commandName("rig", "set-endpoint"),
					Action:   "set-endpoint",
					Name:     args[0],
					Rig:      args[0],
					DryRun:   managementBoolPtr(opts.DryRun),
					Endpoint: rigEndpointJSONFromOptions(opts),
				})
			}
			if cmdRigSetEndpoint(args[0], opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeRigNames,
	}
	cmd.Flags().BoolVar(&opts.External, "external", false, "pin the rig to its own external Dolt endpoint")
	cmd.Flags().StringVar(&opts.Host, "host", "", "external Dolt host")
	cmd.Flags().StringVar(&opts.Port, "port", "", "external Dolt port (required with --external)")
	cmd.Flags().StringVar(&opts.User, "user", "", "external Dolt user")
	cmd.Flags().BoolVar(&opts.AdoptUnverified, "adopt-unverified", false, "record the endpoint without live validation")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "show the canonical changes without writing files")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSONL format")
	return cmd
}

func cmdRigSetEndpoint(rigName string, opts rigEndpointOptions, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc rig set-endpoint: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return doRigSetEndpoint(fsys.OSFS{}, cityPath, rigName, opts, stdout, stderr)
}

//nolint:unparam // FS seam is intentional for command tests
func doRigSetEndpoint(fs fsys.FS, cityPath, rigName string, opts rigEndpointOptions, stdout, stderr io.Writer) int {
	if err := validateRigEndpointOptions(opts); err != nil {
		fmt.Fprintf(stderr, "gc rig set-endpoint: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	tomlPath := filepath.Join(cityPath, "city.toml")
	cfg, err := loadCityConfigForEditFS(fs, tomlPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig set-endpoint: loading config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	persistCfg := *cfg
	persistCfg.Rigs = append([]config.Rig(nil), cfg.Rigs...)
	resolveRigPaths(cityPath, cfg.Rigs)

	rig, ok := rigByName(cfg, rigName)
	if !ok {
		fmt.Fprintln(stderr, rigNotFoundMsg("gc rig set-endpoint", rigName, cfg)) //nolint:errcheck // best-effort stderr
		return 1
	}
	if strings.TrimSpace(rig.Path) == "" {
		// Unbound rig: the downstream helpers join paths against rig.Path
		// (snapshotRigEndpointFiles, ensureCanonicalScopeMetadataIfPresent,
		// syncRigManagedPortArtifact, etc.). Empty rig.Path would produce
		// relative `.beads/...` writes under the current working directory
		// instead of erroring cleanly.
		fmt.Fprintf(stderr, "gc rig set-endpoint: rig %q is declared but has no path binding — run `gc rig add <dir> --name %s` to bind it before setting its endpoint\n", rig.Name, rig.Name) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !scopeUsesManagedBdStoreContract(cityPath, rig.Path) {
		fmt.Fprintln(stderr, "gc rig set-endpoint: only supported for bd-backed beads providers") //nolint:errcheck // best-effort stderr
		return 1
	}

	cityState, err := resolveOwnerCityConfigState(cityPath, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig set-endpoint: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	currentState, err := resolveOwnerRigConfigState(cityPath, rig, cityState)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig set-endpoint: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	targetState := requestedRigEndpointState(rig, currentState, cityState, opts)

	if opts.DryRun {
		printRigEndpointDryRun(stdout, rig, currentState, targetState)
		return 0
	}

	// External endpoints are validated by bd when it initializes the
	// proxied-server-external store; gascity no longer opens its own connection
	// to pre-verify identity (bd owns the endpoint and the project_id is
	// authoritative). The endpoint is recorded as-is and bd verifies on init.

	snapshots, err := snapshotRigEndpointFiles(fs, cityPath, rig.Path)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig set-endpoint: snapshot canonical files: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := ensureCanonicalScopeMetadataIfPresent(fs, rig.Path); err != nil {
		writeRigEndpointRollbackError(fs, stderr, snapshots, "canonicalizing metadata", err)
		return 1
	}
	if err := ensureCanonicalScopeConfig(fs, rig.Path, targetState); err != nil {
		writeRigEndpointRollbackError(fs, stderr, snapshots, "writing canonical config", err)
		return 1
	}
	if err := syncRigManagedPortArtifact(rig.Path); err != nil {
		writeRigEndpointRollbackError(fs, stderr, snapshots, "syncing managed port artifact", err)
		return 1
	}

	printRigEndpointResult(stdout, rig, targetState)
	return 0
}

func validateRigEndpointOptions(opts rigEndpointOptions) error {
	if !opts.External {
		return fmt.Errorf("--external is required")
	}
	host := strings.TrimSpace(opts.Host)
	port := strings.TrimSpace(opts.Port)
	if host == "" {
		return fmt.Errorf("--external requires --host")
	}
	if err := validateExplicitExternalHost(host); err != nil {
		return err
	}
	if port == "" {
		return fmt.Errorf("--external requires --port")
	}
	value, err := strconv.Atoi(port)
	if err != nil || value <= 0 {
		return fmt.Errorf("invalid --port %q", port)
	}
	return nil
}

func rigByName(cfg *config.City, rigName string) (config.Rig, bool) {
	for i := range cfg.Rigs {
		if strings.EqualFold(cfg.Rigs[i].Name, rigName) {
			return cfg.Rigs[i], true
		}
	}
	return config.Rig{}, false
}

func resolveOwnerCityConfigState(cityPath string, cfg *config.City) (contract.ConfigState, error) {
	state, _, err := resolveDesiredCityEndpointState(cityPath, cfg.Beads.Server, config.EffectiveHQPrefix(cfg))
	if err != nil {
		return contract.ConfigState{}, err
	}
	return state, nil
}

func resolveOwnerRigConfigState(cityPath string, rig config.Rig, cityState contract.ConfigState) (contract.ConfigState, error) {
	state, err := resolveDesiredRigEndpointState(cityPath, rig, cityState)
	if err != nil {
		return contract.ConfigState{}, err
	}
	return state, nil
}

func requestedRigEndpointState(rig config.Rig, currentState, _ contract.ConfigState, opts rigEndpointOptions) contract.ConfigState {
	user := strings.TrimSpace(opts.User)
	if user == "" && contract.ConfigHasEndpointAuthority(currentState) {
		user = strings.TrimSpace(currentState.DoltUser)
	}
	return contract.ConfigState{
		IssuePrefix: rig.EffectivePrefix(),
		DoltHost:    strings.TrimSpace(opts.Host),
		DoltPort:    strings.TrimSpace(opts.Port),
		DoltUser:    user,
	}
}

func ensureCanonicalScopeConfig(fs fsys.FS, scopeRoot string, state contract.ConfigState) error {
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := ensureBeadsDir(fs, beadsDir); err != nil {
		return err
	}
	// Go owns the canonical `types.custom` baseline that gc-beads-bd.sh's
	// ensure_types_custom_in_yaml used to shape. doctor.RequiredCustomTypes is the
	// single source; union (not replace) so the baseline is always present even if
	// a caller supplies its own extra types, and EnsureCanonicalConfig then unions
	// the result with any on-disk extensions (never narrowing).
	state.CustomTypes = contract.MergeCustomTypes(state.CustomTypes, doctor.RequiredCustomTypes)
	_, err := contract.EnsureCanonicalConfig(fs, filepath.Join(beadsDir, "config.yaml"), state)
	return err
}

func requireCanonicalScopeMetadata(fs fsys.FS, scopeRoot string) error {
	path := filepath.Join(scopeRoot, ".beads", "metadata.json")
	if _, err := fs.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("missing canonical metadata %s", path)
		}
		return err
	}
	doltDatabase, ok, err := contract.ReadDoltDatabase(fs, path)
	if err != nil {
		return err
	}
	if !ok || strings.TrimSpace(doltDatabase) == "" {
		return fmt.Errorf("missing pinned dolt_database in %s", path)
	}
	return nil
}

func ensureCanonicalScopeMetadataIfPresent(fs fsys.FS, scopeRoot string) error {
	path := filepath.Join(scopeRoot, ".beads", "metadata.json")
	doltDatabase, err := func() (string, error) {
		if err := requireCanonicalScopeMetadata(fs, scopeRoot); err != nil {
			return "", err
		}
		doltDatabase, _, err := contract.ReadDoltDatabase(fs, path)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(doltDatabase), nil
	}()
	if err != nil {
		return err
	}
	_, err = contract.EnsureCanonicalMetadata(fs, path, contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: doltDatabase,
	})
	return err
}

// syncRigManagedPortArtifact clears any stale .beads/dolt-server.port mirror
// under rigPath. In proxied-server mode bd owns the endpoint and gascity
// publishes no managed port, so there is never a port to mirror — the correct
// idempotent action for any endpoint transition is to remove the mirror.
func syncRigManagedPortArtifact(rigPath string) error {
	return removeDoltPortFileStrict(rigPath)
}

func removeDoltPortFileStrict(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	return removeResolvedDoltPortFile(fsys.OSFS{}, dir)
}

// removeResolvedDoltPortFile clears the managed dolt port mirror under dir,
// resolving an operator symlink to its target first so the link entry is
// preserved and only the resolved target is removed. This mirrors the
// symlink-preserving write path (resolveDoltPortFileWritePath); removing the
// unresolved link instead would delete the operator's symlink and make the
// next port publication recreate a regular file at the link path (the
// ga-lurp5d clobber class). Missing files, including dangling links, are not
// an error.
func removeResolvedDoltPortFile(fs fsys.FS, dir string) error {
	portFile := filepath.Join(dir, ".beads", "dolt-server.port")
	target, err := fsys.ResolveSymlinks(fs, portFile)
	if err != nil {
		return fmt.Errorf("resolving managed dolt port file %q for cleanup: %w", portFile, err)
	}
	if err := fs.Remove(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func printRigEndpointDryRun(stdout io.Writer, rig config.Rig, current, target contract.ConfigState) {
	fmt.Fprintln(stdout, "WOULD UPDATE: rig endpoint")                                    //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  rig: %s\n", rig.Name)                                          //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  from: %s\n", describeRigEndpointState(current))                //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  to:   %s\n", describeRigEndpointState(target))                 //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  file: %s\n", filepath.Join(rig.Path, ".beads", "config.yaml")) //nolint:errcheck // best-effort stdout
}

func printRigEndpointResult(stdout io.Writer, rig config.Rig, state contract.ConfigState) {
	fmt.Fprintln(stdout, "UPDATED: rig endpoint")                         //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  rig: %s\n", rig.Name)                          //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  state: %s\n", describeRigEndpointState(state)) //nolint:errcheck // best-effort stdout
	next := rigEndpointFollowupCommand(rig, state)
	if next == "" {
		fmt.Fprintln(stdout, "  next: none") //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintf(stdout, "  next: %s\n", next) //nolint:errcheck // best-effort stdout
	}
}

func rigEndpointFollowupCommand(_ config.Rig, _ contract.ConfigState) string {
	// bd verifies external endpoints at init; there is no gascity-side
	// "verify later" follow-up to suggest.
	return ""
}

func describeRigEndpointState(state contract.ConfigState) string {
	if !contract.ConfigHasEndpointAuthority(state) {
		return "local (bd proxied-server)"
	}
	addr := net.JoinHostPort(defaultHost(state.DoltHost, state.DoltPort), strings.TrimSpace(state.DoltPort))
	parts := []string{"external", addr}
	if user := strings.TrimSpace(state.DoltUser); user != "" {
		parts = append(parts, "user="+user)
	}
	return strings.Join(parts, " ")
}

func defaultHost(host, port string) string {
	host = strings.TrimSpace(host)
	if host == "" && strings.TrimSpace(port) != "" {
		return "127.0.0.1"
	}
	return host
}

// fileSnapshot aliases rig.FileSnapshot so cmd/gc's existing rollback call sites
// keep compiling while the primitives live in internal/rig (C2.1 extraction).
type fileSnapshot = rig.FileSnapshot

func snapshotRigCanonicalFiles(fs fsys.FS, scopeRoot string) ([]fileSnapshot, error) {
	paths := []string{
		filepath.Join(scopeRoot, ".beads", "metadata.json"),
		filepath.Join(scopeRoot, ".beads", "config.yaml"),
	}
	snapshots := make([]fileSnapshot, 0, len(paths))
	for _, path := range paths {
		snap, err := snapshotResolvedFile(fs, path)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snap)
	}
	return snapshots, nil
}

func snapshotRigEndpointFiles(fs fsys.FS, cityPath, scopeRoot string) ([]fileSnapshot, error) {
	cityToml, err := snapshotResolvedFile(fs, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return nil, err
	}
	paths := []string{
		config.SiteBindingPath(cityPath),
		filepath.Join(scopeRoot, ".beads", "metadata.json"),
		filepath.Join(scopeRoot, ".beads", "config.yaml"),
	}
	snapshots := make([]fileSnapshot, 0, len(paths)+1)
	snapshots = append(snapshots, cityToml)
	for _, path := range paths {
		snap, err := snapshotResolvedFile(fs, path)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snap)
	}
	return snapshots, nil
}

// snapshotResolvedFile delegates to internal/rig, which owns the rollback
// primitives (C2.1). The symlink-resolution rationale lives on
// rig.SnapshotResolvedFile.
func snapshotResolvedFile(fs fsys.FS, path string) (fileSnapshot, error) {
	return rig.SnapshotResolvedFile(fs, path)
}

// cityTomlRollbackPath returns the symlink-resolved city.toml path that a
// rollback snapshot must read and later restore. Resolving first means an
// atomic restore rewrites the real target file and leaves a live city.toml
// symlink intact, instead of replacing the link with a regular file (the
// failure ResolveCityRewritePath/ResolveCityAppendPath exist to prevent). When
// city.toml is a plain file (or not yet created), resolution is a no-op and the
// path is unchanged. The controller config-mutation snapshot
// (captureConfigMutationSnapshot) routes through this so it stays symlink-aware,
// matching the CLI rollback snapshots that resolve via snapshotResolvedFile.
func cityTomlRollbackPath(fs fsys.FS, cityPath string) (string, error) {
	return fsys.ResolveSymlinks(fs, filepath.Join(cityPath, "city.toml"))
}

func writeRigEndpointRollbackError(fs fsys.FS, stderr io.Writer, snapshots []fileSnapshot, action string, cause error) {
	if restoreErr := restoreSnapshots(fs, snapshots); restoreErr != nil {
		fmt.Fprintf(stderr, "gc rig set-endpoint: %s: %v (rollback failed: %v)\n", action, cause, restoreErr) //nolint:errcheck // best-effort stderr
		return
	}
	fmt.Fprintf(stderr, "gc rig set-endpoint: %s: %v\n", action, cause) //nolint:errcheck // best-effort stderr
}

func restoreSnapshots(fs fsys.FS, snapshots []fileSnapshot) error {
	return rig.RestoreSnapshots(fs, snapshots)
}

func scopeRootFromMetadataPath(metadataPath string) (string, error) {
	cleaned := filepath.Clean(strings.TrimSpace(metadataPath))
	if filepath.Base(cleaned) != "metadata.json" || filepath.Base(filepath.Dir(cleaned)) != ".beads" {
		return "", fmt.Errorf("metadata path %q is not <scope>/.beads/metadata.json", metadataPath)
	}
	return filepath.Dir(filepath.Dir(cleaned)), nil
}

func readManagedMetadataProjectID(metadataPath string) (string, error) {
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return "", err
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", fmt.Errorf("parse metadata %s: %w", metadataPath, err)
	}
	raw, ok := meta["project_id"]
	if !ok || raw == nil {
		return "", nil
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value), nil
	default:
		projectID := strings.TrimSpace(fmt.Sprint(value))
		if projectID == "" || projectID == "<nil>" || strings.EqualFold(projectID, "null") {
			return "", nil
		}
		return projectID, nil
	}
}
