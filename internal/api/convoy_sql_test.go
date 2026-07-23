package api

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// fakeSQLQuerier is a beads.SQLQuerier that answers the fixed set of bulk reads
// workflowSQLSnapshot issues, routing by SQL substring. It pins the exact
// `bd sql --json` row shape the parser depends on (a JSON array of row objects
// keyed by the aliased column names), so a bd output-format change can't
// silently break workflow rendering.
type fakeSQLQuerier struct {
	responses map[string]string
	queries   []string
}

func (f *fakeSQLQuerier) QueryJSON(query string) ([]byte, error) {
	f.queries = append(f.queries, query)
	q := strings.Join(strings.Fields(query), " ") // normalize whitespace
	switch {
	case strings.Contains(q, "information_schema.tables") && strings.Contains(q, "'issues'"):
		return []byte(`[{"n":1}]`), nil
	case strings.Contains(q, "information_schema.tables") && strings.Contains(q, "'wisps'"):
		return []byte(`[{"n":0}]`), nil
	case strings.Contains(q, "information_schema.tables") && strings.Contains(q, "'dependencies'"):
		return []byte(`[{"n":1}]`), nil
	case strings.Contains(q, "information_schema.tables") && strings.Contains(q, "'labels'"):
		return []byte(`[{"n":1}]`), nil
	case strings.Contains(q, "information_schema.columns"):
		return []byte(`[{"column_name":"depends_on_id"}]`), nil
	// deps/labels reads embed a `FROM issues i` id subquery, so match them
	// before the plain bead read.
	case strings.Contains(q, "FROM dependencies d"):
		return []byte(f.responses["deps"]), nil
	case strings.Contains(q, "FROM labels l"):
		return []byte(f.responses["labels"]), nil
	case strings.Contains(q, "FROM issues i"):
		return []byte(f.responses["beads"]), nil
	}
	return []byte(`[]`), nil
}

// TestWorkflowSQLSnapshotParsesBdSQLJSON is the golden row-shape test: it drives
// the whole snapshot off canned `bd sql --json` payloads and asserts the parsed
// beads, metadata, timestamps, deps, and labels. The metadata column is
// deliberately exercised in both forms dolt emits — an embedded object and a
// JSON-encoded string — plus a null assignee, to lock the decoder's contract.
func TestWorkflowSQLSnapshotParsesBdSQLJSON(t *testing.T) {
	q := &fakeSQLQuerier{responses: map[string]string{
		"beads": `[
			{"id":"bd-1","title":"Root","status":"open","issue_type":"molecule","assignee":null,"description":"root","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-02 03:04:05","metadata":"{\"gc.root_bead_id\":\"bd-1\",\"gc.kind\":\"workflow\"}"},
			{"id":"bd-2","title":"Step","status":"closed","issue_type":"step","assignee":"agent-a","description":"child","created_at":"2026-01-01T01:00:00Z","updated_at":"2026-01-01T02:00:00Z","metadata":{"gc.root_bead_id":"bd-1"}}
		]`,
		"deps":   `[{"issue_id":"bd-2","depends_on":"bd-1","dep_type":"blocks"}]`,
		"labels": `[{"issue_id":"bd-1","label":"priority"},{"issue_id":"bd-1","label":"root"}]`,
	}}

	workflowBeads, index, depMap, err := workflowSQLSnapshot(q, "bd-1")
	if err != nil {
		t.Fatalf("workflowSQLSnapshot() error = %v", err)
	}
	if len(workflowBeads) != 2 {
		t.Fatalf("got %d beads, want 2", len(workflowBeads))
	}

	root, ok := index["bd-1"]
	if !ok {
		t.Fatal("root bd-1 missing from index")
	}
	if root.Title != "Root" || root.Status != "open" || root.Type != "molecule" {
		t.Errorf("root fields = %+v", root)
	}
	if root.Assignee != "" {
		t.Errorf("root assignee = %q, want empty (null)", root.Assignee)
	}
	// metadata supplied as a JSON-encoded string must decode to the map.
	if root.Metadata["gc.root_bead_id"] != "bd-1" || root.Metadata["gc.kind"] != "workflow" {
		t.Errorf("root metadata = %v, want decoded gc.root_bead_id/gc.kind", root.Metadata)
	}
	wantCreated := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !root.CreatedAt.Equal(wantCreated) {
		t.Errorf("root created_at = %v, want %v", root.CreatedAt, wantCreated)
	}
	if got := root.Labels; len(got) != 2 || got[0] != "priority" || got[1] != "root" {
		t.Errorf("root labels = %v, want [priority root]", got)
	}

	child := index["bd-2"]
	// metadata supplied as an embedded object must decode identically.
	if child.Metadata["gc.root_bead_id"] != "bd-1" {
		t.Errorf("child metadata = %v, want gc.root_bead_id=bd-1", child.Metadata)
	}
	if child.Assignee != "agent-a" {
		t.Errorf("child assignee = %q, want agent-a", child.Assignee)
	}

	deps := depMap["bd-2"]
	if len(deps) != 1 || deps[0].DependsOnID != "bd-1" || deps[0].Type != "blocks" {
		t.Errorf("deps[bd-2] = %+v, want one blocks->bd-1", deps)
	}
}

func TestSQLQuoteEscapesSingleQuotes(t *testing.T) {
	cases := map[string]string{
		"bd-1":      "'bd-1'",
		"a'b":       "'a''b'",
		"'; DROP--": "'''; DROP--'",
	}
	for in, want := range cases {
		if got := sqlQuote(in); got != want {
			t.Errorf("sqlQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWorkflowSQLDecodeMetadata(t *testing.T) {
	// embedded object
	if got := workflowSQLDecodeMetadata([]byte(`{"k":"v","n":3}`)); got["k"] != "v" || got["n"] != "3" {
		t.Errorf("embedded object decode = %v", got)
	}
	// JSON-encoded string
	if got := workflowSQLDecodeMetadata([]byte(`"{\"k\":\"v\"}"`)); got["k"] != "v" {
		t.Errorf("string-encoded decode = %v", got)
	}
	// null / empty
	if got := workflowSQLDecodeMetadata([]byte(`null`)); got != nil {
		t.Errorf("null decode = %v, want nil", got)
	}
	if got := workflowSQLDecodeMetadata(nil); got != nil {
		t.Errorf("empty decode = %v, want nil", got)
	}
}

func TestWorkflowSQLParseTime(t *testing.T) {
	for _, in := range []string{
		"2026-01-02 03:04:05",
		"2026-01-02T03:04:05Z",
		"2026-01-02 03:04:05.123456",
	} {
		if workflowSQLParseTime(in).IsZero() {
			t.Errorf("parseTime(%q) returned zero", in)
		}
	}
	if !workflowSQLParseTime("not-a-time").IsZero() {
		t.Error("parseTime(garbage) should be zero")
	}
}

// compile-time assertion that *beads.BdStore satisfies the capability the
// snapshot path type-asserts.
var _ beads.SQLQuerier = (*beads.BdStore)(nil)
