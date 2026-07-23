package main

import (
	"errors"
	"os"
	"strconv"
)

const (
	// doltDiskDefaultMinFreeBytes is the critical floor (500 MiB). Below this
	// threshold disk-growing store maintenance is refused to prevent ENOSPC
	// crashes.
	doltDiskDefaultMinFreeBytes = 500 << 20 // 500 MiB

	// doltDiskDefaultWarnFreeBytes is the soft floor (2 GiB). Below this
	// threshold a warning is emitted but operations are not blocked.
	doltDiskDefaultWarnFreeBytes = 2 << 30 // 2 GiB
)

// errDiskPreflightUnsupported is returned by the Windows stub (and any
// platform where statfs is unavailable). Call sites that receive this error
// must fail-open without logging — it is not a probe failure.
var errDiskPreflightUnsupported = errors.New("disk preflight unavailable on this platform")

// doltDiskMinFreeBytes returns the critical floor from GC_BEADS_MIN_FREE_BYTES,
// defaulting to 500 MiB. Zero disables the check entirely.
func doltDiskMinFreeBytes() int64 {
	if v := os.Getenv("GC_BEADS_MIN_FREE_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return doltDiskDefaultMinFreeBytes
}

// doltDiskWarnFreeBytes returns the soft floor from GC_BEADS_WARN_FREE_BYTES,
// defaulting to 2 GiB.
func doltDiskWarnFreeBytes() int64 {
	if v := os.Getenv("GC_BEADS_WARN_FREE_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return doltDiskDefaultWarnFreeBytes
}
