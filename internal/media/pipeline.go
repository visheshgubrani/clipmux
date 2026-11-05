package media

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// EncodeArgs builds one ffmpeg run that decodes the source once and encodes
// every rung with libx264, keyframes forced on segment boundaries so all
// renditions align. Renditions carry no audio.
func EncodeArgs(src Source, input, encDir string, rungs []Rung) []string {
	gop := strconv.Itoa(int(math.Round(SegmentSeconds * src.FPS)))
	args := []string{"-hide_banner", "-nostdin", "-y", "-nostats", "-progress", "pipe:1"}
	args = append(args, inputGuard()...)
	args = append(args, "-i", input)
	graph := fmt.Sprintf("[0:v:0]setpts=PTS-STARTPTS,split=%d", len(rungs))
	for i := range rungs {
		graph += fmt.Sprintf("[s%d]", i)
	}
	for i, r := range rungs {
		w, h := r.Dimensions(src.Width, src.Height)
		graph += fmt.Sprintf(";[s%d]scale=%d:%d,format=yuv420p[v%d]", i, w, h, i)
	}
	args = append(args, "-filter_complex", graph)
	for i, r := range rungs {
		args = append(args,
			"-map", fmt.Sprintf("[v%d]", i),
			"-c:v", "libx264", "-preset", "veryfast", "-profile:v", "high",
			"-b:v", r.Bitrate, "-maxrate", r.Maxrate, "-bufsize", r.Bufsize,
			"-fps_mode", "cfr", "-r", src.FrameRate,
			"-g", gop, "-keyint_min", gop, "-sc_threshold", "0",
			"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", SegmentSeconds),
			"-flags", "+cgop", "-an",
			"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
			filepath.Join(encDir, "v_"+r.Label()+".mp4"))
	}
	return args
}

// AudioArgs encodes the audio rendition: loudness-normalised AAC, 48 kHz stereo.
func AudioArgs(input, encDir string) []string {
	args := []string{"-hide_banner", "-nostdin", "-y", "-nostats"}
	args = append(args, inputGuard()...)
	return append(args, "-i", input, "-vn",
		"-af", "asetpts=PTS-STARTPTS,loudnorm=I=-16:TP=-1.5:LRA=11,aresample=48000",
		"-c:a", "aac", "-b:a", "128k", "-ac", "2", "-ar", "48000",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		filepath.Join(encDir, "audio.mp4"))
}

// PosterArgs grabs one frame near the start, at most 720 on the short side.
func PosterArgs(src Source, input, outDir string) []string {
	w, h := Rung{Height: min(720, min(src.Width, src.Height)&^1)}.Dimensions(src.Width, src.Height)
	at := strconv.FormatFloat(min(1, src.Duration/2), 'f', 3, 64)
	args := []string{"-hide_banner", "-nostdin", "-y", "-ss", at}
	args = append(args, inputGuard()...)
	return append(args, "-i", input, "-frames:v", "1", "-vf", fmt.Sprintf("scale=%d:%d", w, h),
		"-q:v", "3", filepath.Join(outDir, "poster.jpg"))
}

const drmLabel = "CLIPMUX"

// PackageArgs builds the Shaka Packager command, run with outDir as its
// working directory so every output path is relative. The encryption flags
// are returned separately for a flag file, so the key never appears in argv.
func PackageArgs(encDir string, rungs []Rung, audio bool, key Key) (argv []string, flagfile string) {
	stream := func(in, kind, dir string) string {
		return fmt.Sprintf("in=%s,stream=%s,init_segment=%s/init.mp4,segment_template=%s/seg_$Number%%05d$.m4s,playlist_name=%s/index.m3u8,drm_label=%s",
			in, kind, dir, dir, dir, drmLabel)
	}
	for _, r := range rungs {
		argv = append(argv, stream(filepath.Join(encDir, "v_"+r.Label()+".mp4"), "video", r.Label()))
	}
	if audio {
		argv = append(argv, stream(filepath.Join(encDir, "audio.mp4"), "audio", "audio"))
	}
	argv = append(argv, "--hls_playlist_type", "VOD", "--segment_duration", strconv.Itoa(SegmentSeconds),
		"--hls_master_playlist_output", "master.m3u8")
	flagfile = strings.Join([]string{
		"--protection_scheme=aes128", "--enable_raw_key_encryption", "--clear_lead=0",
		fmt.Sprintf("--keys=label=%s:key_id=%s:key=%s", drmLabel, hex.EncodeToString(key.KeyID[:]), hex.EncodeToString(key.Key[:])),
		"--hls_key_uri=" + key.KeyURI(),
	}, "\n") + "\n"
	return argv, flagfile
}

// Progress reports pipeline progress: a phase and a fraction from 0 to 1.
type Progress func(phase string, fraction float64)

// Transcode encodes and packages src into outDir (which must be empty) and
// writes manifest.json. workDir holds intermediates and must not be inside
// outDir. Nothing in outDir falls outside the output grammar.
func Transcode(ctx context.Context, t Tools, input string, src Source, workDir, outDir string, key Key, progress Progress) (Manifest, error) {
	if progress == nil {
		progress = func(string, float64) {}
	}
	versions, err := t.Versions(ctx)
	if err != nil {
		return Manifest{}, err
	}
	encDir := filepath.Join(workDir, "enc")
	for _, d := range []string{encDir, outDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return Manifest{}, err
		}
	}
	rungs := SelectLadder(src.Width, src.Height)

	progress("transcode", 0)
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		reportFFmpegProgress(pr, src.Duration, func(f float64) { progress("transcode", f) })
	}()
	err = run(ctx, t.FFmpeg, EncodeArgs(src, input, encDir, rungs), workDir, pw)
	pw.Close()
	<-done
	if err != nil {
		return Manifest{}, fmt.Errorf("encode video: %w", err)
	}
	if src.HasAudio {
		if err := run(ctx, t.FFmpeg, AudioArgs(input, encDir), workDir, io.Discard); err != nil {
			return Manifest{}, fmt.Errorf("encode audio: %w", err)
		}
	}

	progress("package", 0)
	argv, flags := PackageArgs(encDir, rungs, src.HasAudio, key)
	flagPath := filepath.Join(workDir, "encryption.flags")
	if err := os.WriteFile(flagPath, []byte(flags), 0o600); err != nil {
		return Manifest{}, err
	}
	argv = append(argv, "--flagfile="+flagPath)
	err = run(ctx, t.Packager, argv, outDir, io.Discard)
	_ = os.Remove(flagPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("package: %w", err)
	}
	dirs := make([]string, 0, len(rungs)+1)
	for _, r := range rungs {
		dirs = append(dirs, r.Label())
	}
	if src.HasAudio {
		dirs = append(dirs, "audio")
	}
	for _, d := range dirs {
		if err := RepairInit(filepath.Join(outDir, d, "init.mp4")); err != nil {
			return Manifest{}, fmt.Errorf("repair %s init: %w", d, err)
		}
		text, err := os.ReadFile(filepath.Join(outDir, d, "index.m3u8"))
		if err != nil {
			return Manifest{}, err
		}
		if _, err := CheckMediaPlaylist(string(text), key.Version); err != nil {
			return Manifest{}, fmt.Errorf("%s: %w", d, err)
		}
	}
	if err := run(ctx, t.FFmpeg, PosterArgs(src, input, outDir), workDir, io.Discard); err != nil {
		return Manifest{}, fmt.Errorf("poster: %w", err)
	}

	m := Manifest{
		OutputFormat: OutputFormat, DurationSeconds: src.Duration, Encryption: "aes-128",
		KeyVersion: key.Version, Audio: src.HasAudio, Tools: versions,
	}
	for _, r := range rungs {
		w, h := r.Dimensions(src.Width, src.Height)
		m.Renditions = append(m.Renditions, Rendition{Name: r.Label(), Width: w, Height: h})
	}
	if m.Files, err = Inventory(outDir); err != nil {
		return Manifest{}, err
	}
	raw, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(outDir, "manifest.json"), raw, 0o640); err != nil {
		return Manifest{}, err
	}
	progress("package", 1)
	return m, nil
}

// Inventory lists the files under dir that match the output grammar, except
// manifest.json, sorted by path. Anything else (which a compromised ffmpeg
// could have written) is not part of the output and is never uploaded.
func Inventory(dir string) ([]File, error) {
	var files []File
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == "manifest.json" || !ValidOutputPath(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files = append(files, File{Path: rel, Size: info.Size()})
		return nil
	})
	slices.SortFunc(files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	return files, err
}

// reportFFmpegProgress reads `-progress pipe:1` output.
func reportFFmpegProgress(r io.Reader, duration float64, report func(float64)) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		v, ok := strings.CutPrefix(sc.Text(), "out_time_us=")
		if !ok || duration <= 0 {
			continue
		}
		if us, err := strconv.ParseFloat(v, 64); err == nil {
			report(min(1, max(0, us/1e6/duration)))
		}
	}
	_, _ = io.Copy(io.Discard, r)
}

// RepairInit works around Shaka Packager issue 1616 in 3.9.3: with aes128,
// fragments reference sample description 2 but the init has one clear
// description. Duplicating that identical description fixes the init
// without touching media, timestamps, keys or IVs. Remove when the pinned
// packager includes the upstream fix.
func RepairInit(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fixed, err := repairInit(data)
	if err != nil {
		return err
	}
	return os.WriteFile(path, fixed, 0o640)
}

func repairInit(data []byte) ([]byte, error) {
	descriptions := 0
	var rewrite func([]byte) ([]byte, error)
	rewrite = func(data []byte) ([]byte, error) {
		var out []byte
		for off := 0; off < len(data); {
			if off+8 > len(data) {
				return nil, errors.New("truncated MP4 box")
			}
			size := int(binary.BigEndian.Uint32(data[off:]))
			kind := string(data[off+4 : off+8])
			if size < 8 || off+size > len(data) {
				return nil, errors.New("invalid MP4 box size")
			}
			body := data[off+8 : off+size]
			switch kind {
			case "moov", "trak", "mdia", "minf", "stbl":
				var err error
				if body, err = rewrite(body); err != nil {
					return nil, err
				}
			case "stsd":
				if len(body) < 16 || binary.BigEndian.Uint32(body[4:]) != 1 {
					return nil, errors.New("expected one sample description")
				}
				entrySize := int(binary.BigEndian.Uint32(body[8:]))
				entryType := string(body[12:16])
				if (entryType != "avc1" && entryType != "mp4a") || entrySize != len(body)-8 {
					return nil, errors.New("expected one clear AVC or AAC sample description")
				}
				nb := make([]byte, 0, len(body)+entrySize)
				nb = append(nb, body[:4]...)
				nb = binary.BigEndian.AppendUint32(nb, 2)
				nb = append(nb, body[8:]...)
				nb = append(nb, body[8:]...)
				body = nb
				descriptions++
			}
			out = binary.BigEndian.AppendUint32(out, uint32(len(body)+8))
			out = append(out, kind...)
			out = append(out, body...)
			off += size
		}
		return out, nil
	}
	fixed, err := rewrite(data)
	if err != nil {
		return nil, err
	}
	if descriptions != 1 {
		return nil, fmt.Errorf("expected one track in the init segment, found %d", descriptions)
	}
	return fixed, nil
}
