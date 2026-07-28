package beads

import (
	"errors"
	"fmt"
)

// ErrSQLQueryUnsupported is returned by SQLQuerier implementations whose
// backing store cannot serve raw SQL (e.g. a file store, or a CachingStore
// wrapping a non-bd backing). Callers use it to fall back to the slow
// Store-interface path rather than treating it as a hard failure.
var ErrSQLQueryUnsupported = errors.New("store does not support SQL queries")

// SQLQuerier is an optional Store capability: run a read-only SQL query through
// bd (the sole interface to the server) and return the raw `--json` result.
//
// bd owns the dolt endpoint in proxied-server mode, so gascity never opens its
// own connection; a bulk read that wants to avoid N+1 Store calls issues a few
// SQL statements through this capability instead. The returned bytes are the
// `bd sql --json` payload — a JSON array of row objects (`[{"col":val,...}]`),
// empty as `[]` — already trimmed to the leading JSON token.
type SQLQuerier interface {
	QueryJSON(query string) ([]byte, error)
}

// QueryJSON runs `bd sql <query> --json` in the store's scope directory and
// returns the trimmed JSON result. It reuses the store's configured command
// runner, so it carries the same environment (BEADS_DIR, credentials) as every
// other bd call this store makes.
func (s *BdStore) QueryJSON(query string) ([]byte, error) {
	out, err := s.runner(s.dir, "bd", "sql", query, "--json")
	if err != nil {
		return nil, fmt.Errorf("bd sql: %w", err)
	}
	return extractJSON(out), nil
}

// QueryJSON delegates to the backing store when it supports SQL, so a bd-backed
// CachingStore stays SQL-capable through the cache wrapper. A non-bd backing
// yields ErrSQLQueryUnsupported.
func (c *CachingStore) QueryJSON(query string) ([]byte, error) {
	if q, ok := c.backing.(SQLQuerier); ok {
		return q.QueryJSON(query)
	}
	return nil, ErrSQLQueryUnsupported
}
