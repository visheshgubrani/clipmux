package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/visheshgubrani/clipmux/internal/listen"
	"github.com/visheshgubrani/clipmux/internal/worker"
)

const listenUsage = `Usage:
  CLIPMUX_API_KEY=cmx_… clipmux listen --server URL --forward-to localhost:3000/webhooks

Relays the key's app's webhook events to a local URL, for development on a
machine the server can't reach. Each event is POSTed exactly as a webhook
endpoint would receive it, signed with a secret printed at start: set your
app's webhook secret to it. The key needs assets:read.

An app built with @clipmux/node can run "npx @clipmux/node listen" instead,
which signs with the app's own CLIPMUX_WEBHOOK_SECRET; this command is for
any other stack. Both take CLIPMUX_URL and CLIPMUX_API_KEY.

Flags:
`

func runListen(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("listen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, listenUsage); fs.PrintDefaults() }
	defaultServer := getenv("CLIPMUX_SERVER")
	if defaultServer == "" {
		defaultServer = getenv("CLIPMUX_URL") // the SDK's env file
	}
	server := fs.String("server", defaultServer, "ClipMux's public URL (or CLIPMUX_SERVER, or CLIPMUX_URL)")
	forward := fs.String("forward-to", "", "the local webhook URL, e.g. localhost:3000/webhooks")
	events := fs.String("events", "", "comma-separated event types to forward (default: all)")
	since := fs.Duration("since", 0, "also replay events from this long ago, e.g. 10m")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	key := getenv("CLIPMUX_API_KEY")
	if *server == "" || key == "" || *forward == "" {
		fmt.Fprint(stderr, "clipmux listen: needs --server, --forward-to and CLIPMUX_API_KEY\n\n")
		fs.Usage()
		return 2
	}
	target := *forward
	if !strings.Contains(target, "://") {
		target = "http://" + target
	}
	secret, err := listenSecret(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "clipmux listen: %v\n", err)
		return 1
	}
	var types []string
	if *events != "" {
		types = strings.Split(*events, ",")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(stderr, "Forwarding events to %s\nWebhook secret: %s  (set it as your app's webhook secret; it stays the same next time)\n\n", target, secret)
	r := &listen.Relay{
		Server: *server, Key: key, ForwardTo: target, Secret: []byte(secret), Types: types, HTTP: worker.DefaultHTTPClient(),
		Report: func(eventType, id string, status int, err error) {
			switch {
			case id == "":
				fmt.Fprintf(stderr, "%s  %v (retrying)\n", time.Now().Format(time.TimeOnly), err)
			case err != nil:
				fmt.Fprintf(stdout, "%s  %-28s %s  -> %v\n", time.Now().Format(time.TimeOnly), eventType, id, err)
			default:
				fmt.Fprintf(stdout, "%s  %-28s %s  -> %d\n", time.Now().Format(time.TimeOnly), eventType, id, status)
			}
		},
	}
	if err := r.Run(ctx, time.Now().Add(-*since)); err != nil {
		fmt.Fprintf(stderr, "clipmux listen: %v\n", err)
		return 1
	}
	return 0
}

// listenSecret is CLIPMUX_LISTEN_SECRET, or one generated once and kept in
// the user's config directory, so the app's webhook secret needn't change
// between sessions.
func listenSecret(getenv func(string) string) (string, error) {
	if s := getenv("CLIPMUX_LISTEN_SECRET"); s != "" {
		return s, nil
	}
	path := filepath.Join(agentDir(getenv), "listen-secret")
	raw, err := os.ReadFile(path)
	if err == nil && len(strings.TrimSpace(string(raw))) > 0 {
		return strings.TrimSpace(string(raw)), nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	secret := "whsec_" + rand.Text()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return secret, os.WriteFile(path, []byte(secret+"\n"), 0o600)
}
