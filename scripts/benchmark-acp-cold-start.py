"""Measure fresh-process ACP startup without touching the Desktop daemon.

The installed and source variants use the same bundled Python interpreter and
dependencies. Each run gets its own config, data, and endpoint directories.
This measures process startup on the current OS cache, not a machine reboot.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import statistics
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timezone
from pathlib import Path

import yaml


REQUEST = b'{"operation":"daemon.status"}\n'
STARTUP_TOTAL = re.compile(
    r"ACP startup endpoint published in [\d.]+s \(total ([\d.]+)s after module load\)"
)
STAGES = {
    "config_seconds": re.compile(r"ACP startup configuration ready in ([\d.]+)s"),
    "session_store_seconds": re.compile(r"ACP startup session store ready in ([\d.]+)s"),
    "checkpointer_seconds": re.compile(r"ACP startup checkpointer ready in ([\d.]+)s"),
    "session_cleanup_seconds": re.compile(
        r"ACP startup session cleanup finished in ([\d.]+)s"
    ),
}


def isolated_config(original: Path, root: Path) -> Path:
    """Keep the real settings and model selection while redirecting writes."""
    raw = yaml.safe_load(original.read_text(encoding="utf-8"))
    data = root / "data"
    config = root / "config" / "config.yaml"
    config.parent.mkdir(parents=True)
    extensions = config.parent / "extensions_config.json"
    shutil.copy2(original.parent / "extensions_config.json", extensions)
    skills = root / "skills"
    shutil.copytree((original.parent / raw["skills"]["path"]).resolve(), skills)
    raw["api"]["data_dir"] = str(data)
    raw["api"]["deerflow_home"] = str(data / "deerflow")
    raw["api"]["extensions_config_path"] = str(extensions)
    raw["local_acp"]["checkpointer_path"] = str(data / "acp-checkpoints.db")
    raw["local_acp"]["session_store_path"] = str(data / "acp-sessions.db")
    raw["skills"]["path"] = str(skills)
    raw["skills"]["extensions_file"] = str(extensions)
    config.write_text(yaml.safe_dump(raw, allow_unicode=True), encoding="utf-8")
    return config


def stages(log_file: Path) -> dict[str, float]:
    if not log_file.exists():
        return {}
    content = log_file.read_text(encoding="utf-8", errors="replace")
    found: dict[str, float] = {}
    for key, pattern in STAGES.items():
        if match := pattern.search(content):
            found[key] = float(match.group(1))
    if match := STARTUP_TOTAL.search(content):
        found["after_module_load_to_endpoint_seconds"] = float(match.group(1))
    return found


def run_once(variant: str, bundle: Path, source: Path, root: Path) -> dict:
    bridge = bundle / "deerflow-acp.exe"
    python = bundle / "runtime" / "python.exe"
    original = bundle / "user-data" / "config" / "config.yaml"
    config = isolated_config(original, root)
    runtime = root / "runtime"
    runtime.mkdir()
    common = ["--config", str(config), "--runtime-dir", str(runtime)]
    if variant == "installed":
        command = [str(python), "-m", "deerflow.acp.daemon", *common]
    else:
        code = (
            "import runpy,sys; "
            f"sys.path.insert(0, {str(source)!r}); "
            "sys.argv=['deerflow.acp.daemon',*sys.argv[1:]]; "
            "runpy.run_module('deerflow.acp.daemon',run_name='__main__')"
        )
        command = [str(python), "-c", code, *common]
    env = os.environ.copy()
    # Do not inherit caller overrides which could redirect data to a live app.
    for name in (
        "DEER_FLOW_CONFIG_PATH",
        "DEER_FLOW_PORTABLE_ROOT",
        "DEER_FLOW_ACP_RUNTIME_DIR",
        "DEER_FLOW_ACP_CHECKPOINTER_PATH",
        "DEER_FLOW_ACP_SESSION_STORE_PATH",
        "DEER_FLOW_HOME",
        "DEER_FLOW_EXTENSIONS_CONFIG_PATH",
    ):
        env.pop(name, None)
    env["DEER_FLOW_ACP_DAEMON_WARMUP"] = "1"
    started_utc = datetime.now(timezone.utc).isoformat(timespec="seconds")
    t0 = time.perf_counter()
    process = subprocess.Popen(
        command,
        cwd=root,
        env=env,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
    )
    endpoint_at = None
    ready_at = None
    error = None
    try:
        deadline = t0 + 150
        while time.perf_counter() < deadline:
            if process.poll() is not None:
                error = f"daemon_exited_{process.returncode}"
                break
            if endpoint_at is None and (runtime / "endpoint.json").exists():
                endpoint_at = time.perf_counter()
            if endpoint_at is not None:
                try:
                    response = subprocess.run(
                        [str(bridge), "--manage", *common],
                        input=REQUEST,
                        stdout=subprocess.PIPE,
                        stderr=subprocess.DEVNULL,
                        timeout=5,
                        check=False,
                        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
                    )
                    if response.returncode == 0:
                        payload = json.loads(response.stdout)
                        state = payload.get("data", {}).get("warmup")
                        if state == "ready":
                            ready_at = time.perf_counter()
                            break
                        if state == "failed":
                            error = "warmup_failed"
                            break
                except (OSError, ValueError, subprocess.TimeoutExpired):
                    pass
            time.sleep(0.1)
        else:
            error = "timeout"
        stage_times = stages(runtime / "daemon.log")
    finally:
        if process.poll() is None:
            try:
                subprocess.run(
                    [str(bridge), "--stop-daemon", *common],
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL,
                    timeout=35,
                    check=False,
                    creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
                )
            except (OSError, subprocess.TimeoutExpired):
                pass
        if process.poll() is None:
            process.terminate()
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=10)
    result = {
        "variant": variant,
        "pid": process.pid,
        "started_utc": started_utc,
        "finished_utc": datetime.now(timezone.utc).isoformat(timespec="seconds"),
        "endpoint_seconds": round(endpoint_at - t0, 3) if endpoint_at else None,
        "ready_seconds": round(ready_at - t0, 3) if ready_at else None,
        "warmup_seconds": round(ready_at - endpoint_at, 3)
        if ready_at and endpoint_at
        else None,
        "error": error,
        **stage_times,
    }
    if endpoint_at and "after_module_load_to_endpoint_seconds" in stage_times:
        result["python_and_module_import_seconds"] = round(
            result["endpoint_seconds"]
            - stage_times["after_module_load_to_endpoint_seconds"],
            3,
        )
    return result


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bundle-root", type=Path, required=True)
    parser.add_argument("--source-root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--pairs", type=int, default=2)
    parser.add_argument("--first", choices=("installed", "source"), default="installed")
    args = parser.parse_args()
    bundle = args.bundle_root.resolve()
    source = args.source_root.resolve()
    if args.pairs < 1 or args.pairs > 5:
        parser.error("--pairs must be between 1 and 5")
    for path in (
        bundle / "deerflow-acp.exe",
        bundle / "runtime" / "python.exe",
        bundle / "user-data" / "config" / "config.yaml",
        source / "deerflow" / "acp" / "daemon.py",
    ):
        if not path.is_file():
            parser.error(f"missing {path}")
    order = ["installed", "source"] * args.pairs
    if args.pairs > 1:
        order[2::2] = ["source"] * len(order[2::2])
        order[3::2] = ["installed"] * len(order[3::2])
    if args.first == "source":
        order = ["source" if value == "installed" else "installed" for value in order]
    results = []
    with tempfile.TemporaryDirectory(prefix="deerflow-acp-source-") as snapshot_dir:
        snapshot = Path(snapshot_dir)
        shutil.copytree(source / "deerflow", snapshot / "deerflow")
        shutil.copy2(source / "pyproject.toml", snapshot / "pyproject.toml")
        digest = hashlib.sha256()
        for path in sorted((snapshot / "deerflow").rglob("*.py")):
            digest.update(path.relative_to(snapshot).as_posix().encode("utf-8"))
            digest.update(path.read_bytes())
        source_hash = digest.hexdigest()[:16]
        print(json.dumps({"source_snapshot_sha256_prefix": source_hash}), flush=True)
        for index, variant in enumerate(order, 1):
            with tempfile.TemporaryDirectory(prefix="deerflow-acp-bench-") as directory:
                result = run_once(variant, bundle, snapshot, Path(directory))
            result["run_index"] = index
            result["source_snapshot_sha256_prefix"] = source_hash
            results.append(result)
            print(json.dumps(result, ensure_ascii=False), flush=True)
    for variant in ("installed", "source"):
        matching = [r for r in results if r["variant"] == variant and r["error"] is None]
        summary = {
            "variant": variant,
            "successful_runs": len(matching),
            **{
                f"median_{key}": round(statistics.median(r[key] for r in matching), 3)
                for key in ("endpoint_seconds", "ready_seconds", "warmup_seconds")
                if matching
            },
        }
        print(json.dumps(summary, ensure_ascii=False), flush=True)
    return 0 if all(r["error"] is None for r in results) else 1


if __name__ == "__main__":
    sys.exit(main())
