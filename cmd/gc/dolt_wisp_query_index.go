package main

import (
	"context"
	"fmt"
	"strings"
)

// wispQueryIndexStatements lists the CREATE INDEX statements applied by
// applyWispQueryIndexes. Each entry is idempotent (IF NOT EXISTS).
//
// Background: the beads library's SearchIssuesWithCountsInTx generates SQL
// with full-table subquery aggregations for wisp_labels and wisp_dependencies
// (JSON_ARRAYAGG labels, dep/rdep/comment counts), all materialized across the
// entire table before the outer WHERE filter is applied. Two indexes are
// missing from the beads schema migrations that would significantly reduce the
// cost of these scans on a busy server:
//
//   - idx_wisp_labels_issue_id: without it, the GROUP BY issue_id scan in the
//     labels subquery does a full wisp_labels scan + sort on every bd query call.
//   - idx_wisps_status_type: composite covering the most common hot filter
//     (status='open' AND issue_type='message') so the outer WHERE can use a
//     single range scan instead of filtering two separate index rows.
//
// These belong upstream in the beads schema migrations; this gc-side guard
// applies them immediately without waiting for a beads version bump.
var wispQueryIndexStatements = []string{
	"CREATE INDEX IF NOT EXISTS idx_wisp_labels_issue_id ON wisp_labels(issue_id)",
	"CREATE INDEX IF NOT EXISTS idx_wisps_status_type ON wisps(status, issue_type)",
}

// applyWispQueryIndexes creates the missing wisp query performance indexes
// through bd (the sole interface to the server in proxied-server mode). It is
// idempotent and fail-open: any error is logged to stderr but never returned,
// so a degraded store at startup does not block the controller.
func (cr *CityRuntime) applyWispQueryIndexes(_ context.Context) {
	if !cityUsesManagedDoltBeadsLifecycle(cr.cityPath) {
		return
	}
	run := bdCommandRunnerForCity(cr.cityPath)
	stmts := append([]string{}, wispQueryIndexStatements...)
	// Persist the schema change so the indexes survive reset/sync, matching
	// the schemas/wisps-composite-index migration convention.
	stmts = append(stmts,
		"CALL DOLT_ADD('.')",
		"CALL DOLT_COMMIT('-m', 'gc: add wisp-query performance indexes (gcy-0m1)', '--author', 'gascity-builder <builder@gascity.local>')",
	)
	for _, stmt := range stmts {
		if _, err := run(cr.cityPath, "bd", "sql", stmt); err != nil {
			// Non-fatal: some statements fail before first bd init (table
			// absent) or on an idempotent re-commit ("nothing to commit").
			if strings.Contains(strings.ToLower(err.Error()), "nothing to commit") {
				continue
			}
			fmt.Fprintf(cr.stderr, "%s: wisp-query-index: %s: %v\n", cr.logPrefix, stmt, err) //nolint:errcheck // best-effort stderr
		}
	}
}
