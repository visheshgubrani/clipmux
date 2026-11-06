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
	"slices"
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
	// Encoder is the stage that encoded the ladder, and the ones that failed
	// before it.
	Encoder *EncoderUsed `json:"encoder,omitempty"`
}

// EncoderUsed records which backend encoded an output.
type EncoderUsed struct {
	Stage     string   `json:"stage"`
	Fallbacks []string `json:"fallbacks,omitempty"`
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

var segmentName = regexp.MustCompile(`^seg_[0-9]{5,6}\.m4s$`)
var attrName = regexp.MustCompile(`^[A-Z0-9-]+$`)
var unsignedInt = regexp.MustCompile(`^[0-9]{1,9}$`)
var extinf = regexp.MustCompile(`^#EXTINF:[0-9]{1,6}(\.[0-9]{1,6})?,$`)
var hexIV = regexp.MustCompile(`^0[xX][0-9A-Fa-f]{32}$`)

// attribute is one value of an HLS attribute list.
type attribute struct {
	value  string
	quoted bool
}

// parseAttributes parses an HLS attribute list (RFC 8216 section 4.2)
// strictly: upper-case names, no duplicates, quoted strings without quotes
// inside, unquoted values without commas, and nothing left over. Lenient
// parsing is how a hostile playlist smuggles a second URI past a check.
func parseAttributes(list string) (map[string]attribute, error) {
	out := map[string]attribute{}
	for rest := list; ; {
		name, after, ok := strings.Cut(rest, "=")
		if !ok || !attrName.MatchString(name) {
			return nil, fmt.Errorf("bad attribute name in %q", list)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("duplicate attribute %s", name)
		}
		var a attribute
		if strings.HasPrefix(after, `"`) {
			end := strings.IndexByte(after[1:], '"')
			if end < 0 {
				return nil, fmt.Errorf("unterminated string in %q", list)
			}
			a = attribute{value: after[1 : end+1], quoted: true}
			after = after[end+2:]
		} else {
			v, tail, more := strings.Cut(after, ",")
			if v == "" || strings.ContainsAny(v, `"`) {
				return nil, fmt.Errorf("bad value for %s", name)
			}
			a = attribute{value: v}
			after = ""
			if more {
				after = "," + tail
			}
		}
		out[name] = a
		if after == "" {
			return out, nil
		}
		if !strings.HasPrefix(after, ",") || after == "," {
			return nil, fmt.Errorf("trailing data in %q", list)
		}
		rest = after[1:]
	}
}

// CheckMediaPlaylist accepts only the media playlist Shaka Packager writes
// for this output format: every line is an allowed tag, the init segment is
// init.mp4, every segment is seg_NNNNN.m4s in the same directory, and every
// segment is preceded by METHOD=AES-128 with the expected key URI. It
// returns the segment names. Workers run it after packaging; the server runs
// it again at commit, because worker output is untrusted.
func CheckMediaPlaylist(text string, keyVersion int32) ([]string, error) {
	wantURI := Key{Version: keyVersion}.KeyURI()
	keyed := false
	var segments []string
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		tag, value, _ := strings.Cut(line, ":")
		switch {
		case line == "" || line == "#EXTM3U" || line == "#EXT-X-ENDLIST" || line == "#EXT-X-INDEPENDENT-SEGMENTS" ||
			line == "#EXT-X-PLAYLIST-TYPE:VOD" || strings.HasPrefix(line, "## "):
		case tag == "#EXT-X-VERSION" || tag == "#EXT-X-TARGETDURATION" || tag == "#EXT-X-MEDIA-SEQUENCE":
			if !unsignedInt.MatchString(value) {
				return nil, fmt.Errorf("media playlist: bad %s", tag)
			}
		case strings.HasPrefix(line, "#EXTINF:"):
			if !extinf.MatchString(line) {
				return nil, fmt.Errorf("media playlist: bad %q", line)
			}
		case tag == "#EXT-X-MAP":
			a, err := parseAttributes(value)
			if err != nil || len(a) != 1 || !a["URI"].quoted || a["URI"].value != "init.mp4" {
				return nil, fmt.Errorf("media playlist init %q, want URI=\"init.mp4\" only", line)
			}
		case tag == "#EXT-X-KEY":
			a, err := parseAttributes(value)
			_, hasIV := a["IV"]
			if err != nil || a["METHOD"].value != "AES-128" || !a["URI"].quoted || a["URI"].value != wantURI ||
				len(a) != 2+map[bool]int{true: 1}[hasIV] || (hasIV && !hexIV.MatchString(a["IV"].value)) {
				return nil, fmt.Errorf("media playlist key %q, want METHOD=AES-128 with URI %q", line, wantURI)
			}
			keyed = true
		case strings.HasPrefix(line, "#"):
			return nil, fmt.Errorf("media playlist: unexpected line %q", line)
		default:
			if !segmentName.MatchString(line) {
				return nil, fmt.Errorf("media playlist references %q", line)
			}
			if !keyed {
				return nil, fmt.Errorf("segment %s comes before any encryption key", line)
			}
			segments = append(segments, line)
		}
	}
	if !keyed {
		return nil, fmt.Errorf("media playlist is not encrypted")
	}
	if len(segments) == 0 {
		return nil, fmt.Errorf("media playlist has no segments")
	}
	return segments, nil
}

var variantPath = regexp.MustCompile(`^([1-9][0-9]{1,3}p|audio)/index\.m3u8$`)

// CheckMaster accepts only the master Shaka Packager writes: allowed tags,
// EXT-X-MEDIA with at most one quoted URI to the output's own audio
// playlist, EXT-X-STREAM-INF with no URI attribute, and variant lines
// naming the output's own media playlists. It returns the rendition
// directories referenced, e.g. ["360p", "audio"].
func CheckMaster(text string) ([]string, error) {
	var dirs []string
	add := func(ref string) error {
		m := variantPath.FindStringSubmatch(ref)
		if m == nil {
			return fmt.Errorf("master playlist references %q", ref)
		}
		if !slices.Contains(dirs, m[1]) {
			dirs = append(dirs, m[1])
		}
		return nil
	}
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		tag, value, _ := strings.Cut(line, ":")
		switch {
		case line == "" || line == "#EXTM3U" || line == "#EXT-X-INDEPENDENT-SEGMENTS" || strings.HasPrefix(line, "## "):
		case tag == "#EXT-X-VERSION":
			if !unsignedInt.MatchString(value) {
				return nil, fmt.Errorf("master playlist: bad version")
			}
		case tag == "#EXT-X-MEDIA":
			a, err := parseAttributes(value)
			if err != nil {
				return nil, fmt.Errorf("master playlist: %w", err)
			}
			if uri, ok := a["URI"]; ok {
				if !uri.quoted {
					return nil, fmt.Errorf("master playlist: unquoted URI")
				}
				if err := add(uri.value); err != nil {
					return nil, err
				}
			}
		case tag == "#EXT-X-STREAM-INF":
			a, err := parseAttributes(value)
			if err != nil {
				return nil, fmt.Errorf("master playlist: %w", err)
			}
			if _, ok := a["URI"]; ok {
				return nil, fmt.Errorf("master playlist: URI on EXT-X-STREAM-INF")
			}
		case strings.HasPrefix(line, "#"):
			return nil, fmt.Errorf("master playlist: unexpected line %q", line)
		default:
			if err := add(line); err != nil {
				return nil, err
			}
		}
	}
	if !slices.ContainsFunc(dirs, func(d string) bool { return d != "audio" }) {
		return nil, fmt.Errorf("master playlist has no video rendition")
	}
	slices.Sort(dirs)
	return dirs, nil
}
