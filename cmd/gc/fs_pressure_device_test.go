package main

import (
	"bytes"
	"strconv"
	"testing"
	"time"
)

// diskstatsLine builds a minimal /proc/diskstats line. Field 13 (column 13 of
// the line) is "ms spent doing IO", which device utilization is derived from.
func diskstatsLine(device string, msDoingIO uint64) string {
	// major minor name reads rd_merged rd_sectors rd_ms writes wr_merged
	// wr_sectors wr_ms in_flight ms_doing_io weighted_ms
	return "   259       0 " + device + " 0 0 0 0 0 0 0 0 0 " +
		strconv.FormatUint(msDoingIO, 10) + " 0\n"
}

func diskstatsSample(t *testing.T, device string, msDoingIO uint64) string {
	t.Helper()
	return diskstatsLine(device, msDoingIO)
}

// withSaturatedDevice makes currentDeviceBusyPercent report a fully busy block
// device for the duration of the test, so tests that exercise the pressure-shed
// path still reach it now that a high PSI reading alone no longer sheds work.
func withSaturatedDevice(t *testing.T) {
	t.Helper()
	prevPath, prevRead, prevSampler := diskstatsPath, diskstatsReadFile, fsDeviceBusySampler
	t.Cleanup(func() {
		diskstatsPath, diskstatsReadFile, fsDeviceBusySampler = prevPath, prevRead, prevSampler
	})

	diskstatsPath = "/test/diskstats"
	busyMs := uint64(0)
	diskstatsReadFile = func(string) ([]byte, error) {
		// Advance far more device-busy time than can elapse between calls, so
		// utilization always clamps to 100%.
		busyMs += 60_000
		return []byte(diskstatsLine("nvme0n1", busyMs)), nil
	}
	fsDeviceBusySampler = newDeviceBusySampler()
	// Prime the sampler so the next observation already yields a rate.
	currentDeviceBusyPercent()
}

// withIdleDevice makes currentDeviceBusyPercent report an essentially idle
// device — the btrfs-host reading that must NOT shed work.
func withIdleDevice(t *testing.T) {
	t.Helper()
	prevPath, prevRead, prevSampler := diskstatsPath, diskstatsReadFile, fsDeviceBusySampler
	t.Cleanup(func() {
		diskstatsPath, diskstatsReadFile, fsDeviceBusySampler = prevPath, prevRead, prevSampler
	})

	diskstatsPath = "/test/diskstats"
	diskstatsReadFile = func(string) ([]byte, error) {
		return []byte(diskstatsLine("nvme0n1", 1_000)), nil
	}
	fsDeviceBusySampler = newDeviceBusySampler()
	currentDeviceBusyPercent()
}

// TestShouldSkipTickWithHighPSIButIdleDevice is the end-to-end regression guard
// for the btrfs false positive: PSI far above threshold, block device idle —
// the supervisor must keep ticking. Before this gate the supervisor throttled
// itself indefinitely on such hosts, starving 30s orders to ~4-minute cadence.
func TestShouldSkipTickWithHighPSIButIdleDevice(t *testing.T) {
	withFakePressureFile(t, []byte(samplePressureHigh), nil)
	withIdleDevice(t)
	t.Setenv(fsPressureThresholdEnv, "")

	var buf bytes.Buffer
	cr := &CityRuntime{stderr: &buf}
	if cr.shouldSkipTickForFSPressure(nil, "patrol") {
		t.Fatal("skipped a tick while the block device was idle; PSI alone must not shed work")
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no pressure log when the device is idle, got %q", buf.String())
	}
}

// TestDeviceBusyPercentNeedsTwoSamples proves the first observation cannot
// produce a utilization figure (there is no delta yet) and reports unknown, so
// the caller fails open rather than inventing a number.
func TestDeviceBusyPercentNeedsTwoSamples(t *testing.T) {
	s := newDeviceBusySampler()
	base := time.Unix(1000, 0)

	if _, known := s.sampleAt(diskstatsSample(t, "nvme0n1", 0), base); known {
		t.Fatal("first sample reported a known utilization; want unknown")
	}
	got, known := s.sampleAt(diskstatsSample(t, "nvme0n1", 500), base.Add(time.Second))
	if !known {
		t.Fatal("second sample reported unknown; want a computed utilization")
	}
	if got < 49 || got > 51 {
		t.Fatalf("utilization = %.1f, want ~50 (500ms busy over 1000ms)", got)
	}
}

// TestDeviceBusyPercentIdleDisk is the regression guard for the btrfs false
// positive that motivated this gate: PSI reports heavy IO stall because btrfs
// endio/delalloc kworkers sit in uninterruptible sleep, while the block device
// is essentially idle. Utilization must reflect the device, not the kworkers.
func TestDeviceBusyPercentIdleDisk(t *testing.T) {
	s := newDeviceBusySampler()
	base := time.Unix(2000, 0)
	s.sampleAt(diskstatsSample(t, "nvme0n1", 1_000_000), base)

	// 32ms of device time over a 5s window — the real-world idle reading.
	got, known := s.sampleAt(diskstatsSample(t, "nvme0n1", 1_000_032), base.Add(5*time.Second))
	if !known {
		t.Fatal("utilization unknown after two samples")
	}
	if got > 1.0 {
		t.Fatalf("idle disk utilization = %.2f%%, want <1%%", got)
	}
}

// TestDeviceBusyPercentIgnoresVirtualDevices proves loop/ram/zram devices do
// not drive the reading — only real block devices should gate real work.
func TestDeviceBusyPercentIgnoresVirtualDevices(t *testing.T) {
	s := newDeviceBusySampler()
	base := time.Unix(3000, 0)
	first := diskstatsSample(t, "nvme0n1", 0) + diskstatsSample(t, "zram0", 0) + diskstatsSample(t, "loop0", 0)
	second := diskstatsSample(t, "nvme0n1", 10) + diskstatsSample(t, "zram0", 990) + diskstatsSample(t, "loop0", 990)
	s.sampleAt(first, base)

	got, known := s.sampleAt(second, base.Add(time.Second))
	if !known {
		t.Fatal("utilization unknown")
	}
	if got > 5 {
		t.Fatalf("utilization = %.1f%%, want ~1%% (zram/loop must be ignored)", got)
	}
}

// TestDeviceBusyPercentClampsToHundred proves a multi-queue device reporting
// more busy-ms than wall time cannot yield a nonsense >100% figure.
func TestDeviceBusyPercentClampsToHundred(t *testing.T) {
	s := newDeviceBusySampler()
	base := time.Unix(4000, 0)
	s.sampleAt(diskstatsSample(t, "sda", 0), base)

	got, _ := s.sampleAt(diskstatsSample(t, "sda", 9_000), base.Add(time.Second))
	if got > 100 {
		t.Fatalf("utilization = %.1f%%, want clamped to 100", got)
	}
}

// TestFSPressureHighRequiresBusyDevice is the core behavioral fix: a high PSI
// reading alone must NOT mark pressure high when the block device is idle.
// Without this, a btrfs host pins PSI near 100 permanently and the supervisor
// throttles every tick forever.
func TestFSPressureHighRequiresBusyDevice(t *testing.T) {
	for _, tc := range []struct {
		name        string
		psi         float64
		deviceBusy  float64
		deviceKnown bool
		wantHigh    bool
	}{
		{name: "psi high but device idle", psi: 92, deviceBusy: 0.6, deviceKnown: true, wantHigh: false},
		{name: "psi high and device busy", psi: 92, deviceBusy: 88, deviceKnown: true, wantHigh: true},
		{name: "psi low and device busy", psi: 10, deviceBusy: 95, deviceKnown: true, wantHigh: false},
		{name: "psi high device unknown fails open", psi: 92, deviceBusy: 0, deviceKnown: false, wantHigh: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := fsPressureIsHigh(tc.psi, defaultFSPressureThreshold, tc.deviceBusy, tc.deviceKnown)
			if got != tc.wantHigh {
				t.Fatalf("fsPressureIsHigh(psi=%.1f, busy=%.1f, known=%t) = %t, want %t",
					tc.psi, tc.deviceBusy, tc.deviceKnown, got, tc.wantHigh)
			}
		})
	}
}
