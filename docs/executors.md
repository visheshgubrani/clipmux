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
- **The worker image.** Machines run the image this server was built from:
  the Image workflow, run by hand on main and on release tags, pushes
  `ghcr.io/visheshgubrani/clipmux:sha-{commit}` once both architectures
  pass, and the binary carries that reference
  (`CLIPMUX_WORKER_IMAGE` overrides it). Until launch the package is private,
  so Connect asks for a registry username and a token with `read:packages`
  (D052). Connect resolves the image to its digest with those credentials,
  which proves they can pull, and machines then run exactly that digest even
  if the tag moves (D058). When the server is upgraded, a provider on the
  default image moves to the new build's digest by itself (D119).
- **A monthly spend limit and an hourly rate.** ClipMux estimates spend from
  the rate you enter and each machine's running time (D055). Modal's own
  billing report replaces the estimate up to the last hour it covers (D119);
  RunPod's spend stays the estimate. A job reserves its estimated run (an hour per 4 GiB of
  source, between 15 minutes and 6 h 15 min) before its machine starts, so
  the limit holds before any machine has run long. A machine already running
  isn't stopped at the limit.
- **A concurrency limit.** At most this many machines run at once.

A provider takes no work until a setup test through it passes: after Connect,
a sample must encode on one of its machines and play. When it passes, the
provider becomes the primary, with the built-in worker as its fallback when
serve runs one (D116); until then new videos encode where they did. On
**Encoding** the owner picks this server or a connected provider, and ticks
whether this server takes over when the provider can't start a machine.
`PUT /v1/settings/routing` sets the same, with the queue-age rule too.

## RunPod

**Encoding → RunPod → Connect RunPod**: **Get a key from RunPod** opens
RunPod's console, which creates a key for ClipMux and shows it (the page
names RunPod's Flash CLI, D109); paste it into the field. A key you create
yourself under **Settings → API Keys**, with read and write access to pods
and container registry auths, works too. The dialog then lists RunPod's
NVENC GPUs with their Secure Cloud price per hour, the L4 recommended, and
ticks the first three. Limits default to one machine and $50 a month
(Change), and the rate is the dearest chosen GPU's price (D111). Or call:

```bash
curl -X POST https://video.example.com/v1/executors/runpod/connect \
  -H 'Content-Type: application/json' --cookie "$SESSION" \
  -d '{"api_key": "rpa_...", "registry_username": "you", "registry_password": "ghp_...",
       "max_concurrent": 2, "monthly_limit_cents": 5000, "rate_cents_per_hour": 45}'
```

Optional fields: `image` (default `CLIPMUX_WORKER_IMAGE`), `gpu_type_ids`
(default an L4, RTX A4000 or RTX A5000, whichever has capacity: cards with
NVENC; RunPod's RTX 4090 hosts refuse it, D097), and `disk_gb` (default 50; a machine takes only sources
whose size, three times over, fits).

Connect checks the key and the public URL, pins the image's digest with the
registry credentials, stores those credentials in RunPod, saves the key
sealed, and queues the setup test. Connect again to
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
- **Disconnect** with `DELETE /v1/executors/runpod`. It is refused while a
  RunPod pod may still run. Routing that named RunPod moves back to this
  server (D116). It forgets the key and removes the registry credentials
  from the account.

## Modal

**Encoding → Modal → Connect Modal**: pick GPUs (Modal's NVENC ones,
priced with each sandbox's CPU and memory; limits default as for RunPod,
D111), then **Connect with Modal** opens the page Modal's CLI uses to issue a token (D109); approve
it there and the token goes straight to this server. To paste one instead,
create a token in Modal (**Settings → API Tokens**); pasting the
`modal token set …` command it shows fills both fields. Or call
`POST /v1/executors/modal/connect` with:

```json
{"token_id": "ak-...", "token_secret": "as-...", "registry_username": "you", "registry_password": "ghp_...",
 "max_concurrent": 2, "monthly_limit_cents": 5000, "rate_cents_per_hour": 90}
```

Optional fields: `environment` (the Modal environment; default the account's
own), `image` (default `CLIPMUX_WORKER_IMAGE`), `gpu` (default `L4,A10G`: GPU
types with NVENC and an AV1 decoder, tried in order until one has capacity, D097,
D132; the T4 can be chosen, but decodes AV1 on the CPU), `cpu` (default 4 cores),
`memory_mib` (default 16384) and `disk_gb` (default 50, held to the
free-disk rule).

Connect checks the token by opening this install's Modal app,
`clipmux-{tag}`, then checks the public URL, pins the image's digest, saves
the token and the registry credentials sealed, and queues the setup test. Connect again to
rotate the token or change settings; Modal then needs a new setup test.

- **Sandboxes** run in the app `clipmux-{tag}`, tagged with this install and
  their dispatch, so ClipMux only ever lists or stops its own. Modal ends a
  sandbox when `clipmux worker --once` exits, or at 6 h 15 min; ClipMux also
  stops one whose attempt ended or that didn't claim in time.
- **Registry credentials** stay sealed in ClipMux's database. Each wake hands
  them to Modal as an ephemeral secret for that pull; no named secret is
  left in the Modal workspace.
- **Modal runs gVisor without Landlock** (D051). The sandbox's rlimits,
  `no_new_privs` and seccomp hold, which is what a serverless claim needs.
- **Disconnect** with `DELETE /v1/executors/modal`, under the same rules as
  RunPod.

## Security

A provider's machine is a worker like any other: it sees one source and one
output key, for one job, and can write only that job's output. Its one-time
key claims that job once, before a deadline, and is stored hashed and sealed.
Its tools always run sandboxed (`409 sandbox_required` otherwise). A source
that failed on any worker never reaches a provider (D053).
