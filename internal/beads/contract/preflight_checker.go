package contract

import (
	"path/filepath"
	"strings"
)

// ProviderUsesBDContract reports whether provider exposes the bd-compatible
// store contract, and therefore resolves to the bd-backed store rather than a
// foreign exec store.
func ProviderUsesBDContract(provider string) bool {
	provider = strings.TrimSpace(provider)
	if provider == "" || provider == "bd" {
		return true
	}
	if !strings.HasPrefix(provider, "exec:") {
		return false
	}
	base := strings.TrimSuffix(filepath.Base(strings.TrimPrefix(provider, "exec:")), ".sh")
	return base == "gc-beads-bd"
}
