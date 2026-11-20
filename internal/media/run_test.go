package media

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// F21: six libx264 encoders that fail at frame 0 still print their
// statistics, kilobytes of it, so a tail alone kept only the statistics and
// the malloc failure that explained everything was lost.
func TestAToolsErrorKeepsItsFirstErrorLinesAsWellAsTheTail(t *testing.T) {
	script := `#!/bin/sh
echo 'Input #0, matroska,webm, from source:' >&2
echo 'x264 [error]: malloc of size 26453440 failed' >&2
echo '[vost#0:0/libx264 @ 0x5] Error submitting video frame to the encoder' >&2
i=0
while [ $i -lt 100 ]; do
  echo "[libx264 @ 0x5] mb P  I16..4:  7.3%  0.4%  0.0%  P16..4: 12.1%  0.4%  0.1% skip:79.8% $i" >&2
  i=$((i+1))
done
echo 'Conversion failed!' >&2
exit 187
`
	bin := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	err := Tools{}.run(context.Background(), bin, nil, t.TempDir(), nil)
	if err == nil {
		t.Fatal("the tool failed, but run returned nil")
	}
	msg := err.Error()
	for _, want := range []string{
		"exit status 187",
		"x264 [error]: malloc of size 26453440 failed",
		"Error submitting video frame to the encoder",
		"Conversion failed!",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "Input #0") {
		t.Errorf("the error keeps a line that isn't an error:\n%s", msg)
	}
	if len(msg) > 4096 {
		t.Errorf("the error is %d bytes; it should stay short", len(msg))
	}
}
