package media

import (
	"bytes"
	"encoding/binary"
	"slices"
	"strings"
	"testing"
)

func TestOutputPathGrammar(t *testing.T) {
	ok := []string{
		"manifest.json", "master.m3u8", "poster.jpg",
		"720p/index.m3u8", "720p/init.mp4", "720p/seg_00001.m4s", "1080p/seg_12345.m4s", "2160p/seg_123456.m4s",
		"audio/index.m3u8", "audio/init.mp4", "audio/seg_00042.m4s",
	}
	bad := []string{
		"", "../master.m3u8", "720p/../master.m3u8", "/master.m3u8", "720p/seg_1.m4s", "720p/seg_00001.mp4",
		"key/1", "720p/key.bin", "720p/index.m3u8/x", "p/index.m3u8", "0720p/init.mp4", "720P/init.mp4",
		"video/index.m3u8", "audio/poster.jpg", "720p\\init.mp4", "master.m3u8 ", "720p/seg_0000001.m4s",
	}
	for _, p := range ok {
		if !ValidOutputPath(p) {
			t.Errorf("ValidOutputPath(%q) = false, want true", p)
		}
	}
	for _, p := range bad {
		if ValidOutputPath(p) {
			t.Errorf("ValidOutputPath(%q) = true, want false", p)
		}
	}
}

func TestLadderIsCappedAtTheSourcesShortSide(t *testing.T) {
	labels := func(rs []Rung) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Label())
		}
		return out
	}
	cases := []struct {
		w, h    int
		allowed []string
		want    []string
	}{
		// Every rung up to the source, by its short side.
		{3840, 2160, nil, []string{"2160p", "1440p", "1080p", "720p", "480p", "360p"}},
		{2560, 1440, nil, []string{"1440p", "1080p", "720p", "480p", "360p"}},
		{1920, 1080, nil, []string{"1080p", "720p", "480p", "360p"}},
		{1280, 720, nil, []string{"720p", "480p", "360p"}},
		{1080, 1920, nil, []string{"1080p", "720p", "480p", "360p"}}, // vertical: rungs follow the short side
		{2160, 3840, nil, []string{"2160p", "1440p", "1080p", "720p", "480p", "360p"}},
		{854, 480, nil, []string{"480p", "360p"}},
		{426, 240, nil, []string{"240p"}}, // below the ladder: one rung at the source size
		{641, 361, nil, []string{"360p"}},
		// 21:9 at 2160 would be 5120 wide, past H.264's and NVENC's 4096.
		{5120, 2160, nil, []string{"1440p", "1080p", "720p", "480p", "360p"}},
		// The uploader's choice, within what the source has.
		{3840, 2160, []string{"1080p", "480p"}, []string{"1080p", "480p"}},
		{3840, 2160, []string{"360p", "2160p"}, []string{"2160p", "360p"}}, // ladder order, not the request's
		// Nothing chosen fits: the largest rung that does.
		{1280, 720, []string{"2160p", "1080p"}, []string{"720p"}},
		{426, 240, []string{"1080p"}, []string{"240p"}},
		// F8: no rung fits 4096 on the long side, so the one rendition is
		// scaled to fit it: 400×5000 at 326 wide is 4076 tall.
		{400, 5000, nil, []string{"326p"}},
		{8192, 700, nil, []string{"350p"}},
	}
	for _, c := range cases {
		if got := labels(SelectLadder(c.w, c.h, c.allowed)); !slices.Equal(got, c.want) {
			t.Errorf("SelectLadder(%d, %d, %v) = %v, want %v", c.w, c.h, c.allowed, got, c.want)
		}
	}
	for _, r := range SelectLadder(3840, 2160, nil)[:2] {
		if r.Bitrate == "" || r.Maxrate == "" || r.Bufsize == "" {
			t.Errorf("%s has no rates: %+v", r.Label(), r)
		}
	}
	if got := LadderNames(); !slices.Equal(got, []string{"2160p", "1440p", "1080p", "720p", "480p", "360p"}) {
		t.Errorf("LadderNames() = %v", got)
	}
}

func TestCheckLadderAcceptsOnlyWhatSomeSourceWouldGet(t *testing.T) {
	cases := []struct {
		produced, allowed []string
		ok                bool
	}{
		{[]string{"1080p", "720p", "480p", "360p"}, nil, true},
		{[]string{"720p", "480p", "360p"}, nil, true},   // a 720p source
		{[]string{"1080p", "480p", "360p"}, nil, false}, // 720p skipped
		{[]string{"2160p", "1440p", "1080p", "720p", "480p", "360p"}, nil, true},
		{[]string{"240p"}, nil, true}, // below the ladder: one rendition at the source size
		{[]string{"240p", "360p"}, nil, false},
		{[]string{"1080p", "480p"}, []string{"1080p", "480p"}, true},
		{[]string{"480p"}, []string{"1080p", "480p"}, true},                   // a 720p source
		{[]string{"1080p"}, []string{"1080p", "480p"}, false},                 // 480p skipped
		{[]string{"1080p", "720p", "480p"}, []string{"1080p", "480p"}, false}, // 720p wasn't asked for
		{[]string{"720p"}, []string{"2160p", "1080p"}, true},                  // nothing asked for fits: the largest that does
		{[]string{"720p", "480p"}, []string{"2160p", "1080p"}, false},
		{[]string{"720p"}, []string{"2160p", "480p"}, false}, // 480p fits, so it is what's encoded
		{[]string{"240p"}, []string{"1080p"}, true},
		{nil, nil, false},
		{[]string{"9999p"}, nil, false},
	}
	for _, c := range cases {
		if err := CheckLadder(c.produced, c.allowed); (err == nil) != c.ok {
			t.Errorf("CheckLadder(%v, %v) = %v, want ok %v", c.produced, c.allowed, err, c.ok)
		}
	}
	// Whatever SelectLadder picks passes.
	for _, size := range [][2]int{{3840, 2160}, {5120, 2160}, {1280, 720}, {426, 240}, {1080, 1920}} {
		for _, allowed := range [][]string{nil, {"1080p", "360p"}, {"2160p"}, {"480p"}} {
			var names []string
			for _, r := range SelectLadder(size[0], size[1], allowed) {
				names = append(names, r.Label())
			}
			if err := CheckLadder(names, allowed); err != nil {
				t.Errorf("%v with %v: SelectLadder gave %v, which CheckLadder refuses: %v", size, allowed, names, err)
			}
		}
	}
}

func TestRungDimensionsKeepAspectAndStayEven(t *testing.T) {
	cases := []struct {
		srcW, srcH, rung int
		w, h             int
	}{
		{1920, 1080, 720, 1280, 720},
		{1080, 1920, 720, 720, 1280}, // vertical
		{1440, 1080, 480, 640, 480},  // 4:3
		{641, 361, 360, 640, 360},    // odd source rounds to even
	}
	for _, c := range cases {
		w, h := Rung{Height: c.rung}.Dimensions(c.srcW, c.srcH)
		if w != c.w || h != c.h {
			t.Errorf("rung %d of %dx%d = %dx%d, want %dx%d", c.rung, c.srcW, c.srcH, w, h, c.w, c.h)
		}
	}
}

func TestEncodeArgsAlignKeyframesToSegments(t *testing.T) {
	src := Source{Width: 1280, Height: 720, FrameRate: "30000/1001", FPS: 29.97, HasAudio: true, Duration: 60}
	args := strings.Join(EncodeArgs(Stage{Backend: CPU}, "", src, "/w/source", "/w/enc", SelectLadder(src.Width, src.Height, nil)), " ")
	for _, want := range []string{
		"-format_whitelist " + allowedDemuxers, "-protocol_whitelist file", "-i /w/source",
		"split=3", "scale=1280:720", "scale=854:480", "scale=640:360",
		"-g 180", "-keyint_min 180", "-sc_threshold 0", "-force_key_frames expr:gte(t,n_forced*6)",
		"-r 30000/1001", "-c:v libx264", "-an", "/w/enc/v_720p.mp4", "/w/enc/v_360p.mp4",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("encode args missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "-c:a") {
		t.Error("video renditions must not carry audio; audio is its own rendition")
	}
}

func TestPackageFlagsKeepTheKeyOutOfArgv(t *testing.T) {
	key := Key{Version: 3, Key: [16]byte{1, 2, 3}, KeyID: [16]byte{9}}
	argv, flagfile := PackageArgs("/w/enc", []Rung{{Height: 720}, {Height: 360}}, true, key)
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "01020300") {
		t.Fatal("the content key must never appear in argv")
	}
	for _, want := range []string{
		"in=/w/enc/v_720p.mp4,stream=video,init_segment=720p/init.mp4,segment_template=720p/seg_$Number%05d$.m4s,playlist_name=720p/index.m3u8,drm_label=CLIPMUX",
		"in=/w/enc/audio.mp4,stream=audio,init_segment=audio/init.mp4,segment_template=audio/seg_$Number%05d$.m4s,playlist_name=audio/index.m3u8",
		"--segment_duration 6", "--hls_master_playlist_output master.m3u8",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("package args missing %q:\n%s", want, joined)
		}
	}
	for _, want := range []string{
		"--protection_scheme=aes128", "--enable_raw_key_encryption", "--clear_lead=0",
		"--keys=label=CLIPMUX:key_id=09000000000000000000000000000000:key=01020300000000000000000000000000",
		"--hls_key_uri=../key/3",
	} {
		if !strings.Contains(flagfile, want) {
			t.Errorf("flagfile missing %q:\n%s", want, flagfile)
		}
	}
}

func TestTheMasterStartsAtTheLargestRungUpTo1080p(t *testing.T) {
	// Shaka lists variants in stream order and Safari starts on the first:
	// a 4K output still starts at 1080p, as every output did before 4K.
	order := func(rungs []Rung) []string {
		argv, _ := PackageArgs("/w/enc", rungs, false, Key{Version: 1})
		var out []string
		for _, a := range argv {
			if in, ok := strings.CutPrefix(a, "in=/w/enc/v_"); ok {
				out = append(out, in[:strings.Index(in, ".mp4")])
			}
		}
		return out
	}
	cases := []struct {
		rungs []Rung
		want  []string
	}{
		{SelectLadder(3840, 2160, nil), []string{"1080p", "2160p", "1440p", "720p", "480p", "360p"}},
		{SelectLadder(2560, 1440, nil), []string{"1080p", "1440p", "720p", "480p", "360p"}},
		{SelectLadder(1920, 1080, nil), []string{"1080p", "720p", "480p", "360p"}},
		{SelectLadder(3840, 2160, []string{"2160p", "1440p"}), []string{"2160p", "1440p"}}, // nothing at or below 1080p
		{SelectLadder(3840, 2160, []string{"2160p", "480p"}), []string{"480p", "2160p"}},
	}
	for _, c := range cases {
		if got := order(c.rungs); !slices.Equal(got, c.want) {
			t.Errorf("streams = %v, want %v", got, c.want)
		}
	}
}

// box builds an MP4 box.
func box(kind string, body ...[]byte) []byte {
	b := bytes.Join(body, nil)
	out := make([]byte, 8, 8+len(b))
	binary.BigEndian.PutUint32(out, uint32(8+len(b)))
	copy(out[4:], kind)
	return append(out, b...)
}

func TestRepairInitDuplicatesTheSampleDescription(t *testing.T) {
	entry := box("avc1", []byte("codec-config"))
	stsd := box("stsd", []byte{0, 0, 0, 0, 0, 0, 0, 1}, entry)
	init := append(box("ftyp", []byte("iso6")), box("moov", box("trak", box("mdia", box("minf", box("stbl", stsd)))))...)

	fixed, err := repairInit(init)
	if err != nil {
		t.Fatal(err)
	}
	wantStsd := box("stsd", []byte{0, 0, 0, 0, 0, 0, 0, 2}, entry, entry)
	if !bytes.Contains(fixed, wantStsd) {
		t.Fatal("stsd should list the clear description twice (Shaka issue 1616)")
	}
	if _, err := repairInit(fixed); err == nil {
		t.Fatal("repairing twice must fail rather than grow the init again")
	}
	if _, err := repairInit([]byte{0, 0, 0, 99, 'm', 'o', 'o', 'v'}); err == nil {
		t.Fatal("a truncated box must be rejected")
	}
}

func TestCheckPlaylistsRequiresEncryptionAndRelativeURIs(t *testing.T) {
	good := "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"../key/1\",IV=0x00000000000000000000000000000001\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6,\nseg_00001.m4s\n"
	if _, err := CheckMediaPlaylist(good, 1); err != nil {
		t.Fatalf("good playlist: %v", err)
	}
	for name, text := range map[string]string{
		"unencrypted":   "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6,\nseg_00001.m4s\n",
		"wrong key":     strings.Replace(good, "../key/1", "https://evil.example/key", 1),
		"absolute seg":  strings.Replace(good, "seg_00001.m4s", "https://cdn.example/seg_00001.m4s", 1),
		"escaping seg":  strings.Replace(good, "seg_00001.m4s", "../../other/seg_00001.m4s", 1),
		"wrong version": strings.Replace(good, "../key/1", "../key/2", 1),
		"clear lead":    "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6,\nseg_00001.m4s\n#EXT-X-KEY:METHOD=AES-128,URI=\"../key/1\",IV=0x00000000000000000000000000000001\n#EXTINF:6,\nseg_00002.m4s\n",
		"extra ref":     strings.Replace(good, "#EXTINF", "#EXT-X-PART:URI=\"../../x\"\n#EXTINF", 1),
	} {
		if _, err := CheckMediaPlaylist(text, 1); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestCheckMasterAllowsOnlyTheOutputsOwnPlaylists(t *testing.T) {
	// Shaka 3.9.3's master for one video rendition and an audio rendition.
	good := `#EXTM3U
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-MEDIA:TYPE=AUDIO,URI="audio/index.m3u8",GROUP-ID="default-audio-group",NAME="stream_0",DEFAULT=NO,AUTOSELECT=YES,CHANNELS="1"
#EXT-X-STREAM-INF:BANDWIDTH=831390,CODECS="avc1.64001e,mp4a.40.2",RESOLUTION=640x360,AUDIO="default-audio-group"
360p/index.m3u8
`
	dirs, err := CheckMaster(good)
	if err != nil || !slices.Equal(dirs, []string{"360p", "audio"}) {
		t.Fatalf("CheckMaster = %v, %v", dirs, err)
	}
	for name, text := range map[string]string{
		"absolute variant": strings.Replace(good, "360p/index.m3u8\n", "https://evil.example/360p/index.m3u8\n", 1),
		"escaping audio":   strings.Replace(good, `URI="audio/index.m3u8"`, `URI="../other/audio/index.m3u8"`, 1),
		"session key":      good + "#EXT-X-SESSION-KEY:METHOD=AES-128,URI=\"k\"\n",
		"audio only":       strings.Replace(good, "360p/index.m3u8\n", "", 1),
	} {
		if _, err := CheckMaster(text); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// Bypasses found in review: duplicate, unquoted or trailing URI attributes
// that a prefix match accepted while hls.js would follow the last one.
func TestPlaylistChecksRejectSmuggledURIs(t *testing.T) {
	media := "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXT-X-KEY:METHOD=AES-128,URI=\"../key/1\",IV=0x00000000000000000000000000000001\n#EXTINF:6.000,\nseg_00001.m4s\n#EXT-X-ENDLIST\n"
	if _, err := CheckMediaPlaylist(media, 1); err != nil {
		t.Fatalf("clean playlist: %v", err)
	}
	for name, bad := range map[string]string{
		"duplicate key URI":      strings.Replace(media, `URI="../key/1",`, `URI="../key/1",URI="https://evil/k",`, 1),
		"trailing key attribute": strings.Replace(media, "IV=0x00000000000000000000000000000001", "IV=0x00000000000000000000000000000001,URI=\"https://evil/k\"", 1),
		"duplicate map URI":      strings.Replace(media, `#EXT-X-MAP:URI="init.mp4"`, `#EXT-X-MAP:URI="init.mp4",URI="https://evil/init.mp4"`, 1),
		"map byterange":          strings.Replace(media, `#EXT-X-MAP:URI="init.mp4"`, `#EXT-X-MAP:URI="init.mp4",BYTERANGE="10@0"`, 1),
		"unknown tag":            strings.Replace(media, "#EXT-X-ENDLIST", "#EXT-X-PRELOAD-HINT:TYPE=PART,URI=\"https://evil/x\"\n#EXT-X-ENDLIST", 1),
		"lower-case uri":         strings.Replace(media, "#EXTINF:6.000,", "#EXT-X-DATERANGE:uri=\"https://evil\"\n#EXTINF:6.000,", 1),
	} {
		if _, err := CheckMediaPlaylist(bad, 1); err == nil {
			t.Errorf("media playlist with %s was accepted", name)
		}
	}

	master := "#EXTM3U\n## Generated with https://github.com/shaka-project/shaka-packager version v3.9.3\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-MEDIA:TYPE=AUDIO,URI=\"audio/index.m3u8\",GROUP-ID=\"a\",NAME=\"x\",DEFAULT=NO,AUTOSELECT=YES,CHANNELS=\"2\"\n#EXT-X-STREAM-INF:BANDWIDTH=1,AVERAGE-BANDWIDTH=1,CODECS=\"avc1.64001e,mp4a.40.2\",RESOLUTION=640x360,FRAME-RATE=25.000,AUDIO=\"a\",CLOSED-CAPTIONS=NONE\n360p/index.m3u8\n"
	if _, err := CheckMaster(master); err != nil {
		t.Fatalf("clean master: %v", err)
	}
	for name, bad := range map[string]string{
		"unquoted URI":         strings.Replace(master, `URI="audio/index.m3u8",`, `URI="audio/index.m3u8",X=1,`, 1) + "#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"y\",URI=https://evil/a.m3u8\n",
		"duplicate URI":        strings.Replace(master, `URI="audio/index.m3u8"`, `URI="audio/index.m3u8",URI="https://evil/a.m3u8"`, 1),
		"URI on STREAM-INF":    strings.Replace(master, "CLOSED-CAPTIONS=NONE", "CLOSED-CAPTIONS=NONE,URI=\"https://evil/x\"", 1),
		"unknown tag":          master + "#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=1,URI=\"https://evil/i.m3u8\"\n",
		"session data":         master + "#EXT-X-SESSION-DATA:DATA-ID=\"x\",URI=\"https://evil/d.json\"\n",
		"lower-case attribute": strings.Replace(master, `URI="audio/index.m3u8"`, `uri="https://evil/a.m3u8",URI="audio/index.m3u8"`, 1),
	} {
		if _, err := CheckMaster(bad); err == nil {
			t.Errorf("master with %s was accepted", name)
		}
	}
}

func TestParseAttributes(t *testing.T) {
	attrs, err := parseAttributes(`TYPE=AUDIO,URI="a/b.m3u8",CODECS="avc1.64001e,mp4a.40.2",X=1`)
	if err != nil || attrs["URI"].value != "a/b.m3u8" || !attrs["URI"].quoted || attrs["CODECS"].value != "avc1.64001e,mp4a.40.2" || attrs["X"].quoted {
		t.Fatalf("parseAttributes = %+v, %v", attrs, err)
	}
	for _, bad := range []string{`URI="a",URI="b"`, `uri="a"`, `URI="a`, `URI="a"x`, `=1`, `A=1,,B=2`, `A=1,`} {
		if _, err := parseAttributes(bad); err == nil {
			t.Errorf("parseAttributes(%q) accepted", bad)
		}
	}
}
