package main

import (
	"context"
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
  CLIPMUX_API_KEY=cmx_… clipmux push --server URL [flags] FILE|DIR…

Uploads videos through the public upload API, straight to storage in 32 MiB
parts. A push that is interrupted resumes where it stopped when run again,
and pushing a finished file again creates nothing new. Directories are walked
for video files. The API key needs uploads:write (and assets:read for --wait);
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
	policy := fs.String("policy", "signed", "playback policy: signed or public")
	passthrough := fs.String("passthrough", "", "your own reference, returned in webhooks (one file only)")
	waitReady := fs.Bool("wait", false, "wait until each video is ready to play")
	quiet := fs.Bool("quiet", false, "print only asset and playback IDs")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	key := getenv("CLIPMUX_API_KEY")
	switch {
	case *server == "" || key == "" || fs.NArg() == 0:
		fmt.Fprint(stderr, "clipmux push: needs --server, CLIPMUX_API_KEY and at least one file\n\n")
		fs.Usage()
		return 2
	case *policy != "signed" && *policy != "public":
		fmt.Fprintln(stderr, "clipmux push: --policy is signed or public")
		return 2
	}
	files, err := videoFiles(fs.Args())
	if err != nil {
		fmt.Fprintf(stderr, "clipmux push: %v\n", err)
		return 1
	}
	if *passthrough != "" && len(files) > 1 {
		fmt.Fprintln(stderr, "clipmux push: --passthrough names one video; push files one at a time to set it")
		return 2
	}
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
		opts := push.Options{Policy: *policy, Passthrough: *passthrough}
		if !*quiet {
			opts.Progress = progressLine(stderr, filepath.Base(f))
		}
		a, err := c.File(ctx, f, opts)
		if err == nil && *waitReady {
			a, err = c.Wait(ctx, a.ID, 2*time.Second)
		}
		if !*quiet {
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
