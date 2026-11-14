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
	"strconv"
	"strings"
	"time"
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

// defaultLadder is every rung a source may get, from 2160p down to 360p.
// 1080p and below carry clipmux-lms's production bitrates; 1440p and 2160p
// follow the same ratios (maxrate 1.2×, buffer 2× the target, D130).
var defaultLadder = []Rung{
	{2160, "14M", "16.8M", "28M"},
	{1440, "8M", "9.6M", "16M"},
	{1080, "5M", "6M", "10M"},
	{720, "3M", "3.5M", "6M"},
	{480, "1.5M", "1.8M", "3M"},
	{360, "800k", "1M", "2M"},
}

// maxSide is the longest side a rung may have: H.264 level 5.2's frame size
// at 16:9, and NVENC's H.264 limit. A 21:9 source tops out at 1440p.
const maxSide = 4096

// LadderNames are the renditions an uploader may choose, largest first.
func LadderNames() []string {
	out := make([]string, len(defaultLadder))
	for i, r := range defaultLadder {
		out[i] = r.Label()
	}
	return out
}

// SelectLadder returns the rungs a source gets: every rung up to its short
// side whose frame fits maxSide, kept to allowed when that names any, in
// ladder order. When none of allowed fits, the largest rung that does is
// encoded instead, so a source always gets a rendition. A source smaller
// than the lowest rung gets one rendition at its own size.
func SelectLadder(width, height int, allowed []string) []Rung {
	short := min(width, height)
	var fits []Rung
	for _, r := range defaultLadder {
		if w, h := r.Dimensions(width, height); r.Height <= short && max(w, h) <= maxSide {
			fits = append(fits, r)
		}
	}
	if len(fits) == 0 {
		low := defaultLadder[len(defaultLadder)-1]
		low.Height = short &^ 1 // even
		return []Rung{low}
	}
	if len(allowed) == 0 {
		return fits
	}
	var out []Rung
	for _, r := range fits {
		if slices.Contains(allowed, r.Label()) {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return fits[:1]
	}
	return out
}

// CheckLadder reports whether produced, an output's video renditions in
// any order, is a ladder SelectLadder gives some source for allowed (nil:
// every rung): the allowed rungs up to the largest one produced, the
// largest rung that fits when none allowed does, or one rendition below the
// ladder. The server never sees the source, so a worker could still stop
// short of the rungs above the largest it made; it can't skip one below
// it, or add one that wasn't asked for.
func CheckLadder(produced, allowed []string) error {
	if len(produced) == 0 {
		return fmt.Errorf("no video rendition")
	}
	if len(allowed) == 0 {
		allowed = LadderNames()
	}
	heights := make(map[string]int, len(defaultLadder))
	for _, r := range defaultLadder {
		heights[r.Label()] = r.Height
	}
	lowest := defaultLadder[len(defaultLadder)-1].Height
	if _, rung := heights[produced[0]]; len(produced) == 1 && !rung {
		h, err := strconv.Atoi(strings.TrimSuffix(produced[0], "p"))
		if err == nil && strings.HasSuffix(produced[0], "p") && h > 0 && h < lowest {
			return nil // a source below the ladder, at its own size
		}
	}
	got := map[string]bool{}
	top := 0
	for _, p := range produced {
		h, ok := heights[p]
		switch {
		case !ok:
			return fmt.Errorf("%s isn't a rung of the ladder", p)
		case got[p]:
			return fmt.Errorf("%s appears twice", p)
		}
		got[p] = true
		top = max(top, h)
	}
	var want []string
	for _, r := range defaultLadder {
		if r.Height <= top && slices.Contains(allowed, r.Label()) {
			want = append(want, r.Label())
		}
	}
	if len(want) == 0 && len(produced) == 1 {
		return nil // none asked for fits: the largest rung that does
	}
	for _, w := range want {
		if !got[w] {
			return fmt.Errorf("renditions %v leave out %s, which was asked for and fits", produced, w)
		}
	}
	if len(want) != len(got) {
		return fmt.Errorf("renditions %v include some that weren't asked for (%v)", produced, allowed)
	}
	return nil
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
	// Took is how long the transcode's parts ran, for the worker's log.
	Took Timings `json:"-"`
}

// Timings are the wall-clock times of a transcode's parts. Video is the
// whole ladder, failed stages included; Package covers packaging, the init
// repair, the playlist checks and the poster.
type Timings struct {
	Video, Audio, Package time.Duration
}

// EncoderUsed records which backend encoded an output.
type EncoderUsed struct {
	Stage     string   `json:"stage"`
	Fallbacks []string `json:"fallbacks,omitempty"`
	// Failed is why each fallback stage failed (ffmpeg's stderr tail), for
	// the worker's log only: stderr never goes into a manifest.
	Failed map[string]string `json:"-"`
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
