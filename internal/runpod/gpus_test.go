package runpod

import (
	"slices"
	"testing"
)

// Every default GPU must encode on NVENC. RunPod's RTX 4090 hosts refused
// NVENC sessions on a real pod even unsandboxed (D097), so pods on them
// encoded on the CPU at GPU prices.
func TestTheDefaultGPUsAreOnesWhereNVENCWorks(t *testing.T) {
	want := []string{"NVIDIA L4", "NVIDIA RTX A4000", "NVIDIA RTX A5000"}
	if !slices.Equal(DefaultGPUTypes, want) {
		t.Fatalf("DefaultGPUTypes = %v, want %v", DefaultGPUTypes, want)
	}
}
