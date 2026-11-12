package modal

import (
	"fmt"
	"math"
	"strings"
)

// Choosing GPUs (D111). Modal has no price API, so the GPUs a sandbox can
// encode on with NVENC carry Modal's published per-second prices
// (modal.com/pricing, 2026-11-12). A100, H100, H200 and B200 have no NVENC.
// The price shown, and the rate the spend estimate uses, is the GPU plus
// the CPU and memory each sandbox gets.

// GPU is one choice in the dialog.
type GPU struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MemoryGB     int    `json:"memory_gb"`
	CentsPerHour int32  `json:"cents_per_hour"`
	Recommended  bool   `json:"recommended,omitempty"`
}

// Modal's prices in dollars per second.
const (
	cpuCorePerSecond = 0.0000131
	memGiBPerSecond  = 0.00000222
)

var catalog = []struct {
	id, name  string
	memoryGB  int
	perSecond float64
	recommend bool
}{
	// The L4: Ada's NVENC with AV1 at a low price, proven on Modal (D051).
	{"L4", "L4", 24, 0.000222, true},
	{"T4", "T4", 16, 0.000164, false},
	{"A10G", "A10", 24, 0.000306, false},
	{"L40S", "L40S", 48, 0.000542, false},
}

// GPUs lists the choices, recommended first, priced for a sandbox of cpu
// cores and memMiB of memory.
func GPUs(cpu float64, memMiB int) []GPU {
	out := make([]GPU, len(catalog))
	for i, g := range catalog {
		perHour := (g.perSecond + cpu*cpuCorePerSecond + float64(memMiB)/1024*memGiBPerSecond) * 3600
		out[i] = GPU{ID: g.id, Name: g.name, MemoryGB: g.memoryGB, CentsPerHour: int32(math.Ceil(perHour * 100)), Recommended: g.recommend}
	}
	return out
}

// rateFor is the dearest GPU in list ("L4,T4"), priced for the sandbox's
// size; an unknown GPU is an error.
func rateFor(list string, cpu float64, memMiB int) (int32, error) {
	prices := map[string]int32{}
	for _, g := range GPUs(cpu, memMiB) {
		prices[g.ID] = g.CentsPerHour
	}
	var rate int32
	for g := range strings.SplitSeq(list, ",") {
		g = strings.TrimSpace(g)
		p, ok := prices[g]
		if !ok {
			return 0, fmt.Errorf("%w: gpu %q isn't one ClipMux can encode on; choose L4, T4, A10G or L40S", ErrInvalid, g)
		}
		rate = max(rate, p)
	}
	return rate, nil
}
