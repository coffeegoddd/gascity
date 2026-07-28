package main

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/config"
)

// proxiedServerInitOptions is the resolved --proxied-server opt-in that `gc
// init` pins for a city's beads ledger. When enabled, bd owns the Dolt
// sql-server lifecycle entirely (port selection, spawn, idle-shutdown,
// credentials) instead of gascity managing it, mirroring the shape of
// hostedDoltInitOptions (cmd/gc/init_hosted_dolt.go) but simpler: there is
// no host/port/user/database/project-id to validate, since bd owns the
// endpoint.
type proxiedServerInitOptions struct {
	Enabled bool
}

// enabled reports whether the opt-in bd proxied-server mode was requested.
func (o proxiedServerInitOptions) enabled() bool {
	return o.Enabled
}

// validate enforces that --proxied-server is not combined with a
// hosted/external Dolt endpoint: both mean "gascity gives up local Dolt
// ownership," to different owners (bd's proxy vs. an explicit external
// server), and combining them would produce a nonsensical dual state where
// neither owner is authoritative.
func (o proxiedServerInitOptions) validate(hosted hostedDoltInitOptions) error {
	if !o.enabled() {
		return nil
	}
	if hosted.enabled() {
		return fmt.Errorf("--proxied-server cannot be combined with --dolt-host: choose bd's proxied-server mode or an explicit hosted Dolt endpoint, not both")
	}
	return nil
}

// applyToCityConfig pins the city's beads backend to "proxied-server" so
// cityUsesProxiedServerMode (cmd/gc/providers.go) resolves true from the
// moment city.toml is written — the same city.toml-backed dispatch seam
// cityUsesDoltliteBeadsBackend already uses for "doltlite". A no-op when
// disabled.
func (o proxiedServerInitOptions) applyToCityConfig(cfg *config.City) {
	if !o.enabled() {
		return
	}
	cfg.Beads.Backend = "proxied-server"
}
