package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pgauth"
)

const defaultManagedDoltHost = "127.0.0.1"

var postgresCredentialResolvedSeen sync.Map // map[string]struct{}

func postgresCredentialResolvedKey(cityPath string, payload pgauth.PostgresCredentialResolvedPayload) string {
	return strings.Join([]string{
		cityPath,
		payload.ScopeKind,
		payload.ScopeName,
		payload.Source,
		payload.Host,
		payload.Port,
		payload.User,
	}, "\x00")
}

// bdCommandRunnerForCity centralizes bd subprocess env construction so all
// GC-managed bd calls resolve Dolt against the same city-scoped runtime.
// Env is rebuilt on each call so GC_BEADS_PORT reflects the current managed
// dolt port (which can change across city restarts).
func bdCommandRunnerForCity(cityPath string) beads.CommandRunner {
	return bdCommandRunnerWithManagedRetryErr(cityPath, func(dir string) (map[string]string, error) {
		env, err := bdRuntimeEnvWithError(cityPath)
		env["BEADS_DIR"] = filepath.Join(dir, ".beads")
		return env, err
	})
}

func bdStoreForCity(dir, cityPath string) *beads.BdStore {
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		cfg = nil
	}
	reapStaleBdExportJSONL(dir)
	return beads.NewBdStoreWithPrefix(
		dir,
		bdCommandRunnerForCity(cityPath),
		issuePrefixForScope(dir, cityPath, cfg),
		bdStoreOptionsForConfig(cfg)...,
	)
}

// bdStoreForRig opens a bead store at rigDir using rig-level Dolt config
// when available, falling back to city-level config. Use this when the rig
// may have its own Dolt server (e.g., shared from another city).
func bdStoreForRig(rigDir, cityPath string, cfg *config.City, knownPrefix ...string) *beads.BdStore {
	prefix := issuePrefixForScope(rigDir, cityPath, cfg)
	if prefix == "" {
		for _, candidate := range knownPrefix {
			if strings.TrimSpace(candidate) != "" {
				prefix = candidate
				break
			}
		}
	}
	reapStaleBdExportJSONL(rigDir)
	return beads.NewBdStoreWithPrefix(
		rigDir,
		bdCommandRunnerForRig(cityPath, cfg, rigDir),
		prefix,
		bdStoreOptionsForConfig(cfg)...,
	)
}

func bdStoreOptionsForConfig(cfg *config.City) []beads.BdStoreOption {
	if cfg != nil && cfg.Beads.UsesBD105CLISemantics() {
		return []beads.BdStoreOption{beads.WithBdStoreListSkipLabels(true)}
	}
	return nil
}

// reapStaleBdExportJSONL removes .beads/issues.jsonl best-effort when the
// scope is gc-managed. The file is a stale export from when bd's auto-export
// was on (the upstream default); keeping it on disk causes bd 1.x to detect
// a "fresh clone" / "empty database" on the next write and stall bd create /
// gc mail send for the full 2m subprocess timeout while it re-imports the
// JSONL (sa-41j3kp).
//
// Cleanup conditions (any of which proves the scope is gc-managed and the
// JSONL is therefore stale):
//
//   - config.yaml explicitly sets export.auto:false (PR 1965 canonical state)
//   - config.yaml's gc.endpoint_origin is one of the managed origins
//
// Best-effort: any error is ignored because the env-var BD_EXPORT_AUTO=false
// in bdRuntimeEnv is a second line of defense, and a concurrent reader of the
// file (e.g., a bd-aware viewer) shouldn't fail the caller's operation. Reads
// use os.Stat/os.Remove (not fsys.OSFS) so the helper stays callable from
// store constructors that don't carry an fs seam.
func reapStaleBdExportJSONL(scopeRoot string) {
	scopeRoot = strings.TrimSpace(scopeRoot)
	if scopeRoot == "" {
		return
	}
	jsonlPath := filepath.Join(scopeRoot, ".beads", "issues.jsonl")
	if _, err := os.Stat(jsonlPath); err != nil {
		// Fast path: no file → nothing to do. This is the steady state
		// once the cleanup has run once, so the rest of the helper is
		// only reached during the one-shot transition.
		return
	}
	if !scopeIsGCManaged(scopeRoot) {
		// Unmanaged scope: leave the file alone. Removing it under those
		// conditions could race with a legitimate auto-exporter (e.g., a
		// rig that opted out of managed canonicalization).
		return
	}
	_ = os.Remove(jsonlPath)
}

// scopeIsGCManaged reports whether a scope's .beads/config.yaml proves the
// scope is gc-managed under the canonical (non-explicit) shape. Either of
// two signals counts as proof:
//   - export.auto is explicitly false (PR 1965 wrote it; the user did not
//     opt back into auto-export afterward)
//   - gc.endpoint_origin is one of the canonical managed origins (the scope
//     was initialized by gc, even if the export.auto key has not yet been
//     normalized into the config on disk)
//
// Either signal alone is sufficient: the first covers post-normalization
// state, the second covers the transitional state where samtown-style
// long-lived cities still have a pre-PR-1965 config but were always
// gc-managed.
//
// EndpointOriginExplicit is intentionally excluded: per PR 1965, that is
// the deliberate opt-out path for rigs that want to keep JSONL-based
// sharing, so issues.jsonl there is load-bearing, not stale. The
// endpoint-origin check runs first so that an opt-out rig that *also*
// has export.auto:false (e.g. left over from a prior canonicalization,
// or hand-set) is still treated as unmanaged and never reaped.
func scopeIsGCManaged(scopeRoot string) bool {
	configPath := filepath.Join(scopeRoot, ".beads", "config.yaml")
	// The legacy gc.endpoint_origin: explicit marker is the deliberate opt-out
	// path for scopes that keep JSONL-based sharing — its issues.jsonl is
	// load-bearing, so it must win over every other managed signal (coords,
	// export.auto:false). bd's config.yaml never carries this key; it survives
	// only on older configs, but the reaper must still honor it.
	if readScopeEndpointOrigin(scopeRoot) == "explicit" {
		return false
	}
	state, stateOK, stateErr := contract.ReadConfigState(fsys.OSFS{}, configPath)
	if stateErr == nil && stateOK {
		if contract.ConfigHasEndpointAuthority(state) {
			// Scope pins its own external endpoint — the deliberate opt-out path;
			// its issues.jsonl is load-bearing, so never treat it as reapable.
			return false
		}
		// Local (bd-owned) scope — gc-managed.
		return true
	}
	if autoExport, ok, err := contract.ReadExportAuto(fsys.OSFS{}, configPath); err == nil && ok && !autoExport {
		return true
	}
	return false
}

// readScopeEndpointOrigin reads the legacy gc.endpoint_origin scalar from a
// scope's .beads/config.yaml, or "" when absent/unreadable.
func readScopeEndpointOrigin(scopeRoot string) string {
	return readSimpleYAMLScalar(filepath.Join(scopeRoot, ".beads", "config.yaml"), "gc.endpoint_origin")
}

func controlBdStoreForCity(dir, cityPath string, cfg *config.City) *beads.BdStore {
	reapStaleBdExportJSONL(dir)
	return beads.NewBdStoreWithPrefix(
		dir,
		controlBdCommandRunnerForCity(cityPath),
		issuePrefixForScope(dir, cityPath, cfg),
		bdStoreOptionsForConfig(cfg)...,
	)
}

func controlBdStoreForRig(rigDir, cityPath string, cfg *config.City, knownPrefix ...string) *beads.BdStore {
	prefix := issuePrefixForScope(rigDir, cityPath, cfg)
	if prefix == "" {
		for _, candidate := range knownPrefix {
			if strings.TrimSpace(candidate) != "" {
				prefix = candidate
				break
			}
		}
	}
	reapStaleBdExportJSONL(rigDir)
	return beads.NewBdStoreWithPrefix(
		rigDir,
		controlBdCommandRunnerForRig(cityPath, cfg, rigDir),
		prefix,
		bdStoreOptionsForConfig(cfg)...,
	)
}

func controlBdCommandRunnerForCity(cityPath string) beads.CommandRunner {
	return bdCommandRunnerWithManagedRetryErr(cityPath, func(dir string) (map[string]string, error) {
		env, err := bdRuntimeEnvWithError(cityPath)
		env["BEADS_DIR"] = filepath.Join(dir, ".beads")
		applyControllerBdEnv(env)
		return env, err
	})
}

func controlBdCommandRunnerForRig(cityPath string, cfg *config.City, rigDir string) beads.CommandRunner {
	return bdCommandRunnerWithManagedRetryErr(cityPath, func(_ string) (map[string]string, error) {
		env, err := bdRuntimeEnvForRigWithError(cityPath, cfg, rigDir)
		applyControllerBdEnv(env)
		return env, err
	})
}

func applyExportSuppressionEnv(env map[string]string) {
	env["BD_EXPORT_AUTO"] = "false"
}

func applyControllerBdEnv(env map[string]string) {
	applyExportSuppressionEnv(env)
	if strings.TrimSpace(os.Getenv("BEADS_ACTOR")) == "" {
		env["BEADS_ACTOR"] = "controller"
	}
}

func issuePrefixForScope(scopeRoot, cityPath string, cfg *config.City) string {
	if prefix := readScopeIssuePrefix(scopeRoot); prefix != "" {
		return prefix
	}
	if cfg == nil {
		return ""
	}
	scopeRoot = filepath.Clean(scopeRoot)
	if filepath.Clean(cityPath) == scopeRoot {
		return config.EffectiveHQPrefix(cfg)
	}
	for i := range cfg.Rigs {
		rigPath := resolveStoreScopeRoot(cityPath, cfg.Rigs[i].Path)
		if filepath.Clean(rigPath) == scopeRoot {
			return cfg.Rigs[i].EffectivePrefix()
		}
	}
	return ""
}

func readScopeIssuePrefix(scopeRoot string) string {
	prefix, ok, err := contract.ReadIssuePrefix(fsys.OSFS{}, filepath.Join(scopeRoot, ".beads", "config.yaml"))
	if err != nil || !ok {
		return ""
	}
	return prefix
}

func bdCommandRunnerForRig(cityPath string, cfg *config.City, rigDir string) beads.CommandRunner {
	return bdCommandRunnerWithManagedRetryErr(cityPath, func(_ string) (map[string]string, error) {
		return bdRuntimeEnvForRigWithError(cityPath, cfg, rigDir)
	})
}

func canonicalScopeDoltTarget(cityPath, scopeRoot string) (contract.DoltConnectionTarget, bool, error) {
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, scopeRoot, "")
	if err != nil {
		return contract.DoltConnectionTarget{}, false, err
	}
	if resolved.Kind != contract.ScopeConfigAuthoritative {
		return contract.DoltConnectionTarget{}, false, nil
	}
	target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityPath, scopeRoot)
	if err != nil {
		return contract.DoltConnectionTarget{}, true, err
	}
	return target, true, nil
}

func applyCanonicalDoltTargetEnv(env map[string]string, target contract.DoltConnectionTarget) {
	if env == nil {
		return
	}
	// GC-owned projections must use the resolved target, not ambient parent
	// shell host/port. Stale GC_BEADS_HOST/PORT was causing gc bd and projected
	// session flows to drift away from the canonical external endpoint.
	if shouldProjectResolvedDoltHost(target) {
		env["GC_BEADS_HOST"] = strings.TrimSpace(target.Host)
	} else {
		delete(env, "GC_BEADS_HOST")
	}
	if strings.TrimSpace(target.Port) != "" {
		env["GC_BEADS_PORT"] = target.Port
	} else {
		delete(env, "GC_BEADS_PORT")
	}
	if user := strings.TrimSpace(target.User); user != "" {
		env["GC_BEADS_USER"] = user
	} else {
		delete(env, "GC_BEADS_USER")
	}
}

func shouldProjectResolvedDoltHost(target contract.DoltConnectionTarget) bool {
	host := strings.TrimSpace(target.Host)
	if host == "" {
		return false
	}
	if target.External {
		return true
	}
	return !managedLocalDoltHost(host)
}

// externalDoltEnvOverrideTarget returns an external Dolt target pinned by the
// ambient GC_BEADS_HOST/PORT/USER env. This is the operator override that lets a
// gc process reach an external endpoint without a canonical .beads/config.yaml —
// bd owns local proxied endpoints, so only a non-local, concrete host counts.
// The reserved .invalid TLD is a stale-ambient sentinel used by tests/runbooks
// and is never promoted into a child bd process.
func externalDoltEnvOverrideTarget() (contract.DoltConnectionTarget, bool) {
	hostOverride := strings.TrimSpace(os.Getenv("GC_BEADS_HOST"))
	if hostOverride == "" || managedLocalDoltHost(hostOverride) {
		return contract.DoltConnectionTarget{}, false
	}
	if strings.HasSuffix(strings.Trim(hostOverride, "[]"), ".invalid") {
		return contract.DoltConnectionTarget{}, false
	}
	return contract.DoltConnectionTarget{
		Host:     hostOverride,
		Port:     strings.TrimSpace(os.Getenv("GC_BEADS_PORT")),
		User:     strings.TrimSpace(os.Getenv("GC_BEADS_USER")),
		External: true,
	}, true
}

// applyAmbientExternalDoltOverride threads the ambient GC_BEADS_HOST external
// override onto env when the scope did not already resolve a canonical external
// endpoint and is not a doltlite/postgres backend. It is the fallback for scopes
// with a missing or local (no-coords) config: bd owns the local proxied server,
// but an operator that exports GC_BEADS_HOST is explicitly pointing gc at an
// external endpoint.
func applyAmbientExternalDoltOverride(env map[string]string, cityPath, scopeRoot string) {
	if env == nil {
		return
	}
	if strings.TrimSpace(env["GC_BEADS_HOST"]) != "" {
		return
	}
	if env["GC_BEADS_BACKEND"] == "doltlite" || env["BEADS_BACKEND"] == "doltlite" {
		return
	}
	if scopeBackendIsPostgres(cityPath, scopeRoot) {
		return
	}
	if target, ok := externalDoltEnvOverrideTarget(); ok {
		applyCanonicalDoltTargetEnv(env, target)
		mirrorBeadsDoltScopeEnv(env, target)
		applyCanonicalDoltAuthEnv(env, cityPath, scopeRoot, target)
		return
	}
	// No external override on a local/missing dolt scope: stamp the non-external
	// mirror so an ambient hosted-gateway BEADS_DOLT_SERVER_TLS=1 does not bleed
	// into this plaintext/local bd env, and no stale endpoint is inherited.
	mirrorBeadsDoltEnv(env)
}

// applyCanonicalScopeBackendEnv dispatches to the appropriate backend
// helper based on the scope's MetadataState.Backend.
//
// Returns (true, nil) when the scope is authoritative and the backend
// projection succeeded. Returns (false, nil) when the scope is
// non-authoritative — caller falls through to inherited-city. Returns
// (true, err) on a known backend that failed to project; caller MUST
// surface this error rather than retrying.
func applyCanonicalScopeBackendEnv(env map[string]string, cityPath, scopeRoot string) (bool, error) {
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, scopeRoot, "")
	if err != nil {
		return false, err
	}
	// Backend detection runs for any scope that has a config on disk. A bare
	// bd-written config (issue_prefix only, or carrying stale gc.* keys) now
	// resolves as LegacyMinimal, not Authoritative — that is the common shape
	// in proxied-server mode — so both kinds drive backend env; only a wholly
	// missing config threads nothing.
	if resolved.Kind == contract.ScopeConfigMissing {
		return false, nil
	}
	meta, _, metaErr := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(scopeRoot))
	if metaErr != nil {
		return true, metaErr
	}
	if scopeInheritsCityConfig(cityPath, scopeRoot, resolved.State) && meta.Backend == "" {
		if usedPostgres, err := applyCityPostgresBackendEnv(env, cityPath); err != nil {
			return true, err
		} else if usedPostgres {
			return true, nil
		}
	}
	if scopeInheritsCityConfig(cityPath, scopeRoot, resolved.State) &&
		(meta.Backend == "" || meta.Backend == "doltlite") &&
		cityUsesDoltliteBeadsBackend(cityPath) {
		clearProjectedDoltEnv(env)
		clearProjectedPostgresEnv(env)
		env["GC_BEADS_BACKEND"] = "doltlite"
		env["BEADS_BACKEND"] = "doltlite"
		mirrorBeadsDoltEnv(env)
		return true, nil
	}
	switch meta.Backend {
	case "", "dolt":
		clearProjectedBeadsBackendEnv(env)
		clearProjectedPostgresEnv(env)
		target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityPath, scopeRoot)
		if err != nil {
			return true, err
		}
		applyCanonicalDoltTargetEnv(env, target)
		mirrorBeadsDoltScopeEnv(env, target)
		applyCanonicalDoltAuthEnv(env, cityPath, scopeRoot, target)
		return true, nil
	case "doltlite":
		clearProjectedDoltEnv(env)
		clearProjectedPostgresEnv(env)
		env["GC_BEADS_BACKEND"] = "doltlite"
		env["BEADS_BACKEND"] = "doltlite"
		mirrorBeadsDoltEnv(env)
		return true, nil
	case "postgres":
		if err := applyResolvedScopePostgresEnv(env, cityPath, scopeRoot, meta); err != nil {
			return true, err
		}
		return true, nil
	default:
		return true, fmt.Errorf("unsupported backend %q for scope %s", meta.Backend, scopeRoot)
	}
}

func applyCityPostgresBackendEnv(env map[string]string, cityPath string) (bool, error) {
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, cityPath, "")
	if err != nil {
		return false, err
	}
	if resolved.Kind == contract.ScopeConfigMissing {
		return false, nil
	}
	meta, _, metaErr := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(cityPath))
	if metaErr != nil {
		return true, metaErr
	}
	switch meta.Backend {
	case "postgres":
		if err := applyResolvedScopePostgresEnv(env, cityPath, cityPath, meta); err != nil {
			return true, err
		}
		return true, nil
	case "", "dolt", "doltlite":
		return false, nil
	default:
		return true, fmt.Errorf("unsupported backend %q for scope %s", meta.Backend, cityPath)
	}
}

func scopeBackendIsDoltlite(cityPath, scopeRoot string) bool {
	meta, ok, err := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(scopeRoot))
	if err == nil && ok && meta.Backend != "" {
		return meta.Backend == "doltlite"
	}
	if samePath(cityPath, scopeRoot) {
		return cityUsesDoltliteBeadsBackend(cityPath)
	}
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, scopeRoot, "")
	if err != nil || resolved.Kind == contract.ScopeConfigMissing {
		return false
	}
	return scopeInheritsCityConfig(cityPath, scopeRoot, resolved.State) &&
		cityUsesDoltliteBeadsBackend(cityPath)
}

// scopeInheritsCityConfig reports whether a scope is a rig that carries no
// external endpoint of its own, and so follows the city's config (a local rig).
// A scope is external iff its OWN config has coords — there is no cross-scope
// endpoint inheritance; this helper only classifies "local rig vs the rest".
func scopeInheritsCityConfig(cityPath, scopeRoot string, state contract.ConfigState) bool {
	return !samePath(cityPath, scopeRoot) && !contract.ConfigHasEndpointAuthority(state)
}

func scopeOverridesCityBackend(cityPath, scopeRoot string) bool {
	if samePath(cityPath, scopeRoot) {
		return false
	}
	meta, ok, err := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(scopeRoot))
	if err == nil && ok && strings.TrimSpace(meta.Backend) != "" {
		return true
	}
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, scopeRoot, "")
	if err != nil || resolved.Kind != contract.ScopeConfigAuthoritative {
		return false
	}
	return contract.ConfigHasEndpointAuthority(resolved.State)
}

// scopeMetadataJSONPath returns the absolute path to a scope's
// .beads/metadata.json. Centralized so the dispatcher and the recovery
// hook helpers agree on the file location.
func scopeMetadataJSONPath(scopeRoot string) string {
	return filepath.Join(scopeRoot, ".beads", "metadata.json")
}

// applyResolvedScopePostgresEnv projects PG credentials and connection
// info into env. Caller guarantees meta.Backend == "postgres". The
// resolver chain in internal/pgauth supplies the password; the host,
// port, user, and database come straight from MetadataState.
//
// On resolver exhaustion returns an error wrapping
// pgauth.ErrNoPasswordResolvable; callers can match with errors.Is.
//
// On success emits a pg.credential_resolved event identifying the scope
// and the resolution tier that supplied the value (best-effort; recorder
// failures do not propagate).
func applyResolvedScopePostgresEnv(env map[string]string, cityPath, scopeRoot string, meta contract.MetadataState) error {
	if env == nil {
		return nil
	}
	clearProjectedBeadsBackendEnv(env)
	clearProjectedDoltEnv(env)
	mirrorBeadsDoltEnv(env)
	clearProjectedPostgresEnv(env)
	endpoint := pgauth.Endpoint{
		Host: meta.PostgresHost,
		Port: meta.PostgresPort,
		User: meta.PostgresUser,
	}
	// Scope projection clears inherited PG keys first, so credential
	// resolution intentionally starts at process and file-backed sources.
	resolved, err := pgauth.ResolveFromEnv(nil, scopeRoot, endpoint)
	if err != nil {
		return fmt.Errorf("resolving postgres credentials for %s: %w", scopeRoot, err)
	}
	env["GC_POSTGRES_PASSWORD"] = resolved.Password
	env["BEADS_POSTGRES_PASSWORD"] = resolved.Password
	env["BEADS_POSTGRES_HOST"] = meta.PostgresHost
	env["BEADS_POSTGRES_PORT"] = meta.PostgresPort
	env["BEADS_POSTGRES_USER"] = meta.PostgresUser
	env["BEADS_POSTGRES_DATABASE"] = meta.PostgresDatabase
	mirrorBeadsPostgresEnv(env)
	emitPostgresCredentialResolved(cityPath, scopeRoot, meta, resolved.Source)
	return nil
}

// emitPostgresCredentialResolved records a pg.credential_resolved event
// describing the scope and the resolution tier that supplied the password.
// Best-effort: recorder failures (file unreachable, JSONL write error) do
// not propagate to the caller. The payload deliberately omits the password
// value (asserted by TestPostgresEventOmitsPassword).
func emitPostgresCredentialResolved(cityPath, scopeRoot string, meta contract.MetadataState, source pgauth.Source) {
	scopeKind, scopeName := scopeKindAndName(cityPath, scopeRoot)
	subject := "city/" + scopeName
	if scopeKind == "rig" {
		subject = "rigs/" + scopeName
	}
	payload := pgauth.PostgresCredentialResolvedPayload{
		ScopeKind: scopeKind,
		ScopeName: scopeName,
		Source:    source.String(),
		Host:      meta.PostgresHost,
		Port:      meta.PostgresPort,
		User:      meta.PostgresUser,
	}
	if _, loaded := postgresCredentialResolvedSeen.LoadOrStore(postgresCredentialResolvedKey(cityPath, payload), struct{}{}); loaded {
		return
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		// Marshal of a flat-string struct should never fail; if it
		// does, there is no useful payload to record.
		return
	}
	rec, err := events.NewFileRecorder(filepath.Join(cityPath, ".gc", "events.jsonl"), io.Discard)
	if err != nil {
		return
	}
	defer rec.Close() //nolint:errcheck // best-effort: emission must not surface I/O errors
	rec.Record(events.Event{
		Type:    events.PostgresCredentialResolved,
		Actor:   eventActor(),
		Subject: subject,
		Payload: payloadBytes,
	})
}

// scopeKindAndName returns ("city", <city-name>) when scopeRoot is the
// city itself, or ("rig", <rig-name>) when scopeRoot is a rig under or
// outside the city. Path comparison is best-effort lexical equality after
// filepath.Clean; callers tolerate ambiguity (the recorded event remains
// useful even if a non-canonical path snaps to the rig branch).
func scopeKindAndName(cityPath, scopeRoot string) (string, string) {
	if filepath.Clean(scopeRoot) == filepath.Clean(cityPath) {
		return "city", filepath.Base(filepath.Clean(cityPath))
	}
	return "rig", filepath.Base(filepath.Clean(scopeRoot))
}

// mirrorBeadsPostgresEnv copies canonical (GC_*) PG env keys to their
// bd-expected (BEADS_POSTGRES_*) names. Today only the password has a
// canonical-vs-bd split; the function exists so future bd-side
// renames become a one-line change.
func mirrorBeadsPostgresEnv(env map[string]string) {
	if env == nil {
		return
	}
	if pass := env["GC_POSTGRES_PASSWORD"]; pass != "" {
		env["BEADS_POSTGRES_PASSWORD"] = pass
	} else {
		delete(env, "BEADS_POSTGRES_PASSWORD")
	}
	// HOST/PORT/USER/DATABASE: applyResolvedScopePostgresEnv populates
	// BEADS_POSTGRES_* directly from MetadataState; no GC_-side canonical
	// exists for those today. If a future refactor introduces GC_POSTGRES_HOST
	// etc., add the mirror copies here in the same shape as the password.
}

// projectedPostgresEnvKeys mirrors projectedDoltEnvKeys: the canonical
// list of env keys gc projects when a scope's backend is "postgres".
// The list MUST match exactly the PG-keys segment added to
// mergeRuntimeEnv's keys slice; TestProjectedKeysCoverage asserts the
// symmetry so a key added here without a matching strip-list entry
// fails CI.
var projectedPostgresEnvKeys = []string{
	"GC_POSTGRES_PASSWORD",
	"BEADS_POSTGRES_PASSWORD",
	"BEADS_POSTGRES_HOST",
	"BEADS_POSTGRES_PORT",
	"BEADS_POSTGRES_USER",
	"BEADS_POSTGRES_DATABASE",
}

func postgresMetadataForScope(cityPath, scopeRoot string) (contract.MetadataState, bool, error) {
	if scopeRoot == "" {
		scopeRoot = cityPath
	}
	meta, ok, err := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(scopeRoot))
	if err != nil {
		return contract.MetadataState{}, false, fmt.Errorf("loading metadata for scope %s: %w", scopeRoot, err)
	}
	if ok {
		switch meta.Backend {
		case "postgres":
			return meta, true, nil
		case "dolt":
			return contract.MetadataState{}, false, nil
		}
	}
	if samePath(scopeRoot, cityPath) {
		return contract.MetadataState{}, false, nil
	}
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, scopeRoot, "")
	if err != nil {
		return contract.MetadataState{}, false, fmt.Errorf("resolving config for scope %s: %w", scopeRoot, err)
	}
	// A bare bd-written scope config (issue_prefix only, no gc.* endpoint keys)
	// resolves as LegacyMinimal, not Authoritative; an inherited postgres rig is
	// exactly that shape, so both kinds qualify for city-metadata inheritance.
	if (resolved.Kind != contract.ScopeConfigAuthoritative && resolved.Kind != contract.ScopeConfigLegacyMinimal) || !scopeInheritsCityConfig(cityPath, scopeRoot, resolved.State) {
		return contract.MetadataState{}, false, nil
	}
	cityMeta, cityOK, cityErr := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(cityPath))
	if cityErr != nil {
		return contract.MetadataState{}, false, fmt.Errorf("loading city metadata for inherited scope %s: %w", scopeRoot, cityErr)
	}
	if !cityOK || cityMeta.Backend != "postgres" {
		return contract.MetadataState{}, false, nil
	}
	return cityMeta, true, nil
}

// scopeBackendIsPostgres returns true when the scope's MetadataState
// has Backend == "postgres" or when the scope inherits a city-level
// Postgres backend. On any read error returns false for best-effort callers
// that do not need to make recovery decisions.
func scopeBackendIsPostgres(cityPath, scopeRoot string) bool {
	_, ok, err := postgresMetadataForScope(cityPath, scopeRoot)
	if err != nil {
		return false
	}
	return ok
}

func applyCanonicalConfigStateDoltEnv(env map[string]string, cityPath, scopeRoot string, state contract.ConfigState) {
	target := contract.DoltConnectionTarget{
		Host:     strings.TrimSpace(state.DoltHost),
		Port:     strings.TrimSpace(state.DoltPort),
		User:     strings.TrimSpace(state.DoltUser),
		External: true,
	}
	applyCanonicalDoltTargetEnv(env, target)
	mirrorBeadsDoltScopeEnv(env, target)
	applyCanonicalDoltAuthEnv(env, cityPath, scopeRoot, target)
}

func applyCanonicalScopeInitDoltEnv(env map[string]string, cityPath, scopeRoot string) error {
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, scopeRoot, "")
	if err != nil {
		return err
	}
	if resolved.Kind != contract.ScopeConfigAuthoritative {
		return nil
	}
	// A scope is external iff its OWN config carries coords; local scopes need
	// no dolt endpoint env (bd owns the proxied local server).
	if contract.ConfigHasEndpointAuthority(resolved.State) {
		applyCanonicalConfigStateDoltEnv(env, cityPath, scopeRoot, resolved.State)
	}
	return nil
}

var projectedDoltEnvKeys = []string{
	"GC_BEADS_HOST",
	"GC_BEADS_PORT",
	"GC_BEADS_USER",
	"BEADS_DOLT_SERVER_HOST",
	"BEADS_DOLT_SERVER_PORT",
	"BEADS_DOLT_SERVER_USER",
	// BEADS_DOLT_SERVER_TLS is intentionally NOT a projected key: it is an
	// ambient hosted-gateway credential passthrough (see
	// hostedBeadsCredentialPassthroughKeys) with no per-scope source, so
	// mergeRuntimeEnv must not strip it. mirrorBeadsDoltEnv clears it (non-external
	// scopes) and mirrorBeadsDoltScopeEnv carries it (external endpoints) into the
	// native-open env instead.
}

var bdCLIRemoteSyncOptOutEnvKeys = [...]string{
	// BD_DOLT_SYNC_CLI_REMOTES is the key bd's BD-prefixed Viper env
	// binding consumes today; keep BEADS_DOLT_SYNC_CLI_REMOTES as a
	// compatibility alias only.
	"BD_DOLT_SYNC_CLI_REMOTES",
	"BEADS_DOLT_SYNC_CLI_REMOTES",
}

func appendBdCLIRemoteSyncOptOutEnvKeys(keys []string) []string {
	for _, key := range bdCLIRemoteSyncOptOutEnvKeys {
		keys = append(keys, key)
	}
	return keys
}

// bdAutoBackupOptOutEnvKeys disables bd's PersistentPostRun auto-backup —
// the hardcoded "backup_export" Dolt remote bd syncs on (almost) every
// invocation. A stuck-looping backup_export sync was the root cause of the
// 2026-06-08 town-wide Dolt wedge (ga-0eq): it saturated the commit path
// while oscillating not-found/already-exists. gc never relies on this path
// (managed backups run through mol-dog-backup), so it is pure downside here.
// BD_BACKUP_ENABLED is the key bd's BD-prefixed Viper env binding consumes
// today; keep BEADS_BACKUP_ENABLED as a compatibility alias only.
var bdAutoBackupOptOutEnvKeys = [...]string{
	"BD_BACKUP_ENABLED",
	"BEADS_BACKUP_ENABLED",
}

func appendBdAutoBackupOptOutEnvKeys(keys []string) []string {
	for _, key := range bdAutoBackupOptOutEnvKeys {
		keys = append(keys, key)
	}
	return keys
}

// bdContributorRoutingOptOutEnvKeys disables bd's fork/contributor
// auto-routing for gc-managed bd invocations. When a gcy-style store has
// routing.mode=auto and routing.contributor=~/.beads-planning persisted in
// its .beads config, upstream bd silently routes `create`/`list`/`update`
// to that out-of-band "planning" store while `show` (prefix-routed) and gc's
// in-process dispatch (sling/scale-check/hook pickup, which open the scope
// store directly via openCityStoreAt) keep reading the scope store. The
// result is a three-way split brain: a bead that `bd list --rig` shows is
// invisible to `bd show` and unresolvable by `gc sling`. gc owns scope→store
// resolution itself (BEADS_DIR + the rig registry), so contributor routing is
// pure downside here. Forcing routing.mode=off via the env override (which
// beadslib's getRoutingConfigValue / resolveRoutingConfigValue honor ahead of
// the persisted DB value) makes every gc-managed bd subcommand operate on the
// scope's own store — the same store sling and show already use.
//
// BD_ROUTING_MODE is the key bd's BD-prefixed Viper env binding consumes;
// BEADS_ROUTING_MODE is kept as a compatibility alias only.
var bdContributorRoutingOptOutEnvKeys = [...]string{
	"BD_ROUTING_MODE",
	"BEADS_ROUTING_MODE",
}

func appendBdContributorRoutingOptOutEnvKeys(keys []string) []string {
	for _, key := range bdContributorRoutingOptOutEnvKeys {
		keys = append(keys, key)
	}
	return keys
}

var (
	beadsExecCommandRunnerWithEnv = beads.ExecCommandRunnerWithEnv
	// processEnvSnapshot / ambientEnv are indirection seams so tests can stub
	// the ambient process environment. gc reaches the store solely through the
	// bd subprocess and never transiently projects scoped BEADS_* env into its
	// own process, so a plain os.Environ()/os.Getenv() is the ambient truth.
	processEnvSnapshot = os.Environ
	ambientEnv         = os.Getenv
)

func setProjectedDoltEnvEmpty(env map[string]string) {
	for _, key := range projectedDoltEnvKeys {
		env[key] = ""
	}
}

func ensureProjectedDoltEnvExplicit(env map[string]string) {
	for _, key := range projectedDoltEnvKeys {
		if _, ok := env[key]; !ok {
			env[key] = ""
		}
	}
}

func clearProjectedDoltEnv(env map[string]string) {
	for _, key := range projectedDoltEnvKeys {
		delete(env, key)
	}
}

var projectedBeadsBackendEnvKeys = []string{
	"GC_BEADS_BACKEND",
	"BEADS_BACKEND",
}

func clearProjectedBeadsBackendEnv(env map[string]string) {
	for _, key := range projectedBeadsBackendEnvKeys {
		delete(env, key)
	}
}

func clearProjectedPostgresEnv(env map[string]string) {
	for _, key := range projectedPostgresEnvKeys {
		delete(env, key)
	}
}

func ensureProjectedPostgresEnvExplicit(env map[string]string) {
	for _, key := range projectedPostgresEnvKeys {
		if _, ok := env[key]; !ok {
			env[key] = ""
		}
	}
}

func managedLocalDoltHost(host string) bool {
	return contract.DoltHostIsLocal(host)
}

// currentResolvableManagedDoltPort returns a live managed Dolt port from the
// published runtime state, or from provider state when publication has not
// caught up yet. Provider fallback uses validDoltRuntimeState instead of the
// contract package's lighter published-state validation because callers may
// mirror or publish this value into user-visible runtime files.

// bdSilentFallbackMarkerImport and bdSilentFallbackMarkerEmptyDB are the
// substring pair bd emits when it loses the managed Dolt server and silently
// falls back to opening the on-disk store with a JSONL auto-import. They are
// load-bearing in three places — the two transport-error classifiers below
// and bdOutputIndicatesSilentFallback — so they live here as the single
// source of truth. If bd's banner wording ever changes, this is the only
// edit site. (The root cause is fixed upstream in beads post-#3691; these
// markers remain the symptom detector for deployments still on stable bd.)
const (
	bdSilentFallbackMarkerImport  = "auto-importing"
	bdSilentFallbackMarkerEmptyDB = "into empty database"
)

// bdOutputIndicatesSilentFallback reports whether the given bd output
// (typically captured stderr) contains the marker pair that bd emits
// when it loses the managed Dolt server and silently falls back to
// opening the on-disk store with a JSONL auto-import. Operators of
// `gc bd ...` rely on this to convert the silent fallback into a loud,
// non-zero-exit error — in managed mode (BD_EXPORT_AUTO=false) the
// fallback drops writes. See gastownhall/gascity#2080 (bd update path)
// and gastownhall/gascity#2079 (bd close path) — both subcommands flow
// through the shared doBd handoff, so a single detection site covers
// the bd-write-persistence quad.
func bdOutputIndicatesSilentFallback(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, bdSilentFallbackMarkerImport) &&
		strings.Contains(lower, bdSilentFallbackMarkerEmptyDB)
}

func bdCommandRunnerWithManagedRetry(cityPath string, envFn func(dir string) map[string]string) beads.CommandRunner {
	return bdCommandRunnerWithManagedRetryErr(cityPath, func(dir string) (map[string]string, error) {
		return envFn(dir), nil
	})
}

func bdCommandRunnerWithManagedRetryErr(cityPath string, envFn func(dir string) (map[string]string, error)) beads.CommandRunner {
	return func(dir, name string, args ...string) ([]byte, error) {
		env, envErr := envFn(dir)
		if envErr != nil {
			return nil, envErr
		}
		if env == nil {
			env = map[string]string{}
		}
		ensureProjectedDoltEnvExplicit(env)
		ensureProjectedPostgresEnvExplicit(env)
		// No gascity-side managed-Dolt recovery: in proxied-server mode bd owns
		// the proxy + child dolt and brings them up (and recovers) on the command
		// itself, so a transient connection blip self-heals within the single bd
		// invocation.
		return beadsExecCommandRunnerWithEnv(env)(dir, name, args...)
	}
}

func applyResolvedCityDoltEnvContext(_ context.Context, env map[string]string, cityPath string, _ bool) error {
	// For a LOCAL dolt city, bd owns endpoint resolution AND credentials in
	// proxied-server mode (metadata.json, proxied_server_client_info.json, and
	// the external credential supplied at `bd init --proxied-server-external`),
	// so gascity threads nothing. Backend detection still runs, however: a
	// postgres or doltlite city threads its own backend env (pgauth credentials,
	// the doltlite backend marker), and an external dolt city threads its
	// canonical coords — exactly as the rig path does.
	if _, err := applyCanonicalScopeBackendEnv(env, cityPath, cityPath); err != nil {
		return err
	}
	applyAmbientExternalDoltOverride(env, cityPath, cityPath)
	return nil
}

func rigConfigForScopeRoot(cityPath, rigPath string, rigs []config.Rig) *config.Rig {
	rigPath = filepath.Clean(rigPath)
	for i := range rigs {
		rp := rigs[i].Path
		if !filepath.IsAbs(rp) {
			rp = filepath.Join(cityPath, rp)
		}
		if samePath(rp, rigPath) {
			return &rigs[i]
		}
	}
	return nil
}

func applyResolvedRigDoltEnvContext(_ context.Context, env map[string]string, cityPath, rigPath string, _ *config.Rig, _ bool) error {
	// Backend detection (postgres vs dolt) still runs; a canonical postgres rig
	// threads its own env. For dolt, each rig scope is its own bd proxied-server
	// store — bd resolves the endpoint AND owns the credential from the rig's
	// .beads/, so gascity threads nothing for a dolt rig.
	if _, err := applyCanonicalScopeBackendEnv(env, cityPath, rigPath); err != nil {
		return err
	}
	applyAmbientExternalDoltOverride(env, cityPath, rigPath)
	return nil
}

// applyLegacyRigExternalTarget projects a legacy config.Rig{DoltHost,DoltPort}
// external endpoint onto env and returns the resolved external
// DoltConnectionTarget. A rig that sets an explicit Dolt host/port is an
// external endpoint by construction — the same way applyCanonicalConfigStateDoltEnv
// treats an explicit canonical endpoint as External — so callers mirror the
// returned target through mirrorBeadsDoltScopeEnv. That helper carries the
// hosted-gateway BEADS_DOLT_SERVER_TLS requirement into the native-open env only
// for a non-local endpoint (targetCarriesHostedGatewayTLS): a legacy rig that
// points at a hosted gateway connects with TLS, while an explicit or port-only
// 127.0.0.1 rig stays plaintext instead of inheriting a controller's ambient
// TLS=1. Using the non-scoped mirrorBeadsDoltEnv here would clear TLS for the
// gateway case too and force a TLS-required gateway rig configured through this
// compatibility path to attempt plaintext.

func rigRuntimeEnvIndependentOfCityProjection(cityPath, rigPath string, _ *config.Rig) bool {
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, rigPath, "")
	if err != nil {
		return false
	}
	return resolved.Kind == contract.ScopeConfigAuthoritative && contract.ConfigHasEndpointAuthority(resolved.State)
}

func cityPostgresProjectionErrorCanBeBypassed(cityPath string, err error) bool {
	if err == nil || !errors.Is(err, pgauth.ErrNoPasswordResolvable) {
		return false
	}
	_, ok, metaErr := postgresMetadataForScope(cityPath, cityPath)
	return metaErr == nil && ok
}

func bdRuntimeEnvForRigWithError(cityPath string, cfg *config.City, rigPath string) (map[string]string, error) {
	return bdRuntimeEnvForRigWithErrorRecovery(cityPath, cfg, rigPath, true)
}

// bdRuntimeEnvForRigWithErrorNoRecovery is bdRuntimeEnvForRigWithError
// without the managed-dolt recovery side effects; see
// bdRuntimeEnvWithErrorNoRecovery for why (gascity ga-cdmx6x).
func bdRuntimeEnvForRigWithErrorNoRecovery(cityPath string, cfg *config.City, rigPath string) (map[string]string, error) {
	return bdRuntimeEnvForRigWithErrorRecovery(cityPath, cfg, rigPath, false)
}

func bdRuntimeEnvForRigWithErrorRecovery(cityPath string, cfg *config.City, rigPath string, allowRecovery bool) (map[string]string, error) {
	return bdRuntimeEnvForRigWithErrorRecoveryContext(context.Background(), cityPath, cfg, rigPath, allowRecovery)
}

func bdRuntimeEnvForRigWithErrorRecoveryContext(ctx context.Context, cityPath string, cfg *config.City, rigPath string, allowRecovery bool) (map[string]string, error) {
	env, cityErr := bdRuntimeEnvWithErrorRecoveryContext(ctx, cityPath, allowRecovery)
	rigPath = filepath.Clean(rigPath)
	// Pin the rig store explicitly. The gc-beads-bd provider derives its Dolt
	// data root from GC_CITY_PATH unless BEADS_DIR is set, so cwd-based
	// discovery is not sufficient for rig-scoped operations.
	env["BEADS_DIR"] = filepath.Join(rigPath, ".beads")
	env["GC_RIG_ROOT"] = rigPath
	var explicitRig *config.Rig
	if cfg != nil {
		explicitRig = rigConfigForScopeRoot(cityPath, rigPath, cfg.Rigs)
		if explicitRig != nil {
			env["GC_RIG"] = explicitRig.Name
		}
	}
	rigDoltlite := scopeBackendIsDoltlite(cityPath, rigPath)
	cityDoltlite := scopeBackendIsDoltlite(cityPath, cityPath)
	if rigDoltlite || (cityDoltlite && !scopeOverridesCityBackend(cityPath, rigPath)) {
		clearProjectedDoltEnv(env)
		clearProjectedPostgresEnv(env)
		env["GC_BEADS_BACKEND"] = "doltlite"
		env["BEADS_BACKEND"] = "doltlite"
		mirrorBeadsDoltEnv(env)
		return env, nil
	}
	if err := applyResolvedRigDoltEnvContext(ctx, env, cityPath, rigPath, explicitRig, allowRecovery); err != nil {
		clearProjectedDoltEnv(env)
		clearProjectedPostgresEnv(env)
		mirrorBeadsDoltEnv(env)
		return env, err
	}
	if cityErr != nil && (!cityPostgresProjectionErrorCanBeBypassed(cityPath, cityErr) || !rigRuntimeEnvIndependentOfCityProjection(cityPath, rigPath, explicitRig)) {
		return env, cityErr
	}
	return env, nil
}

func bdRuntimeEnvWithError(cityPath string) (map[string]string, error) {
	return bdRuntimeEnvWithErrorRecovery(cityPath, true)
}

// bdRuntimeEnvWithErrorNoRecovery is bdRuntimeEnvWithError without the
// managed-dolt recovery/health-check/autostart side effects: it reads
// existing published or configured connection state only, and fails fast
// (env still gets the non-Dolt opt-out vars set) when no managed server is
// currently reachable. Recovering a managed dolt server is legitimate
// work, but doing it from every concurrent, short-budget scoped-store
// construction would multiply exactly the load a read-storm mitigation
// exists to bound (gascity ga-cdmx6x) — those callers use this instead of
// bdRuntimeEnvWithError.
func bdRuntimeEnvWithErrorNoRecovery(cityPath string) (map[string]string, error) {
	return bdRuntimeEnvWithErrorRecovery(cityPath, false)
}

func bdRuntimeEnvWithErrorRecovery(cityPath string, allowRecovery bool) (map[string]string, error) {
	return bdRuntimeEnvWithErrorRecoveryContext(context.Background(), cityPath, allowRecovery)
}

func bdRuntimeEnvWithErrorRecoveryContext(ctx context.Context, cityPath string, allowRecovery bool) (map[string]string, error) {
	env := cityRuntimeEnvMapForCity(cityPath)
	env["BEADS_DIR"] = filepath.Join(cityPath, ".beads")
	env["GC_RIG"] = ""
	env["GC_RIG_ROOT"] = ""
	// Suppress bd's built-in Dolt auto-start. The gc controller manages the
	// Dolt server lifecycle via gc-beads-bd; bd's CLI auto-start ignores the
	// dolt.auto-start:false config (beads resolveAutoStart priority bug) and
	// starts rogue servers from the agent's cwd with the wrong data_dir.
	env["BEADS_DOLT_AUTO_START"] = "0"
	// Suppress bd's auto-export of issues.jsonl on every write. The canonical
	// config also persists export.auto:false (see internal/beads/contract/files.go),
	// but the env var is the bulletproof per-invocation guard: it covers fresh
	// scopes whose config has not yet been canonicalized, and it short-circuits
	// the export → next-write-auto-import stall cycle (sa-41j3kp) even when an
	// out-of-band caller has left a stale .beads/issues.jsonl on disk. Without
	// this, bd's "auto-importing N bytes ... into empty database" path can
	// stall bd create / gc mail send for the full 2m subprocess timeout on
	// large datasets.
	env["BD_EXPORT_AUTO"] = "false"
	// Disable bd's fork/contributor auto-routing. Without this, a store with
	// routing.mode=auto + routing.contributor (gcy's ~/.beads-planning) sends
	// bd create/list/update to that out-of-band store while gc's in-process
	// dispatch (sling) and bd show read the scope store — a three-way split
	// brain. See bdContributorRoutingOptOutEnvKeys.
	applyBdContributorRoutingOptOut(env)
	applyBdCLIRemoteSyncOptOut(env)
	// Suppress bd's PersistentPostRun auto-backup (the "backup_export" Dolt
	// remote). Like BD_EXPORT_AUTO above, the env var is the bulletproof
	// per-invocation guard: it covers fresh rig scopes whose config has not
	// been canonicalized and overrides any drifted backup.enabled:true. A
	// stuck-looping backup_export sync wedged the whole town on 2026-06-08
	// (ga-0eq); managed backups run through mol-dog-backup, not this path.
	applyBdAutoBackupOptOut(env)
	if !cityUsesBdStoreContract(cityPath) {
		return env, nil
	}
	if scopeBackendIsDoltlite(cityPath, cityPath) {
		clearProjectedDoltEnv(env)
		clearProjectedPostgresEnv(env)
		env["GC_BEADS_BACKEND"] = "doltlite"
		env["BEADS_BACKEND"] = "doltlite"
		mirrorBeadsDoltEnv(env)
		return env, nil
	}
	if usedPostgres, err := applyCityPostgresBackendEnv(env, cityPath); err != nil {
		clearProjectedDoltEnv(env)
		clearProjectedPostgresEnv(env)
		mirrorBeadsDoltEnv(env)
		return env, err
	} else if usedPostgres {
		return env, nil
	}
	if err := applyResolvedCityDoltEnvContext(ctx, env, cityPath, allowRecovery); err != nil {
		clearProjectedDoltEnv(env)
		mirrorBeadsDoltEnv(env)
		return env, err
	}
	return env, nil
}

func cityRuntimeEnvMapForCity(cityPath string) map[string]string {
	return citylayout.CityRuntimeEnvMapForRuntimeDir(cityPath, citylayout.TrustedAmbientCityRuntimeDir(cityPath))
}

// cityIdentityAnchorsForCity returns only the three identity anchors
// (GC_CITY, GC_CITY_PATH, GC_CITY_RUNTIME_DIR) for cityPath. The shared
// projection lives in internal/citylayout so CLI and API session resolvers
// keep the identity-only contract in sync.
func cityIdentityAnchorsForCity(cityPath string) map[string]string {
	return citylayout.CityIdentityEnvMap(cityPath)
}

func cityRuntimeProcessEnvWithError(cityPath string) ([]string, error) {
	cityPath = normalizePathForCompare(cityPath)
	overrides := cityRuntimeEnvMapForCity(cityPath)
	var projectionErr error
	if cityUsesBdStoreContract(cityPath) {
		source := map[string]string{"BEADS_DOLT_AUTO_START": "0"}
		applyBdContributorRoutingOptOut(source)
		applyBdCLIRemoteSyncOptOut(source)
		applyBdAutoBackupOptOut(source)
		if usedPostgres, err := applyCityPostgresBackendEnv(source, cityPath); err != nil {
			clearProjectedDoltEnv(source)
			clearProjectedPostgresEnv(source)
			mirrorBeadsDoltEnv(source)
			projectionErr = err
		} else if !usedPostgres {
			err := applyResolvedCityDoltEnvContext(context.Background(), source, cityPath, false)
			if err != nil {
				// Mirror the postgres-error branch: clearing the projected Dolt
				// keys alone leaves BEADS_DOLT_SERVER_TLS unset in source, so it
				// never reaches overrides and preserveHostedBeadsCredentialEnv
				// re-injects the ambient hosted-gateway TLS=1 onto this
				// local/plaintext fallback. mirrorBeadsDoltEnv stamps the
				// non-external TLS="" clear so the fallback stays plaintext.
				clearProjectedDoltEnv(source)
				mirrorBeadsDoltEnv(source)
			}
		}
		keys := execProjectedBackendCopyKeys()
		// BEADS_DOLT_AUTO_START and BEADS_DOLT_SERVER_TLS are carried explicitly:
		// neither is in execProjectedBackendCopyKeys. TLS is deliberately kept out
		// of projectedDoltEnvKeys because it is a hosted-gateway credential
		// passthrough (preserveHostedBeadsCredentialEnv must be able to keep an
		// ambient value), but the scope mirror sets source[BEADS_DOLT_SERVER_TLS]=""
		// for a non-external/local city. That cleared value has to reach overrides —
		// present including empty — or preserveHostedBeadsCredentialEnv re-injects
		// the ambient hosted-gateway TLS=1 and forces TLS against a plaintext local
		// Dolt server.
		keys = append(keys, "BEADS_DOLT_AUTO_START", "BEADS_DOLT_SERVER_TLS")
		// The external-dolt credential (resolved by applyCanonicalDoltAuthEnv) and
		// its lookup path are not in the projected-key lists above, but a child bd
		// process needs them to authenticate to the external endpoint.
		keys = append(keys, projectedDoltAuthEnvKeys...)
		for _, key := range keys {
			if value, ok := source[key]; ok {
				overrides[key] = value
			}
		}
	}
	return mergeRuntimeEnv(processEnvSnapshot(), overrides), projectionErr
}

func applyBdCLIRemoteSyncOptOut(env map[string]string) {
	if env == nil {
		return
	}
	for _, key := range bdCLIRemoteSyncOptOutEnvKeys {
		env[key] = "false"
	}
}

// applyBdAutoBackupOptOut forces bd's PersistentPostRun auto-backup off for
// gc-managed bd invocations. It overrides any ambient or per-scope config
// value so a fresh or drifted rig store cannot re-enable the destructive
// backup_export sync (ga-0eq). See bdAutoBackupOptOutEnvKeys.
func applyBdAutoBackupOptOut(env map[string]string) {
	if env == nil {
		return
	}
	for _, key := range bdAutoBackupOptOutEnvKeys {
		env[key] = "false"
	}
}

// applyBdContributorRoutingOptOut forces bd's fork/contributor auto-routing
// off for gc-managed bd invocations. It overrides any ambient or per-scope
// routing.mode=auto config so a gcy-style store cannot siphon create/list/
// update to ~/.beads-planning while sling/show read the scope store — the
// three-way split brain documented on bdContributorRoutingOptOutEnvKeys.
func applyBdContributorRoutingOptOut(env map[string]string) {
	if env == nil {
		return
	}
	for _, key := range bdContributorRoutingOptOutEnvKeys {
		env[key] = "off"
	}
}

// mirrorBeadsDoltEnv projects the GC_BEADS_* connection values onto the
// BEADS_DOLT_SERVER_* names beadslib's in-process native store reads, and clears
// the native-open TLS requirement. Clearing TLS is the safe default for every
// non-external scope (doltlite, postgres, managed-local dolt, cleared/error
// fallbacks): such a scope must never negotiate TLS, including a requirement
// inherited from a hosted-gateway city env this scope's map was cloned from (rig
// runtime env is built on top of the city env). A scope that resolves to an
// external endpoint uses mirrorBeadsDoltScopeEnv instead, which carries TLS only
// for a non-local hosted-gateway endpoint.
func mirrorBeadsDoltEnv(env map[string]string) {
	mirrorBeadsDoltServerEnv(env, false)
}

// mirrorBeadsDoltScopeEnv is mirrorBeadsDoltEnv keyed on a resolved Dolt target:
// it carries the hosted-gateway BEADS_DOLT_SERVER_TLS requirement into the
// native-open env only for a target that actually speaks hosted-gateway TLS — an
// external endpoint with a non-local host (targetCarriesHostedGatewayTLS).
// Callers that already hold the resolved DoltConnectionTarget use this so ambient
// TLS reaches only genuine hosted gateways and never bleeds into a local/plaintext
// scope, including an explicit or port-only 127.0.0.1 endpoint that resolves
// External by topology but connects in the clear.
func mirrorBeadsDoltScopeEnv(env map[string]string, target contract.DoltConnectionTarget) {
	mirrorBeadsDoltServerEnv(env, targetCarriesHostedGatewayTLS(target))
}

// targetCarriesHostedGatewayTLS reports whether ambient BEADS_DOLT_SERVER_TLS
// should be carried into target's native-open env. TLS is transport policy, not
// endpoint topology: DoltConnectionTarget.External marks every non-managed
// explicit/city-canonical endpoint — including a plaintext 127.0.0.1 or a
// port-only legacy rig that populateExternalTarget/canonicalExternalHost default
// to loopback — so gating the carry on External alone forces TLS onto plaintext
// local endpoints and breaks them under a controller running with ambient
// BEADS_DOLT_SERVER_TLS=1 (PR #4008 review finding). A hosted beads-gateway, the
// only endpoint that terminates client TLS, is remote by construction, so the
// carry is gated on an external endpoint with a non-local host. This is
// deliberately narrower than the identity-deferral gate (which stays keyed on
// External): deferral only Pass-marks and relies on beadslib's open-time identity
// check, so it is safe for a local endpoint the plaintext probe can already
// authenticate, whereas forcing TLS onto that same endpoint is not.
func targetCarriesHostedGatewayTLS(target contract.DoltConnectionTarget) bool {
	return target.External && !contract.DoltHostIsLocal(target.Host)
}

func mirrorBeadsDoltServerEnv(env map[string]string, carryAmbientTLS bool) {
	if env == nil {
		return
	}
	if host := strings.TrimSpace(env["GC_BEADS_HOST"]); host != "" {
		env["BEADS_DOLT_SERVER_HOST"] = host
	} else {
		delete(env, "BEADS_DOLT_SERVER_HOST")
	}
	if port := strings.TrimSpace(env["GC_BEADS_PORT"]); port != "" {
		env["BEADS_DOLT_SERVER_PORT"] = port
	} else {
		// Keep the key present so child bd processes cannot inherit a stale
		// BEADS_DOLT_SERVER_PORT from an ambient parent environment.
		env["BEADS_DOLT_SERVER_PORT"] = ""
	}
	if user := strings.TrimSpace(env["GC_BEADS_USER"]); user != "" {
		env["BEADS_DOLT_SERVER_USER"] = user
	} else {
		delete(env, "BEADS_DOLT_SERVER_USER")
	}
	// Carry the hosted beads-gateway credential command into the projected env.
	// bd authenticates to the gateway by running the helper named in
	// BEADS_DOLT_CREDENTIAL_COMMAND; without it bd falls back to the static/root
	// user and the gateway rejects the connection (MySQL Error 1045). That key
	// contains "CREDENTIAL", so execenv.FilterInherited strips it from every
	// gc-spawned bd subprocess and agent session. preserveHostedBeadsCredentialEnv
	// re-adds it on the slice-merge paths (overlayEnvEntries / mergeRuntimeEnv),
	// but only when it is already present in the pre-filter environ and only on
	// those paths — the agent session env is built from this projected map, which
	// does not carry the ambient value, and a controller that exports the helper
	// under only the non-sensitive GC_BEADS_CRED_CMD (which survives filtering) has
	// nothing for that pass to preserve. Mirror GC_BEADS_CRED_CMD into
	// BEADS_DOLT_CREDENTIAL_COMMAND here (map value wins, else the ambient value
	// of either key) so bd authenticates with the resolved credential command.
	//
	// Two intentional asymmetries with the sibling BEADS_DOLT_* branches above,
	// kept deliberately (do not "normalize" them into the map->map convention):
	//  1. Ambient fallback via a bare os.Getenv: this credential branch reads the
	//     ambient process env directly, because a controller commonly exports
	//     only the helper and never seeds it into the projected map.
	//  2. Preserve-not-clear: when no source exists this branch leaves any
	//     existing target value untouched instead of deleting/emptying it. The
	//     siblings clear their target to defeat stale tmux inheritance; the
	//     credential key is instead preserved from ambient by
	//     preserveHostedBeadsCredentialEnv on the slice-merge paths, so clearing
	//     it here would fight that pass.
	if cred := strings.TrimSpace(env["GC_BEADS_CRED_CMD"]); cred != "" {
		env["BEADS_DOLT_CREDENTIAL_COMMAND"] = cred
	} else if cred := strings.TrimSpace(env["BEADS_DOLT_CREDENTIAL_COMMAND"]); cred != "" {
		env["BEADS_DOLT_CREDENTIAL_COMMAND"] = cred
	} else if ambient := strings.TrimSpace(os.Getenv("GC_BEADS_CRED_CMD")); ambient != "" {
		env["BEADS_DOLT_CREDENTIAL_COMMAND"] = ambient
	} else if ambient := strings.TrimSpace(os.Getenv("BEADS_DOLT_CREDENTIAL_COMMAND")); ambient != "" {
		env["BEADS_DOLT_CREDENTIAL_COMMAND"] = ambient
	}
	mirrorNativeDoltTLSEnv(env, carryAmbientTLS)
}

// mirrorNativeDoltTLSEnv scopes the BEADS_DOLT_SERVER_TLS native-open requirement
// to the resolved target, keeping the GC_BEADS_*->BEADS_DOLT_* projection in
// mirrorBeadsDoltServerEnv flat and the TLS gate readable as one unit. Only a
// hosted-gateway target (carryAmbientTLS, computed by targetCarriesHostedGatewayTLS)
// negotiates TLS; every non-carry scope — non-external, and an external-but-local
// plaintext endpoint — clears it.
func mirrorNativeDoltTLSEnv(env map[string]string, carryAmbientTLS bool) {
	if !carryAmbientTLS {
		// Non-carry scope (non-external, or an external endpoint with a local host
		// such as an explicit or port-only 127.0.0.1 rig): clear any TLS
		// requirement — including one inherited from a hosted-gateway city env this
		// map was cloned from, or a stale ambient value — so the native store, and
		// the bd fallback built from the same map, connect with the scope's real
		// (plaintext) transport instead of forcing TLS against a non-TLS server.
		// Key kept present but empty (like the PORT projection in
		// mirrorBeadsDoltServerEnv) so a child bd or reused map cannot resurrect it.
		// TLS is not a DoltConnectionTarget field, so there is no GC_BEADS_* source
		// to project.
		env["BEADS_DOLT_SERVER_TLS"] = ""
		return
	}
	// Hosted-gateway endpoint: carry the TLS requirement into the projected map
	// so the shell-out bd negotiates TLS. A hosted beads-gateway terminates
	// client TLS and rejects plaintext ("TLS required"). An explicit scoped value
	// wins; otherwise mirror it from the ambient process env — the same signal
	// bd reads when it inherits the controller env directly.
	if tls := strings.TrimSpace(env["BEADS_DOLT_SERVER_TLS"]); tls != "" {
		env["BEADS_DOLT_SERVER_TLS"] = tls
	} else if ambient := strings.TrimSpace(ambientEnv("BEADS_DOLT_SERVER_TLS")); ambient != "" {
		env["BEADS_DOLT_SERVER_TLS"] = ambient
	} else {
		env["BEADS_DOLT_SERVER_TLS"] = ""
	}
}

// cityForStoreDir resolves ambient store contexts. GC_CITY intentionally wins
// over filesystem discovery here; callers with an authoritative city path or
// hook-projected store root must pass that city directly.
func cityForStoreDir(dir string) string {
	if cityPath, ok := resolveExplicitCityPathEnv(); ok {
		return cityPath
	}
	if p, err := findCity(dir); err == nil {
		return p
	}
	return dir
}

func overlayEnvEntries(environ []string, overrides map[string]string) []string {
	out := execenv.FilterInherited(environ)
	if len(overrides) == 0 {
		return out
	}
	overrideKeys := make([]string, 0, len(overrides))
	for key := range overrides {
		overrideKeys = append(overrideKeys, key)
	}
	sort.Strings(overrideKeys)
	for _, key := range overrideKeys {
		out = removeEnvKey(out, key)
		out = append(out, key+"="+overrides[key])
	}
	out = preserveHostedBeadsCredentialEnv(out, environ, overrides)
	return out
}

func mergeRuntimeEnv(environ []string, overrides map[string]string) []string {
	keys := []string{
		"BEADS_CREDENTIALS_FILE",
		"BEADS_BACKEND",
		"BEADS_DIR",
		"BEADS_DOLT_AUTO_START",
		"BEADS_DOLT_PASSWORD",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_USER",
		"BEADS_POSTGRES_DATABASE",
		"BEADS_POSTGRES_HOST",
		"BEADS_POSTGRES_PASSWORD",
		"BEADS_POSTGRES_PORT",
		"BEADS_POSTGRES_USER",
		"GC_CITY",
		"GC_CITY_ROOT", // kept for stripping: no code emits this anymore, but inherited values must be cleaned
		"GC_CITY_PATH",
		"GC_CITY_RUNTIME_DIR",
		"GC_BEADS_BACKEND",
		"GC_BEADS_SKIP",
		"GC_BEADS_CONFIG_FILE",
		"GC_BEADS_DATA_DIR",
		"GC_BEADS_HOST",
		"GC_BEADS_LOCK_FILE",
		"GC_BEADS_LOG_FILE",
		"GC_BEADS_MANAGED_LOCAL",
		"GC_BEADS_PASSWORD",
		"GC_BEADS_PID_FILE",
		"GC_BEADS_PORT",
		"GC_BEADS_STATE_FILE",
		"GC_BEADS_USER",
		"GC_PACK_STATE_DIR",
		"GC_POSTGRES_PASSWORD",
		"GC_RIG",
		"GC_RIG_ROOT",
	}
	keys = appendBdCLIRemoteSyncOptOutEnvKeys(keys)
	keys = appendBdAutoBackupOptOutEnvKeys(keys)
	keys = appendBdContributorRoutingOptOutEnvKeys(keys)
	if len(overrides) > 0 {
		for key := range overrides {
			if !containsString(keys, key) {
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys)
	out := execenv.FilterInherited(environ)
	for _, key := range keys {
		out = removeEnvKey(out, key)
	}
	overrideKeys := make([]string, 0, len(overrides))
	for key := range overrides {
		overrideKeys = append(overrideKeys, key)
	}
	sort.Strings(overrideKeys)
	for _, key := range overrideKeys {
		out = append(out, key+"="+overrides[key])
	}
	out = preserveHostedBeadsCredentialEnv(out, environ, overrides)
	return out
}

// hostedBeadsCredentialPassthroughKeys are env vars the bd provider needs to
// authenticate to a hosted beads-gateway: the credential command and the inputs
// its helper (e.g. eia-helper) reads. Several contain execenv.IsSensitiveKey
// markers (CREDENTIAL / TOKEN) and would otherwise be stripped by
// FilterInherited, but they carry command/URL/path references — not secret
// values (the orchestrator key itself stays in a file mount) — so they are
// preserved explicitly for gc-spawned bd subprocesses.
var hostedBeadsCredentialPassthroughKeys = []string{
	"BEADS_DOLT_CREDENTIAL_COMMAND",
	"BEADS_DOLT_SERVER_TLS",
	"ORCHESTRATOR_KEY_FILE",
	"EIA_AUDIENCE",
	"EIA_SCOPES",
	"STS_MACHINE_URL",
	"STS_TOKEN_URL",
}

// githubTokenExecEnvKeys are the GitHub CLI auth env vars an exec order needs
// to run `gh`. Merge orders (and other PR housekeeping) shell out to `gh`,
// which authenticates from GH_TOKEN (preferred) or GITHUB_TOKEN. Both keys
// contain the substring TOKEN, so execenv.IsSensitiveKey reports them sensitive
// and the curated order-exec env — built from a map, then merged through
// FilterInherited — never carries the controller's ambient token into the child
// process. Every `gh` call the order runs then fails auth even though the
// controller holds a valid token. GH_TOKEN wins over GITHUB_TOKEN in `gh`'s own
// precedence, but both are projected independently when present.
var githubTokenExecEnvKeys = []string{
	"GH_TOKEN",
	"GITHUB_TOKEN",
}

// projectGitHubTokenExecEnv copies the controller's ambient GitHub CLI auth
// tokens into an exec-order env map so shelled-out `gh` invocations
// authenticate. It mirrors the ambient value the same way mirrorBeadsDoltEnv
// carries the hosted-gateway credential command, rather than weakening
// execenv.IsSensitiveKey: keeping these keys sensitive means execenv.RedactText
// still masks their values in captured exec output and logs. A value already in
// the map (an explicit [order.env] entry) is left untouched so an order can
// scope its own credential, and only non-empty ambient values are projected.
func projectGitHubTokenExecEnv(env map[string]string) {
	if env == nil {
		return
	}
	for _, key := range githubTokenExecEnvKeys {
		if strings.TrimSpace(env[key]) != "" {
			continue
		}
		if ambient := strings.TrimSpace(os.Getenv(key)); ambient != "" {
			env[key] = ambient
		}
	}
}

// preserveHostedBeadsCredentialEnv re-adds the hosted-gateway credential env
// from the original (pre-filter) environ, unless an override already set the
// key. Without this, FilterInherited drops the credential command (and the
// STS token URL) and gc-spawned bd cannot reach a hosted beads-gateway.
func preserveHostedBeadsCredentialEnv(out, environ []string, overrides map[string]string) []string {
	for _, key := range hostedBeadsCredentialPassthroughKeys {
		if _, ok := overrides[key]; ok {
			continue
		}
		prefix := key + "="
		for _, entry := range environ {
			if strings.HasPrefix(entry, prefix) {
				out = removeEnvKey(out, key)
				out = append(out, entry)
				break
			}
		}
	}
	return out
}

func removeEnvKey(environ []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return out
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
