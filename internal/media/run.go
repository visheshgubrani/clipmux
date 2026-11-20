package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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

// ErrScratchFull marks a tool that failed because the work directory's disk
// filled. It says nothing about the source or the encoder (bug hunt F21).
var ErrScratchFull = errors.New("media: the work directory's disk is full")

// audioBitrate is the AAC rendition's rate.
const audioBitrate = "128k"

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
// when there is one. stderr is kept only as its first error lines and a
// short tail for the error, and
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
	var stderr stderrLog
	cmd.Stderr = &stderr
	if log != nil {
		cmd.Stderr = io.MultiWriter(&stderr, log)
	}
	cmd.WaitDelay = 5 * time.Second
	sandbox.DieWithParent(cmd)
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "No space left on device") {
			return fmt.Errorf("%s: %w: %w: %s", name, ErrScratchFull, err, msg)
		}
		return fmt.Errorf("%s: %w: %s", name, err, msg)
	}
	return nil
}

// stderrLog keeps what a tool's failure needs: its first few error lines,
// which usually say why, and the last 2 KiB, which say where it stopped. A
// tail alone loses the reason when the tool prints pages of statistics on
// its way out, as six libx264 encoders do.
type stderrLog struct {
	tail []byte
	line []byte // the line being written, up to stderrLineMax bytes
	errs []string
}

const (
	stderrTailMax  = 2048
	stderrErrLines = 8
	stderrLineMax  = 256
)

func (s *stderrLog) Write(p []byte) (int, error) {
	s.tail = append(s.tail, p...)
	if len(s.tail) > stderrTailMax {
		s.tail = s.tail[len(s.tail)-stderrTailMax:]
	}
	for rest := p; len(rest) > 0 && len(s.errs) < stderrErrLines; {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			s.add(rest)
			break
		}
		s.add(rest[:i])
		s.endLine()
		rest = rest[i+1:]
	}
	return len(p), nil
}

func (s *stderrLog) add(p []byte) {
	s.line = append(s.line, p[:min(len(p), stderrLineMax-len(s.line))]...)
}

func (s *stderrLog) endLine() {
	if line := strings.TrimSpace(string(s.line)); isErrorLine(line) {
		s.errs = append(s.errs, line)
	}
	s.line = s.line[:0]
}

// isErrorLine reports whether an ffmpeg, x264 or packager line reports a
// failure rather than progress or stream information.
func isErrorLine(line string) bool {
	l := strings.ToLower(line)
	for _, w := range []string{"error", "failed", "cannot", "invalid", "not supported", "unsupported", "denied"} {
		if strings.Contains(l, w) {
			return true
		}
	}
	return false
}

// String is the error lines the tail no longer holds, then the tail.
func (s *stderrLog) String() string {
	if len(s.errs) < stderrErrLines && len(s.line) > 0 {
		s.endLine()
	}
	tail := string(s.tail)
	var head []string
	for _, e := range s.errs {
		if !strings.Contains(tail, e) {
			head = append(head, e)
		}
	}
	if len(head) == 0 {
		return tail
	}
	return strings.Join(head, "\n") + "\n…\n" + tail
}

// Source is what the pipeline needs to know about an input.
type Source struct {
	Width, Height int    // as displayed, after rotation
	FrameRate     string // ffmpeg rational, e.g. "30000/1001"
	FPS           float64
	Duration      float64
	HasAudio      bool
	// Codec, PixFmt and Rotation decide whether a GPU can decode the source.
	Codec, PixFmt string
	// Rotation is the display matrix's angle in degrees, 0 to 359.
	Rotation int
	// VideoStart is how far the first video frame comes after the file's
	// start, in seconds; the encode subtracts it so renditions start at zero.
	VideoStart float64
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
			StartTime    string `json:"start_time"`
			SideDataList []struct {
				Rotation float64 `json:"rotation"`
			} `json:"side_data_list"`
			Disposition struct {
				AttachedPic int `json:"attached_pic"`
			} `json:"disposition"`
		} `json:"streams"`
		Format struct {
			Duration  string `json:"duration"`
			StartTime string `json:"start_time"`
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
			// An attached picture is an audio file's cover art, not a
			// video (bug hunt F10).
			if video || s.Width <= 0 || s.Height <= 0 || s.Disposition.AttachedPic == 1 {
				continue
			}
			video = true
			src.Width, src.Height = s.Width, s.Height
			vstart, _ := strconv.ParseFloat(s.StartTime, 64)
			fstart, _ := strconv.ParseFloat(probe.Format.StartTime, 64)
			src.VideoStart = max(0, vstart-fstart)
			for _, sd := range s.SideDataList {
				if r := (int(sd.Rotation)%360 + 360) % 360; r != 0 {
					src.Rotation = r
				}
			}
			if src.Rotation == 90 || src.Rotation == 270 {
				src.Width, src.Height = src.Height, src.Width
			}
			src.FrameRate, src.FPS = frameRate(s.RFrameRate)
			src.Codec, src.PixFmt = s.CodecName, s.PixFmt
		case "audio":
			src.HasAudio = true
		}
	}
	src.Duration, _ = strconv.ParseFloat(probe.Format.Duration, 64)
	if video && src.Duration <= 0 {
		// A file written as it was recorded (MediaRecorder's WebM) has no
		// duration in its header: read it from the packets (F17).
		d, err := packetSeconds(ctx, t, path)
		if err != nil {
			if ctx.Err() != nil {
				return Source{}, err
			}
			return Source{}, fmt.Errorf("%w: %v", ErrUnsupportedSource, err)
		}
		src.Duration = d
	}
	if !video || src.Duration <= 0 || min(src.Width, src.Height) < 16 ||
		SelectLadder(src.Width, src.Height, nil)[0].Height < 16 {
		return Source{}, fmt.Errorf("%w: no usable video stream", ErrUnsupportedSource)
	}
	return src, nil
}

// packetSeconds measures the first video stream from its packets' times,
// for a source whose header gives no duration. It reads the file without
// decoding it.
func packetSeconds(ctx context.Context, t Tools, path string) (float64, error) {
	var out bytes.Buffer
	args := append(inputGuard(), "-v", "error", "-select_streams", "v:0",
		"-show_entries", "packet=pts_time,duration_time", "-of", "csv=p=0", "-i", path)
	if err := t.run(ctx, t.FFprobe, args, filepath.Dir(path), &out); err != nil {
		return 0, err
	}
	first, end := math.Inf(1), math.Inf(-1)
	for line := range strings.Lines(out.String()) {
		pts, dur, _ := strings.Cut(strings.TrimSpace(line), ",")
		p, err := strconv.ParseFloat(pts, 64)
		if err != nil {
			continue // a packet without a time
		}
		d, _ := strconv.ParseFloat(dur, 64)
		first, end = min(first, p), max(end, p+d)
	}
	if end <= first {
		return 0, errors.New("no timed video packets")
	}
	return end - first, nil
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
