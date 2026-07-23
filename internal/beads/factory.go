package beads

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

const (
	// BeadsStoreNameBdStore is the diagnostic store name for bd-backed stores.
	BeadsStoreNameBdStore = "BdStore"
	// BeadsStoreNameFileStore is the diagnostic store name for file-backed stores.
	BeadsStoreNameFileStore = "FileStore"
	// BeadsStoreNameExecStore is the diagnostic store name for exec-backed stores.
	BeadsStoreNameExecStore = "ExecStore"

	storeNameBdStore   = BeadsStoreNameBdStore
	storeNameFileStore = BeadsStoreNameFileStore
	storeNameExecStore = BeadsStoreNameExecStore
)

// BeadsDiagnostic summarizes store selection for status surfaces. The
// PreflightGate/PreflightReason fields are reused by the conditional-writes
// seam to report a degrade/refusal (see conditional_writes_resolve.go).
//
//nolint:revive // The design names this operator-facing struct BeadsDiagnostic.
type BeadsDiagnostic struct {
	Store               string `json:"beads_store"`
	NativeStoreEligible bool   `json:"native_store_eligible"`
	PreflightGate       string `json:"preflight_gate,omitempty"`
	PreflightReason     string `json:"preflight_reason,omitempty"`
}

// StoreOpenOptions holds dependencies for opening a beads Store.
type StoreOpenOptions struct {
	ScopeRoot     string
	CityPath      string
	Provider      string
	Logger        *slog.Logger
	OpenBdStore   func() (Store, error)
	OpenFileStore func() (Store, error)
	OpenExecStore func() (Store, error)

	// ConditionalWrites is the resolved city-global beads.conditional_writes
	// mode, stamped onto every store this open produces and latched for the
	// store's lifetime — the factory is the ONE home of the mode (DESIGN
	// §6.3); there is deliberately no per-store caller option. The zero value
	// (unset) maps to Off with a defaulted marker, so an unthreaded open path
	// behaves exactly like today's default and can never raise enforcement.
	ConditionalWrites gate.Mode

	// OnConditionalWritesDegraded receives the first (and only the first)
	// capability degrade of each store this open produces — the composition
	// root converts it into the typed beads.conditional_writes.degraded
	// event wherever a bus exists. Nil on busless paths: the seam's
	// per-resolve diagnostic remains the only surface there.
	OnConditionalWritesDegraded func(ConditionalWritesDegrade)
}

// StoreOpenResult contains the selected Store plus native-selection diagnostics.
type StoreOpenResult struct {
	Store      Store
	Diagnostic BeadsDiagnostic
}

// ExecStoreDiagnostic returns the diagnostic for an explicitly configured exec store.
func ExecStoreDiagnostic() BeadsDiagnostic {
	return BeadsDiagnostic{Store: storeNameExecStore}
}

// OpenStoreAtForCity opens the configured Store for a city or rig scope.
//
// An explicit foreign exec provider (exec: that is not the bd contract) opens
// the exec store; a "file" provider opens the file store; everything else —
// including the bd contract and the "gc-beads-bd" exec provider — resolves to
// the bd-backed store, the sole interface to the beads/Dolt server.
func OpenStoreAtForCity(_ context.Context, opts StoreOpenOptions) (StoreOpenResult, error) {
	provider := strings.TrimSpace(opts.Provider)
	switch {
	case provider == "file":
		store, err := callStoreOpen("file store", opts.OpenFileStore)
		return opts.stampedResult(StoreOpenResult{Store: store, Diagnostic: BeadsDiagnostic{Store: storeNameFileStore}}, err)
	case strings.HasPrefix(provider, "exec:") && !contract.ProviderUsesBDContract(provider):
		store, err := callStoreOpen("exec store", opts.OpenExecStore)
		return opts.stampedResult(StoreOpenResult{Store: store, Diagnostic: BeadsDiagnostic{Store: storeNameExecStore}}, err)
	}
	return opts.openBdFallback(provider, BeadsDiagnostic{})
}

func (opts StoreOpenOptions) openBdFallback(provider string, diag BeadsDiagnostic) (StoreOpenResult, error) {
	if strings.HasPrefix(strings.TrimSpace(provider), "exec:") && contract.ProviderUsesBDContract(provider) && opts.OpenExecStore != nil {
		diag.Store = storeNameExecStore
		store, err := callStoreOpen("exec store", opts.OpenExecStore)
		return opts.stampedResult(StoreOpenResult{Store: store, Diagnostic: diag}, err)
	}
	diag.Store = storeNameBdStore
	store, err := callStoreOpen("bd store", opts.OpenBdStore)
	return opts.stampedResult(StoreOpenResult{Store: store, Diagnostic: diag}, err)
}

// stampedResult stamps the resolved conditional-writes mode onto a
// successfully opened store — the factory is the ONE home of the mode (§6.3),
// so every selection path funnels its result through here. ModeUnset maps to
// Off with the defaulted marker: an unthreaded open path behaves exactly like
// today's default and can never raise enforcement. The default and any
// carrier-less store (exec.Store lives outside this package and cannot
// implement the unexported carrier) are logged at debug rather than recorded
// on the wire-bound BeadsDiagnostic — a deliberate §6.3 deviation; the wire
// surface for per-store verdicts is the §12.5 status wire, stage 4.
func (opts StoreOpenOptions) stampedResult(result StoreOpenResult, err error) (StoreOpenResult, error) {
	if err != nil || result.Store == nil {
		return result, err
	}
	mode, defaulted := opts.ConditionalWrites, false
	if mode == gate.ModeUnset {
		mode, defaulted = gate.Off, true
	}
	carrier, ok := result.Store.(conditionalWritesModeCarrier)
	if !ok {
		return opts.unstampableResult(result, mode, "store cannot carry the conditional-writes mode")
	}
	carrier.setConditionalWritesDegradeCallback(opts.OnConditionalWritesDegraded)
	if !carrier.stampConditionalWritesMode(mode, defaulted) {
		return opts.unstampableResult(result, mode, "store forwards the stamp into a backing that cannot carry it")
	}
	if defaulted && opts.Logger != nil {
		opts.Logger.Debug("conditional_writes mode not threaded; defaulted to off",
			slog.String("store", result.Diagnostic.Store),
			slog.String("scope", opts.ScopeRoot))
	}
	return result, nil
}

// unstampableResult resolves an open whose store cannot carry the
// conditional-writes mode. The outcome follows the gate's own cell contract
// instead of silently succeeding (the pre-review behavior): under require the
// OPEN refuses — a store that cannot enforce the fence must never be handed
// to a caller whose config promises fencing; under auto the open succeeds but
// degrades LOUDLY (warn log plus the degrade notification, fired directly —
// there is no stamp to latch on, and an open happens once per store); off and
// unset stay a debug note.
func (opts StoreOpenOptions) unstampableResult(result StoreOpenResult, mode gate.Mode, reason string) (StoreOpenResult, error) {
	switch mode {
	case gate.Require:
		return StoreOpenResult{}, fmt.Errorf("opening %s at %s: %w",
			result.Diagnostic.Store, opts.ScopeRoot,
			&ConditionalWritesRequiredError{StoreKind: result.Diagnostic.Store, Reason: reason})
	case gate.Auto:
		if opts.Logger != nil {
			opts.Logger.Warn("conditional_writes degraded at open",
				slog.String("store", result.Diagnostic.Store),
				slog.String("mode", string(mode)),
				slog.String("reason", reason),
				slog.String("scope", opts.ScopeRoot))
		}
		if opts.OnConditionalWritesDegraded != nil {
			opts.OnConditionalWritesDegraded(ConditionalWritesDegrade{
				StoreKind: result.Diagnostic.Store,
				Mode:      string(mode),
				Reason:    reason,
			})
		}
		return result, nil
	default:
		if opts.Logger != nil {
			opts.Logger.Debug("conditional_writes stamp skipped",
				slog.String("store", result.Diagnostic.Store),
				slog.String("reason", reason),
				slog.String("scope", opts.ScopeRoot))
		}
		return result, nil
	}
}

func callStoreOpen(name string, open func() (Store, error)) (Store, error) {
	if open == nil {
		return nil, fmt.Errorf("opening %s: opener is not configured", name)
	}
	return open()
}
