package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/visheshgubrani/clipmux/internal/push"
	"github.com/visheshgubrani/clipmux/internal/worker"
)

const pushUsage = `Usage:
  CLIPMUX_API_KEY=cmx_… clipmux push --server URL [flags] FILE|DIR|URL…
  clipmux push --local [flags] FILE|DIR…

With --local the files stay on this PC: its connected agent (clipmux worker)
encodes them in place and uploads only the outputs. Without an API key they
become your own uploads, as from the dashboard; with one, the key's app owns
them. Such a video re-encodes only on this PC, unless --upload-source also
uploads the file once it has been encoded, so any worker can re-encode it.

Uploads videos through the public upload API, straight to storage in 32 MiB
parts. A push that is interrupted resumes where it stopped when run again,
and pushing a finished file again creates nothing new. Directories are walked
for video files. An http or https URL is imported instead: the server fetches
it, and running the same import again creates nothing new. The API key needs
uploads:write for files, assets:write for URLs (and assets:read for --wait);
it comes from CLIPMUX_API_KEY so it never shows up in a process listing.

Flags:
`

// videoExts are the files a directory push picks up.
var videoExts = []string{".mp4", ".m4v", ".mov", ".mkv", ".webm", ".avi", ".mxf", ".ts", ".mts", ".m2ts", ".flv", ".wmv", ".mpg", ".mpeg", ".ogv", ".3gp"}

func runPush(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, pushUsage); fs.PrintDefaults() }
	server := fs.String("server", getenv("CLIPMUX_SERVER"), "ClipMux's public URL (or CLIPMUX_SERVER)")
	policy := fs.String("policy", "signed", "playback policies, one ID each: signed, public, or signed,public")
	passthrough := fs.String("passthrough", "", "your own reference, returned in webhooks (one file only)")
	renditions := fs.String("renditions", "", "comma-separated rungs to encode, e.g. 1080p,480p (default: every rung up to the source)")
	waitReady := fs.Bool("wait", false, "wait until each video is ready to play")
	quiet := fs.Bool("quiet", false, "print only asset and playback IDs")
	local := fs.Bool("local", false, "keep the files on this PC; its agent encodes them and uploads only the outputs")
	uploadSource := fs.Bool("upload-source", false, "with --local: once encoded, also upload each file, so any worker can re-encode it")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	key := getenv("CLIPMUX_API_KEY")
	if *uploadSource && !*local {
		fmt.Fprint(stderr, "clipmux push: --upload-source goes with --local (a plain push uploads the file already)\n")
		return 2
	}
	switch {
	case fs.NArg() == 0 || (!*local && (*server == "" || key == "")) || (*local && key != "" && *server == ""):
		fmt.Fprint(stderr, "clipmux push: needs --server and CLIPMUX_API_KEY (or --local), and at least one file\n\n")
		fs.Usage()
		return 2
	}
	policies := splitList(*policy)
	for i, p := range policies {
		if (p != "signed" && p != "public") || slices.Contains(policies[:i], p) {
			fmt.Fprintln(stderr, "clipmux push: --policy is signed, public, or signed,public")
			return 2
		}
	}
	var urls, paths []string
	for _, a := range fs.Args() {
		if strings.HasPrefix(a, "http://") || strings.HasPrefix(a, "https://") {
			urls = append(urls, a)
		} else {
			paths = append(paths, a)
		}
	}
	if len(urls) > 0 && *local {
		fmt.Fprintln(stderr, "clipmux push: --local takes files on this PC; a URL is imported by the server, without --local")
		return 2
	}
	var files []string
	if len(paths) > 0 {
		var err error
		if files, err = videoFiles(paths); err != nil {
			fmt.Fprintf(stderr, "clipmux push: %v\n", err)
			return 1
		}
	}
	files = append(files, urls...)
	if *passthrough != "" && len(files) > 1 {
		fmt.Fprintln(stderr, "clipmux push: --passthrough names one video; push files one at a time to set it")
		return 2
	}
	chosen := splitList(*renditions)
	state, err := os.UserCacheDir()
	if err != nil {
		state = os.TempDir()
	}
	c := &push.Client{
		Server: *server, Key: key, HTTP: worker.DefaultHTTPClient(),
		StateDir: orEnv(getenv("CLIPMUX_PUSH_STATE_DIR"), filepath.Join(state, "clipmux", "push")),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	failed := 0
	for _, f := range files {
		opts := push.Options{Policies: policies, Passthrough: *passthrough, Renditions: chosen}
		isURL := slices.Contains(urls, f)
		if !*quiet && !isURL {
			opts.Progress = progressLine(stderr, filepath.Base(f))
		}
		var a push.Asset
		var err error
		switch {
		case isURL:
			a, err = c.URL(ctx, f, opts)
		case *local:
			a, err = pushLocal(ctx, c, agentDir(getenv), f, opts, *uploadSource)
		default:
			a, err = c.File(ctx, f, opts)
		}
		if err == nil && *waitReady && c.Key != "" {
			a, err = c.Wait(ctx, a.ID, 2*time.Second)
		}
		if !*quiet && !isURL {
			fmt.Fprintln(stderr)
		}
		if err != nil {
			failed++
			fmt.Fprintf(stderr, "clipmux push: %s: %v\n", f, err)
			if ctx.Err() != nil {
				fmt.Fprintln(stderr, "clipmux push: stopped; run the same command again to resume")
				return 1
			}
			continue
		}
		pid := ""
		if len(a.PlaybackIDs) > 0 {
			pid = a.PlaybackIDs[0].ID
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", a.ID, pid, a.Status, f)
		if a.Status == "errored" {
			failed++
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// pushLocal hands one file to this PC's agent. With an API key, the key's
// app creates the asset; without, the agent creates it as the owner's.
func pushLocal(ctx context.Context, c *push.Client, dir, path string, o push.Options, uploadSource bool) (push.Asset, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return push.Asset{}, err
	}
	res, err := worker.RegisterLocal(ctx, dir, worker.LocalRequest{
		Path: abs, Attach: c.Key == "", PlaybackPolicies: o.Policies, Passthrough: o.Passthrough, UploadSource: uploadSource,
	})
	if err != nil {
		return push.Asset{}, err
	}
	if c.Key == "" {
		var a push.Asset
		return a, json.Unmarshal(res.Asset, &a)
	}
	return c.AttachLocal(ctx, res.SourceID, o)
}

// splitList reads a comma-separated flag, ignoring blanks.
func splitList(v string) []string {
	var out []string
	for s := range strings.SplitSeq(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// videoFiles expands directories into the video files under them.
func videoFiles(args []string) ([]string, error) {
	var out []string
	for _, a := range args {
		fi, err := os.Stat(a)
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			out = append(out, a)
			continue
		}
		err = filepath.WalkDir(a, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && p != a && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			if d.Type().IsRegular() && slices.Contains(videoExts, strings.ToLower(filepath.Ext(p))) {
				out = append(out, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no video files found")
	}
	return out, nil
}

// progressLine redraws one status line per file.
func progressLine(w io.Writer, name string) func(done, total int64) {
	var last time.Time
	return func(done, total int64) {
		if done < total && time.Since(last) < 250*time.Millisecond {
			return
		}
		last = time.Now()
		fmt.Fprintf(w, "\r%s  %3d%%  %s / %s", name, done*100/max(total, 1), size(done), size(total))
	}
}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KiB", n>>10)
	}
}
