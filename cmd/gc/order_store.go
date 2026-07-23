package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
)

type (
	orderStoreResolver  func(orders.Order) (beads.OrdersStore, error)
	orderStoresResolver func(orders.Order) ([]beads.OrdersStore, error)
)

// orderFrontDoorsForStores wraps a federation of raw stores (the dispatcher's
// city + rig scopes) as order front doors for the mixed orders+graph reads
// (LastRunAcross / CursorAcross). Each store is used as BOTH the orders leg and
// the graph leg: on a single-store city the order-tracking beads and the
// wisp/molecule roots are colocated, so the two legs wrap one store and the
// union deduplicates to a single read — byte-identical to the pre-split behavior.
// Under a graph-store split the dispatcher's per-scope store resolution would
// supply a distinct graph leg; that resolution is a separate concern from this
// front-door construction.
func orderFrontDoorsForStores(stores []beads.Store) []*orders.Store {
	out := make([]*orders.Store, 0, len(stores))
	for _, s := range stores {
		out = append(out, orders.NewStoreWithGraph(beads.OrdersStore{Store: s}, beads.GraphStore{Store: s}))
	}
	return out
}

// orderFrontDoorsForTypedStores is orderFrontDoorsForStores over already
// class-typed orders stores (the per-order resolution outputs), preserving the
// same orders-leg/graph-leg pairing.
func orderFrontDoorsForTypedStores(stores []beads.OrdersStore) []*orders.Store {
	out := make([]*orders.Store, 0, len(stores))
	for _, s := range stores {
		out = append(out, orders.NewStoreWithGraph(s, beads.GraphStore(s)))
	}
	return out
}

// rawOrderStores returns the underlying stores of a typed orders-store slice for
// the ONE remaining raw federated read: bdCursorAcrossStores, which reads the
// order:<name> event-cursor labels the dispatcher stamps on wisp/molecule roots
// (a graph-class residual read, tracked separately from the typed Cursor path).
// The typed LastRun/Cursor reads no longer unwrap — they take order front doors.
func rawOrderStores(stores []beads.OrdersStore) []beads.Store {
	out := make([]beads.Store, len(stores))
	for i, s := range stores {
		out[i] = s.Store
	}
	return out
}

type orderTrackingSweepTarget struct {
	target execStoreTarget
	label  string
}

// orderTrackingSweepScopedStore wraps one store in the multi-scope order-tracking
// sweep (city + every rig) with its scope label and dedup key. The sweep is a
// federated READ/close across heterogeneous scopes — the orders federated-read
// exception (analogous to the by-id sweep) — so it carries a bare beads.Store
// rather than a beads.OrdersStore: the sweep iterates a []beads.Store and
// recovers each store's label/key via a structural type assertion
// (orderTrackingSweepStoreKey / orderTrackingSweepStoreLabel), which would not
// promote through the OrdersStore wrapper. Per-order resolution (openOrderStoreForOrder,
// cachedOrderStoresResolver) IS strongly typed as beads.OrdersStore; only this
// federated sweep enumeration stays beads.Store by design.
type orderTrackingSweepScopedStore struct {
	beads.Store
	label string
	key   string
}

func (s orderTrackingSweepScopedStore) orderTrackingSweepLabel() string {
	return s.label
}

func (s orderTrackingSweepScopedStore) orderTrackingSweepKey() string {
	return s.key
}

func openCityOrderStore(stderr io.Writer, cmdName string) (beads.OrdersStore, int) {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return beads.OrdersStore{}, 1
	}
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err)                   //nolint:errcheck // best-effort stderr
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck // best-effort stderr
		return beads.OrdersStore{}, 1
	}
	return beads.OrdersStore{Store: store}, 0
}

func openOrderStoreForOrder(cityPath string, cfg *config.City, a orders.Order, stderr io.Writer, cmdName string) (beads.OrdersStore, int) {
	target, err := resolveOrderStoreTarget(cityPath, cfg, a)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return beads.OrdersStore{}, 1
	}
	store, err := openStoreAtForCity(target.ScopeRoot, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err)                   //nolint:errcheck // best-effort stderr
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck // best-effort stderr
		return beads.OrdersStore{}, 1
	}
	return beads.OrdersStore{Store: store}, 0
}

func resolveOrderStoreTarget(cityPath string, cfg *config.City, a orders.Order) (execStoreTarget, error) {
	if strings.TrimSpace(a.Rig) == "" {
		prefix := ""
		if cfg != nil {
			prefix = config.EffectiveHQPrefix(cfg)
		}
		return execStoreTarget{ScopeRoot: cityPath, ScopeKind: "city", Prefix: prefix}, nil
	}
	if cfg == nil {
		return execStoreTarget{}, fmt.Errorf("rig-scoped order %q requires city config", a.ScopedName())
	}
	if strings.TrimSpace(a.Pool) != "" {
		pool, err := qualifyOrderPool(a, cfg)
		if err != nil {
			return execStoreTarget{}, err
		}
		if !strings.Contains(pool, "/") {
			return execStoreTarget{
				ScopeRoot: cityPath,
				ScopeKind: "city",
				Prefix:    config.EffectiveHQPrefix(cfg),
			}, nil
		}
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	rig, ok := rigByName(cfg, a.Rig)
	if !ok {
		return execStoreTarget{}, fmt.Errorf("rig %q not found in %s", a.Rig, filepath.Join(cityPath, "city.toml"))
	}
	if strings.TrimSpace(rig.Path) == "" {
		return execStoreTarget{}, fmt.Errorf("rig %q is declared but has no path binding — run `gc rig add <dir> --name %s` to bind it before dispatching rig-scoped orders", rig.Name, rig.Name)
	}
	return execStoreTarget{
		ScopeRoot: rig.Path,
		ScopeKind: "rig",
		Prefix:    rig.EffectivePrefix(),
		RigName:   rig.Name,
	}, nil
}

func resolveOrderExecTarget(cityPath string, cfg *config.City, a orders.Order) (execStoreTarget, error) {
	return resolveOrderStoreTarget(cityPath, cfg, a)
}

func orderStoreTargetKey(target execStoreTarget) string {
	return target.ScopeKind + "\x00" + filepath.Clean(target.ScopeRoot)
}

func orderExecEnvWithError(cityPath string, cfg *config.City, target execStoreTarget, a orders.Order, vars map[string]string) ([]string, error) {
	if err := validateOrderExecEnvOverrides(a); err != nil {
		return nil, err
	}
	var env map[string]string
	var err error
	if target.ScopeKind == "rig" {
		env, err = bdRuntimeEnvForRigWithError(cityPath, cfg, target.ScopeRoot)
	} else {
		env, err = bdRuntimeEnvWithError(cityPath)
		env["BEADS_DIR"] = filepath.Join(target.ScopeRoot, ".beads")
	}
	if err != nil {
		return nil, fmt.Errorf("building order env for %s: %w", a.ScopedName(), err)
	}
	env["GC_STORE_ROOT"] = target.ScopeRoot
	env["GC_STORE_SCOPE"] = target.ScopeKind
	env["GC_BEADS_PREFIX"] = target.Prefix
	// Tag every bd interaction this exec order produces with the order's
	// name so audit logs and the dashboard can attribute housekeeping
	// activity to the responsible order rather than an ambient identity.
	if name := strings.TrimSpace(a.Name); name != "" {
		env["BEADS_ACTOR"] = "order:" + name
	}
	if target.ScopeKind == "rig" {
		env["GC_RIG"] = target.RigName
		env["GC_RIG_ROOT"] = target.ScopeRoot
	} else {
		env["GC_RIG"] = ""
		env["GC_RIG_ROOT"] = ""
	}
	if a.Source != "" {
		env["ORDER_DIR"] = filepath.Dir(a.Source)
	}
	if a.FormulaLayer != "" {
		packDir := filepath.Dir(a.FormulaLayer)
		env["PACK_DIR"] = packDir
		env["GC_PACK_DIR"] = packDir

		packName := filepath.Base(packDir)
		if packName != "." && packName != string(filepath.Separator) {
			env["GC_PACK_NAME"] = packName
			env["GC_PACK_STATE_DIR"] = citylayout.PackStateDir(cityPath, packName)
		}
	}
	if a.Rig != "" && target.RigName == "" {
		env["GC_RIG"] = a.Rig
	}
	applyOrderExecCanonicalDoltEnv(cityPath, target.ScopeRoot, env)
	ensureProjectedDoltEnvExplicit(env)
	ensureProjectedPostgresEnvExplicit(env)
	// Carry the controller's GitHub CLI auth token into the exec order so its
	// `gh` calls authenticate. Projected before the [order.env] loop below so an
	// order can still scope its own GH_TOKEN; see projectGitHubTokenExecEnv.
	projectGitHubTokenExecEnv(env)
	// Order-supplied [order.env] entries take effect last so they can tune
	// non-controller thresholds (e.g. raising GC_DOCTOR_LATENCY_WARN_S for a
	// noisy city) without editing the order's shell scripts or the parent
	// process environment.
	for k, v := range a.Env {
		env[k] = v
	}
	// Dispatch-time vars (webhook rule args / `gc order run --var`) overlay
	// last so the args channel reaches an exec order's process. Reserved-key
	// namespacing/guarding for these dynamic vars is deferred (see the design's
	// R4); the static [order.env] guard above is unchanged.
	for k, v := range vars {
		env[k] = v
	}
	return mergeRuntimeEnv(nil, env), nil
}

func validateOrderExecEnvOverrides(a orders.Order) error {
	return orders.ValidateExecEnvOverrides(a)
}

func isReservedOrderExecEnvKey(key string) bool {
	return orders.IsReservedExecEnvKey(key)
}

func orderTriggerOptions(cityPath string, cfg *config.City, a orders.Order) (orders.TriggerOptions, error) {
	if a.Trigger != "condition" || strings.TrimSpace(cityPath) == "" {
		return orders.TriggerOptions{}, nil
	}
	target, err := resolveOrderExecTarget(cityPath, cfg, a)
	if err != nil {
		return orders.TriggerOptions{}, err
	}
	return orderTriggerOptionsForTarget(cityPath, cfg, target, a)
}

func orderTriggerOptionsForTarget(cityPath string, cfg *config.City, target execStoreTarget, a orders.Order) (orders.TriggerOptions, error) {
	if a.Trigger != "condition" || strings.TrimSpace(cityPath) == "" {
		return orders.TriggerOptions{}, nil
	}
	env, err := orderExecEnvWithError(cityPath, cfg, target, a, nil)
	if err != nil {
		return orders.TriggerOptions{}, err
	}
	return orders.TriggerOptions{
		ConditionDir:     target.ScopeRoot,
		ConditionEnv:     env,
		ConditionTimeout: a.CheckTimeoutOrDefault(),
	}, nil
}

func applyOrderExecCanonicalDoltEnv(cityPath, scopeRoot string, env map[string]string) {
	if env == nil {
		return
	}
	if strings.TrimSpace(scopeRoot) == "" {
		scopeRoot = cityPath
	}
	if scopeBackendIsPostgres(cityPath, scopeRoot) {
		// Postgres env is threaded by the order/exec env builder itself; the dolt
		// projection has nothing to add for a postgres-backed scope.
		return
	}
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, scopeRoot, "")
	if err != nil || resolved.Kind == contract.ScopeConfigMissing {
		return
	}
	target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityPath, scopeRoot)
	if err != nil {
		return
	}
	if target.External {
		// External dolt: bd needs the canonical coords AND the resolved
		// credential to reach the endpoint.
		applyCanonicalDoltTargetEnv(env, target)
		mirrorBeadsDoltScopeEnv(env, target)
		applyCanonicalDoltAuthEnv(env, cityPath, scopeRoot, target)
		env["GC_BEADS_MANAGED_LOCAL"] = "0"
		return
	}
	// Local bd-managed dolt scope: bd owns the proxied local server, so strip any
	// stale ambient endpoint env and mark the scope managed-local so pack
	// commands can branch on it.
	clearProjectedDoltEnv(env)
	env["GC_BEADS_MANAGED_LOCAL"] = "1"
}

func cachedOrderStoresResolver(cityPath string, cfg *config.City) orderStoresResolver {
	stores := make(map[string]beads.Store)
	openCached := func(target execStoreTarget) (beads.Store, error) {
		key := orderStoreTargetKey(target)
		if store, ok := stores[key]; ok {
			return store, nil
		}
		store, err := openStoreAtForCity(target.ScopeRoot, cityPath)
		if err != nil {
			return nil, err
		}
		stores[key] = store
		return store, nil
	}
	return func(a orders.Order) ([]beads.OrdersStore, error) {
		target, err := resolveOrderStoreTarget(cityPath, cfg, a)
		if err != nil {
			return nil, err
		}
		primary, err := openCached(target)
		if err != nil {
			return nil, err
		}
		out := []beads.OrdersStore{{Store: primary}}
		if legacyOrderCityFallbackNeeded(cityPath, target) {
			legacy, err := openCached(legacyOrderCityTarget(cityPath, cfg))
			if err != nil {
				return nil, err
			}
			out = append(out, beads.OrdersStore{Store: legacy})
		}
		return out, nil
	}
}

func orderTrackingSweepTargetsForConfig(cityPath string, cfg *config.City) []orderTrackingSweepTarget {
	targets := []orderTrackingSweepTarget{{
		target: legacyOrderCityTarget(cityPath, cfg),
		label:  "city",
	}}
	if cfg != nil {
		resolveRigPaths(cityPath, cfg.Rigs)
		for _, rig := range cfg.Rigs {
			if strings.TrimSpace(rig.Path) == "" {
				continue
			}
			targets = append(targets, orderTrackingSweepTarget{
				target: execStoreTarget{
					ScopeRoot: rig.Path,
					ScopeKind: "rig",
					Prefix:    rig.EffectivePrefix(),
					RigName:   rig.Name,
				},
				label: fmt.Sprintf("rig %q", rig.Name),
			})
		}
	}
	return targets
}

func orderTrackingSweepStoresForConfigTargets(cityPath string, cfg *config.City, requiredTargets map[string][]string) ([]beads.Store, error) {
	targets := orderTrackingSweepTargetsForConfig(cityPath, cfg)
	if len(requiredTargets) > 0 {
		filtered := targets[:0]
		for _, target := range targets {
			if _, ok := requiredTargets[orderStoreTargetKey(target.target)]; ok {
				filtered = append(filtered, target)
			}
		}
		targets = filtered
	}
	return orderTrackingSweepStoresFromTargets(targets, func(sweepTarget orderTrackingSweepTarget) (beads.Store, error) {
		return openStoreAtForCity(sweepTarget.target.ScopeRoot, cityPath)
	})
}

func orderTrackingSweepStoresFromTargets(targets []orderTrackingSweepTarget, openStore func(orderTrackingSweepTarget) (beads.Store, error)) ([]beads.Store, error) {
	stores := make([]beads.Store, 0, len(targets))
	seen := make(map[string]struct{}, len(targets))
	var errs []error
	for _, sweepTarget := range targets {
		key := orderStoreTargetKey(sweepTarget.target)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		store, err := openStore(sweepTarget)
		if err != nil {
			errs = append(errs, fmt.Errorf("opening %s order store: %w", sweepTarget.label, err))
			continue
		}
		stores = append(stores, orderTrackingSweepScopedStore{
			Store: store,
			label: sweepTarget.label,
			key:   key,
		})
	}
	return stores, errors.Join(errs...)
}

func cachedOrderHistoryStoresResolver(cityPath string, cfg *config.City, stderr io.Writer) orderStoresResolver {
	stores := make(map[string]beads.Store)
	openCached := func(target execStoreTarget) (beads.Store, error) {
		key := orderStoreTargetKey(target)
		if store, ok := stores[key]; ok {
			return store, nil
		}
		store, err := openStoreAtForCity(target.ScopeRoot, cityPath)
		if err != nil {
			return nil, err
		}
		stores[key] = store
		return store, nil
	}
	return func(a orders.Order) ([]beads.OrdersStore, error) {
		target, err := resolveOrderStoreTarget(cityPath, cfg, a)
		if err != nil {
			return nil, err
		}
		primary, err := openCached(target)
		if err != nil {
			return nil, err
		}
		out := []beads.OrdersStore{{Store: primary}}
		if legacyOrderCityFallbackNeeded(cityPath, target) {
			legacy, err := openCached(legacyOrderCityTarget(cityPath, cfg))
			if err != nil {
				fmt.Fprintf(stderr, "gc order history: legacy city fallback unavailable for %s: %v\n", a.ScopedName(), err) //nolint:errcheck
				return out, nil
			}
			out = append(out, beads.OrdersStore{Store: legacy})
		}
		return out, nil
	}
}

func legacyOrderCityFallbackNeeded(cityPath string, target execStoreTarget) bool {
	return target.ScopeKind == "rig" && filepath.Clean(target.ScopeRoot) != filepath.Clean(cityPath)
}

func legacyOrderCityTarget(cityPath string, cfg *config.City) execStoreTarget {
	prefix := ""
	if cfg != nil {
		prefix = config.EffectiveHQPrefix(cfg)
	}
	return execStoreTarget{ScopeRoot: cityPath, ScopeKind: "city", Prefix: prefix}
}
