package main

import "errors"

// runDoltliteReindex reports that an in-process DoltLite reindex is unavailable:
// gc reaches the beads/Dolt store solely through the bd subprocess and links no
// in-process SQLite driver to REINDEX a physical .beads/doltlite/<db>.db file.
func runDoltliteReindex(_ string) error {
	return errors.New("in-process doltlite reindex is not supported; gc reaches the store through the bd subprocess")
}

// doltliteReindexSupported reports that this build cannot rebuild DoltLite
// SQLite indexes in process. The maintenance path probes this before running
// the stale-index-producing flatten/gc so it never creates index corruption it
// cannot heal (ga-7hei).
func doltliteReindexSupported() bool { return false }
