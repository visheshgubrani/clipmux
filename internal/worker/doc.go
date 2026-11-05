// Package worker is the worker side of the worker API: the built-in child
// worker, PC agent mode and --once serverless mode. ffmpeg and Shaka Packager
// run sandboxed with no network, an empty environment and a demuxer
// allowlist. Lands in P1-T06.
package worker
