package main

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultFSDeviceBusyThreshold is the block-device utilization (percent of wall
// time the device had IO in flight) above which the device is considered
// genuinely saturated. PSI alone cannot establish this: see fsPressureIsHigh.
const defaultFSDeviceBusyThreshold = 50.0

// diskstatsMsDoingIOField is the 1-indexed column of "milliseconds spent doing
// IO" in a /proc/diskstats line (major, minor, name, then 11 counters).
const diskstatsMsDoingIOField = 13

// deviceBusySampler converts successive /proc/diskstats readings into a
// block-device utilization percentage. Utilization is a rate, so it needs two
// samples; the first observation only primes the sampler.
//
// The host's disks are shared by every city in a supervisor process, so a
// single shared sampler is semantically correct — see fsDeviceBusySampler.
type deviceBusySampler struct {
	mu       sync.Mutex
	prev     map[string]uint64
	prevTime time.Time
	primed   bool
}

func newDeviceBusySampler() *deviceBusySampler {
	return &deviceBusySampler{prev: make(map[string]uint64)}
}

// fsDeviceBusySampler is the process-wide sampler used by the pressure gate.
var fsDeviceBusySampler = newDeviceBusySampler()

// isVirtualBlockDevice reports whether name is a RAM/loopback-backed device.
// These never represent real storage contention, so they must not gate work —
// a busy zram swap device in particular says nothing about filesystem IO.
func isVirtualBlockDevice(name string) bool {
	switch {
	case strings.HasPrefix(name, "loop"),
		strings.HasPrefix(name, "ram"),
		strings.HasPrefix(name, "zram"):
		return true
	default:
		return false
	}
}

// sampleAt records a /proc/diskstats body observed at now and returns the
// busiest real device's utilization since the previous sample, as a percentage
// of elapsed wall time. The second return is false when no rate can be derived
// yet (first sample, no elapsed time, or no parsable devices), in which case
// callers must fail open rather than assume a value.
//
// Partitions report no more busy time than their parent disk, so taking the
// maximum across all real devices yields the parent's figure without needing to
// model the partition topology.
func (s *deviceBusySampler) sampleAt(diskstats string, now time.Time) (float64, bool) {
	current := parseDiskstatsBusyMs(diskstats)
	if len(current) == 0 {
		return 0, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	primed, prevTime := s.primed, s.prevTime
	prev := s.prev
	s.prev = current
	s.prevTime = now
	s.primed = true

	if !primed {
		return 0, false
	}
	// Sub-millisecond resolution: consecutive observations can land inside the
	// same millisecond, which integer-millisecond math would round to a zero
	// window and discard as unusable.
	elapsed := now.Sub(prevTime)
	if elapsed <= 0 {
		return 0, false
	}
	elapsedMs := float64(elapsed) / float64(time.Millisecond)

	busiest, sawDelta := 0.0, false
	for name, busyMs := range current {
		before, ok := prev[name]
		if !ok || busyMs < before {
			// New device, or counter reset — no usable delta this round.
			continue
		}
		sawDelta = true
		pct := float64(busyMs-before) / elapsedMs * 100
		if pct > busiest {
			busiest = pct
		}
	}
	if !sawDelta {
		return 0, false
	}
	// A multi-queue device can accumulate more busy-ms than wall time.
	if busiest > 100 {
		busiest = 100
	}
	return busiest, true
}

// parseDiskstatsBusyMs maps real block devices to their cumulative
// "ms spent doing IO" counter.
func parseDiskstatsBusyMs(diskstats string) map[string]uint64 {
	out := make(map[string]uint64)
	for _, line := range strings.Split(diskstats, "\n") {
		fields := strings.Fields(line)
		if len(fields) < diskstatsMsDoingIOField {
			continue
		}
		name := fields[2]
		if isVirtualBlockDevice(name) {
			continue
		}
		busyMs, err := strconv.ParseUint(fields[diskstatsMsDoingIOField-1], 10, 64)
		if err != nil {
			continue
		}
		out[name] = busyMs
	}
	return out
}

// fsPressureIsHigh decides whether filesystem pressure warrants shedding work.
//
// PSI "some avg60" counts wall time during which at least one task was stalled
// on IO, which is NOT a saturation signal: on btrfs the endio/endio-write and
// delalloc kworkers sit in uninterruptible sleep as a matter of course, so a
// completely idle disk can report PSI above 90 indefinitely. Gating on PSI
// alone therefore throttles the supervisor forever on such hosts (observed:
// PSI some avg60=92 with the NVMe at 0.6% utilization, which starved 30s
// orders down to one run every ~4 minutes).
//
// Pressure is high only when PSI says tasks are stalling AND the block device
// is actually busy. When device utilization cannot be determined we fail open
// (not high) so an unreadable /proc/diskstats can never wedge the supervisor.
func fsPressureIsHigh(psiAvg60, psiThreshold, deviceBusyPercent float64, deviceBusyKnown bool) bool {
	if psiAvg60 <= psiThreshold {
		return false
	}
	if !deviceBusyKnown {
		return false
	}
	return deviceBusyPercent > defaultFSDeviceBusyThreshold
}
