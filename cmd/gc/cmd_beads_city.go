package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

type cityEndpointOptions struct {
	External        bool
	Host            string
	Port            string
	User            string
	AdoptUnverified bool
	DryRun          bool
}

type cityRigEndpointPlan struct {
	Rig     config.Rig
	Current contract.ConfigState
	Target  contract.ConfigState
	Update  bool
}

func newBeadsCityCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "city",
		Short: "Manage canonical city endpoint topology",
		Long: `Manage the canonical city endpoint topology for bd-backed beads stores.

Use use-external to pin the city to an external Dolt endpoint and rewrite
inherited rig mirrors. A city with no external endpoint is local by default —
bd owns the proxied-server, so no command is needed for the common case.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc beads city: missing subcommand (use-external)") //nolint:errcheck
			} else {
				fmt.Fprintf(stderr, "gc beads city: unknown subcommand %q\n", args[0]) //nolint:errcheck
			}
			return errExit
		},
	}
	cmd.AddCommand(
		newBeadsCityUseExternalCmd(stdout, stderr),
	)
	return cmd
}

func newBeadsCityUseExternalCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts cityEndpointOptions
	opts.External = true
	cmd := &cobra.Command{
		Use:   "use-external",
		Short: "Set the city endpoint to an external Dolt server",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdBeadsCityUseExternal(opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.Host, "host", "", "external Dolt host")
	cmd.Flags().StringVar(&opts.Port, "port", "", "external Dolt port")
	cmd.Flags().StringVar(&opts.User, "user", "", "external Dolt user")
	cmd.Flags().BoolVar(&opts.AdoptUnverified, "adopt-unverified", false, "record the endpoint without live validation")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "show the canonical changes without writing files")
	return cmd
}

func cmdBeadsCityUseExternal(opts cityEndpointOptions, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc beads city use-external: %v\n", err) //nolint:errcheck
		return 1
	}
	return doBeadsCityEndpoint(fsys.OSFS{}, cityPath, opts, stdout, stderr)
}

//nolint:unparam // FS seam is intentional for command tests
func doBeadsCityEndpoint(fs fsys.FS, cityPath string, opts cityEndpointOptions, stdout, stderr io.Writer) int {
	name := cityEndpointCommandName(opts)
	if err := validateCityEndpointOptions(opts); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err) //nolint:errcheck
		return 1
	}
	if !cityUsesBdStoreContract(cityPath) {
		fmt.Fprintf(stderr, "%s: only supported for bd-backed beads providers\n", name) //nolint:errcheck
		return 1
	}

	rawCfg, err := loadCityConfigForEditFS(fs, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		fmt.Fprintf(stderr, "%s: loading config: %v\n", name, err) //nolint:errcheck
		return 1
	}
	cfg, err := loadCityConfigFS(fs, filepath.Join(cityPath, "city.toml"), stderr)
	if err != nil {
		fmt.Fprintf(stderr, "%s: loading expanded config: %v\n", name, err) //nolint:errcheck
		return 1
	}
	tomlCfg := *rawCfg
	tomlCfg.Rigs = append([]config.Rig(nil), rawCfg.Rigs...)
	resolveRigPaths(cityPath, cfg.Rigs)

	currentState, err := resolveOwnerCityConfigState(cityPath, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err) //nolint:errcheck
		return 1
	}
	targetState := requestedCityEndpointState(cfg, currentState, opts)
	plans, err := planCityRigEndpointUpdates(cityPath, cfg.Rigs, currentState, targetState)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err) //nolint:errcheck
		return 1
	}

	if opts.DryRun {
		printCityEndpointDryRun(stdout, currentState, targetState, plans)
		return 0
	}

	// External endpoints are validated by bd when it initializes the
	// proxied-server-external store; gascity no longer opens its own connection
	// to pre-verify identity (bd owns the endpoint and the project_id is
	// authoritative), and bd owns the server lifecycle across the transition.
	// The endpoint is recorded as-is and bd verifies/reconfigures on init.

	snapshots, err := snapshotCityTopologyFiles(fs, cityPath, plans)
	if err != nil {
		fmt.Fprintf(stderr, "%s: snapshot canonical files: %v\n", name, err) //nolint:errcheck
		return 1
	}
	if err := ensureCanonicalScopeMetadataIfPresent(fs, cityPath); err != nil {
		writeCityEndpointRollbackError(fs, stderr, snapshots, name, "canonicalizing metadata", err)
		return 1
	}
	if err := ensureCanonicalScopeConfig(fs, cityPath, targetState); err != nil {
		writeCityEndpointRollbackError(fs, stderr, snapshots, name, "writing canonical config", err)
		return 1
	}
	for _, plan := range plans {
		if !plan.Update {
			continue
		}
		if err := ensureCanonicalScopeMetadataIfPresent(fs, plan.Rig.Path); err != nil {
			writeCityEndpointRollbackError(fs, stderr, snapshots, name, "canonicalizing inherited rig metadata", err)
			return 1
		}
		if err := ensureCanonicalScopeConfig(fs, plan.Rig.Path, plan.Target); err != nil {
			writeCityEndpointRollbackError(fs, stderr, snapshots, name, "writing inherited rig config", err)
			return 1
		}
	}

	printCityEndpointResult(stdout, targetState, plans)
	return 0
}

func cityEndpointCommandName(_ cityEndpointOptions) string {
	return "gc beads city use-external"
}

func validateExplicitExternalHost(host string) error {
	host = strings.TrimSpace(host)
	switch strings.Trim(host, "[]") {
	case "0.0.0.0", "::":
		return fmt.Errorf("invalid --host %q: use a concrete host, not a bind address", host)
	default:
		return nil
	}
}

func validateCityEndpointOptions(opts cityEndpointOptions) error {
	host := strings.TrimSpace(opts.Host)
	if host == "" {
		return fmt.Errorf("use-external requires --host")
	}
	if err := validateExplicitExternalHost(host); err != nil {
		return err
	}
	port := strings.TrimSpace(opts.Port)
	if port == "" {
		return fmt.Errorf("use-external requires --port")
	}
	value, err := strconv.Atoi(port)
	if err != nil || value <= 0 {
		return fmt.Errorf("invalid --port %q", port)
	}
	return nil
}

func requestedCityEndpointState(cfg *config.City, currentState contract.ConfigState, opts cityEndpointOptions) contract.ConfigState {
	prefix := config.EffectiveHQPrefix(cfg)
	user := strings.TrimSpace(opts.User)
	if user == "" && contract.ConfigHasEndpointAuthority(currentState) {
		user = strings.TrimSpace(currentState.DoltUser)
	}
	return contract.ConfigState{
		IssuePrefix: prefix,
		DoltHost:    strings.TrimSpace(opts.Host),
		DoltPort:    strings.TrimSpace(opts.Port),
		DoltUser:    user,
	}
}

func planCityRigEndpointUpdates(cityPath string, rigs []config.Rig, currentCityState, targetCityState contract.ConfigState) ([]cityRigEndpointPlan, error) {
	plans := make([]cityRigEndpointPlan, 0, len(rigs))
	for i := range rigs {
		current, err := resolveOwnerRigConfigState(cityPath, rigs[i], currentCityState)
		if err != nil {
			return nil, err
		}
		plan := cityRigEndpointPlan{Rig: rigs[i], Current: current, Target: current}
		if contract.ConfigHasEndpointAuthority(current) {
			plans = append(plans, plan)
			continue
		}

		plan.Current = inheritedRigDoltConfigState(rigs[i].Path, rigs[i].EffectivePrefix(), currentCityState)
		plan.Target = inheritedRigDoltConfigState(rigs[i].Path, rigs[i].EffectivePrefix(), targetCityState)
		plan.Update = true
		plans = append(plans, plan)
	}
	return plans, nil
}

func snapshotCityTopologyFiles(fs fsys.FS, cityPath string, plans []cityRigEndpointPlan) ([]fileSnapshot, error) {
	snapshots := make([]fileSnapshot, 0, len(plans)+3)
	cityToml, err := snapshotResolvedFile(fs, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, cityToml)
	siteToml, err := snapshotResolvedFile(fs, config.SiteBindingPath(cityPath))
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, siteToml)
	citySnapshots, err := snapshotRigCanonicalFiles(fs, cityPath)
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, citySnapshots...)
	for _, plan := range plans {
		if !plan.Update {
			continue
		}
		rigSnapshots, err := snapshotRigCanonicalFiles(fs, plan.Rig.Path)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, rigSnapshots...)
	}
	portSnapshots, err := snapshotCityManagedPortFiles(fs, cityPath, plans)
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, portSnapshots...)
	return snapshots, nil
}

func snapshotCityManagedPortFiles(fs fsys.FS, cityPath string, plans []cityRigEndpointPlan) ([]fileSnapshot, error) {
	seen := map[string]struct{}{}
	paths := []string{filepath.Join(cityPath, ".beads", "dolt-server.port")}
	for _, plan := range plans {
		paths = append(paths, filepath.Join(plan.Rig.Path, ".beads", "dolt-server.port"))
	}
	snapshots := make([]fileSnapshot, 0, len(paths))
	for _, path := range paths {
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		snap, err := snapshotResolvedFile(fs, path)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snap)
	}
	return snapshots, nil
}

func printCityEndpointDryRun(stdout io.Writer, current, target contract.ConfigState, plans []cityRigEndpointPlan) {
	fmt.Fprintln(stdout, "WOULD UPDATE: city endpoint")                                                            //nolint:errcheck
	fmt.Fprintf(stdout, "  city: %s -> %s\n", describeRigEndpointState(current), describeRigEndpointState(target)) //nolint:errcheck
	fmt.Fprintf(stdout, "  file: %s\n", filepath.Join(".beads", "config.yaml"))                                    //nolint:errcheck
	for _, plan := range plans {
		if !plan.Update {
			continue
		}
		fmt.Fprintf(stdout, "  rig %s: %s -> %s\n", plan.Rig.Name, describeRigEndpointState(plan.Current), describeRigEndpointState(plan.Target)) //nolint:errcheck
	}
}

func printCityEndpointResult(stdout io.Writer, state contract.ConfigState, plans []cityRigEndpointPlan) {
	fmt.Fprintln(stdout, "UPDATED: city endpoint")                        //nolint:errcheck
	fmt.Fprintf(stdout, "  state: %s\n", describeRigEndpointState(state)) //nolint:errcheck
	updated := 0
	for _, plan := range plans {
		if plan.Update {
			updated++
		}
	}
	fmt.Fprintf(stdout, "  inherited rigs updated: %d\n", updated) //nolint:errcheck
	next := cityEndpointFollowupCommand(state)
	if next == "" {
		fmt.Fprintln(stdout, "  next: none") //nolint:errcheck
	} else {
		fmt.Fprintf(stdout, "  next: %s\n", next) //nolint:errcheck
	}
}

func cityEndpointFollowupCommand(_ contract.ConfigState) string {
	// bd verifies external endpoints at init; there is no gascity-side
	// "verify later" follow-up to suggest.
	return ""
}

func writeCityEndpointRollbackError(fs fsys.FS, stderr io.Writer, snapshots []fileSnapshot, name, action string, cause error) {
	if restoreErr := restoreSnapshots(fs, snapshots); restoreErr != nil {
		fmt.Fprintf(stderr, "%s: %s: %v (rollback failed: %v)\n", name, action, cause, restoreErr) //nolint:errcheck
		return
	}
	fmt.Fprintf(stderr, "%s: %s: %v\n", name, action, cause) //nolint:errcheck
}
