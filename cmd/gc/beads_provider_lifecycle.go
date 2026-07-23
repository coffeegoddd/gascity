package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// providerLifecycleLaunchctlGetenv reads a value from `launchctl getenv` on
// macOS. Used by providerLifecycleProcessEnv as a fallback when an env var
// the bd-provider script consumes (currently only GC_BEADS_LOGLEVEL) isn't
// set in os.Environ — without this, `gc start` from a user shell silently
// drops `launchctl setenv` values because they live in launchd's domain,
// not the shell's env. Returns "" on non-Darwin or when the key is unset
// or launchctl is unavailable.
//
// var so tests can stub without invoking real launchctl. Same pattern as
// supervisorLaunchctlRun / supervisorLaunchdActive in cmd_supervisor_lifecycle.go.
var providerLifecycleLaunchctlGetenv = func(key string) string {
	if goruntime.GOOS != "darwin" {
		return ""
	}
	out, err := exec.Command("launchctl", "getenv", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// cityDoltConfigs stores per-city Dolt configuration keyed by cityPath.
// Registered by startBeadsLifecycle so env builders and isExternalDolt can
// read city-scoped config without relying on process-global env vars (which
// break supervisor multi-tenancy where multiple cities share one process).
var cityDoltConfigs sync.Map // cityPath → config.DoltConfig

// providerOpSemaphores limits concurrent provider operations per city.
// When dolt goes down, health checks and recovery attempts from multiple
// callers can pile up. Without backpressure, all queued operations fire
// simultaneously when dolt restarts, causing a thundering herd that
// hammers the server back down. Each semaphore allows at most 1
// concurrent provider operation per city (serialize lifecycle ops).
var providerOpSemaphores sync.Map // cityPath → chan struct{}

// lastBeadsProviderRecover records the timestamp of the most recent
// recover attempt per city so healthBeadsProvider can refuse a 2nd
// recover within providerRecoverCooldown of the prior one. Together
// with the breaker-aware skip, this breaks the low-RSS restart-loop
// where each patrol tick re-trips the bd circuit breaker and
// re-desyncs the managed-dolt PID.
var lastBeadsProviderRecover sync.Map // cityPath → time.Time

// providerRecoverCooldown is the minimum interval between consecutive
// managed-dolt recover attempts on a single city. Stubbable for tests.
// 30s is the lower bound suggested by issue #2792 — long enough to
// span the bd breaker cooldown + dolt startup, short enough that a
// genuinely-degraded server still recovers on the next tick.
var providerRecoverCooldown = func() time.Duration { return 30 * time.Second }

// providerRecoverNow is the clock for the recover-backoff window.
// Stubbable for tests.
var providerRecoverNow = time.Now

func cityDoltConfigHasLifecycleFields(cfg config.DoltConfig) bool {
	return cfg.ArchiveLevel != nil ||
		cfg.AutoGCEnabled != nil ||
		cfg.MaxConnections != 0 ||
		cfg.ReadTimeoutMillis != 0 ||
		cfg.WriteTimeoutMillis != 0 ||
		cfg.DoltLockReleaseTimeout != ""
}

func registerCityDoltConfig(cityPath string, cfg config.DoltConfig) {
	cityDoltConfigs.Store(normalizePathForCompare(cityPath), cfg)
}

func clearCityDoltConfig(cityPath string) {
	cityDoltConfigs.Delete(normalizePathForCompare(cityPath))
}

// registerCityDoltConfigIfAbsent registers cfg for cityPath only when nothing is
// registered yet, returning true when it added the entry (so the caller knows to
// clear it). It never overwrites an existing registration: in the controller
// process the city dolt config is registered persistently at boot and on every
// reload by startBeadsLifecycle, and a transient per-request provisioning window
// must not delete or clobber it.
func registerCityDoltConfigIfAbsent(cityPath string, cfg config.DoltConfig) (added bool) {
	_, loaded := cityDoltConfigs.LoadOrStore(normalizePathForCompare(cityPath), cfg)
	return !loaded
}

var resolveProviderLifecycleGCBinary = func() string {
	if isTestBinary() {
		return ""
	}
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	if path, err := exec.LookPath("gc"); err == nil && path != "" {
		return path
	}
	return ""
}

var (
	providerProbeTimeout = 10 * time.Second
	// Override only in tests that do not call t.Parallel while the hook is changed.
	providerLifecycleContext = context.WithTimeout
)

var (
	initDirIfReadyEnsureBeadsProvider = ensureBeadsProvider
	initDirIfReadyInitAndHookDir      = initAndHookDir
)

func isRetryableManagedDoltLifecycleError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "dolt server exited during startup") ||
		strings.Contains(msg, "did not become query-ready") ||
		strings.Contains(msg, "signal: terminated") ||
		strings.Contains(msg, "table not found: issues") ||
		strings.Contains(msg, "table not found: config")
}

// ── Consolidated lifecycle operations ────────────────────────────────────
//
// The bead store lifecycle has a strict ordering:
//
//   start → [init + hooks]* → (agents run) → health* → stop
//
// These high-level functions enforce that ordering so call sites don't
// need to know the sequence. Use these instead of calling the low-level
// functions (ensureBeadsProvider, initBeadsForDir, installBeadHooks)
// directly.
//
// Exec provider protocol operations:
//   start         — start the backing service
//   init          — initialize beads in a directory
//   health        — check provider health
//   stop          — stop the backing service

// startBeadsLifecycle runs the full bead store startup sequence:
// start → init+hooks(city) → init+hooks(each rig) → regenerate routes.
// Called by gc start and controller config reload. Rigs must have absolute
// paths before calling (resolve relative paths first).
func startBeadsLifecycle(cityPath, _ string, cfg *config.City, stderr io.Writer) error {
	// Register per-city dolt config so env builders and isExternalDolt can
	// read it without process-global env vars. This is the single
	// registration point — supervisor, standalone, and reload all flow
	// through here. Always write (or clear) to handle config reload:
	// removing [dolt] after a reload must not leave stale entries.
	if cityDoltConfigHasLifecycleFields(cfg.Beads.Server) {
		registerCityDoltConfig(cityPath, cfg.Beads.Server)
	} else {
		clearCityDoltConfig(cityPath)
	}
	// PostgreSQL-backed bd stores talk to Postgres directly and never use the
	// managed dolt provider; mirror the guard initDirIfReady applies so
	// startBeadsLifecycle does not spawn/probe a dolt provider for them.
	cityUsesPostgres, pgErr := scopeUsesPostgresBackendForInit(cityPath, cityPath)
	if pgErr != nil {
		return fmt.Errorf("bead store: %w", pgErr)
	}
	skipLocalDolt := cityUsesPostgres
	switch {
	case isExternalDolt(cityPath):
		// An externally-pinned dolt endpoint (city_canonical / explicit, e.g. a
		// hosted beads-gateway) is not a gc-managed local lifecycle: connect to
		// the external server, never spawn or adopt a local managed Dolt for it.
		skipLocalDolt = true
	case cityUsesDoltliteBeadsBackend(cityPath):
		skipLocalDolt = true
	}
	if !skipLocalDolt {
		if err := ensureBeadsProvider(cityPath); err != nil {
			return fmt.Errorf("bead store: %w", err)
		}
	}
	beadsPrefix := config.EffectiveHQPrefix(cfg)
	// Leave doltDatabase empty unless the caller knows a canonical server DB
	// identity that differs from the bead prefix. New managed bd stores still
	// default to prefix-named databases, but older/imported metadata may carry
	// a different dolt_database that gc-beads-bd should preserve.
	if err := initAndHookDir(cityPath, cityPath, beadsPrefix); err != nil {
		return fmt.Errorf("init city beads: %w", err)
	}
	for i := range cfg.Rigs {
		if strings.TrimSpace(cfg.Rigs[i].Path) == "" {
			continue
		}
		prefix := cfg.Rigs[i].EffectivePrefix()
		if err := initAndHookDir(cityPath, cfg.Rigs[i].Path, prefix); err != nil {
			return fmt.Errorf("init rig %q beads: %w", cfg.Rigs[i].Name, err)
		}
	}
	// Regenerate routes for cross-rig routing.
	if len(cfg.Rigs) > 0 {
		allRigs := collectRigRoutes(cityPath, cfg)
		if err := writeAllRoutes(allRigs); err != nil {
			return fmt.Errorf("writing routes: %w", err)
		}
	}
	return nil
}

// initDirIfReady initializes beads for a single directory, ensuring the
// backing service is ready first. For the bd provider, this is a no-op
// (Dolt isn't running until gc start). Used by gc init and gc rig add.
//
// Returns (deferred bool, err). deferred=true means the bd provider
// skipped init — the caller should tell the user it's deferred to gc start.
func initDirIfReady(cityPath, dir, prefix string) (deferred bool, err error) {
	provider := beadsProvider(cityPath)
	if cityUsesManagedDoltBeadsLifecycle(cityPath) {
		// PostgreSQL-backed bd stores talk to Postgres directly and never use
		// the managed dolt provider. Skip the dolt provider start/hook path for
		// them (the deferred path below applies the same guard); postgres
		// provider readiness is handled at gc start.
		if usesPostgres, pgErr := scopeUsesPostgresBackendForInit(cityPath, dir); pgErr != nil {
			return false, pgErr
		} else if usesPostgres {
			return false, nil
		}
		if gcDoltSkip() {
			// Defer to controller/startup without forcing a new dolt_database:
			// preserve existing metadata identity when present.
			if err := seedDeferredManagedBeadsErr(cityPath, dir, prefix, ""); err != nil {
				return false, err
			}
			return true, nil
		}
		// bd's proxied-server provider owns the server lifecycle and ownership
		// resolution (local vs external); gascity just runs the init op, which
		// brings the proxy up lazily.
		if err := initDirIfReadyManagedDolt(cityPath, dir, prefix, provider); err != nil {
			return false, err
		}
		return false, nil
	}

	if provider == "" {
		if err := seedDeferredManagedBeadsErr(cityPath, dir, prefix, ""); err != nil {
			return false, err
		}
		return true, nil
	}
	// For exec: providers, probe to check if the backing service is available.
	// If not available (exit 2 or error), defer initialization to gc start.
	if strings.HasPrefix(provider, "exec:") {
		script := strings.TrimPrefix(provider, "exec:")
		if !runProviderProbe(script, cityPath, provider) {
			if cityUsesBdStoreContract(cityPath) {
				if err := seedDeferredManagedBeadsErr(cityPath, dir, prefix, ""); err != nil {
					return false, err
				}
			}
			return true, nil // Not running — defer to gc start.
		}
	}
	if err := initDirIfReadyManagedDolt(cityPath, dir, prefix, provider); err != nil {
		return false, err
	}
	return false, nil
}

func initDirIfReadyManagedDolt(cityPath, dir, prefix, _ string) error {
	if err := initDirIfReadyEnsureBeadsProvider(cityPath); err != nil {
		return fmt.Errorf("bead store: %w", err)
	}
	// No wait for a managed server: bd's proxied-server provider starts the
	// proxy + child dolt lazily on the init command itself.
	return initDirIfReadyInitAndHookDir(cityPath, dir, prefix)
}

//nolint:unparam // keep fs seam for future testable FS injection

func seedDeferredManagedBeads(cityPath, dir, prefix, doltDatabase string) {
	_ = seedDeferredManagedBeadsErr(cityPath, dir, prefix, doltDatabase)
}

func seedDeferredManagedBeadsErr(cityPath, dir, _, _ string) error {
	if !cityUsesBdStoreContract(cityPath) {
		return nil
	}
	if usesPostgres, err := scopeUsesPostgresBackendForInit(cityPath, dir); err != nil {
		return err
	} else if usesPostgres {
		return nil
	}
	// Deferred init: bd's proxied-server init (run at gc start) writes all
	// .beads state (metadata.json, config.yaml, client info, project_id) itself,
	// so seeding a deferred dolt scope only needs the .beads directory to exist.
	return ensureBeadsDir(fsys.OSFS{}, filepath.Join(dir, ".beads"))
}

func readDeferredManagedDoltDatabase(path, fallback string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fallback
	}

	var meta map[string]any
	if json.Unmarshal(data, &meta) != nil {
		return fallback
	}
	if db := strings.TrimSpace(fmt.Sprint(meta["dolt_database"])); db != "" && db != "<nil>" {
		return db
	}
	return fallback
}

func defaultScopeDoltDatabase(cityPath, dir, prefix string) string {
	if samePath(cityPath, dir) {
		return "hq"
	}
	return prefix
}

func isReservedManagedDoltDatabase(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "information_schema", "mysql", "dolt", "dolt_cluster", "performance_schema", "sys":
		return true
	}
	return false
}

func canonicalScopeDoltDatabase(cityPath, dir, prefix string) string {
	return readDeferredManagedDoltDatabase(filepath.Join(dir, ".beads", "metadata.json"), defaultScopeDoltDatabase(cityPath, dir, prefix))
}

// initAndHookDir is the atomic unit of bead store initialization:
// init the directory, then remove any stale gc-managed bead event hooks.
// The ordering matters because init (bd init) may recreate .beads/ and
// wipe existing hooks. installBeadHooks only removes gc-stamped hooks and
// is always safe to run regardless of event_hooks config.
func initAndHookDir(cityPath, dir, prefix string) error {
	if usesPostgres, err := scopeUsesPostgresBackendForInit(cityPath, dir); err != nil {
		return err
	} else if usesPostgres {
		if err := installBeadHooks(dir, cityPath); err != nil {
			return fmt.Errorf("install hooks at %s: %w", dir, err)
		}
		return nil
	}
	doltDatabase := canonicalScopeDoltDatabase(cityPath, dir, prefix)
	// bd's proxied-server init owns every beads state file (metadata.json,
	// config.yaml, proxied_server_client_info.json, project_id) and the server
	// lifecycle. gascity no longer pre-writes canonical scope files, direct-dials
	// the server to verify the database, or mirrors a managed port — it passes
	// prefix/database to `bd init --proxied-server` and lets beads do the rest.
	if err := initBeadsForDir(cityPath, dir, prefix, doltDatabase); err != nil {
		return err
	}
	// Non-fatal: hooks are convenience (event forwarding), not critical.
	if err := installBeadHooks(dir, cityPath); err != nil {
		return fmt.Errorf("install hooks at %s: %w", dir, err)
	}
	return nil
}

func scopeUsesPostgresBackendForInit(cityPath, dir string) (bool, error) {
	if !cityUsesBdStoreContract(cityPath) {
		return false, nil
	}
	path := scopeMetadataJSONPath(dir)
	state, ok, err := contract.LoadMetadataState(fsys.OSFS{}, path)
	if err != nil {
		if allowLegacyDoltMetadataRepair(fsys.OSFS{}, path, err) {
			return false, nil
		}
		return false, err
	}
	if ok {
		switch state.Backend {
		case "postgres":
			return true, nil
		case "dolt":
			return false, nil
		}
	}
	_, usesPostgres, err := postgresMetadataForScope(cityPath, dir)
	return usesPostgres, err
}

func allowLegacyDoltMetadataRepair(fs fsys.FS, path string, err error) bool {
	var parseErr *contract.MetadataParseError
	if !errors.As(err, &parseErr) {
		return false
	}
	data, readErr := fs.ReadFile(path)
	if readErr != nil {
		return false
	}
	var raw struct {
		Backend          string `json:"backend"`
		PostgresHost     string `json:"postgres_host"`
		PostgresPort     string `json:"postgres_port"`
		PostgresUser     string `json:"postgres_user"`
		PostgresDatabase string `json:"postgres_database"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(raw.Backend), "legacy") {
		return false
	}
	return strings.TrimSpace(raw.PostgresHost) == "" &&
		strings.TrimSpace(raw.PostgresPort) == "" &&
		strings.TrimSpace(raw.PostgresUser) == "" &&
		strings.TrimSpace(raw.PostgresDatabase) == ""
}

// verifyManagedDoltDatabaseExistsAfterInit confirms the named database is
// present in the running managed Dolt server's catalog. Used as a post-init
// guardrail to catch the silent-init failure mode where bd init reports
// success but the database was never actually created. Returns nil when
// the database is found, or an actionable error otherwise.
//
// The function is a no-op (returns nil) when the city does not use the bd
// store contract or when no managed Dolt port is resolvable — the caller
// already gates on those conditions, but we double-check defensively so
// the helper is safe to call from new sites without re-checking.

func shouldRetryExecBdInit(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "bd schema not visible")
}

func isBdAlreadyInitializedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already initialized") || strings.Contains(msg, "already exists")
}

// resolveRigPaths resolves relative rig paths to absolute (relative to
// cityPath). Mutates rigs in place. Must be called after loading city config
// and before any access to rigs[i].Path for filesystem operations. Required
// call sites include: doRigList, doRigAdd, doRigRemove, doRigDefault,
// cmd_start, cmd_hook, cmd_sling, dispatch_runtime, city_runtime,
// cmd_supervisor, cmd_convoy_dispatch.
func resolveRigPaths(cityPath string, rigs []config.Rig) {
	for i := range rigs {
		if strings.TrimSpace(rigs[i].Path) == "" {
			continue
		}
		if !filepath.IsAbs(rigs[i].Path) {
			rigs[i].Path = filepath.Join(cityPath, rigs[i].Path)
		}
	}
}

// ── Low-level provider operations ────────────────────────────────────────
//
// These are the building blocks. Prefer the consolidated functions above
// for new call sites. These remain exported for tests that need to verify
// individual operations.

// ensureBeadsProvider starts the bead store's backing service if needed.
// For exec providers, fires "start". For file providers, always available.
// Acquires a per-city semaphore to prevent concurrent start operations
// from causing spawn storms.
func ensureBeadsProvider(cityPath string) error {
	if cityUsesBdStoreContract(cityPath) && gcDoltSkip() {
		return nil
	}
	if cityUsesDoltliteBeadsBackend(cityPath) {
		return nil
	}
	provider := beadsProvider(cityPath)
	if strings.HasPrefix(provider, "exec:") {
		release, err := acquireProviderSemaphoreForOp(cityPath, "start")
		if err != nil {
			return err
		}
		defer release()

		script := strings.TrimPrefix(provider, "exec:")
		providerEnv, envErr := providerLifecycleProcessEnvWithError(cityPath, provider)
		if envErr != nil {
			return envErr
		}
		// bd's proxied-server provider owns the dolt server lifecycle; "start"
		// is a no-op there and there is no gascity-managed dolt runtime state to
		// publish. Firing it preserves the provider contract.
		if err := runProviderOpWithEnv(script, providerEnv, "start"); err != nil {
			return err
		}
	}
	return nil
}

// shutdownBeadsProvider stops the bead store's backing service.
// Called by gc stop after agents have been terminated.
// For exec providers, fires "stop". For file providers, always available.
func shutdownBeadsProvider(cityPath string) error {
	if cityUsesBdStoreContract(cityPath) && gcDoltSkip() {
		return nil
	}
	if cityUsesDoltliteBeadsBackend(cityPath) {
		return nil
	}
	// An externally-pinned dolt endpoint (its own config carries coords, e.g. a
	// hosted beads-gateway or an operator-run loopback server) is not a
	// gc-managed local lifecycle: gascity connects to it but never owns its
	// teardown, so skip the provider "stop" op — even for a loopback host.
	if target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityPath, cityPath); err == nil && target.External {
		return nil
	}
	provider := beadsProvider(cityPath)
	if strings.HasPrefix(provider, "exec:") {
		script := strings.TrimPrefix(provider, "exec:")
		providerEnv, err := providerLifecycleProcessEnvWithError(cityPath, provider)
		if err != nil {
			return err
		}
		// bd's proxied-server provider owns teardown: "stop" runs `bd dolt stop`,
		// which tears down the proxy + child dolt. gascity keeps no managed
		// runtime state to clear.
		if err := runProviderOpWithEnv(script, providerEnv, "stop"); err != nil {
			return err
		}
	}
	return nil
}

// initBeadsForDir initializes bead store infrastructure in a directory.
// Idempotent — skips if already initialized. Callers should use
// initAndHookDir instead to ensure hooks are installed afterward.
//
// Every load-bearing exec path that invokes bd init locally ensures
// BEADS_DIR=<dir>/.beads. bd init creates a .git/ as a side effect when
// BEADS_DIR is unset (upstream gastownhall/beads cmd/bd/init.go), so generic
// exec providers get the scope's bead directory in the subprocess env and
// providers that run bd init elsewhere (for example gc-beads-k8s inside the
// pod) must set it in their own wrapper before invoking bd init.
func initBeadsForDir(cityPath, dir, prefix, doltDatabase string) error {
	return initBeadsForDirWithExecutor(cityPath, dir, prefix, doltDatabase, runProviderOpWithEnv)
}

type providerOpExecutor func(script string, environ []string, args ...string) error

func initBeadsForDirWithExecutor(cityPath, dir, prefix, doltDatabase string, execute providerOpExecutor) error {
	if cityUsesBdStoreContract(cityPath) && gcDoltSkip() {
		if err := seedDeferredManagedBeadsErr(cityPath, dir, prefix, doltDatabase); err != nil {
			return err
		}
		return nil
	}
	provider := beadsProvider(cityPath)
	if provider == "file" {
		return initFileStoreForDir(cityPath, dir)
	}
	if strings.HasPrefix(provider, "exec:") {
		args := []string{"init", dir, prefix}
		if strings.TrimSpace(doltDatabase) != "" {
			args = append(args, doltDatabase)
		}
		script := strings.TrimPrefix(provider, "exec:")
		if execProviderUsesCanonicalBdScopeFiles(provider) && cityUsesDoltliteBeadsBackend(cityPath) {
			env, err := providerLifecycleProcessEnvWithError(cityPath, provider)
			if err != nil {
				return err
			}
			if err := execute(script, env, args...); err != nil {
				if isBdAlreadyInitializedError(err) {
					return nil
				}
				return err
			}
			return nil
		}
		if execProviderUsesCanonicalBdScopeFiles(provider) && !execProviderNeedsScopedDoltInit(provider) {
			baseEnv, err := providerLifecycleProcessEnvForScopeInitWithError(cityPath, dir, provider)
			if err != nil {
				return err
			}
			overrides := map[string]string{
				"BEADS_DIR": filepath.Join(dir, ".beads"),
			}
			canonicalDoltDatabase := strings.TrimSpace(doltDatabase)
			if canonicalDoltDatabase == "" {
				canonicalDoltDatabase = canonicalScopeDoltDatabase(cityPath, dir, prefix)
			}
			if strings.TrimSpace(canonicalDoltDatabase) != "" {
				args = []string{"init", dir, prefix, canonicalDoltDatabase}
			}
			if strings.TrimSpace(cityPath) != "" {
				overrides["GC_PACK_STATE_DIR"] = citylayout.PackStateDir(cityPath, "dolt")
				if err := applyCanonicalScopeInitDoltEnv(overrides, cityPath, dir); err != nil {
					return err
				}
			}
			env := overlayEnvEntries(baseEnv, overrides)
			if err := execute(script, env, args...); err != nil {
				if isBdAlreadyInitializedError(err) {
					return nil
				}
				if shouldRetryExecBdInit(err) {
					for attempt := 0; attempt < 3; attempt++ {
						time.Sleep(time.Second)
						retryErr := execute(script, env, args...)
						if retryErr == nil {
							return nil
						}
						if !shouldRetryExecBdInit(retryErr) {
							return retryErr
						}
						err = retryErr
					}
				}
				return err
			}
			return nil
		}
		if !execProviderNeedsScopedDoltInit(provider) {
			baseEnv, err := cityRuntimeProcessEnvWithError(cityPath)
			if err != nil {
				return err
			}
			if strings.TrimSpace(cityPath) == "" {
				baseEnv = os.Environ()
			}
			env := overlayEnvEntries(baseEnv, map[string]string{
				"BEADS_DIR": filepath.Join(dir, ".beads"),
			})
			if err := execute(script, env, args...); err != nil {
				if shouldRetryExecBdInit(err) {
					for attempt := 0; attempt < 3; attempt++ {
						time.Sleep(time.Second)
						retryErr := execute(script, env, args...)
						if retryErr == nil {
							return nil
						}
						if !shouldRetryExecBdInit(retryErr) {
							return retryErr
						}
						err = retryErr
					}
				}
				return err
			}
			return nil
		}
		target, err := resolveConfiguredExecStoreTarget(cityPath, dir)
		if err != nil {
			return err
		}
		providerEnv, err := gcExecLifecycleInitProcessEnv(cityPath, target, provider)
		if err != nil {
			return err
		}
		return execute(script, providerEnv, args...)
	}
	if shouldInitDefaultRigBdStore(cityPath, dir, provider) {
		return initDefaultRigBdStore(cityPath, dir, prefix, doltDatabase)
	}
	return nil
}

func shouldInitDefaultRigBdStore(cityPath, dir, provider string) bool {
	if strings.TrimSpace(cityPath) == "" || strings.TrimSpace(dir) == "" {
		return false
	}
	if samePath(resolveStoreScopeRoot(cityPath, dir), resolveStoreScopeRoot(cityPath, cityPath)) {
		return false
	}
	provider = strings.TrimSpace(provider)
	return provider != "" && provider != "file" && !strings.HasPrefix(provider, "exec:") && !providerUsesBdStoreContract(provider)
}

func initDefaultRigBdStore(cityPath, dir, prefix, doltDatabase string) error {
	canonicalDoltDatabase := strings.TrimSpace(doltDatabase)
	if canonicalDoltDatabase == "" {
		canonicalDoltDatabase = canonicalScopeDoltDatabase(cityPath, dir, prefix)
	}
	env := map[string]string{
		"BEADS_DIR": filepath.Join(dir, ".beads"),
	}
	applyExportSuppressionEnv(env)
	args := []string{"init", "--server", "-p", prefix, "--skip-hooks"}
	if canonicalDoltDatabase != "" {
		args = append(args, "--database", canonicalDoltDatabase)
	}
	if _, err := beads.ExecCommandRunnerWithEnv(env)(dir, "bd", args...); err != nil {
		if isBdAlreadyInitializedError(err) {
			return nil
		}
		return fmt.Errorf("bd init: %w", err)
	}
	return nil
}

//nolint:unparam // error slot preserves the resolver-shaped contract

func initFileStoreForDir(cityPath, dir string) error {
	if !fileStoreUsesScopedRoots(cityPath) {
		return nil
	}
	return ensurePersistedScopeLocalFileStore(dir)
}

type healthyManagedRuntimePublicationDeps struct {
	currentPort     func(string) string
	lifecycleOwned  func(string) (bool, error)
	publishIfOwned  func(string) error
	waitScopesReady func(string, time.Duration) error
}

// healthBeadsProvider checks the bead store's backing service health.
// For exec providers, fires the "health" operation. For bd (dolt), runs
// a three-layer health check and attempts recovery on failure. For file
// provider, always healthy (no-op).
//
// Acquires a per-city semaphore to prevent concurrent health/recovery
// operations from causing a thundering herd when dolt bounces.
func healthBeadsProvider(cityPath string) error {
	return healthBeadsProviderContext(context.Background(), cityPath, true)
}

// healthBeadsProviderContext is healthBeadsProvider with a caller-owned
// deadline. Native read reconnects skip the all-scope readiness barrier: their
// immediately following OpenNativeStorage call is the scoped readiness check
// and already shares this context.
func healthBeadsProviderContext(ctx context.Context, cityPath string, waitForScopes bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cityUsesBdStoreContract(cityPath) && gcDoltSkip() {
		return nil
	}
	if cityUsesDoltliteBeadsBackend(cityPath) {
		return nil
	}
	provider := beadsProvider(cityPath)
	if strings.HasPrefix(provider, "exec:") {
		release, err := acquireProviderSemaphoreForOpContext(ctx, cityPath, "health")
		if err != nil {
			return err
		}
		defer release()

		script := strings.TrimPrefix(provider, "exec:")
		providerEnv, err := providerLifecycleProcessEnvWithError(cityPath, provider)
		if err != nil {
			return err
		}
		// "health" is a bd-level liveness probe. On failure, fire the provider's
		// "recover" op so the proxy brings the child dolt back. For a bd-contract
		// provider the recover is rate-limited per city (providerRecoverCooldown)
		// so a patrol tick storm cannot re-trip the bd circuit breaker.
		healthErr := runProviderOpWithEnvContext(ctx, script, providerEnv, "health")
		if healthErr == nil {
			return nil
		}
		// An externally-pinned dolt endpoint (its own config carries coords) is
		// not a gc-managed local lifecycle: gascity connects to it but never owns
		// its recovery, so surface the health failure directly instead of firing a
		// "recover" op — even for a loopback host.
		if target, terr := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityPath, cityPath); terr == nil && target.External {
			return healthErr
		}
		if providerUsesBdStoreContract(provider) {
			cityKey := normalizePathForCompare(cityPath)
			now := providerRecoverNow()
			if v, loaded := lastBeadsProviderRecover.Load(cityKey); loaded {
				if last, ok := v.(time.Time); ok && now.Sub(last) < providerRecoverCooldown() {
					return healthErr
				}
			}
			lastBeadsProviderRecover.Store(cityKey, now)
		}
		if recErr := runProviderOpWithEnvContext(ctx, script, providerEnv, "recover"); recErr != nil {
			return fmt.Errorf("unhealthy (%w) and recovery failed: %w", healthErr, recErr)
		}
		return nil
	}
	return nil // file: always healthy
}

// isExternalDolt returns true when the city uses an explicitly configured
// (user-managed) Dolt server rather than the managed local one.
//
// Checks canonical city .beads config first, then falls back to deprecated
// city.toml-derived registration only when the canonical file does not exist.
// Env vars remain explicit per-process overrides for non-controller paths.
// With canonical or compat config, any explicit host or port means
// "user-managed" regardless of whether the host resolves to localhost.
// Without config, the env-var fallback excludes localhost addresses for
// backwards compatibility.
func isExternalDolt(cityPath string) bool {
	_, _, ok, _ := resolveConfiguredCityDoltTarget(cityPath)
	return ok
}

// doltHostForCity returns the effective Dolt host for a city.
// Canonical or compat-configured targets win over ambient env so child
// processes stay aligned with the resolved city endpoint. Env-only host
// overrides remain a last-resort fallback when no configured target exists.
func doltHostForCity(cityPath string) string {
	host, _, ok, _ := resolveConfiguredCityDoltTarget(cityPath)
	if !ok {
		return ""
	}
	return host
}

// doltPortForCity returns the effective Dolt port for a city.
// Canonical or compat-configured targets win over ambient env so child
// processes stay aligned with the resolved city endpoint. Env-only port
// overrides remain a last-resort fallback when no configured target exists.
func doltPortForCity(cityPath string) string {
	_, port, ok, _ := resolveConfiguredCityDoltTarget(cityPath)
	if !ok {
		return ""
	}
	return port
}

func configuredCityDoltTarget(cityPath string) (string, string, bool) {
	host, port, ok, _ := resolveConfiguredCityDoltTarget(cityPath)
	return host, port, ok
}

func resolveConfiguredCityDoltTarget(cityPath string) (string, string, bool, bool) {
	cityPath = normalizePathForCompare(cityPath)
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, cityPath, "")
	if err != nil {
		var invalid *contract.InvalidCanonicalConfigError
		if errors.As(err, &invalid) {
			return "", "", false, true
		}
		return "", "", false, false
	}
	if resolved.Kind == contract.ScopeConfigAuthoritative {
		if contract.ConfigHasEndpointAuthority(resolved.State) {
			return canonicalExternalHost(resolved.State.DoltHost, resolved.State.DoltPort), strings.TrimSpace(resolved.State.DoltPort), true, false
		}
		return "", "", false, false
	}
	// Missing / legacy-minimal config is a local bd-owned scope; the external
	// endpoint (if any) lives only in canonical .beads/config.yaml, never in
	// city.toml.
	return "", "", false, false
}

type doltRuntimeState struct {
	Running   bool   `json:"running"`
	PID       int    `json:"pid"`
	Port      int    `json:"port"`
	DataDir   string `json:"data_dir"`
	StartedAt string `json:"started_at"`
}

// currentDoltPort returns the controller-managed Dolt port for the city.
// Published runtime state is preferred; valid provider state is accepted while
// publication catches up so the raw-bd compatibility mirror does not get
// removed during a live managed-Dolt window.
// .beads/dolt-server.port is a compatibility mirror for raw bd, not a GC
// control-plane input.

// writeDoltPortFile writes the managed Dolt port into dir/.beads/dolt-server.port.
// When the existing file contains a non-empty port different from the one being
// written, a WARN line naming scopeLabel and both ports is emitted on warn so
// operators can see that their on-disk port file is being reconciled to the
// canonical managed port. scopeLabel may be empty for silent callers; warn may
// be nil or io.Discard to suppress warnings entirely.


//nolint:unparam // keep fs seam for future testable FS injection

//nolint:unparam // keep fs seam for future testable FS injection

// normalizeCanonicalBdScopeFiles reconciles canonical bd metadata/config/port
// mirrors under the city and each rig. warn receives operator-visible WARN
// lines when port-file rewrites change on-disk contents (pass io.Discard to
// suppress, or a stderr writer from the caller to show them). When omitted,
// warning output is suppressed.

// syncConfiguredDoltPortFiles reconciles each scope's .beads/dolt-server.port
// compatibility mirror with the canonical managed-city Dolt port. When warn is
// non-nil, a WARN line is emitted for every port file whose prior non-empty
// contents disagreed with the canonical port (operator-visible signal that gc
// is overriding a rig-local or stale port). Pass io.Discard to suppress.

func normalizedRigConfig(cityPath string, rig config.Rig) config.Rig {
	if !filepath.IsAbs(rig.Path) {
		rig.Path = filepath.Join(cityPath, rig.Path)
	}
	return rig
}

func wrapInvalidEndpointStateError(scope string, err error) error {
	var invalid *contract.InvalidCanonicalConfigError
	if !errors.As(err, &invalid) {
		return err
	}
	switch scope {
	case "city":
		return fmt.Errorf("invalid canonical city endpoint state in %s: %w", invalid.Path, invalid.Err)
	case "rig":
		return fmt.Errorf("invalid canonical rig endpoint state in %s: %w", invalid.Path, invalid.Err)
	default:
		return err
	}
}

func desiredCityDoltConfigState(_ string, _ config.DoltConfig, cityPrefix string) contract.ConfigState {
	// A city with no canonical external endpoint is local (bd owns the proxied
	// server). External endpoints are pinned only in .beads/config.yaml, never
	// derived from city.toml.
	return contract.ConfigState{IssuePrefix: cityPrefix}
}

func desiredRigDoltConfigState(cityPath string, rig config.Rig, cityState contract.ConfigState) contract.ConfigState {
	// A rig's external endpoint is pinned only in its own .beads/config.yaml; a
	// rig with none is local (inherits the city's config).
	rig = normalizedRigConfig(cityPath, rig)
	return inheritedRigDoltConfigState(rig.Path, rig.EffectivePrefix(), cityState)
}

func inheritedRigDoltConfigState(_, prefix string, cityState contract.ConfigState) contract.ConfigState {
	state := contract.ConfigState{IssuePrefix: prefix}
	if contract.ConfigHasEndpointAuthority(cityState) {
		state.DoltHost = cityState.DoltHost
		state.DoltPort = cityState.DoltPort
		state.DoltUser = strings.TrimSpace(cityState.DoltUser)
	}
	return state
}

func canonicalExternalHost(host, port string) string {
	host = strings.TrimSpace(host)
	if host == "" && strings.TrimSpace(port) != "" {
		return "127.0.0.1"
	}
	return host
}

func preservedDoltUser(dir string, want contract.ConfigState) string {
	existing, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(dir, ".beads", "config.yaml"))
	if err != nil || !ok {
		return ""
	}
	// Preserve an existing external dolt.user when the config already points at
	// the same external endpoint we are canonicalizing.
	if strings.TrimSpace(want.DoltHost) != "" || strings.TrimSpace(want.DoltPort) != "" {
		if strings.TrimSpace(existing.DoltPort) == strings.TrimSpace(want.DoltPort) && canonicalExternalHost(existing.DoltHost, existing.DoltPort) == canonicalExternalHost(want.DoltHost, want.DoltPort) {
			return strings.TrimSpace(existing.DoltUser)
		}
	}
	return ""
}

// runProviderProbe runs a "probe" operation against an exec beads script.
// Returns true if the backing service is available (exit 0), false if not
// available (exit 2) or on any error. Unlike runProviderOp, exit 2 means
// "not running" rather than "not needed."
func runProviderProbe(script, cityPath, provider string) bool {
	ctx, cancel := providerLifecycleContext(context.Background(), providerProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, script, "probe")
	cmd.WaitDelay = 2 * time.Second
	prepareProviderOpCommand(cmd)
	if cityPath != "" {
		env, err := providerLifecycleProcessEnvWithError(cityPath, provider)
		if err != nil {
			return false
		}
		cmd.Env = env
	}
	return cmd.Run() == nil
}

func providerLifecycleDoltPathEnv(cityPath string) []string {
	cityPath = normalizePathForCompare(cityPath)
	packStateDir := citylayout.PackStateDir(cityPath, "dolt")
	dataDir := filepath.Join(cityPath, ".beads", "dolt")
	return []string{
		"GC_PACK_STATE_DIR=" + packStateDir,
		"GC_BEADS_DATA_DIR=" + dataDir,
		"GC_BEADS_LOG_FILE=" + filepath.Join(packStateDir, "dolt.log"),
		"GC_BEADS_STATE_FILE=" + filepath.Join(packStateDir, "dolt-provider-state.json"),
		"GC_BEADS_PID_FILE=" + filepath.Join(packStateDir, "dolt.pid"),
		"GC_BEADS_LOCK_FILE=" + filepath.Join(packStateDir, "dolt.lock"),
		"GC_BEADS_CONFIG_FILE=" + filepath.Join(packStateDir, "dolt-config.yaml"),
	}
}

func providerLifecycleProcessEnvWithError(cityPath, provider string) ([]string, error) {
	if strings.TrimSpace(cityPath) == "" {
		return nil, nil
	}
	cityPath = normalizePathForCompare(cityPath)
	env, err := cityRuntimeProcessEnvWithError(cityPath)
	if err != nil {
		return nil, err
	}
	return providerLifecycleProcessEnvFromBase(cityPath, provider, env), nil
}

func providerLifecycleProcessEnvForScopeInitWithError(cityPath, scopeRoot, provider string) ([]string, error) {
	env, err := providerLifecycleProcessEnvWithError(cityPath, provider)
	if err == nil {
		if providerUsesBdStoreContract(provider) && scopeRuntimeEnvIndependentOfCityProjection(cityPath, scopeRoot) {
			env = providerLifecycleIndependentScopeInitEnv(cityPath, scopeRoot, env)
		}
		return env, nil
	}
	if !providerUsesBdStoreContract(provider) || !cityPostgresProjectionErrorCanBeBypassed(cityPath, err) || !scopeRuntimeEnvIndependentOfCityProjection(cityPath, scopeRoot) {
		return nil, err
	}
	cityPath = normalizePathForCompare(cityPath)
	overrides := cityRuntimeEnvMapForCity(cityPath)
	setExecProjectedBackendEnvEmpty(overrides)
	overrides["BEADS_DOLT_AUTO_START"] = "0"
	applyLegacyRigScopeInitDoltEnv(overrides, cityPath, scopeRoot)
	baseEnv := mergeRuntimeEnv(os.Environ(), overrides)
	return providerLifecycleProcessEnvFromBase(cityPath, provider, baseEnv), nil
}

func providerLifecycleIndependentScopeInitEnv(cityPath, scopeRoot string, env []string) []string {
	cityPath = normalizePathForCompare(cityPath)
	overrides := map[string]string{}
	applyLegacyRigScopeInitDoltEnv(overrides, cityPath, scopeRoot)
	ensureProjectedPostgresEnvExplicit(overrides)
	return overlayEnvEntries(env, overrides)
}

func scopeRuntimeEnvIndependentOfCityProjection(cityPath, scopeRoot string) bool {
	if strings.TrimSpace(cityPath) == "" || samePath(cityPath, scopeRoot) {
		return false
	}
	var explicitRig *config.Rig
	if cfg, err := loadCityConfig(cityPath, io.Discard); err == nil && cfg != nil {
		explicitRig = rigConfigForScopeRoot(cityPath, scopeRoot, cfg.Rigs)
	}
	return rigRuntimeEnvIndependentOfCityProjection(cityPath, scopeRoot, explicitRig)
}

func applyLegacyRigScopeInitDoltEnv(env map[string]string, _, scopeRoot string) {
	// Legacy explicit-endpoint rigs resolve their endpoint through bd's
	// proxied-server mode now (bd reads the rig's .beads/), so gascity threads
	// only auth (external credential), never a server host/port.
}

func providerLifecycleProcessEnvFromBase(cityPath, provider string, env []string) []string {
	if !providerUsesBdStoreContract(provider) {
		return env
	}
	if cityUsesDoltliteBeadsBackend(cityPath) {
		env = removeEnvKey(env, "GC_BEADS_BACKEND")
		env = removeEnvKey(env, "BEADS_BACKEND")
		env = append(env, "GC_BEADS_BACKEND=doltlite", "BEADS_BACKEND=doltlite")
		envMap := runtimeEnvEntriesToMap(env)
		clearProjectedDoltEnv(envMap)
		clearProjectedPostgresEnv(envMap)
		return mergeRuntimeEnv(nil, envMap)
	}
	for _, key := range []string{
		"GC_PACK_STATE_DIR",
		"GC_BEADS_DATA_DIR",
		"GC_BEADS_LOG_FILE",
		"GC_BEADS_STATE_FILE",
		"GC_BEADS_PID_FILE",
		"GC_BEADS_LOCK_FILE",
		"GC_BEADS_CONFIG_FILE",
		"GC_BEADS_ARCHIVE_LEVEL",
		"GC_BEADS_AUTO_GC_ENABLED",
		"GC_BEADS_MAX_CONNECTIONS",
		"GC_BEADS_READ_TIMEOUT_MILLIS",
		"GC_BEADS_WRITE_TIMEOUT_MILLIS",
		"GC_BEADS_LOCK_RELEASE_TIMEOUT_MS",
	} {
		env = removeEnvKey(env, key)
	}
	env = append(env, providerLifecycleDoltPathEnv(cityPath)...)
	if gcBin := resolveProviderLifecycleGCBinary(); gcBin != "" {
		env = removeEnvKey(env, "GC_BIN")
		env = append(env, "GC_BIN="+gcBin)
	}
	// Propagate archive_level from city config so the managed dolt
	// server inherits it without shell-script changes.
	if v, ok := cityDoltConfigs.Load(cityPath); ok {
		dc, _ := v.(config.DoltConfig)
		if dc.ArchiveLevel != nil {
			env = append(env, fmt.Sprintf("GC_BEADS_ARCHIVE_LEVEL=%d", *dc.ArchiveLevel))
		}
		if dc.AutoGCEnabled != nil {
			env = append(env, fmt.Sprintf("GC_BEADS_AUTO_GC_ENABLED=%t", *dc.AutoGCEnabled))
		}
		if dc.MaxConnections > 0 {
			env = append(env, fmt.Sprintf("GC_BEADS_MAX_CONNECTIONS=%d", dc.MaxConnections))
		}
		if dc.ReadTimeoutMillis > 0 {
			env = append(env, fmt.Sprintf("GC_BEADS_READ_TIMEOUT_MILLIS=%d", dc.ReadTimeoutMillis))
		}
		if dc.WriteTimeoutMillis > 0 {
			env = append(env, fmt.Sprintf("GC_BEADS_WRITE_TIMEOUT_MILLIS=%d", dc.WriteTimeoutMillis))
		}
		// An explicit "0s" is meaningful (probe once, no wait), so gate on
		// field presence rather than a non-zero duration.
		if dc.DoltLockReleaseTimeout != "" {
			env = append(env, fmt.Sprintf("GC_BEADS_LOCK_RELEASE_TIMEOUT_MS=%d", dc.DoltLockReleaseTimeoutDuration().Milliseconds()))
		}
	}
	// `gc start` runs in the user's shell, which doesn't see vars set
	// only via `launchctl setenv` — those live in launchd's domain.
	// Fall back to launchctl-getenv so the managed dolt server's log
	// level honors `launchctl setenv GC_BEADS_LOGLEVEL` even when the
	// shell hasn't `export`ed it. The supervisor's reconcile path
	// runs the same lookup; either source delivers the value.
	const loglevelPrefix = "GC_BEADS_LOGLEVEL="
	loglevelInEnv := false
	for _, entry := range env {
		if strings.HasPrefix(entry, loglevelPrefix) {
			loglevelInEnv = true
			break
		}
	}
	if !loglevelInEnv {
		if val := providerLifecycleLaunchctlGetenv("GC_BEADS_LOGLEVEL"); val != "" {
			env = append(env, loglevelPrefix+val)
		}
	}
	return env
}

func runtimeEnvEntriesToMap(environ []string) map[string]string {
	out := make(map[string]string, len(environ))
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			out[key] = value
		}
	}
	return out
}

// acquireProviderSemaphore returns a per-city semaphore channel and waits
// until a slot is available or ctx is canceled. Call the returned function to
// release. Semaphore entries intentionally live for the process lifetime:
// deleting an entry while a lifecycle operation is still running would allow a
// second channel for the same city and break serialization. The map is bounded
// by city roots seen by this controller process.
// This serializes lifecycle operations per city to prevent thundering herd
// when dolt bounces: without this, concurrent health checks all trigger
// recovery simultaneously, spawning a storm of processes that overwhelm
// dolt on restart.
func acquireProviderSemaphore(ctx context.Context, cityPath string) (func(), error) {
	cityPath = normalizePathForCompare(cityPath)
	v, _ := providerOpSemaphores.LoadOrStore(cityPath, make(chan struct{}, 1))
	sem := v.(chan struct{})
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for provider lifecycle slot for %q: %w", cityPath, ctx.Err())
	}
}

func acquireProviderSemaphoreForOp(cityPath, op string) (func(), error) {
	return acquireProviderSemaphoreForOpContext(context.Background(), cityPath, op)
}

func acquireProviderSemaphoreForOpContext(parent context.Context, cityPath, op string) (func(), error) {
	ctx, cancel := providerLifecycleContext(parent, providerOpTimeout(op))
	release, err := acquireProviderSemaphore(ctx, cityPath)
	if err != nil {
		cancel()
		return nil, err
	}
	return func() {
		release()
		cancel()
	}, nil
}

// providerOpTimeout returns the context timeout for a given lifecycle
// operation. The "start", "recover", and "init" operations get a longer
// timeout: dolt server startup can take 30+ seconds for large data dirs, and
// initializing a rig's bead store can likewise exceed 30s when it creates or
// migrates a database on a busy shared dolt server. Under the old 30s budget,
// init of an existing-but-unmigrated rig DB during a config reload was
// SIGKILLed, leaving the supervisor "keeping old config" so newly configured
// rigs never came online. All other operations use 30s.
var providerOpTimeout = func(op string) time.Duration {
	switch op {
	case "start", "recover", "init":
		return 120 * time.Second
	default:
		return 30 * time.Second
	}
}

// runProviderOp runs a lifecycle operation against an exec beads script.
// Exit 2 = not needed (treated as success, no-op). Used for start,
// init, health, recover, and stop operations.
// cityPath is exported via the canonical city runtime env so scripts can
// locate the city root and runtime directories.
func runProviderOp(script, cityPath string, args ...string) error {
	if cityPath == "" {
		return runProviderOpWithEnv(script, nil, args...)
	}
	env, err := cityRuntimeProcessEnvWithError(cityPath)
	if err != nil {
		return err
	}
	return runProviderOpWithEnv(script, env, args...)
}

func runProviderOpWithEnv(script string, environ []string, args ...string) error {
	return runProviderOpWithEnvContext(context.Background(), script, environ, args...)
}

func runProviderOpWithEnvContext(parent context.Context, script string, environ []string, args ...string) error {
	op := ""
	if len(args) > 0 {
		op = args[0]
	}
	ctx, cancel := providerLifecycleContext(parent, providerOpTimeout(op))
	defer cancel()

	cmd := exec.CommandContext(ctx, script, args...)
	cmd.WaitDelay = 2 * time.Second
	prepareProviderOpCommand(cmd)
	if len(environ) > 0 {
		cmd.Env = environ
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("exec beads %s: %w", args[0], ctxErr)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
			return nil // Not needed
		}
		// Detect missing script or missing dolt binary.
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("exec beads %s: provider script not found (%s); run \"gc doctor\" for diagnostics", args[0], script)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("exec beads %s: %s", args[0], msg)
	}
	return nil
}
