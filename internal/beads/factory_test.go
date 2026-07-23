package beads

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// TestOpenStoreAtForCitySelectsStoreByProvider pins the provider→store routing
// after the in-process native store was removed: a "file" provider opens the
// file store, a foreign exec provider opens the exec store, and everything else
// — the bd contract, the gc-beads-bd exec provider, and any unrecognized
// provider — resolves to the bd-backed store, the sole interface to the server.
func TestOpenStoreAtForCitySelectsStoreByProvider(t *testing.T) {
	const scope = "/city"

	cases := []struct {
		name      string
		provider  string
		wantStore string
		opts      func(o *StoreOpenOptions)
	}{
		{
			name:      "file provider opens the file store",
			provider:  "file",
			wantStore: storeNameFileStore,
			opts: func(o *StoreOpenOptions) {
				o.OpenFileStore = func() (Store, error) { return NewMemStore(), nil }
			},
		},
		{
			name:      "bd provider opens the bd store",
			provider:  "bd",
			wantStore: storeNameBdStore,
			opts: func(o *StoreOpenOptions) {
				o.OpenBdStore = func() (Store, error) { return NewMemStore(), nil }
			},
		},
		{
			name:      "unknown provider falls back to the bd store",
			provider:  "unknown-provider",
			wantStore: storeNameBdStore,
			opts: func(o *StoreOpenOptions) {
				o.OpenBdStore = func() (Store, error) { return NewMemStore(), nil }
			},
		},
		{
			name:      "foreign exec provider opens the exec store",
			provider:  "exec:custom-tool",
			wantStore: storeNameExecStore,
			opts: func(o *StoreOpenOptions) {
				o.OpenExecStore = func() (Store, error) { return NewMemStore(), nil }
			},
		},
		{
			name:      "gc-beads-bd exec provider resolves to the exec store",
			provider:  "exec:/tmp/gc-beads-bd.sh",
			wantStore: storeNameExecStore,
			opts: func(o *StoreOpenOptions) {
				o.OpenExecStore = func() (Store, error) { return NewMemStore(), nil }
				o.OpenBdStore = func() (Store, error) {
					t.Fatal("OpenBdStore called: gc-beads-bd exec provider should use the exec opener")
					return nil, nil
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			opts := StoreOpenOptions{ScopeRoot: scope, Provider: tc.provider}
			tc.opts(&opts)
			result, err := OpenStoreAtForCity(context.Background(), opts)
			if err != nil {
				t.Fatalf("OpenStoreAtForCity() error = %v", err)
			}
			if result.Diagnostic.Store != tc.wantStore {
				t.Fatalf("diagnostic store = %q, want %q", result.Diagnostic.Store, tc.wantStore)
			}
			if result.Diagnostic.NativeStoreEligible {
				t.Fatal("native_store_eligible = true; the native store no longer exists")
			}
		})
	}
}

// TestOpenStoreAtForCityStampsConditionalWritesMode pins §6.3: the factory is
// the ONE home of the conditional-writes mode — every store it opens comes back
// stamped, on every selection path (file, bd fallback, exec).
func TestOpenStoreAtForCityStampsConditionalWritesMode(t *testing.T) {
	const scope = "/city"

	assertStamped := func(t *testing.T, store Store, wantMode gate.Mode, wantDefaulted bool) {
		t.Helper()
		carrier, ok := store.(conditionalWritesModeCarrier)
		if !ok {
			t.Fatalf("store %T carries no conditional-writes stamp", store)
		}
		mode, defaulted := carrier.conditionalWritesMode()
		if mode != wantMode || defaulted != wantDefaulted {
			t.Fatalf("stamp = (%q, %v), want (%q, %v)", mode, defaulted, wantMode, wantDefaulted)
		}
	}

	t.Run("file path stamps the resolved mode", func(t *testing.T) {
		result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:         scope,
			Provider:          "file",
			ConditionalWrites: gate.Require,
			OpenFileStore:     func() (Store, error) { return NewMemStore(), nil },
		})
		if err != nil {
			t.Fatalf("OpenStoreAtForCity: %v", err)
		}
		assertStamped(t, result.Store, gate.Require, false)
	})

	t.Run("bd fallback path stamps the resolved mode", func(t *testing.T) {
		result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:         scope,
			Provider:          "unknown-provider",
			ConditionalWrites: gate.Auto,
			OpenBdStore: func() (Store, error) {
				return NewBdStore(scope, func(string, string, ...string) ([]byte, error) {
					t.Fatal("no bd subprocess may run during open")
					return nil, nil
				}), nil
			},
		})
		if err != nil {
			t.Fatalf("OpenStoreAtForCity: %v", err)
		}
		assertStamped(t, result.Store, gate.Auto, false)
	})

	t.Run("exec-direct path stamps a carrier store", func(t *testing.T) {
		result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:         scope,
			Provider:          "exec:custom-tool",
			ConditionalWrites: gate.Auto,
			OpenExecStore:     func() (Store, error) { return NewMemStore(), nil },
		})
		if err != nil {
			t.Fatalf("OpenStoreAtForCity: %v", err)
		}
		assertStamped(t, result.Store, gate.Auto, false)
	})

	t.Run("exec-bd-contract fallback path stamps a carrier store", func(t *testing.T) {
		result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:         scope,
			Provider:          "exec:/tmp/gc-beads-bd.sh",
			ConditionalWrites: gate.Auto,
			OpenExecStore:     func() (Store, error) { return NewMemStore(), nil },
		})
		if err != nil {
			t.Fatalf("OpenStoreAtForCity: %v", err)
		}
		if result.Diagnostic.Store != storeNameExecStore {
			t.Fatalf("diagnostic store = %q, want the exec fallback arm", result.Diagnostic.Store)
		}
		assertStamped(t, result.Store, gate.Auto, false)
	})

	t.Run("unset maps to off and marks the default", func(t *testing.T) {
		result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:     scope,
			Provider:      "file",
			OpenFileStore: func() (Store, error) { return NewMemStore(), nil },
		})
		if err != nil {
			t.Fatalf("OpenStoreAtForCity: %v", err)
		}
		assertStamped(t, result.Store, gate.Off, true)
		w, diag, resolveErr := ResolveConditionalWriter(result.Store)
		if w != nil || diag != nil || resolveErr != nil {
			t.Fatal("defaulted-off store must resolve to the legacy path")
		}
	})

	t.Run("require refuses a carrier-less store at open", func(t *testing.T) {
		bare := &struct{ Store }{Store: NewMemStore()}
		_, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:         scope,
			Provider:          "file",
			ConditionalWrites: gate.Require,
			OpenFileStore:     func() (Store, error) { return bare, nil },
		})
		if !IsConditionalWritesRequired(err) {
			t.Fatalf("err = %v, want the typed require refusal: a store that cannot carry the mode must never be handed to a caller whose config promises fencing", err)
		}
	})

	t.Run("auto degrades a carrier-less store loudly at open", func(t *testing.T) {
		bare := &struct{ Store }{Store: NewMemStore()}
		var degraded []ConditionalWritesDegrade
		result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:                   scope,
			Provider:                    "file",
			ConditionalWrites:           gate.Auto,
			OpenFileStore:               func() (Store, error) { return bare, nil },
			OnConditionalWritesDegraded: func(d ConditionalWritesDegrade) { degraded = append(degraded, d) },
		})
		if err != nil {
			t.Fatalf("OpenStoreAtForCity: %v", err)
		}
		if result.Store != Store(bare) {
			t.Fatalf("store = %T, want the carrier-less store returned under auto", result.Store)
		}
		if len(degraded) != 1 || degraded[0].Mode != "auto" {
			t.Fatalf("degrade notifications = %+v, want exactly one auto degrade at open", degraded)
		}
		if w, diag, resolveErr := ResolveConditionalWriter(result.Store); w != nil || diag != nil || resolveErr != nil {
			t.Fatal("carrier-less store must resolve to the legacy path under auto")
		}
	})

	t.Run("off leaves a carrier-less store silent", func(t *testing.T) {
		bare := &struct{ Store }{Store: NewMemStore()}
		result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:         scope,
			Provider:          "file",
			ConditionalWrites: gate.Off,
			OpenFileStore:     func() (Store, error) { return bare, nil },
		})
		if err != nil || result.Store != Store(bare) {
			t.Fatalf("off over carrier-less = (%T, %v), want the store as-is", result.Store, err)
		}
	})

	t.Run("open error does not stamp", func(t *testing.T) {
		_, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
			ScopeRoot:         scope,
			Provider:          "file",
			ConditionalWrites: gate.Require,
			OpenFileStore:     func() (Store, error) { return nil, errors.New("boom") },
		})
		if err == nil {
			t.Fatal("want open error to propagate")
		}
	})
}
