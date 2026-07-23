package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// projectedDoltAuthEnvKeys are the external-dolt credential keys threaded by
// applyCanonicalDoltAuthEnv. They live outside projectedDoltEnvKeys because bd
// owns the credential for local scopes and the resolution is conditional on an
// external target; the city-process env builder copies them explicitly.
var projectedDoltAuthEnvKeys = []string{
	"GC_BEADS_PASSWORD",
	"BEADS_DOLT_PASSWORD",
	"BEADS_CREDENTIALS_FILE",
}

// applyCanonicalDoltAuthEnv resolves and projects the external-dolt credential
// (GC_BEADS_PASSWORD / BEADS_DOLT_PASSWORD) for a scope, and forwards the
// BEADS_CREDENTIALS_FILE lookup path. In proxied-server mode bd owns the
// credential for a LOCAL scope, so this only threads a password for an EXTERNAL
// endpoint; for any non-external target it strips stale projected passwords.
//
// The auth scope root (which .beads/.env supplies a store-local password) is the
// city for a rig that shares the city's endpoint and the rig itself for a rig
// with a distinct external endpoint. Switching auth scope clears any password
// the caller already projected for the previous scope so one scope's secret
// cannot contaminate another.
func applyCanonicalDoltAuthEnv(env map[string]string, cityPath, scopeRoot string, target contract.DoltConnectionTarget) {
	if env == nil {
		return
	}
	if !target.External {
		delete(env, "GC_BEADS_PASSWORD")
		delete(env, "BEADS_DOLT_PASSWORD")
		return
	}
	authScopeRoot := doltAuthScopeRoot(cityPath, scopeRoot, target)
	if !samePath(authScopeRoot, cityPath) {
		delete(env, "GC_BEADS_PASSWORD")
		delete(env, "BEADS_DOLT_PASSWORD")
	}
	if override := credentialsFileOverride(env); override != "" {
		env["BEADS_CREDENTIALS_FILE"] = override
	}
	password := resolveDoltScopePassword(env, authScopeRoot, target)
	if password != "" {
		env["GC_BEADS_PASSWORD"] = password
		env["BEADS_DOLT_PASSWORD"] = password
	} else {
		delete(env, "GC_BEADS_PASSWORD")
		delete(env, "BEADS_DOLT_PASSWORD")
	}
}

// doltAuthScopeRoot returns the scope root that owns the credential for target.
// A rig whose resolved external endpoint matches the city's inherits the city's
// credential scope; a rig with a distinct endpoint (or a local city) owns its
// own.
func doltAuthScopeRoot(cityPath, scopeRoot string, target contract.DoltConnectionTarget) string {
	if samePath(scopeRoot, cityPath) {
		return cityPath
	}
	cityTarget, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityPath, cityPath)
	if err == nil && cityTarget.External &&
		strings.EqualFold(strings.TrimSpace(cityTarget.Host), strings.TrimSpace(target.Host)) &&
		strings.TrimSpace(cityTarget.Port) == strings.TrimSpace(target.Port) {
		return cityPath
	}
	return scopeRoot
}

// credentialsFileOverride returns the effective BEADS_CREDENTIALS_FILE path,
// preferring an already-projected value over the ambient process env.
func credentialsFileOverride(env map[string]string) string {
	if path := strings.TrimSpace(env["BEADS_CREDENTIALS_FILE"]); path != "" {
		return path
	}
	return strings.TrimSpace(os.Getenv("BEADS_CREDENTIALS_FILE"))
}

// resolveDoltScopePassword resolves the external-dolt password for an auth scope
// in priority order: the ambient process GC_BEADS_PASSWORD override, then the
// scope-local .beads/.env BEADS_DOLT_PASSWORD, then the credentials file keyed by
// the target's host:port. An ambient BEADS_DOLT_PASSWORD is intentionally NOT a
// fallback for scoped GC projections — one external rig's password must not
// contaminate another scope's connection.
func resolveDoltScopePassword(env map[string]string, authScopeRoot string, target contract.DoltConnectionTarget) string {
	if pass := strings.TrimSpace(os.Getenv("GC_BEADS_PASSWORD")); pass != "" {
		return pass
	}
	if pass := readStoreLocalDoltPassword(authScopeRoot); pass != "" {
		return pass
	}
	host := strings.TrimSpace(target.Host)
	port := strings.TrimSpace(target.Port)
	if host == "" || port == "" {
		return ""
	}
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum <= 0 {
		return ""
	}
	lookupPath := credentialsFileOverride(env)
	if lookupPath == "" {
		lookupPath = defaultBeadsCredentialsPath()
	}
	if lookupPath == "" {
		return ""
	}
	return readCredentialsFilePassword(lookupPath, host, portNum)
}

// readStoreLocalDoltPassword returns BEADS_DOLT_PASSWORD from a scope-local
// .beads/.env file, or empty when absent.
func readStoreLocalDoltPassword(scopeRoot string) string {
	if strings.TrimSpace(scopeRoot) == "" {
		return ""
	}
	return readSimpleEnvFileValue(filepath.Join(scopeRoot, ".beads", ".env"), "BEADS_DOLT_PASSWORD")
}

// defaultBeadsCredentialsPath returns the default beads credentials file path
// for the current user, or empty when the home directory is unresolvable.
func defaultBeadsCredentialsPath() string {
	if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
		return filepath.Join(appData, "beads", "credentials")
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".config", "beads", "credentials")
}

// readSimpleEnvFileValue reads a KEY=value pair from a simple .env file,
// tolerating an optional `export ` prefix and single/double quoting.
func readSimpleEnvFileValue(path, key string) string {
	f, err := os.Open(path) //nolint:gosec // path is derived from scope roots
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	value := ""
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		name, raw, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) != key {
			continue
		}
		value = strings.TrimSpace(raw)
		if len(value) >= 2 {
			if (value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"') {
				if unquoted, uerr := strconv.Unquote(value); uerr == nil {
					value = unquoted
				} else {
					value = value[1 : len(value)-1]
				}
			}
		}
	}
	return value
}

// readSimpleYAMLScalar reads a top-level `key: value` scalar from a simple
// flat YAML file (bd's config.yaml shape), tolerating quoting and inline
// comments. Returns "" when the key is absent or the file is unreadable.
func readSimpleYAMLScalar(path, key string) string {
	f, err := os.Open(path) //nolint:gosec // path is derived from scope roots
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	value := ""
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, raw, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) != key {
			continue
		}
		raw = strings.TrimSpace(raw)
		if idx := strings.Index(raw, " #"); idx >= 0 {
			raw = strings.TrimSpace(raw[:idx])
		}
		if len(raw) >= 2 {
			if (raw[0] == '\'' && raw[len(raw)-1] == '\'') || (raw[0] == '"' && raw[len(raw)-1] == '"') {
				raw = raw[1 : len(raw)-1]
			}
		}
		value = raw
	}
	return value
}

// readCredentialsFilePassword returns the password for host:port from a beads
// credentials file. The file is INI-like: a `[host:port]` section header
// followed by `password=...`.
func readCredentialsFilePassword(path, host string, port int) string {
	f, err := os.Open(path) //nolint:gosec // path comes from env or os.UserHomeDir
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	sectionKey := host + ":" + strconv.Itoa(port)
	inSection := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := line[1 : len(line)-1]
			if section == sectionKey {
				inSection = true
			} else if inSection {
				break
			}
			continue
		}
		if !inSection {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == "password" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
