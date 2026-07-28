package beads_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestBdStoreQueryJSONReturnsTrimmedResult(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		"bd sql SELECT 1 --json": {out: []byte(`[{"a":1}]`)},
	})
	store := beads.NewBdStore("/city", runner)

	got, err := store.QueryJSON("SELECT 1")
	if err != nil {
		t.Fatalf("QueryJSON() error = %v", err)
	}
	if string(got) != `[{"a":1}]` {
		t.Fatalf("QueryJSON() = %q, want %q", got, `[{"a":1}]`)
	}
}

func TestBdStoreQueryJSONWrapsRunnerError(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		"bd sql SELECT 1 --json": {err: errors.New("boom")},
	})
	store := beads.NewBdStore("/city", runner)

	_, err := store.QueryJSON("SELECT 1")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("QueryJSON() error = %v, want wrapped %q", err, "boom")
	}
}

func TestCachingStoreQueryJSONDelegatesToBackingBdStore(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		"bd sql SELECT 1 --json": {out: []byte(`[]`)},
	})
	backing := beads.NewBdStore("/city", runner)
	caching := beads.NewCachingStoreForTest(backing, nil)

	got, err := caching.QueryJSON("SELECT 1")
	if err != nil {
		t.Fatalf("QueryJSON() error = %v", err)
	}
	if string(got) != "[]" {
		t.Fatalf("QueryJSON() = %q, want %q", got, "[]")
	}
}

func TestCachingStoreQueryJSONUnsupportedForNonSQLBacking(t *testing.T) {
	caching := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)

	_, err := caching.QueryJSON("SELECT 1")
	if !errors.Is(err, beads.ErrSQLQueryUnsupported) {
		t.Fatalf("QueryJSON() error = %v, want ErrSQLQueryUnsupported", err)
	}
}
