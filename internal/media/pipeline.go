package media

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// loudnormMinSeconds is the shortest audio loudnorm is given: under 3 s it
// measures a clip of digital silence as NaN, which the AAC encoder refuses
// (bug hunt F6). A clip that short gains little from normalising anyway.
const loudnormMinSeconds = 3.5

// AudioArgs encodes the audio rendition: loudness-normalised AAC, 48 kHz stereo.
func AudioArgs(src Source, input, encDir string) []string {
	args := []string{"-hide_banner", "-nostdin", "-y", "-nostats"}
	args = append(args, inputGuard()...)
	filter := "asetpts=PTS-STARTPTS,loudnorm=I=-16:TP=-1.5:LRA=11,aresample=48000"
	if src.Duration < loudnormMinSeconds {
		filter = "asetpts=PTS-STARTPTS,aresample=48000"
	}
	return append(args, "-i", input, "-vn",
		"-af", filter,
		"-c:a", "aac", "-b:a", "128k", "-ac", "2", "-ar", "48000",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		filepath.Join(encDir, "audio.mp4"))
}

// PosterArgs grabs the frame at the given second, at most 720 on the short
// side. The seek is on the output, so ffmpeg decodes from the start: an
// input seek into a stream whose keyframes are far apart (an OBS recording
// in MPEG-TS) can land where nothing decodes (bug hunt F5). Decoding up to a
// second costs little.
func PosterArgs(src Source, input, outDir string, at float64) []string {
	w, h := Rung{Height: min(720, min(src.Width, src.Height)&^1)}.Dimensions(src.Width, src.Height)
	args := []string{"-hide_banner", "-nostdin", "-y"}
	args = append(args, inputGuard()...)
	args = append(args, "-i", input)
	if at > 0 {
		args = append(args, "-ss", strconv.FormatFloat(at, 'f', 3, 64))
	}
	return append(args, "-frames:v", "1", "-vf", fmt.Sprintf("scale=%d:%d", w, h),
		"-q:v", "3", filepath.Join(outDir, "poster.jpg"))
}

// poster grabs a frame a second in (or halfway through a shorter video),
// and the first frame when that finds none: a video of one frame, or one
// whose last frame comes before the halfway point (bug hunt F9).
func poster(ctx context.Context, t Tools, src Source, input, workDir, outDir string) error {
	err := t.run(ctx, t.FFmpeg, PosterArgs(src, input, outDir, min(1, src.Duration/2)), workDir, io.Discard)
	if err != nil && ctx.Err() == nil {
		err = t.run(ctx, t.FFmpeg, PosterArgs(src, input, outDir, 0), workDir, io.Discard)
	}
	return err
}

// audioPackets counts the packets in an encoded audio rendition.
func audioPackets(ctx context.Context, t Tools, path string) (int, error) {
	var out bytes.Buffer
	args := append(inputGuard(), "-v", "error", "-select_streams", "a:0", "-count_packets",
		"-show_entries", "stream=nb_read_packets", "-of", "csv=p=0", "-i", path)
	if err := t.run(ctx, t.FFprobe, args, filepath.Dir(path), &out); err != nil {
		return 0, err
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out.String())) // empty: no audio stream at all
	return n, nil
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
	for _, r := range masterOrder(rungs) {
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

// startHeight is the rung a player should start on: Shaka lists variants
// in stream order, and Safari starts on the first one listed.
const startHeight = 1080

// masterOrder puts the largest rung up to startHeight first, then the rest
// in ladder order, so a 4K output starts where a 1080p one does.
func masterOrder(rungs []Rung) []Rung {
	for i, r := range rungs {
		if r.Height <= startHeight {
			return append(append([]Rung{r}, rungs[:i]...), rungs[i+1:]...)
		}
	}
	return rungs
}

// Progress reports pipeline progress: a phase and a fraction from 0 to 1.
type Progress func(phase string, fraction float64)

// Transcode encodes and packages src into outDir (which must be empty) and
// writes manifest.json. renditions are the rungs the uploader chose (nil:
// every rung up to the source; see SelectLadder). workDir holds
// intermediates and must not be inside outDir. Nothing in outDir falls
// outside the output grammar.
func Transcode(ctx context.Context, t Tools, input string, src Source, renditions []string, workDir, outDir string, key Key, progress Progress) (Manifest, error) {
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
	rungs := SelectLadder(src.Width, src.Height, renditions)

	took, used, err := encodeVideoAndAudio(ctx, t, src, input, workDir, encDir, rungs, progress)
	if err != nil {
		return Manifest{}, err
	}
	if src.HasAudio {
		// A declared track with no packets (a recorder whose mic was dead)
		// encodes to an empty rendition: the output is video-only (F7).
		n, err := audioPackets(ctx, t, filepath.Join(encDir, "audio.mp4"))
		if err != nil {
			return Manifest{}, fmt.Errorf("count audio packets: %w", err)
		}
		src.HasAudio = n > 0
	}

	progress("package", 0)
	start := time.Now()
	argv, flags := PackageArgs(encDir, rungs, src.HasAudio, key)
	flagPath := filepath.Join(workDir, "encryption.flags")
	if err := os.WriteFile(flagPath, []byte(flags), 0o600); err != nil {
		return Manifest{}, err
	}
	argv = append(argv, "--flagfile="+flagPath)
	err = t.run(ctx, t.Packager, argv, outDir, io.Discard)
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
	if err := poster(ctx, t, src, input, workDir, outDir); err != nil {
		return Manifest{}, fmt.Errorf("poster: %w", err)
	}

	took.Package = time.Since(start)
	m := Manifest{
		OutputFormat: OutputFormat, DurationSeconds: src.Duration, Encryption: "aes-128",
		KeyVersion: key.Version, Audio: src.HasAudio, Tools: versions, Encoder: &used, Took: took,
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

// encodeVideoAndAudio encodes the ladder and the audio rendition side by
// side: they read the same source and write different files, and the audio's
// loudness pass is CPU work a GPU encode leaves the machine free for. The
// first failure stops the other and is the one returned.
func encodeVideoAndAudio(ctx context.Context, t Tools, src Source, input, workDir, encDir string, rungs []Rung, progress Progress) (Timings, EncoderUsed, error) {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	var (
		once  sync.Once
		first error
		took  Timings
	)
	fail := func(err error) {
		once.Do(func() { first = err; stop() })
	}
	var wg sync.WaitGroup
	if src.HasAudio {
		wg.Go(func() {
			start := time.Now()
			if err := t.run(ctx, t.FFmpeg, AudioArgs(src, input, encDir), workDir, io.Discard); err != nil {
				fail(fmt.Errorf("encode audio: %w", err))
			}
			took.Audio = time.Since(start)
		})
	}
	start := time.Now()
	used, err := encodeLadder(ctx, t, src, input, workDir, encDir, rungs, progress)
	took.Video = time.Since(start)
	if err != nil {
		fail(err)
	}
	wg.Wait()
	return took, used, first
}

// encodeLadder runs the stages of the source's chain until one encodes the
// whole ladder. A GPU stage that fails hands everything to the next, so
// renditions never mix encoders; the CPU stage's failure is final. Only a
// cancelled context stops the chain early.
func encodeLadder(ctx context.Context, t Tools, src Source, input, workDir, encDir string, rungs []Rung, progress Progress) (EncoderUsed, error) {
	var used EncoderUsed
	chain := t.HW.Chain(src)
	for i, stage := range chain {
		progress("transcode", 0)
		pr, pw := io.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			reportFFmpegProgress(pr, src.Duration, func(f float64) { progress("transcode", f) })
		}()
		st := t.forStage(stage)
		err := st.run(ctx, t.FFmpeg, EncodeArgs(stage, t.HW.VAAPIDevice, src, input, encDir, rungs), workDir, pw)
		pw.Close()
		<-done
		if err == nil {
			used.Stage = stage.Name()
			return used, nil
		}
		if ctx.Err() != nil || i == len(chain)-1 {
			return used, fmt.Errorf("encode video (%s): %w", stage.Name(), err)
		}
		used.Fallbacks = append(used.Fallbacks, stage.Name())
		if used.Failed == nil {
			used.Failed = map[string]string{}
		}
		used.Failed[stage.Name()] = err.Error()
	}
	return used, errors.New("encode video: no encoder")
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
