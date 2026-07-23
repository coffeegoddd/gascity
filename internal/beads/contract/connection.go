// Package contract owns canonical beads/Dolt config and connection resolution.
package contract

import (
	"fmt"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
)

// ManagedCityHostEnv lets deployments override the host used to reach a
// managed-city Dolt server. Default is loopback; containerised callers
// (MCP servers, proxies on Docker Desktop) set this to e.g.
// "host.docker.internal" because 127.0.0.1 inside the container is the
// container's own loopback, not the Dolt-hosting machine.
//
// Name matches gc's existing GC_BEADS_HOST convention; the bd-side env
// (BEADS_DOLT_SERVER_HOST) is already derived from GC_BEADS_HOST by
// cmd/gc/bd_env.go#mirrorBeadsDoltEnv, so a single env var serves both
// the gc-internal direct connection (this helper) and bd subprocesses.
// Ambient GC_BEADS_HOST redirects managed-city targets too; unset it when
// default managed loopback behavior is desired.
const ManagedCityHostEnv = "GC_BEADS_HOST"

// DoltHostIsLocal reports whether host names the caller's local network
// namespace for managed Dolt process ownership decisions.
func DoltHostIsLocal(host string) bool {
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "" || host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return addr.IsLoopback() || addr.IsUnspecified()
}

// DoltConnectionTarget is the resolved connection info for a beads scope. bd
// owns the endpoint in proxied-server mode: a target is either LOCAL
// (External=false — no gascity host/port, bd owns a proxied local server) or
// EXTERNAL (External=true — coordinates gascity records only to feed bd's
// proxied-server-external init).
type DoltConnectionTarget struct {
	Host     string
	Port     string
	Database string
	User     string
	External bool
}

// ScopeConfigResolutionKind describes how a scope config was resolved.
type ScopeConfigResolutionKind string

// Scope config resolution kinds.
const (
	ScopeConfigMissing       ScopeConfigResolutionKind = "missing"
	ScopeConfigLegacyMinimal ScopeConfigResolutionKind = "legacy_minimal"
	ScopeConfigAuthoritative ScopeConfigResolutionKind = "authoritative"
)

// ScopeConfigResolution reports the authoritative-state resolution for a scope.
type ScopeConfigResolution struct {
	Kind  ScopeConfigResolutionKind
	State ConfigState
}

// InvalidCanonicalConfigError reports invalid canonical scope config.
type InvalidCanonicalConfigError struct {
	Path string
	Err  error
}

func (e *InvalidCanonicalConfigError) Error() string {
	return fmt.Sprintf("invalid canonical endpoint state in %s: %v", e.Path, e.Err)
}

func (e *InvalidCanonicalConfigError) Unwrap() error {
	return e.Err
}

// ResolveDoltConnectionTarget returns the effective Dolt target for a scope. A
// scope with dolt coords (its own, or inherited from an external city) is
// EXTERNAL; otherwise it is LOCAL and bd owns the proxied endpoint.
func ResolveDoltConnectionTarget(fs fsys.FS, cityRoot, scopeRoot string) (DoltConnectionTarget, error) {
	cfgPath := filepath.Join(scopeRoot, ".beads", "config.yaml")
	cfg, ok, err := ReadConfigState(fs, cfgPath)
	if err != nil {
		return DoltConnectionTarget{}, err
	}
	if !ok {
		cfg = ConfigState{}
	}
	if err := ValidateConnectionConfigState(fs, cityRoot, scopeRoot, cfg); err != nil {
		return DoltConnectionTarget{}, err
	}

	target := DoltConnectionTarget{Database: "beads", User: strings.TrimSpace(cfg.DoltUser)}
	if db, ok, err := ReadDoltDatabase(fs, filepath.Join(scopeRoot, ".beads", "metadata.json")); err != nil {
		return DoltConnectionTarget{}, err
	} else if ok && strings.TrimSpace(db) != "" {
		target.Database = strings.TrimSpace(db)
	}

	if scopeHasCoords(cfg) {
		port := strings.TrimSpace(cfg.DoltPort)
		if port == "" {
			return DoltConnectionTarget{}, fmt.Errorf("external scope requires dolt.port")
		}
		if _, convErr := strconv.Atoi(port); convErr != nil {
			return DoltConnectionTarget{}, fmt.Errorf("invalid dolt.port %q: %w", port, convErr)
		}
		host := canonicalExternalHost(cfg.DoltHost, port)
		if hostErr := validateExternalHostValue(host, port); hostErr != nil {
			return DoltConnectionTarget{}, hostErr
		}
		target.Host = host
		target.Port = port
		target.External = true
	}
	return target, nil
}

// scopeHasCoords reports whether a scope config carries its own external dolt
// endpoint coordinates. A scope is external iff its OWN config has coords —
// there is no cross-scope inheritance: bd owns each scope's endpoint per-scope.
func scopeHasCoords(cfg ConfigState) bool {
	return strings.TrimSpace(cfg.DoltHost) != "" || strings.TrimSpace(cfg.DoltPort) != ""
}

// ValidateCanonicalConfigState validates canonical scope config invariants.
func ValidateCanonicalConfigState(fs fsys.FS, cityRoot, scopeRoot string, cfg ConfigState) error {
	return ValidateConnectionConfigState(fs, cityRoot, scopeRoot, cfg)
}

// ValidateConnectionConfigState validates the config needed to build a target.
// A local (no-coords) scope needs no validation; an external scope must carry a
// non-wildcard host and a dolt.port.
func ValidateConnectionConfigState(_ fsys.FS, _, _ string, cfg ConfigState) error {
	if !scopeHasCoords(cfg) {
		return nil
	}
	if strings.TrimSpace(cfg.DoltPort) == "" {
		return fmt.Errorf("external dolt config requires dolt.port")
	}
	return validateExternalHostValue(cfg.DoltHost, cfg.DoltPort)
}

// ResolveAuthoritativeConfigState returns a normalized authoritative scope
// config when present. It reflects the scope's OWN config: an external scope
// carries canonicalized coords, a local (or inheriting) scope carries none.
// Callers that need a rig's EFFECTIVE endpoint (following city inheritance) use
// ResolveDoltConnectionTarget; ConfigHasEndpointAuthority on the returned state
// answers "does this scope own its own external endpoint".
func ResolveAuthoritativeConfigState(fs fsys.FS, cityRoot, scopeRoot, issuePrefix string) (ConfigState, bool, error) {
	existing, ok, err := ReadConfigState(fs, filepath.Join(scopeRoot, ".beads", "config.yaml"))
	if err != nil || !ok {
		return ConfigState{}, ok, err
	}
	existing.IssuePrefix = issuePrefix
	if err := ValidateConnectionConfigState(fs, cityRoot, scopeRoot, existing); err != nil {
		return ConfigState{}, false, err
	}
	if scopeHasCoords(existing) {
		existing.DoltPort = strings.TrimSpace(existing.DoltPort)
		existing.DoltHost = canonicalExternalHost(existing.DoltHost, existing.DoltPort)
		existing.DoltUser = strings.TrimSpace(existing.DoltUser)
	}
	return existing, true, nil
}

// ResolveScopeConfigState resolves a scope config into canonical, legacy, or missing state.
func ResolveScopeConfigState(fs fsys.FS, cityRoot, scopeRoot, issuePrefix string) (ScopeConfigResolution, error) {
	cfgPath := filepath.Join(scopeRoot, ".beads", "config.yaml")
	existing, ok, err := ReadConfigState(fs, cfgPath)
	if err != nil {
		return ScopeConfigResolution{}, err
	}
	if !ok {
		return ScopeConfigResolution{Kind: ScopeConfigMissing}, nil
	}
	if IsLegacyMinimalEndpointConfig(existing) {
		return ScopeConfigResolution{Kind: ScopeConfigLegacyMinimal}, nil
	}
	state, ok, err := ResolveAuthoritativeConfigState(fs, cityRoot, scopeRoot, issuePrefix)
	if err != nil {
		return ScopeConfigResolution{}, &InvalidCanonicalConfigError{Path: cfgPath, Err: err}
	}
	if !ok {
		return ScopeConfigResolution{}, &InvalidCanonicalConfigError{Path: cfgPath, Err: fmt.Errorf("unrecognized endpoint authority")}
	}
	return ScopeConfigResolution{Kind: ScopeConfigAuthoritative, State: state}, nil
}

func canonicalExternalHost(host, port string) string {
	host = strings.TrimSpace(host)
	if host == "" && strings.TrimSpace(port) != "" {
		return "127.0.0.1"
	}
	return host
}

func validateExternalHostValue(host, port string) error {
	host = canonicalExternalHost(host, port)
	switch strings.Trim(host, "[]") {
	case "0.0.0.0", "::":
		return fmt.Errorf("external endpoint host %q is invalid; use a concrete host, not a bind address", host)
	default:
		return nil
	}
}

func sameScope(a, b string) bool {
	return normalizeScopePathForCompare(a) == normalizeScopePathForCompare(b)
}

func normalizeScopePathForCompare(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

// ConfigHasEndpointAuthority reports whether config pins an external endpoint.
func ConfigHasEndpointAuthority(cfg ConfigState) bool {
	return scopeHasCoords(cfg)
}

// IsLegacyMinimalEndpointConfig reports whether config carries no endpoint
// coordinates (a bare, local bd-owned scope).
func IsLegacyMinimalEndpointConfig(cfg ConfigState) bool {
	return !configTracksEndpoint(cfg)
}

func configTracksEndpoint(cfg ConfigState) bool {
	return strings.TrimSpace(cfg.DoltHost) != "" || strings.TrimSpace(cfg.DoltPort) != "" || strings.TrimSpace(cfg.DoltUser) != ""
}
