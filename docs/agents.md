# PC agents

A PC agent is a computer you own that encodes for your ClipMux server, with
its GPU if it has one. It connects out to the server, so it works behind NAT,
and it holds no storage credentials: each job gives it an attempt token and
signed URLs for that job only.

## Connect the PC you're using

```bash
clipmux worker --server https://video.example.com
```

The agent prints a code and opens the dashboard's connect page in your
browser. Check that the code matches and click **Connect**. The agent keeps its
key in `agent.json` (mode 0600) in your config directory, so from then on
`clipmux worker` alone starts it. On a machine without a browser, open the
printed link anywhere you're signed in to the dashboard.

## Connect another machine

On the dashboard's **Workers** page, choose **Create a command**. It shows a
one-time command that works for 15 minutes:

```bash
clipmux worker --server https://video.example.com --pair ABCD-EFGH
```

The page shows the PC as connected the moment the command runs.

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

## Disconnect

**Revoke** on the Workers page stops the agent at once. Its running job moves
to another worker without using up a retry. Afterwards the page offers to
re-encode the videos whose current version that PC encoded.
