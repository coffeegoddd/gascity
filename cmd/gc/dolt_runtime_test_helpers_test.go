package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// writeReachableManagedDoltState provisions a scope's `.beads` directory and
// returns a bound-but-idle ephemeral port. gascity no longer manages a dolt
// sql-server or publishes runtime endpoint state — bd owns the proxied server
// behind the bd CLI — so this only stands up the on-disk scope shape a test
// needs. The port is returned for callers that still thread it through their
// fixtures; the listener stays open for the test's lifetime so the port cannot
// be reused underneath the test.
func writeReachableManagedDoltState(t *testing.T, cityPath string) int {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatalf("MkdirAll(city .beads): %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})
	return ln.Addr().(*net.TCPAddr).Port
}

// writeReachableProviderManagedDoltState is the provider-scope variant of
// writeReachableManagedDoltState: it additionally creates the `.beads/dolt`
// data subdirectory some provider tests expect on disk.
func writeReachableProviderManagedDoltState(t *testing.T, cityPath string) int {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt"), 0o755); err != nil {
		t.Fatalf("MkdirAll(city .beads/dolt): %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})
	return ln.Addr().(*net.TCPAddr).Port
}

//nolint:unused // exercised by native_dolt_rebind_integration_test.go
func occupyManagedDoltPort(t *testing.T, port int) {
	t.Helper()

	cmd := exec.Command("python3", "-c", `
import signal
import socket
import sys
import time

port = int(sys.argv[1])
deadline = time.time() + 10.0
sock = None
while time.time() < deadline:
    candidate = socket.socket()
    candidate.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        candidate.bind(("127.0.0.1", port))
        candidate.listen(5)
        sock = candidate
        break
    except OSError:
        candidate.close()
        time.sleep(0.05)

if sock is None:
    raise SystemExit(3)

def _stop(*_args):
    raise SystemExit(0)

signal.signal(signal.SIGTERM, _stop)
signal.signal(signal.SIGINT, _stop)
while True:
    conn, _ = sock.accept()
    conn.close()
`, strconv.Itoa(port))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start managed port blocker: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			t.Fatalf("managed port blocker for %d exited early", port)
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("managed port blocker on %d did not become ready", port)
}
