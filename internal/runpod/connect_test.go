package runpod

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckSettingsNamesMissingDefaultImage(t *testing.T) {
	err := (&Connector{}).CheckSettings(Request{})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "CLIPMUX_WORKER_IMAGE") {
		t.Fatalf("CheckSettings with no image and no default = %v, want ErrInvalid naming CLIPMUX_WORKER_IMAGE", err)
	}
}
