package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/test/tmuxtest"
)

func canonicalTestPath(path string) string {
	return testutil.CanonicalPath(path)
}

func assertSameTestPath(t *testing.T, got, want string) {
	t.Helper()
	testutil.AssertSamePath(t, got, want)
}

func shortSocketTempDir(t *testing.T, prefix string) string {
	t.Helper()
	return testutil.ShortTempDir(t, prefix)
}

// cmdGCTmuxSocketRoot returns a short-path tmux socket root under /tmp (not
// testTempRoot, which can be an arbitrarily long macOS $TMPDIR path that
// blows Unix socket path limits), plus the parent dir to remove at teardown
// and the *os.File holding its alive sentinel. The sentinel must stay
// referenced by the caller for the process lifetime so a concurrent sibling
// run's orphan sweep (tmuxtest.SweepOrphanPIDPrefixedDirs, invoked inside
// NewSocketParentDir) does not reclaim this still-active directory.
func cmdGCTmuxSocketRoot(testTempRoot string) (string, string, *os.File, error) {
	parent, sentinel, err := tmuxtest.NewSocketParentDir("/tmp")
	if err != nil {
		root := filepath.Join(testTempRoot, "tmux")
		if err := os.MkdirAll(root, 0o700); err != nil {
			return "", "", nil, fmt.Errorf("creating fallback cmd/gc tmux socket root: %w", err)
		}
		return root, "", nil, nil
	}
	root := filepath.Join(parent, "tmux")
	if err := os.MkdirAll(root, 0o700); err != nil {
		_ = sentinel.Close()
		_ = os.RemoveAll(parent)
		return "", "", nil, fmt.Errorf("creating cmd/gc tmux socket root: %w", err)
	}
	return root, parent, sentinel, nil
}

// clearInheritedBeadsEnv prevents tests that explicitly write
// [beads]\nprovider = "file" from being silently overridden by an agent
// session's inherited GC_BEADS=bd, which would trigger gc-beads-bd.sh and
// leak an orphan dolt sql-server because test cleanup paths do not call
// shutdownBeadsProvider.
func clearInheritedBeadsEnv(t *testing.T) {
	t.Helper()
	for _, key := range liveEnvKeysForTests() {
		if key == "GC_HOME" {
			continue
		}
		t.Setenv(key, "")
	}
}

// requireNoLeakedDoltAfterForPaths is retained as a no-op guard. gascity no
// longer spawns or manages dolt sql-server processes — bd owns the proxied
// server lifecycle (spawn, idle-shutdown, cleanup) behind the bd CLI — so there
// is no gascity-owned dolt process for a test to leak. Callers keep invoking it
// at scope-setup sites; it stays a stable seam should per-scope store-process
// leak detection ever move back in-tree.
func requireNoLeakedDoltAfterForPaths(t *testing.T, paths ...string) {
	t.Helper()
	_ = paths
}

// cleanupManagedDoltTestCity registers teardown for a test city: it stops the
// controller and shuts down the bead provider. gascity no longer manages the
// dolt sql-server, so there is no store process to stop here — bd's proxied
// server idles out or is torn down by bd/gc stop.
func cleanupManagedDoltTestCity(t *testing.T, cityPath string) {
	t.Helper()
	requireNoLeakedDoltAfterForPaths(t, cityPath)
	t.Cleanup(func() {
		tryStopController(cityPath, io.Discard)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if controllerAlive(cityPath) == 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err := shutdownBeadsProvider(cityPath); err != nil {
			t.Logf("shutdownBeadsProvider(%s): %v", cityPath, err)
		}
	})
}
