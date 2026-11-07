package sandbox

import (
	"slices"
	"testing"
)

func TestTheCPUBudgetCountsEveryThread(t *testing.T) {
	// 6 hours on each CPU: an 8-thread encode isn't killed after 45 minutes.
	for cpus, want := range map[int]uint64{0: 21600, 1: 21600, 8: 172800, 32: 691200} {
		if got := CPUBudget(cpus); got != want {
			t.Errorf("CPUBudget(%d) = %d, want %d", cpus, got, want)
		}
	}
	args := Args(Policy{Write: "/w", CPUSeconds: CPUBudget(8)}, "/usr/bin/ffmpeg")
	if i := slices.Index(args, "--cpu-seconds"); i < 0 || args[i+1] != "172800" {
		t.Fatalf("args = %v", args)
	}
}
