//go:build !linux

package main

// diskstatsPath is unused on non-Linux but declared so shared code and tests
// can reference it without build-tag juggling.
var diskstatsPath = ""

// diskstatsReadFile mirrors the Linux declaration so cross-platform test
// helpers compile. The non-Linux currentDeviceBusyPercent stub never reads it.
//
//nolint:unused // assigned cross-platform by tests; only read on Linux
var diskstatsReadFile = func(string) ([]byte, error) { return nil, nil }

// currentDeviceBusyPercent reports "unknown" on non-Linux. PSI itself is
// Linux-only (readFSPressureAvg60 returns 0 there), so the pressure gate is
// already a no-op on these platforms.
func currentDeviceBusyPercent() (float64, bool) {
	return 0, false
}
