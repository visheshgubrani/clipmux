# PC agents

A PC agent is a computer you own that encodes for your ClipMux server, with
its GPU if it has one. It connects out to the server, so it works behind NAT,
and it holds no storage credentials: each job gives it an attempt token and
signed URLs for that job only.

## An advanced, command-line option

The dashboard doesn't offer PCs (D116). Most installs don't need one: the
server's own worker already uses its GPU when it can see one, and a VPS has
Modal or RunPod on **Encoding**. A PC agent still works, for the case of a
small server and a GPU at home, and is managed from the command line and the
API instead:

| To | Run |
| --- | --- |
| Connect a PC | `clipmux worker --server URL` on the PC (below) |
| Pair one without a browser | `clipmux admin create-pairing [--app NAME]` on the server |
| Send jobs to PCs | `PUT /v1/settings/routing {"primary_executor": "workers"}` (this server and PCs) or `"agents"` (PCs only) |
| List PCs | `GET /v1/agents` |
| Revoke a PC | `DELETE /v1/agents/{id}`, then `POST /v1/agents/{id}/reencode` if you no longer trust what it encoded |

The `/v1/settings/routing` and `/v1/agents` routes need the owner's session
(D025): call them from a signed-in browser, or with its session cookie.

**Which jobs a PC takes.** A fresh install's routing is `workers`, so a
paired PC takes jobs alongside this server. Choosing **This server** on
Encoding writes `builtin` and choosing a cloud GPU writes that provider; a
PC then takes only the jobs of videos it registered with `push --local`.
Because the dashboard lists no PCs, revoke one you no longer use; it keeps
its key until you do.

## Connect the PC you're using

```bash
clipmux worker --server https://video.example.com
```

The agent prints a code and opens the server's connect page
(`/workers/connect`) in your browser. Check that the code matches and click
**Connect**. The agent keeps its
key in `agent.json` (mode 0600) in your config directory, so from then on
`clipmux worker` alone starts it. On a machine without a browser, open the
printed link anywhere you're signed in to the dashboard.

## Connect another machine

On the server, `clipmux admin create-pairing` (or `POST /v1/agents/pairings`)
makes a one-time command that works for 15 minutes:

```bash
clipmux worker --server https://video.example.com --pair ABCD-EFGH
```

The PC is connected the moment the command runs; `GET /v1/agents` lists it.

## Linux without Docker

The agent tarball (`clipmux-agent-linux-amd64.tar.gz` or `-arm64`) is
clipmux with its own FFmpeg, libraries, VAAPI drivers (AMD, and Intel on
amd64) and Shaka Packager, so nothing else needs installing. It needs a
glibc at least as new as Ubuntu 24.04's (2.39), such as Ubuntu 24.04,
Debian 13 or Fedora 40 and later. Until launch it is a CI artifact (the
Agent tarball workflow), or build it from this repository:

```bash
docker build --target agent --output type=local,dest=dist .
tar -xzf dist/clipmux-agent-linux-amd64.tar.gz
clipmux-agent/clipmux worker doctor --offline
clipmux-agent/clipmux worker --server https://video.example.com
```

`clipmux-agent/clipmux` points the agent at its bundled tools; link it into
your PATH as `clipmux`. `SHA256SUMS` lists every file, and `licenses/` holds
FFmpeg's GPL notices and the copyright of every bundled library.

## Docker (Windows, macOS, or Linux without installing anything)

The ClipMux image is also the agent. The Docker variant of the command runs it
as your user and keeps its connection in `~/.config/clipmux`, where a
restarted container finds it again:

```bash
docker run -d --name clipmux-agent --restart unless-stopped --no-healthcheck \
  --user "$(id -u):$(id -g)" \
  -v "$HOME/.config/clipmux:/var/lib/clipmux-agent" -e CLIPMUX_AGENT_DIR=/var/lib/clipmux-agent \
  ghcr.io/visheshgubrani/clipmux worker --server https://video.example.com --pair ABCD-EFGH
```

For a GPU, add one of these after `docker run`:

- **NVIDIA** (Linux, or Windows through WSL2 and Docker Desktop): `--gpus all`.
  This needs the NVIDIA Container Toolkit.
- **AMD or Intel** (Linux): `--device /dev/dri --group-add "$(getent group render | cut -d: -f3)"`.

Docker Desktop on a Mac runs Linux in a VM without the GPU, so it encodes on
the CPU. The agent's sandbox still holds there.

The image isn't published until launch (D005), so build it from this
repository with `docker build -t ghcr.io/visheshgubrani/clipmux .` until then.

## Encoders

When it starts, an agent tries a two-frame test encode on each GPU path,
inside the same sandbox jobs use. It offers only the paths that pass:

| Path | Hardware | What runs where |
| --- | --- | --- |
| `nvenc` | NVIDIA with NVENC | decode, scale and encode on the GPU |
| `vaapi` | AMD (Radeon), Intel (iHD driver) | decode, scale and encode on the GPU |
| `cuda-decode` | NVIDIA without NVENC | decode on the GPU, encode with libx264 |
| `cpu` | everything | libx264 |

A source the GPU can't decode, such as 10-bit or 4:2:2, decodes on the CPU and
still encodes on the GPU (`nvenc-hybrid`, `vaapi-hybrid`). If a GPU path fails
during a job, the whole ladder runs again on the next path, ending with the
CPU, within the same attempt. The Jobs page shows the path each output used
and any that failed. `--encoder vaapi` (or `CLIPMUX_ENCODER`) chooses the
first path to try, `CLIPMUX_VAAPI_DEVICE` picks a render node, and
`--encoder cpu` skips the GPU entirely.

## Videos that stay on the PC

```bash
clipmux push --local ~/Videos/course
```

The files aren't uploaded. The agent on this PC registers them, encodes them
where they are, and uploads only the encrypted outputs. Without an API key
they become your own uploads, as if from the dashboard; with
`CLIPMUX_API_KEY`, the key's app owns them. Jobs on these videos run only on
this PC: while it's asleep they wait, and they end as `agent_unavailable` if
it stays away for 7 days. A file that changes or disappears before it's encoded
ends its job as `source_changed` or `source_missing`, and the video keeps
playing what it had.

To keep re-encoding possible without this PC, add `--upload-source`:

```bash
clipmux push --local --upload-source ~/Videos/course
```

Each video is still encoded here first, so it plays without waiting for an
upload. Then the agent uploads the file to the server's storage, in 32 MiB
parts, and from then on the video is like any other upload: any worker,
including a cloud fallback, can re-encode it. If the file changes before it
is uploaded, the upload is refused (`source_changed`) and the video stays
tied to this PC.

To limit what `push --local` may hand over, list folders in `agent.json`:

```json
{ "roots": ["/home/me/Videos"] }
```

With Docker, mount the videos at the same path inside the container, for
example `-v "$HOME/Videos:$HOME/Videos:ro"`.

## Sandbox

Every ffmpeg and packager run is confined: no network, writes only to the
job's own directory, reads only system paths and the job's source, and
CPU, memory and file limits. A GPU path may also open its own device nodes,
and, for CUDA's internal IPC, local `SOCK_SEQPACKET` sockets; never a
network socket.
An agent where the sandbox can't hold (not Linux, an old kernel) refuses to
start unless you pass `--unsandboxed`. It then takes only jobs pinned to it
and your own uploads, never an app's public uploads.

## Check a PC

```bash
clipmux worker doctor
```

This checks the connection, the server, the key, the tool versions, the
sandbox, each encoder and scratch disk space. The first ✗ is the one to fix.
`clipmux worker doctor --offline` skips the first three, to check a machine
before it is connected.

## Disconnect

`DELETE /v1/agents/{id}` stops the agent at once. Its running job moves to
another worker without using up a retry. `POST /v1/agents/{id}/reencode`
then re-encodes the videos whose current version that PC encoded.
