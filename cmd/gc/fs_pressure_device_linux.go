//go:build linux

package main

import (
	"os"
	"time"
)

// diskstatsPath is the Linux block-device statistics file. It is a var so
// tests can inject a fake.
var diskstatsPath = "/proc/diskstats"

// diskstatsReadFile is the reader used to load diskstatsPath. It is a var so
// tests can substitute a fake without touching the real /proc.
var diskstatsReadFile = os.ReadFile

// currentDeviceBusyPercent returns the busiest real block device's utilization
// since the previous call. The second return is false when no rate is
// available yet (first call, unreadable diskstats), and callers must then fail
// open rather than shed work on an unverified signal.
func currentDeviceBusyPercent() (float64, bool) {
	data, err := diskstatsReadFile(diskstatsPath)
	if err != nil {
		return 0, false
	}
	return fsDeviceBusySampler.sampleAt(string(data), time.Now())
}
