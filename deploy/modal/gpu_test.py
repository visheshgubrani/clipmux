"""Real NVIDIA encodes on Modal (P3-T03, D046).

GitHub's runners have no GPU, and the owner's machine is AMD, so NVENC and
cuda-decode are proven here: an L4 has NVENC, and an A100 has NVDEC but no
NVENC. The image builds the same pinned FFmpeg and Shaka Packager as the
ClipMux image, from toolchain/, and runs the media package's test binary,
which is also its own `sandbox-exec`.

    CGO_ENABLED=0 go test -c -o deploy/modal/media.test ./internal/media
    uvx --from modal modal run deploy/modal/gpu_test.py

The first run builds FFmpeg (about 15 minutes); Modal caches the image.
"""

import pathlib
import subprocess

import modal

# Paths on this machine. Inside the container the module is /root/gpu_test.py
# and the image is already built, so they are never read there.
HERE = pathlib.Path(__file__).resolve()
ROOT = HERE.parents[2] if len(HERE.parents) > 2 else HERE.parent
TEST_BINARY = HERE.with_name("media.test")

image = (
    modal.Image.from_registry("ubuntu:24.04", add_python="3.12")
    .env({"DEBIAN_FRONTEND": "noninteractive"})
    .add_local_dir(ROOT / "toolchain", "/opt/clipmux-toolchain", copy=True)
    .run_commands(
        "bash -c '. /opt/clipmux-toolchain/apt-packages.env && apt-get update"
        ' && apt-get install -y --no-install-recommends "${CLIPMUX_BUILD_PACKAGES[@]}" "${CLIPMUX_RUNTIME_PACKAGES[@]}"'
        " curl ca-certificates xz-utils'",
        "CLIPMUX_BUILD_DIR=/var/tmp/ffmpeg-build bash /opt/clipmux-toolchain/build_ffmpeg.sh /usr/local",
        "bash /opt/clipmux-toolchain/install_packager.sh /usr/local/bin",
        "ldconfig && rm -rf /var/tmp/ffmpeg-build",
    )
    .apt_install("strace")
    .add_local_file(TEST_BINARY, "/opt/media.test")
)

app = modal.App("clipmux-gpu-test", image=image)

TESTS = "GPUBackends|Golden|DecryptsBack"


PROBES = {
    "nvenc": ["-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30", "-frames:v", "2", "-c:v", "h264_nvenc", "-f", "null", "-"],
    "cuda": ["-init_hw_device", "cuda=cu", "-filter_hw_device", "cu", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30",
             "-frames:v", "2", "-vf", "format=nv12,hwupload,hwdownload,format=nv12", "-f", "null", "-"],
}


def diagnose(nodes: list[str]) -> str:
    """The probe encodes, unconfined and through the sandbox, with stderr."""
    out = []
    base = ["/usr/local/bin/ffmpeg", "-hide_banner", "-nostdin", "-loglevel", "error"]
    sandbox = ["/opt/media.test", "sandbox-exec", "--write", "/tmp", "--gpu"] + [a for n in nodes for a in ("--device", n)]
    for name, args in PROBES.items():
        for how, prefix in (("unconfined", []), ("sandboxed", sandbox + ["--"]),
                            ("sandboxed, no memory limit", sandbox + ["--memory-mb", "1048576", "--"])):
            r = subprocess.run(prefix + base + args, capture_output=True, text=True, cwd="/tmp", env={"PATH": "/usr/local/bin:/usr/bin:/bin"})
            out.append(f"{name} {how}: exit {r.returncode} {r.stderr.strip()[-600:]}")
    # Which of the calls the sandbox refuses does CUDA make?
    refused = "socket,ptrace,mount,umount2,add_key,request_key,keyctl,perf_event_open,process_vm_readv,process_vm_writev,bpf,userfaultfd,io_uring_setup"
    r = subprocess.run(["strace", "-f", "-e", "trace=" + refused] + base + PROBES["cuda"], capture_output=True, text=True, cwd="/tmp",
                       env={"PATH": "/usr/local/bin:/usr/bin:/bin"})
    calls = [l for l in r.stderr.splitlines() if "(" in l and not l.startswith("[CUDA")]
    out.append("refused calls CUDA makes unconfined:\n" + "\n".join(calls[-40:]))
    return "\n".join(out)


def run() -> str:
    # The NVIDIA runtime's environment reaches the sandboxed tools only
    # through the allow-list (CUDA_VISIBLE_DEVICES and friends).
    nodes = sorted(str(p) for p in pathlib.Path("/dev").glob("nvidia*"))
    probe = subprocess.run(["/opt/media.test", "sandbox-exec", "--probe"], capture_output=True, text=True)
    result = subprocess.run(
        ["/opt/media.test", "-test.run", TESTS, "-test.v", "-test.count=1"],
        env={"PATH": "/usr/local/bin:/usr/bin:/bin", "CLIPMUX_GPU_TEST": "1", "CLIPMUX_REQUIRE_MEDIA_TOOLS": "1", "HOME": "/tmp"},
        capture_output=True,
        text=True,
        cwd="/tmp",
    )
    smi = subprocess.run(["nvidia-smi", "--query-gpu=name,driver_version", "--format=csv,noheader"], capture_output=True, text=True)
    return (
        f"GPU: {smi.stdout.strip()}\ndevice nodes: {nodes}\nsandbox probe: {probe.stdout.strip()} {probe.stderr.strip()}\n"
        f"{diagnose(nodes)}\n"
        f"exit {result.returncode}\n{result.stdout}\n{result.stderr}"
    )


@app.function(gpu=["L4", "A10G", "T4"], timeout=1200)
def nvenc() -> str:
    return run()


@app.function(gpu="A100", timeout=1200)
def cuda_decode() -> str:
    return run()


@app.local_entrypoint()
def main(which: str = "both"):
    calls = []
    if which in ("both", "nvenc"):
        calls.append(("NVENC (L4, A10G or T4)", nvenc.spawn()))
    if which in ("both", "cuda-decode"):
        calls.append(("A100 (no NVENC)", cuda_decode.spawn()))
    for name, call in calls:
        print(f"===== {name} =====")
        print(call.get())
