"""Real NVIDIA encodes and transcriptions on Modal (P3-T03, D046, D131).

GitHub's runners have no GPU, and the owner's machine is AMD, so NVENC,
cuda-decode and CUDA whisper are proven here: an L4 has NVENC and AV1
decode, a T4 NVENC but no AV1 decode, and an A100 NVDEC but no NVENC. The
image builds the same pinned FFmpeg, Shaka Packager and whisper.cpp (with
its CUDA backend, as the amd64 ClipMux image does) from toolchain/, bakes the
speech models, and runs the media package's test binary, which is also its
own `sandbox-exec`.

    CGO_ENABLED=0 go test -c -o deploy/modal/media.test ./internal/media
    uvx --from modal modal run deploy/modal/gpu_test.py --which l4

--which takes a comma-separated list: nvenc (any of L4, A10G, T4), l4, t4,
cuda-decode (an A100). The first run builds FFmpeg and whisper.cpp with CUDA
on 16 cores; Modal caches each step.
"""

import pathlib
import subprocess

import modal

# Paths on this machine. Inside the container the module is /root/gpu_test.py
# and the image is already built, so they are never read there.
HERE = pathlib.Path(__file__).resolve()
ROOT = HERE.parents[2] if len(HERE.parents) > 2 else HERE.parent
TEST_BINARY = HERE.with_name("media.test")
TC = "/opt/clipmux-toolchain"


def sh(cmd: str) -> None:
    subprocess.run(["bash", "-c", cmd], check=True)


# The two long builds run as functions, which can have many cores: Modal's
# plain build steps get one, and nvcc at -j1 takes hours.
def build_ffmpeg() -> None:
    sh(f"JOBS=16 CLIPMUX_BUILD_DIR=/var/tmp/ffmpeg-build bash {TC}/build_ffmpeg.sh /usr/local && rm -rf /var/tmp/ffmpeg-build")


def build_whisper() -> None:
    # As the amd64 image does: install the CUDA toolkit, build against it,
    # purge it, keeping only cudart and cuBLAS beside whisper-cli.
    sh(
        f"bash {TC}/install_cuda.sh"
        f" && JOBS=12 CLIPMUX_WHISPER_CUDA=1 CLIPMUX_BUILD_DIR=/var/tmp/whisper-build bash {TC}/build_whisper.sh /usr/local"
        f" && bash {TC}/install_cuda.sh --remove && rm -rf /var/tmp/whisper-build"
        f" && ldconfig && CLIPMUX_WHISPER_CUDA=1 bash {TC}/verify_toolchain.sh /usr/local/bin/ffmpeg /usr/local/bin/ffprobe /usr/local/bin/whisper-cli"
    )


image = (
    modal.Image.from_registry("ubuntu:24.04", add_python="3.12")
    # graphics asks for NVIDIA's Vulkan ICD, which gVisor passes only where
    # Modal allows it; vulkan() reports whether it did.
    .env({"DEBIAN_FRONTEND": "noninteractive", "NVIDIA_DRIVER_CAPABILITIES": "compute,video,utility,graphics"})
    .add_local_dir(ROOT / "toolchain", TC, copy=True)
    .run_commands(
        f"bash -c '. {TC}/apt-packages.env && apt-get update"
        ' && apt-get install -y --no-install-recommends "${CLIPMUX_BUILD_PACKAGES[@]}" "${CLIPMUX_RUNTIME_PACKAGES[@]}" "${CLIPMUX_RUNTIME_PACKAGES_AMD64[@]}"'
        " curl ca-certificates xz-utils'"
    )
    .run_function(build_ffmpeg, cpu=16, memory=32768, timeout=3600)
    .run_commands(f"bash {TC}/install_packager.sh /usr/local/bin")
    .run_function(build_whisper, cpu=16, memory=65536, timeout=5400)
    .run_commands(f"bash {TC}/fetch_speech_models.sh /models && chmod 0644 /models/*")
    .apt_install("strace")
    .add_local_dir(ROOT / "internal" / "media" / "testdata", "/opt/media/testdata")
    .add_local_file(TEST_BINARY, "/opt/media.test")
)

app = modal.App("clipmux-gpu-test", image=image)

TESTS = "GPUBackends|GPU4KLadder|Golden|DecryptsBack|WhisperOnCUDA|WhisperOnVulkan|ProbeSpeech|TheSpeechProbe"
MODEL = "/models/ggml-large-v3-turbo-q8_0.bin"
PATH_ENV = {"PATH": "/usr/local/bin:/usr/bin:/bin"}


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
        for how, prefix in (("unconfined", []), ("sandboxed", sandbox + ["--"])):
            r = subprocess.run(prefix + base + args, capture_output=True, text=True, cwd="/tmp", env=PATH_ENV)
            out.append(f"{name} {how}: exit {r.returncode} {r.stderr.strip()[-600:]}")
    # Which of the calls the sandbox refuses does CUDA make?
    refused = "socket,ptrace,mount,umount2,add_key,request_key,keyctl,perf_event_open,process_vm_readv,process_vm_writev,bpf,userfaultfd,io_uring_setup"
    r = subprocess.run(["strace", "-f", "-e", "trace=" + refused] + base + PROBES["cuda"], capture_output=True, text=True, cwd="/tmp", env=PATH_ENV)
    calls = [l for l in r.stderr.splitlines() if "(" in l and not l.startswith("[CUDA")]
    out.append("refused calls CUDA makes unconfined:\n" + "\n".join(calls[-40:]))
    return "\n".join(out)


def vulkan() -> str:
    """Whether Vulkan sees the NVIDIA card here (graphics asked for), and
    which GPU whisper takes with CUDA hidden: unconfined, so only Modal's
    runtime decides."""
    subprocess.run(["/usr/local/bin/ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=16000:duration=2",
                    "-ac", "1", "-c:a", "pcm_s16le", "/tmp/probe.wav"], check=True, env=PATH_ENV)
    icds = sorted(str(p) for d in ("/usr/share/vulkan/icd.d", "/etc/vulkan/icd.d") for p in pathlib.Path(d).glob("*.json"))
    r = subprocess.run(["/usr/local/bin/whisper-cli", "-m", MODEL, "-f", "/tmp/probe.wav", "-l", "en", "-t", "4", "-ojf", "-of", "/tmp/vk"],
                       capture_output=True, text=True, cwd="/tmp", env={**PATH_ENV, "CUDA_VISIBLE_DEVICES": ""})
    lines = [l for l in r.stderr.splitlines() if l.startswith(("ggml_vulkan", "whisper_backend_init_gpu", "load_backend"))]
    return f"Vulkan ICDs: {icds}\nwhisper with CUDA hidden: exit {r.returncode}\n" + "\n".join(lines)


def run() -> str:
    # The NVIDIA runtime's environment reaches the sandboxed tools only
    # through the allow-list (CUDA_VISIBLE_DEVICES and friends).
    nodes = sorted(str(p) for p in pathlib.Path("/dev").glob("nvidia*"))
    probe = subprocess.run(["/opt/media.test", "sandbox-exec", "--probe"], capture_output=True, text=True)
    result = subprocess.run(
        ["/opt/media.test", "-test.run", TESTS, "-test.v", "-test.count=1", "-test.timeout=30m"],
        env={**PATH_ENV, "CLIPMUX_GPU_TEST": "1", "CLIPMUX_REQUIRE_MEDIA_TOOLS": "1", "HOME": "/tmp",
             "CLIPMUX_WHISPER": "/usr/local/bin/whisper-cli", "CLIPMUX_TEST_MODELS_DIR": "/models"},
        capture_output=True,
        text=True,
        cwd="/opt/media",  # the speech tests read testdata/ beside them
    )
    smi = subprocess.run(["nvidia-smi", "--query-gpu=name,driver_version", "--format=csv,noheader"], capture_output=True, text=True)
    return (
        f"GPU: {smi.stdout.strip()}\ndevice nodes: {nodes}\nsandbox probe: {probe.stdout.strip()} {probe.stderr.strip()}\n"
        f"{diagnose(nodes)}\n{vulkan()}\n"
        f"exit {result.returncode}\n{result.stdout}\n{result.stderr}"
    )


@app.function(gpu=["L4", "A10G", "T4"], timeout=2400)
def nvenc() -> str:
    return run()


@app.function(gpu="L4", timeout=2400)
def l4() -> str:
    return run()


@app.function(gpu="T4", timeout=2400)
def t4() -> str:
    return run()


@app.function(gpu="A100", timeout=2400)
def cuda_decode() -> str:
    return run()


@app.local_entrypoint()
def main(which: str = "nvenc"):
    functions = {
        "nvenc": ("NVENC (L4, A10G or T4)", nvenc),
        "l4": ("L4", l4),
        "t4": ("T4 (no AV1 decode)", t4),
        "cuda-decode": ("A100 (no NVENC)", cuda_decode),
    }
    calls = [(functions[w][0], functions[w][1].spawn()) for w in which.split(",")]
    for name, call in calls:
        print(f"===== {name} =====")
        print(call.get())
