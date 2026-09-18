package app

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cgroupRoot is a variable so the parser can be tested against fixtures.
var cgroupRoot = "/sys/fs/cgroup"

// detectCPUQuota returns the CPU limit the cgroup imposes, in CPUs, or 0 when
// there is no limit or it cannot be read.
//
// runtime.GOMAXPROCS(0) is not a substitute. The Go runtime applies the quota
// itself, but a GOMAXPROCS environment variable takes precedence over it — and
// `GOMAXPROCS` is also one of FARS's own legacy config shortcuts, so the very
// deployments most likely to have pinned it are the ones where the runtime can
// no longer say what the limit was. Reading the quota is the only answer that
// holds however the value arrived.
func detectCPUQuota() float64 {
	// cgroup v2: "$MAX $PERIOD", where MAX is "max" when unlimited.
	if data, err := os.ReadFile(filepath.Join(cgroupRoot, "cpu.max")); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) == 2 && fields[0] != "max" {
			quota, qErr := strconv.ParseFloat(fields[0], 64)
			period, pErr := strconv.ParseFloat(fields[1], 64)
			if qErr == nil && pErr == nil && quota > 0 && period > 0 {
				return quota / period
			}
		}
		return 0
	}

	// cgroup v1: two files, and a quota of -1 means unlimited.
	quota, err := readCgroupInt(filepath.Join(cgroupRoot, "cpu", "cpu.cfs_quota_us"))
	if err != nil || quota <= 0 {
		return 0
	}
	period, err := readCgroupInt(filepath.Join(cgroupRoot, "cpu", "cpu.cfs_period_us"))
	if err != nil || period <= 0 {
		return 0
	}
	return float64(quota) / float64(period)
}

func readCgroupInt(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
}
