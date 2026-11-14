package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/visheshgubrani/clipmux/internal/sandbox"
)

// allowedDemuxers are the only input formats ffmpeg may open. HLS playlists
// and concat lists are left out: a file disguised as one can pull local
// files into the output.
const allowedDemuxers = "mov,matroska,webm,avi,flv,mpegts,mxf,mpeg,ogg,wav,mp3,aac,flac,asf"

// inputGuard goes before every -i that reads a source.
func inputGuard() []string {
	return []string{"-format_whitelist", allowedDemuxers, "-protocol_whitelist", "file"}
}

// ErrUnsupportedSource is a permanent failure: the source is corrupt, has
// no video, or is in a format outside the allowlist.
var ErrUnsupportedSource = errors.New("media: unsupported or corrupt source")

// Tools are the binaries the pipeline runs.
type Tools struct {
	FFmpeg, FFprobe, Packager string
	// Whisper is whisper-cli, or "" on a worker that doesn't transcribe.
	Whisper string
	// Sandbox, when set, confines every tool (internal/sandbox).
	Sandbox *Sandbox
	// HW is what this worker's GPUs proved they can do; the zero value
	// encodes on the CPU.
	HW Hardware
	// Env are GPU driver variables for the tools (sandbox.EnvAllowed).
	Env []string
}

// Sandbox runs each tool as `Exec sandbox-exec … -- tool args`.
type Sandbox struct {
	// Exec is the clipmux binary.
	Exec string
	// Dir is the only directory the tools may write: one attempt's.
	Dir string
	// Read are files outside Dir the tools may read: a PC agent's local
	// source, read where it lies.
	Read []string
	// MemoryMB bounds each tool's address space (0: the default).
	MemoryMB uint64
	// GPU and Devices are set for one GPU stage (Tools.forStage).
	GPU     bool
	Devices []string
}

// In returns the tools confined to dir, for one attempt, also allowed to
// read the given files.
func (t Tools) In(dir string, read ...string) Tools {
	if t.Sandbox != nil {
		s := *t.Sandbox
		s.Dir = dir
		s.Read = append([]string(nil), read...)
		t.Sandbox = &s
	}
	return t
}

// DefaultTools finds the binaries on PATH.
func DefaultTools() Tools {
	return Tools{FFmpeg: "ffmpeg", FFprobe: "ffprobe", Packager: "packager", Whisper: "whisper-cli"}
}

// Versions reports the tool versions, e.g. "8.0.1-3ubuntu2" and "v3.9.3".
func (t Tools) Versions(ctx context.Context) (ToolVersions, error) {
	var ff, pk bytes.Buffer
	if err := t.run(ctx, t.FFmpeg, []string{"-hide_banner", "-version"}, "", &ff); err != nil {
		return ToolVersions{}, err
	}
	if err := t.run(ctx, t.Packager, []string{"--version"}, "", &pk); err != nil {
		return ToolVersions{}, err
	}
	v := ToolVersions{}
	if f := strings.Fields(ff.String()); len(f) >= 3 {
		v.FFmpeg = f[2]
	}
	if f := strings.Fields(pk.String()); len(f) >= 3 {
		v.Packager, _, _ = strings.Cut(f[2], "-")
	}
	return v, nil
}

// run executes a tool with no inherited environment (so no secrets), the
// given working directory and the caller's deadline, and through the sandbox
// when there is one. stderr is kept only as a short tail for the error, and
// never contains a key because keys go through a flag file.
func (t Tools) run(ctx context.Context, bin string, args []string, dir string, stdout io.Writer) error {
	return t.runLogged(ctx, bin, args, dir, stdout, nil)
}

// runLogged is run that also copies the tool's stderr to log, for a probe
// that reads what the tool says about itself.
func (t Tools) runLogged(ctx context.Context, bin string, args []string, dir string, stdout, log io.Writer) error {
	name := filepath.Base(bin)
	if t.Sandbox != nil {
		abs, err := exec.LookPath(bin)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		policy := sandbox.Policy{
			Write: t.Sandbox.Dir, Read: t.Sandbox.Read, MemoryMB: t.Sandbox.MemoryMB,
			CPUSeconds: sandbox.CPUBudget(runtime.NumCPU()),
			GPU:        t.Sandbox.GPU, Devices: t.Sandbox.Devices, Env: t.Env,
		}
		args, bin = sandbox.Args(policy, append([]string{abs}, args...)...), t.Sandbox.Exec
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = sandbox.Env(t.Env)
	cmd.Dir = dir
	cmd.Stdout = stdout
	var stderr tail
	cmd.Stderr = &stderr
	if log != nil {
		cmd.Stderr = io.MultiWriter(&stderr, log)
	}
	cmd.WaitDelay = 5 * time.Second
	sandbox.DieWithParent(cmd)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// tail keeps the last 2 KiB written to it.
type tail struct{ b []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2048 {
		t.b = t.b[len(t.b)-2048:]
	}
	return len(p), nil
}

func (t *tail) String() string { return string(t.b) }

// Source is what the pipeline needs to know about an input.
type Source struct {
	Width, Height int    // as displayed, after rotation
	FrameRate     string // ffmpeg rational, e.g. "30000/1001"
	FPS           float64
	Duration      float64
	HasAudio      bool
	// Codec and PixFmt decide whether a GPU can decode the source.
	Codec, PixFmt string
}

// Probe inspects a source with ffprobe.
func Probe(ctx context.Context, t Tools, path string) (Source, error) {
	var out bytes.Buffer
	args := append(inputGuard(), "-v", "error", "-show_streams", "-show_format", "-of", "json", "-i", path)
	if err := t.run(ctx, t.FFprobe, args, filepath.Dir(path), &out); err != nil {
		if ctx.Err() != nil {
			return Source{}, err
		}
		return Source{}, fmt.Errorf("%w: %v", ErrUnsupportedSource, err)
	}
	var probe struct {
		Streams []struct {
			CodecType    string `json:"codec_type"`
			CodecName    string `json:"codec_name"`
			PixFmt       string `json:"pix_fmt"`
			Width        int    `json:"width"`
			Height       int    `json:"height"`
			RFrameRate   string `json:"r_frame_rate"`
			SideDataList []struct {
				Rotation float64 `json:"rotation"`
			} `json:"side_data_list"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out.Bytes(), &probe); err != nil {
		return Source{}, fmt.Errorf("%w: unreadable probe output", ErrUnsupportedSource)
	}
	var src Source
	video := false
	for _, s := range probe.Streams {
		switch s.CodecType {
		case "video":
			if video || s.Width <= 0 || s.Height <= 0 {
				continue
			}
			video = true
			src.Width, src.Height = s.Width, s.Height
			for _, sd := range s.SideDataList {
				if r := int(sd.Rotation) % 180; r == 90 || r == -90 {
					src.Width, src.Height = src.Height, src.Width
				}
			}
			src.FrameRate, src.FPS = frameRate(s.RFrameRate)
			src.Codec, src.PixFmt = s.CodecName, s.PixFmt
		case "audio":
			src.HasAudio = true
		}
	}
	src.Duration, _ = strconv.ParseFloat(probe.Format.Duration, 64)
	if !video || src.Duration <= 0 || min(src.Width, src.Height) < 16 {
		return Source{}, fmt.Errorf("%w: no usable video stream", ErrUnsupportedSource)
	}
	return src, nil
}

// frameRate parses an ffprobe rational, falling back to 30 fps for missing
// or absurd values (variable-frame-rate sources are encoded at constant rate).
func frameRate(r string) (string, float64) {
	num, den, ok := strings.Cut(r, "/")
	n, err1 := strconv.ParseFloat(num, 64)
	d, err2 := strconv.ParseFloat(den, 64)
	if !ok || err1 != nil || err2 != nil || d <= 0 || n/d < 1 || n/d > 120 {
		return "30", 30
	}
	return r, n / d
}
