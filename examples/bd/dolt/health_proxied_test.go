package dolt_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// fakeBdHealth is a stand-in `bd` for the proxied health path: it answers the
// liveness probe, the database catalog, and per-db count queries the health
// report issues — all through `bd sql`, never a dolt port.
const fakeBdHealth = `#!/bin/sh
verb=""
while [ $# -gt 0 ]; do
  case "$1" in
    -C) shift 2 ;;
    context) verb=context; shift ;;
    sql) verb=sql; shift; break ;;
    *) shift ;;
  esac
done
if [ "$verb" = context ]; then
  printf '{"dolt_mode":"proxied-server","database":"hq"}\n'; exit 0
fi
case "${1:-}" in --csv|--json) shift ;; esac
q="${1:-}"
case "$q" in
  *"SELECT 1"*)        printf '1\n1\n' ;;
  *"SHOW DATABASES"*)  printf 'Database\nhq\n' ;;
  *dolt_log*)          printf 'COUNT(*)\n42\n' ;;
  *"status='open'"*)   printf 'COUNT(*)\n7\n' ;;
  *) : ;;
esac
`

// TestHealthProxiedReportsReachableViaBd proves `gc dolt health --json` under
// proxied-server mode reports server.reachable from a bd SELECT-1 probe and
// per-db counts from bd, with no dependence on a resolvable Dolt port or the
// on-disk zombie/orphan scans.
func TestHealthProxiedReportsReachableViaBd(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	cityPath := t.TempDir()
	writeStoreMetadata(t, cityPath, "proxied-server")

	root := repoRoot(t)
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "bd"), fakeBdHealth)
	// No dolt on PATH: a proxied health report must never shell out to dolt.
	writeExecutable(t, filepath.Join(binDir, "dolt"), "#!/bin/sh\necho 'dolt must not be called in proxied mode' >&2\nexit 97\n")

	cmd := exec.Command("sh", filepath.Join(root, healthScript), "--json")
	cmd.Env = append(filteredEnv("PATH"),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("health --json failed: %v\n%s", err, out)
	}

	var report struct {
		Server struct {
			Reachable bool `json:"reachable"`
			External  bool `json:"external"`
		} `json:"server"`
		Databases []struct {
			Name      string `json:"name"`
			Commits   int    `json:"commits"`
			OpenBeads int    `json:"open_beads"`
		} `json:"databases"`
		Processes struct {
			ZombieCount int `json:"zombie_count"`
		} `json:"processes"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("parse health JSON: %v\n%s", err, out)
	}
	if !report.Server.Reachable {
		t.Fatalf("server.reachable = false, want true\n%s", out)
	}
	if len(report.Databases) != 1 || report.Databases[0].Name != "hq" {
		t.Fatalf("databases = %+v, want one entry named hq\n%s", report.Databases, out)
	}
	if report.Databases[0].Commits != 42 || report.Databases[0].OpenBeads != 7 {
		t.Fatalf("hq counts = commits %d open %d, want 42/7\n%s",
			report.Databases[0].Commits, report.Databases[0].OpenBeads, out)
	}
	if report.Processes.ZombieCount != 0 {
		t.Fatalf("zombie_count = %d, want 0 (no process scan in proxied mode)\n%s",
			report.Processes.ZombieCount, out)
	}
}
