// Package media turns a source file into an output (docs/architecture.md,
// "Output format"): ffmpeg encodes the whole ladder in one pass with
// keyframes aligned to 6-second segments, and Shaka Packager cuts CMAF fMP4
// segments, encrypts each whole segment with AES-128 and writes the
// playlists. Audio is its own rendition, shared by every video rendition.
//
// Every tool runs sandboxed (run.go): an empty environment, its own work
// directory, a wall-clock limit, and input demuxer and protocol allowlists.
package media

import (
	"fmt"
	"regexp"
	"strings"
)

// OutputFormat is the version of the output layout below.
const OutputFormat = 1

// SegmentSeconds is the segment duration for video and audio alike.
const SegmentSeconds = 6

// outputPath is the output grammar: the only names a worker may write. It
// has no name for a key file, so a content key can never be uploaded.
var outputPath = regexp.MustCompile(`^(manifest\.json|master\.m3u8|poster\.jpg|([1-9][0-9]{1,3}p|audio)/(index\.m3u8|init\.mp4|seg_[0-9]{5,6}\.m4s))$`)

// ValidOutputPath reports whether p is a name an output may contain,
// relative to the output's prefix.
func ValidOutputPath(p string) bool { return outputPath.MatchString(p) }

// Rung is one video rendition of the ladder, named by its short side.
type Rung struct {
	Height  int // the short side, in pixels
	Bitrate string
	Maxrate string
	Bufsize string
}

// Label is the rendition's directory name, e.g. "720p".
func (r Rung) Label() string { return fmt.Sprintf("%dp", r.Height) }

// Dimensions scales a source so its short side is the rung, keeping the
// aspect ratio and both sides even (H.264 4:2:0 needs even sizes).
func (r Rung) Dimensions(srcW, srcH int) (w, h int) {
	even := func(v float64) int { return int(v/2+0.5) * 2 }
	if srcW >= srcH {
		return even(float64(srcW) * float64(r.Height) / float64(srcH)), r.Height
	}
	return r.Height, even(float64(srcH) * float64(r.Height) / float64(srcW))
}

// defaultLadder is 1080p, 720p, 480p and 360p, with clipmux-lms's
// production bitrates.
var defaultLadder = []Rung{
	{1080, "5M", "6M", "10M"},
	{720, "3M", "3.5M", "6M"},
	{480, "1.5M", "1.8M", "3M"},
	{360, "800k", "1M", "2M"},
}

// SelectLadder returns the default ladder capped at the source's short side.
// A source smaller than the lowest rung gets one rendition at its own size.
func SelectLadder(width, height int) []Rung {
	short := min(width, height)
	var out []Rung
	for _, r := range defaultLadder {
		if r.Height <= short {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		low := defaultLadder[len(defaultLadder)-1]
		low.Height = short &^ 1 // even
		out = []Rung{low}
	}
	return out
}

// Key is the content key an output is encrypted with.
type Key struct {
	Version int32
	Key     [16]byte
	KeyID   [16]byte
}

// KeyURI is the key reference every media playlist carries: relative, so it
// resolves to .../{playback_id}/{output_id}/key/{version} with the token
// still in the path.
func (k Key) KeyURI() string { return fmt.Sprintf("../key/%d", k.Version) }

// Manifest describes an output; delivery never serves it.
type Manifest struct {
	OutputFormat    int          `json:"output_format"`
	DurationSeconds float64      `json:"duration_seconds"`
	Encryption      string       `json:"encryption"`
	KeyVersion      int32        `json:"key_version"`
	Renditions      []Rendition  `json:"renditions"`
	Audio           bool         `json:"audio"`
	Files           []File       `json:"files"`
	Tools           ToolVersions `json:"tools"`
}

// Rendition is one video rendition in a manifest.
type Rendition struct {
	Name   string `json:"name"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// File is one file of an output, other than manifest.json.
type File struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// ToolVersions records what produced an output.
type ToolVersions struct {
	FFmpeg   string `json:"ffmpeg"`
	Packager string `json:"packager"`
}

var keyLine = regexp.MustCompile(`^#EXT-X-KEY:METHOD=([A-Z0-9-]+),URI="([^"]*)"`)
var mapLine = regexp.MustCompile(`^#EXT-X-MAP:URI="([^"]*)"`)
var segmentName = regexp.MustCompile(`^seg_[0-9]{5,6}\.m4s$`)

// checkMediaPlaylist rejects a media playlist that isn't fully encrypted
// with the expected key URI, or that references anything outside its own
// directory. The server repeats this check at commit.
func checkMediaPlaylist(text string, keyVersion int32) error {
	wantURI := Key{Version: keyVersion}.KeyURI()
	keys, segments := 0, 0
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			m := keyLine.FindStringSubmatch(line)
			if m == nil || m[1] != "AES-128" || m[2] != wantURI {
				return fmt.Errorf("media playlist key %q, want METHOD=AES-128 with URI %q", line, wantURI)
			}
			keys++
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			if m := mapLine.FindStringSubmatch(line); m == nil || m[1] != "init.mp4" {
				return fmt.Errorf("media playlist init %q, want init.mp4", line)
			}
		case line == "" || strings.HasPrefix(line, "#"):
		default:
			if !segmentName.MatchString(line) {
				return fmt.Errorf("media playlist references %q", line)
			}
			segments++
		}
	}
	if keys == 0 {
		return fmt.Errorf("media playlist is not encrypted")
	}
	if segments == 0 {
		return fmt.Errorf("media playlist has no segments")
	}
	return nil
}
