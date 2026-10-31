#!/usr/bin/env python3
"""GuideLLM matrix runner.

One command per contender runs the whole profile matrix from matrix.json
against one OpenAI-compatible endpoint, after checking that slo.json still
matches its recorded checksum. Standard library only; GuideLLM itself is
invoked as an external command, natively or through the container image.

Subcommands: verify, freeze, run, env, summary (see README.md).
"""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import platform
import shlex
import shutil
import subprocess
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
SLO_FILE = HERE / "slo.json"
SLO_SUM = HERE / "slo.sha256"
MATRIX_FILE = HERE / "matrix.json"
REPO_ROOT = HERE.parent.parent
RUN_KINDS = ("DRY RUN", "REHEARSAL", "MEASURED")
DEFAULT_IMAGE = "marbor-guidellm:0.8.1"
API_KEY_ENV = "GUIDELLM_API_KEY"


# ---------------------------------------------------------------- slo checksum

def sha256_of(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def recorded_sum() -> str | None:
    if not SLO_SUM.exists():
        return None
    return SLO_SUM.read_text().split()[0]


def cmd_freeze(_: argparse.Namespace) -> int:
    SLO_SUM.write_bytes(f"{sha256_of(SLO_FILE)}  slo.json\n".encode())
    print(f"recorded {SLO_SUM.name}: {sha256_of(SLO_FILE)}")
    return 0


def cmd_verify(_: argparse.Namespace) -> int:
    want, got = recorded_sum(), sha256_of(SLO_FILE)
    if want is None:
        print("FAIL: slo.sha256 missing; run 'freeze' once before the first measured run")
        return 1
    if want != got:
        print(f"FAIL: slo.json changed after freeze\n  recorded {want}\n  current  {got}")
        return 1
    print(f"OK: slo.json matches slo.sha256 ({got})")
    return 0


# ----------------------------------------------------------------- environment

def sh(cmd: list[str], timeout: int = 60) -> str:
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
        return (r.stdout + r.stderr).strip()
    except (OSError, subprocess.SubprocessError) as exc:
        return f"unavailable: {exc}"


def guidellm_argv(args: argparse.Namespace, out_dir: Path) -> list[str]:
    """Command prefix that runs guidellm, natively or inside the image."""
    if args.container:
        argv = ["docker", "run", "--rm", "-v", f"{out_dir}:/out"]
        cache = Path(args.hf_cache).expanduser().resolve()
        cache.mkdir(parents=True, exist_ok=True)
        argv += ["-v", f"{cache}:/root/.cache/huggingface"]
        argv += ["--add-host", "host.docker.internal:host-gateway"]
        for var in (API_KEY_ENV, "HF_TOKEN"):
            if os.environ.get(var):
                argv += ["-e", var]
        return argv + [args.container]
    return shlex.split(args.guidellm_cmd)


def container_path(args: argparse.Namespace, out_dir: Path, p: Path) -> str:
    """Path of p as GuideLLM sees it."""
    if args.container:
        return "/out/" + p.relative_to(out_dir).as_posix()
    return str(p)


def capture_env(args: argparse.Namespace, out_dir: Path, commands: list[str]) -> None:
    env_dir = out_dir / "env" / args.contender
    env_dir.mkdir(parents=True, exist_ok=True)
    info = {
        "run_kind": args.run_kind,
        "captured_utc": dt.datetime.now(dt.timezone.utc).isoformat(),
        "endpoint": args.endpoint,
        "model": args.model,
        "contender": args.contender,
        "notes": dict(n.split("=", 1) for n in args.note),
        "host": {"hostname": platform.node(), "platform": platform.platform(),
                 "python": sys.version.split()[0]},
        "git_sha": sh(["git", "-C", str(REPO_ROOT), "rev-parse", "HEAD"]),
        "git_dirty": bool(sh(["git", "-C", str(REPO_ROOT), "status", "--porcelain", "--",
                              "bench/guidellm"])),
        "guidellm_version": sh(guidellm_argv(args, out_dir) + ["--version"], 300),
        "slo_sha256": sha256_of(SLO_FILE),
        "vllm_bench_version": (sh(shlex.split(args.vllm_cmd) + ["--version"])
                               if args.cross_check else "not requested"),
    }
    if args.container:
        info["container"] = {
            "image": args.container,
            "image_id": sh(["docker", "image", "inspect", args.container,
                            "--format", "{{.Id}}"]),
            "docker": sh(["docker", "version", "--format", "{{.Server.Version}}"])}
    (env_dir / "environment.json").write_text(json.dumps(info, indent=2) + "\n")
    shutil.copy(MATRIX_FILE, env_dir / "matrix.json")
    shutil.copy(SLO_FILE, env_dir / "slo.json")
    shutil.copy(SLO_SUM, env_dir / "slo.sha256")
    if shutil.which("nvidia-smi"):
        (env_dir / "nvidia-smi.txt").write_text(sh(["nvidia-smi"]))
        (env_dir / "nvidia-smi-query.csv").write_text(sh(
            ["nvidia-smi", "--query-gpu=index,name,driver_version,memory.total,pci.bus_id",
             "--format=csv"]))
    (env_dir / "commands.txt").write_text("\n".join(commands) + "\n")


def cmd_env(args: argparse.Namespace) -> int:
    out_dir = Path(args.out).resolve()
    capture_env(args, out_dir, [])
    print(f"environment captured under {out_dir / 'env' / args.contender}")
    return 0


# ----------------------------------------------------------------------- runs

def redact(argv: list[str], secret: str | None) -> str:
    line = shlex.join(argv)
    return line.replace(secret, "REDACTED") if secret else line


def build_run_argv(args, out_dir, shape, prof, json_path, requests):
    backend = (f"kind=openai_http,target={args.endpoint},model={args.model},"
               f"request_format={args.request_format},validate_backend=false")
    key = os.environ.get(API_KEY_ENV)
    if key:
        backend += f",api_key={key}"
    kind = prof["profile"]
    if kind == "sweep":
        profile = f"kind=sweep,sweep_size={prof['rate']},strategy_type=constant"
    elif kind == "concurrent":
        profile = f"kind=concurrent,streams=[{prof['rate']}]"
    else:
        profile = f"kind={kind},rate=[{prof['rate']}]"
    return guidellm_argv(args, out_dir) + [
        "run",
        "--backend", backend,
        "--tokenizer", f"kind=hf_auto,model={args.tokenizer or args.model}",
        "--profile", profile,
        "--constraint", f"kind=max_requests,count={requests}",
        "--data", f"kind=synthetic_text,prompt_tokens={shape['prompt_tokens']},"
                  f"output_tokens={shape['output_tokens']}",
        "--seed", f"kind=static,value={args.seed}",
        "--output", f"kind=json,path={container_path(args, out_dir, json_path)}",
        "--label", f"run_kind={args.run_kind.replace(' ', '-')}",
        "--label", f"contender={args.contender}",
        "--disable-console-interactive",
    ]


class GpuSampler:
    """nvidia-smi samples every 5 s for the duration of one run, when present."""

    def __init__(self, path: Path):
        self.proc = None
        if shutil.which("nvidia-smi"):
            self.fh = path.open("w")
            self.proc = subprocess.Popen(
                ["nvidia-smi", "--query-gpu=timestamp,index,utilization.gpu,memory.used,"
                 "power.draw,temperature.gpu", "--format=csv", "-l", "5"],
                stdout=self.fh, stderr=subprocess.DEVNULL)

    def stop(self) -> None:
        if self.proc:
            self.proc.terminate()
            self.proc.wait()
            self.fh.close()


def execute(argv: list[str], log: Path, timeout: int) -> tuple[str, int | None, str]:
    """Returns (status, exit_code, reason)."""
    with log.open("w") as fh:
        try:
            r = subprocess.run(argv, stdout=fh, stderr=subprocess.STDOUT, timeout=timeout)
        except subprocess.TimeoutExpired:
            return "failed", None, f"timeout after {timeout}s"
        except OSError as exc:
            return "failed", None, f"could not start: {exc}"
    if r.returncode != 0:
        return "failed", r.returncode, f"exit code {r.returncode}, see {log.name}"
    return "ok", 0, ""


def cmd_run(args: argparse.Namespace) -> int:
    if cmd_verify(args) != 0:
        return 2
    matrix = json.loads(MATRIX_FILE.read_text())
    args.seed = args.seed if args.seed is not None else matrix["seed"]
    out_dir = Path(args.out).resolve()
    base = out_dir / args.contender
    base.mkdir(parents=True, exist_ok=True)
    commands: list[str] = []
    manifest = (base / "manifest.jsonl").open("a")
    key = os.environ.get(API_KEY_ENV)
    shapes = {k: v for k, v in matrix["shapes"].items() if not args.shape or k in args.shape}
    profiles = [p for p in matrix["profiles"] if not args.profile or p["name"] in args.profile]

    def one(shape_name, shape, prof, stage):
        sub = base / ("warmup" if stage == "warmup" else shape_name)
        sub.mkdir(parents=True, exist_ok=True)
        stem = f"{shape_name}-{prof['name']}" if stage == "warmup" else prof["name"]
        requests = max(1, round(prof["max_requests"] * args.requests_scale))
        jpath = sub / f"{stem}.json"
        argv = build_run_argv(args, out_dir, shape, prof, jpath, requests)
        commands.append(redact(argv, key))
        sampler = GpuSampler(sub / f"{stem}.gpu.csv")
        started = time.time()
        status, code, reason = execute(argv, sub / f"{stem}.log", args.run_timeout)
        sampler.stop()
        if status == "ok" and not jpath.exists():
            status, reason = "failed", "guidellm exited 0 but wrote no JSON"
        rec = {"stage": stage, "contender": args.contender, "shape": shape_name,
               "profile": prof["name"], "requests": requests, "status": status,
               "exit_code": code, "reason": reason, "run_kind": args.run_kind,
               "seconds": round(time.time() - started, 1),
               "json": str(jpath.relative_to(out_dir)), "command": redact(argv, key)}
        manifest.write(json.dumps(rec) + "\n")
        manifest.flush()
        print(f"[{stage}] {args.contender} {shape_name} {prof['name']}: {status} "
              f"({rec['seconds']}s){' - ' + reason if reason else ''}", flush=True)

    first_shape = next(iter(shapes))
    one(first_shape, shapes[first_shape], {**matrix["warmup"], "name": "warmup"}, "warmup")
    for shape_name, shape in shapes.items():
        for prof in profiles:
            one(shape_name, shape, prof, "measure")
        if args.cross_check:
            cross_check(args, base, shape_name, shape, commands, manifest)
    manifest.close()
    capture_env(args, out_dir, commands)
    print(f"done; summarize with: runner.py summary --results {out_dir}")
    return 0


def cross_check(args, base, shape_name, shape, commands, manifest) -> None:
    """One fixed-concurrency vllm bench serve run per shape, as a cross-check."""
    sub = base / "crosscheck"
    sub.mkdir(parents=True, exist_ok=True)
    stem = f"vllm-bench-{shape_name}"
    argv = shlex.split(args.vllm_cmd) + [
        "bench", "serve", "--base-url", args.endpoint, "--model", args.model,
        "--endpoint", "/v1/completions", "--dataset-name", "random",
        "--random-input-len", str(shape["prompt_tokens"]),
        "--random-output-len", str(shape["output_tokens"]),
        "--num-prompts", str(max(1, round(200 * args.requests_scale))),
        "--max-concurrency", "8", "--seed", str(args.seed),
        "--save-result", "--result-dir", str(sub), "--result-filename", f"{stem}.json"]
    commands.append(shlex.join(argv))
    if not shutil.which(argv[0]):
        status, code, reason = "skipped", None, f"{argv[0]} not found on PATH"
    else:
        status, code, reason = execute(argv, sub / f"{stem}.log", args.run_timeout)
    manifest.write(json.dumps({"stage": "crosscheck", "contender": args.contender,
                               "shape": shape_name, "profile": "vllm-bench-c8",
                               "status": status, "exit_code": code, "reason": reason,
                               "run_kind": args.run_kind}) + "\n")
    print(f"[crosscheck] {shape_name}: {status}{' - ' + reason if reason else ''}")


# ------------------------------------------------------------------------ main

def add_target_args(p: argparse.ArgumentParser) -> None:
    p.add_argument("--endpoint", required=True,
                   help="base URL as reachable from where GuideLLM runs")
    p.add_argument("--model", required=True, help="model name sent in requests")
    p.add_argument("--contender", required=True,
                   help="label: direct, llama-swap, litellm, marbor")
    p.add_argument("--out", required=True, help="output directory (outside the repo)")
    p.add_argument("--run-kind", required=True, choices=RUN_KINDS)
    p.add_argument("--container", nargs="?", const=DEFAULT_IMAGE, default=None,
                   help="run GuideLLM from this image instead of a native install")
    p.add_argument("--guidellm-cmd", default="guidellm", help="native GuideLLM command")
    p.add_argument("--hf-cache", default="~/.cache/marbor-guidellm-hf")
    p.add_argument("--tokenizer", help="tokenizer repo, default: the model name")
    p.add_argument("--request-format", default="/v1/chat/completions",
                   choices=("/v1/chat/completions", "/v1/completions"))
    p.add_argument("--cross-check", action="store_true",
                   help="also run vllm bench serve per shape")
    p.add_argument("--vllm-cmd", default="vllm")
    p.add_argument("--note", action="append", default=[], metavar="K=V",
                   help="extra environment fact to record, repeatable")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("verify", help="check slo.json against slo.sha256").set_defaults(fn=cmd_verify)
    sub.add_parser("freeze", help="record slo.json sha256").set_defaults(fn=cmd_freeze)
    r = sub.add_parser("run", help="run the whole matrix for one contender")
    add_target_args(r)
    r.add_argument("--shape", action="append", help="limit to a shape (repeatable)")
    r.add_argument("--profile", action="append", help="limit to a profile name (repeatable)")
    r.add_argument("--seed", type=int, help="override matrix seed")
    r.add_argument("--requests-scale", type=float, default=1.0, help="scale request counts")
    r.add_argument("--run-timeout", type=int, default=7200, help="seconds per run")
    r.set_defaults(fn=cmd_run)
    e = sub.add_parser("env", help="capture the environment bundle only")
    add_target_args(e)
    e.set_defaults(fn=cmd_env)
    s = sub.add_parser("summary", help="contender x profile table from GuideLLM JSON")
    s.add_argument("--results", required=True, type=Path)
    s.set_defaults(fn=lambda a: __import__("summary").main(a.results))
    args = ap.parse_args()
    if getattr(args, "out", None):
        args.out = str(Path(args.out).resolve())
    return args.fn(args)


if __name__ == "__main__":
    sys.path.insert(0, str(HERE))
    sys.exit(main())
