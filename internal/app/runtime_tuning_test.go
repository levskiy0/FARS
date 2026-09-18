package app

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"fars/internal/config"
	"fars/internal/metrics"
)

func gaugeValue(t *testing.T, name string) float64 {
	t.Helper()
	families, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() == name {
			for _, m := range family.GetMetric() {
				return m.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("metric %q is not exposed", name)
	return 0
}

// TestRuntimeTuningLeavesTheRuntimeAlone is the point of the change: since Go
// 1.25 the runtime reads the cgroup CPU quota itself, so the correct thing to
// do with an unset gomaxprocs is nothing at all. Overriding it with a
// hand-maintained number is what let a container ask for more threads than its
// quota could run.
func TestRuntimeTuningLeavesTheRuntimeAlone(t *testing.T) {
	before := runtime.GOMAXPROCS(0)
	t.Cleanup(func() { runtime.GOMAXPROCS(before) })

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	applyRuntimeTuning(logger, &config.Config{})

	if after := runtime.GOMAXPROCS(0); after != before {
		t.Fatalf("GOMAXPROCS changed from %d to %d with nothing configured", before, after)
	}
	if got := gaugeValue(t, "fars_gomaxprocs"); got != float64(before) {
		t.Errorf("fars_gomaxprocs = %v, want %d", got, before)
	}
	// libvips is never left to count the host's CPUs, even with nothing set.
	if got := gaugeValue(t, "fars_vips_concurrency"); got != 1 {
		t.Errorf("fars_vips_concurrency = %v, want 1", got)
	}
	if !strings.Contains(buf.String(), "GOMAXPROCS derived from the CPU limit") {
		t.Errorf("startup log does not report the effective value: %q", buf.String())
	}
}

func writeCgroupV2(t *testing.T, contents string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cpu.max"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = previous })
}

// TestDetectCPUQuota covers the forms the kernel actually writes. The parser
// exists because runtime.GOMAXPROCS(0) cannot answer the question: a
// GOMAXPROCS environment variable — which is also one of FARS's own legacy
// config shortcuts — overrides the quota inside the runtime, so the
// deployments most likely to have pinned it are exactly the ones where the
// runtime can no longer report the limit.
func TestDetectCPUQuota(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contents string
		want     float64
	}{
		{"two cpus", "200000 100000\n", 2},
		{"fractional", "50000 100000\n", 0.5},
		{"six cpus", "600000 100000\n", 6},
		{"unlimited", "max 100000\n", 0},
		{"malformed", "nonsense\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeCgroupV2(t, tc.contents)
			if got := detectCPUQuota(); got != tc.want {
				t.Fatalf("detectCPUQuota() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRuntimeTuningWarnsAboutOversubscription(t *testing.T) {
	before := runtime.GOMAXPROCS(0)
	t.Cleanup(func() { runtime.GOMAXPROCS(before) })
	// A two-CPU container, asked for four threads: the shape that produced
	// throttling in 72% of scheduling periods in production.
	writeCgroupV2(t, "200000 100000\n")

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	applyRuntimeTuning(logger, &config.Config{Runtime: config.RuntimeConfig{GOMAXPROCS: 4}})

	if got := runtime.GOMAXPROCS(0); got != 4 {
		t.Fatalf("GOMAXPROCS = %d, want the configured 4", got)
	}
	if got := gaugeValue(t, "fars_gomaxprocs"); got != 4 {
		t.Errorf("fars_gomaxprocs = %v, want 4", got)
	}
	if got := gaugeValue(t, "fars_cpu_quota"); got != 2 {
		t.Errorf("fars_cpu_quota = %v, want 2", got)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "throttle") {
		t.Errorf("no throttling warning for a pin above the quota: %q", out)
	}
	if !strings.Contains(out, "cpu_quota=2") {
		t.Errorf("the warning does not name the quota it compared against: %q", out)
	}
}

func TestRuntimeTuningAcceptsAPinWithinTheLimit(t *testing.T) {
	before := runtime.GOMAXPROCS(0)
	t.Cleanup(func() { runtime.GOMAXPROCS(before) })
	writeCgroupV2(t, "600000 100000\n")

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	applyRuntimeTuning(logger, &config.Config{Runtime: config.RuntimeConfig{GOMAXPROCS: 6}})

	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a pin equal to the quota should not warn: %q", buf.String())
	}
	if got := gaugeValue(t, "fars_cpu_quota"); got != 6 {
		t.Errorf("fars_cpu_quota = %v, want 6", got)
	}
}
