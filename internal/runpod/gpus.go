package runpod

import (
	"cmp"
	"context"
	"math"
	"net/http"
	"slices"
)

// Choosing GPUs (D111): the dialog lists the GPUs RunPod rents on Secure
// Cloud that can encode with NVENC, with their price per hour from RunPod's
// catalog, and the rate the spend estimate uses is the dearest one chosen.

// DefaultCatalog is RunPod's v2 API, where the GPU catalog lives.
const DefaultCatalog = "https://api.runpod.io/v2"

// nvenc are the GPU types whose NVENC the worker can use on RunPod. Data
// centre cards without NVENC (A100, H100, H200, B200) would encode on the
// CPU, and RunPod's RTX 4090 hosts refuse NVENC sessions (D097).
var nvenc = map[string]bool{
	"NVIDIA L4": true, "NVIDIA L40": true, "NVIDIA L40S": true,
	"NVIDIA RTX A4000": true, "NVIDIA RTX A4500": true, "NVIDIA RTX A5000": true, "NVIDIA RTX A6000": true,
	"NVIDIA A40": true, "NVIDIA RTX 2000 Ada Generation": true, "NVIDIA RTX 4000 Ada Generation": true,
	"NVIDIA RTX 6000 Ada Generation": true,
}

// recommended is the best value for encoding: Ada's NVENC with AV1, two
// encoders and a low price, and the type proven on a real pod (D097).
const recommended = "NVIDIA L4"

// GPU is one choice in the dialog.
type GPU struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MemoryGB     int    `json:"memory_gb"`
	CentsPerHour int32  `json:"cents_per_hour"`
	Recommended  bool   `json:"recommended,omitempty"`
}

// GPUs lists the NVENC GPUs RunPod rents on Secure Cloud with key, the
// recommended one first, then by price.
func (c *Connector) GPUs(ctx context.Context, key string) ([]GPU, error) {
	var out struct {
		GPUs []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Memory int    `json:"memory"`
			Secure bool   `json:"secure"`
			Price  struct {
				Secure float64 `json:"secure"`
			} `json:"price"`
		} `json:"gpus"`
	}
	if err := NewClient(cmp.Or(c.Catalog, DefaultCatalog), key).do(ctx, "list gpus", http.MethodGet, "/catalog/gpus", nil, &out); err != nil {
		return nil, err
	}
	list := []GPU{}
	for _, g := range out.GPUs {
		if !nvenc[g.ID] || !g.Secure || g.Price.Secure <= 0 {
			continue
		}
		list = append(list, GPU{ID: g.ID, Name: g.Name, MemoryGB: g.Memory,
			CentsPerHour: int32(math.Ceil(g.Price.Secure * 100)), Recommended: g.ID == recommended})
	}
	slices.SortStableFunc(list, func(a, b GPU) int {
		if a.Recommended != b.Recommended {
			if a.Recommended {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.CentsPerHour, b.CentsPerHour)
	})
	return list, nil
}

// rateFor is the dearest of ids among gpus, in cents per hour; zero when
// none of them is listed.
func rateFor(gpus []GPU, ids []string) int32 {
	var rate int32
	for _, g := range gpus {
		if slices.Contains(ids, g.ID) {
			rate = max(rate, g.CentsPerHour)
		}
	}
	return rate
}
