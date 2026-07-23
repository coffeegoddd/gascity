package main

import "github.com/gastownhall/gascity/internal/beads"

// openOptimizedDoltliteStore is a no-op: gc has no in-process store and reaches
// the beads/Dolt store solely through the bd subprocess (*BdStore). It remains
// a seam so openBdStoreAt need not special-case the absence of an optimized
// read path.
func openOptimizedDoltliteStore(_ string, _ *beads.BdStore) (beads.Store, bool) {
	return nil, false
}
