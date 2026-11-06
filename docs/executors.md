# Serverless encoding (RunPod, Modal)

ClipMux encodes on its own server, or on PCs you connect (docs/agents.md). A
serverless provider rents a GPU machine for one job at a time, either as the
primary executor or as a fallback while your PC is away (docs/architecture.md,
"Routing and fallback"; D053–D056). Each machine runs the ClipMux image as
`clipmux worker --once`: it claims one job with a one-time key, encodes and
uploads it through the worker API, and holds no storage credentials.

## What a provider needs

- **A public URL its machines can reach.** Connect refuses a public URL that
  resolves to a private, loopback or link-local address: the machines call the
  worker API there, and, on filesystem storage, download sources and upload
  outputs there too.
- **The worker image.** Machines pull the image this server was built with.
  Until launch it lives in a private ghcr package, so Connect asks for a
  registry username and a token that can read packages (D052). Set
  `CLIPMUX_WORKER_IMAGE` to the image reference until builds carry their own.
- **A monthly spend limit and an hourly rate.** ClipMux estimates spend from
  the rate you enter and each machine's running time (D055); it isn't the
  provider's bill. A job reserves its estimated run (an hour per 4 GiB of
  source, between 15 minutes and 6 h 15 min) before its machine starts, so
  the limit holds before any machine has run long. A machine already running
  isn't stopped at the limit.
- **A concurrency limit.** At most this many machines run at once.

A provider takes no work until a setup test through it passes: after Connect,
a sample must encode on one of its machines and play. Routing (**Workers →
Routing**, or `PUT /v1/settings/routing`) can name it only after that.

## RunPod

Create an API key in RunPod (**Settings → API Keys**) with read and write
access to pods and container registry auths. Then **Workers → Connect
RunPod**, or:

```bash
curl -X POST https://video.example.com/v1/executors/runpod/connect \
  -H 'Content-Type: application/json' --cookie "$SESSION" \
  -d '{"api_key": "rpa_...", "registry_username": "you", "registry_password": "ghp_...",
       "max_concurrent": 2, "monthly_limit_cents": 5000, "rate_cents_per_hour": 45}'
```

Optional fields: `image` (default `CLIPMUX_WORKER_IMAGE`), `gpu_type_ids`
(default an L4, RTX A4000, RTX A5000 or RTX 4090, whichever has capacity:
cards with NVENC), and `disk_gb` (default 50; a machine takes only sources
whose size, three times over, fits).

Connect checks the key, the public URL, stores the registry credentials in
RunPod, saves the key sealed, and queues the setup test. Connect again to
rotate the key or change settings; the provider then needs a new setup test.

- **Pods** are named `clipmux-{tag}-{dispatch}`, where the tag is this
  install's. ClipMux never lists, stops or touches any other pod in the
  account.
- **Secure Cloud only.** A worker sees the plaintext source and the output's
  content key. Community Cloud hosts are other people's machines, so ClipMux
  doesn't offer them.
- **Pods are stopped by ClipMux.** RunPod restarts a container that exits, so
  ClipMux stops each pod when its attempt ends, when it doesn't claim within
  10 minutes, or after 6 h 15 min. Every 15 minutes, and at boot, it also
  stops any of its pods that no dispatch accounts for.
- **Disconnect** with `DELETE /v1/executors/runpod`. It is refused while
  routing names RunPod, or while a RunPod pod may still run. It forgets the
  key and removes the registry credentials from the account.

## Security

A provider's machine is a worker like any other: it sees one source and one
output key, for one job, and can write only that job's output. Its one-time
key claims that job once, before a deadline, and is stored hashed and sealed.
Its tools always run sandboxed (`409 sandbox_required` otherwise). A source
that failed on any worker never reaches a provider (D053).
