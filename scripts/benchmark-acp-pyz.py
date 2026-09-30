"""Compare flat and PYZ imports with an isolated, fixed Desktop runtime.

This is a diagnostic benchmark. It never starts or stops the user's daemon.
Run with the Desktop bundle's Python, so PyYAML and bytecode version match.
"""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import os
import re
import shutil
import statistics
import subprocess
import sys
import tempfile
import time
import zipfile
from pathlib import Path

import yaml


PACKAGES = (
    "openai",
    "langchain_openai",
    "langchain_core",
    "langchain",
    "langgraph",
    "pydantic",
)
REQUEST = b'{"operation":"daemon.status"}\n'
AFTER_IMPORT = re.compile(
    r"ACP startup endpoint published in [\d.]+s \(total ([\d.]+)s after module load\)"
)
BOOTSTRAP = (
    "import os,runpy,sys; "
    "p=os.environ.get('DEERFLOW_BENCH_PYZ'); "
    "p and sys.path.insert(0,p); "
    "sys.argv=['deerflow.acp.daemon',*sys.argv[1:]]; "
    "runpy.run_module('deerflow.acp.daemon',run_name='__main__')"
)
MODEL_PROBE = (
    "import json,os,sys,time; "
    "p=os.environ.get('DEERFLOW_BENCH_PYZ'); "
    "p and sys.path.insert(0,p); "
    "t0=time.perf_counter(); "
    "from deerflow.models.patched_openai import PatchedChatOpenAI; "
    "t1=time.perf_counter(); "
    "m=PatchedChatOpenAI(model='gpt-4o-mini',api_key='benchmark-placeholder'); "
    "t2=time.perf_counter(); "
    "print(json.dumps({'import_seconds':round(t1-t0,3),"
    "'construct_seconds':round(t2-t1,3),"
    "'total_seconds':round(t2-t0,3)}))"
)


def tree_hash(root: Path) -> tuple[str, int, int]:
    digest = hashlib.sha256()
    count = size = 0
    for path in sorted(p for p in root.rglob("*") if p.is_file()):
        relative = path.relative_to(root).as_posix().encode("utf-8")
        digest.update(len(relative).to_bytes(4, "big"))
        digest.update(relative)
        with path.open("rb") as stream:
            while chunk := stream.read(1024 * 1024):
                digest.update(chunk)
                size += len(chunk)
        count += 1
    return digest.hexdigest(), count, size


def make_snapshot(bundle: Path, config_bundle: Path, root: Path) -> None:
    shutil.copytree(bundle / "runtime", root / "runtime")
    shutil.copy2(bundle / "deerflow-acp.exe", root / "deerflow-acp.exe")
    config_dir = root / "user-data" / "config"
    config_dir.mkdir(parents=True)
    source_config = config_bundle / "user-data" / "config"
    for name in ("config.yaml", "extensions_config.json"):
        shutil.copy2(source_config / name, config_dir / name)
    raw = yaml.safe_load((source_config / "config.yaml").read_text(encoding="utf-8"))
    source_skills = (source_config / raw["skills"]["path"]).resolve()
    shutil.copytree(source_skills, root / "user-data" / "skills")


def make_pyz(snapshot: Path, output: Path) -> dict:
    site = snapshot / "runtime" / "Lib" / "site-packages"
    bytecode_count = resource_count = 0
    with zipfile.ZipFile(output, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=1) as archive:
        for name in PACKAGES:
            package = site / name
            if not package.is_dir():
                raise FileNotFoundError(package)
            for source in sorted(package.rglob("*.py")):
                cache = Path(importlib.util.cache_from_source(str(source)))
                if not cache.is_file():
                    raise FileNotFoundError(f"Missing precompiled bytecode: {cache}")
                archive.write(cache, source.relative_to(site).with_suffix(".pyc").as_posix())
                bytecode_count += 1
            for resource in sorted(p for p in package.rglob("*") if p.is_file()):
                if resource.suffix in (".py", ".pyc"):
                    continue
                archive.write(resource, resource.relative_to(site).as_posix())
                resource_count += 1
    return {
        "pyz_modules": bytecode_count,
        "pyz_resources": resource_count,
        "pyz_bytes": output.stat().st_size,
        "pyz_sha256": hashlib.sha256(output.read_bytes()).hexdigest(),
    }


def isolated_config(snapshot: Path, run_root: Path) -> Path:
    original = snapshot / "user-data" / "config" / "config.yaml"
    raw = yaml.safe_load(original.read_text(encoding="utf-8"))
    config_dir = run_root / "config"
    config_dir.mkdir()
    extensions = config_dir / "extensions_config.json"
    shutil.copy2(original.parent / "extensions_config.json", extensions)
    skills = run_root / "skills"
    shutil.copytree(snapshot / "user-data" / "skills", skills)
    data = run_root / "data"
    raw["api"]["data_dir"] = str(data)
    raw["api"]["deerflow_home"] = str(data / "deerflow")
    raw["api"]["extensions_config_path"] = str(extensions)
    raw["local_acp"]["checkpointer_path"] = str(data / "acp-checkpoints.db")
    raw["local_acp"]["session_store_path"] = str(data / "acp-sessions.db")
    raw["skills"]["path"] = str(skills)
    raw["skills"]["extensions_file"] = str(extensions)
    config = config_dir / "config.yaml"
    config.write_text(yaml.safe_dump(raw, allow_unicode=True), encoding="utf-8")
    return config


def process_env(snapshot: Path, variant: str, pyz: Path) -> dict[str, str]:
    env = os.environ.copy()
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
    env["DEER_FLOW_PORTABLE_ROOT"] = str(snapshot)
    env["DEER_FLOW_ACP_DAEMON_WARMUP"] = "1"
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    env["DEERFLOW_BENCH_PYZ"] = str(pyz) if variant == "pyz" else ""
    return env


def run_daemon(snapshot: Path, variant: str, pyz: Path, run_root: Path) -> dict:
    python = snapshot / "runtime" / "python.exe"
    bridge = snapshot / "deerflow-acp.exe"
    config = isolated_config(snapshot, run_root)
    runtime = run_root / "runtime"
    runtime.mkdir()
    common = ["--config", str(config), "--runtime-dir", str(runtime)]
    command = [str(python), "-c", BOOTSTRAP, *common]
    env = process_env(snapshot, variant, pyz)
    t0 = time.perf_counter()
    process = subprocess.Popen(
        command,
        cwd=run_root,
        env=env,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
    )
    endpoint_at = ready_at = None
    error = None
    try:
        deadline = t0 + 90
        while time.perf_counter() < deadline:
            if process.poll() is not None:
                error = f"daemon_exited_{process.returncode}"
                break
            if endpoint_at is None and (runtime / "endpoint.json").exists():
                endpoint_at = time.perf_counter()
            if endpoint_at is not None:
                try:
                    result = subprocess.run(
                        [str(bridge), "--manage", *common],
                        input=REQUEST,
                        stdout=subprocess.PIPE,
                        stderr=subprocess.DEVNULL,
                        timeout=5,
                        check=False,
                        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
                    )
                    if result.returncode == 0:
                        state = json.loads(result.stdout).get("data", {}).get("warmup")
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
        log_file = runtime / "daemon.log"
        log = log_file.read_text(encoding="utf-8", errors="replace") if log_file.exists() else ""
    finally:
        if process.poll() is None:
            try:
                subprocess.run(
                    [str(bridge), "--stop-daemon", *common],
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL,
                    timeout=20,
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
    after_import = AFTER_IMPORT.search(log)
    endpoint = endpoint_at - t0 if endpoint_at else None
    ready = ready_at - t0 if ready_at else None
    return {
        "kind": "daemon",
        "variant": variant,
        "endpoint_seconds": round(endpoint, 3) if endpoint is not None else None,
        "ready_seconds": round(ready, 3) if ready is not None else None,
        "warmup_seconds": round(ready - endpoint, 3) if ready and endpoint else None,
        "python_and_import_seconds": round(endpoint - float(after_import.group(1)), 3)
        if endpoint is not None and after_import else None,
        "error": error,
        "log_tail": log[-1200:] if error else None,
    }


def run_model_probe(snapshot: Path, variant: str, pyz: Path) -> dict:
    command = [str(snapshot / "runtime" / "python.exe"), "-c", MODEL_PROBE]
    t0 = time.perf_counter()
    result = subprocess.run(
        command,
        cwd=snapshot,
        env=process_env(snapshot, variant, pyz),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=60,
        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
    )
    record = {"kind": "model_probe", "variant": variant, "process_seconds": round(time.perf_counter() - t0, 3)}
    if result.returncode:
        record["error"] = f"exit_{result.returncode}"
        record["stderr_tail"] = result.stderr.decode(errors="replace")[-1200:]
    else:
        record.update(json.loads(result.stdout))
        record["error"] = None
    return record


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bundle-root", type=Path, required=True)
    parser.add_argument("--config-bundle-root", type=Path, required=True)
    parser.add_argument("--pairs", type=int, default=2)
    args = parser.parse_args()
    if args.pairs not in (1, 2):
        parser.error("--pairs must be 1 or 2")
    bundle = args.bundle_root.resolve()
    config_bundle = args.config_bundle_root.resolve()
    for path in (
        bundle / "runtime" / "python.exe",
        bundle / "deerflow-acp.exe",
        config_bundle / "user-data" / "config" / "config.yaml",
        config_bundle / "user-data" / "config" / "extensions_config.json",
    ):
        if not path.is_file():
            parser.error(f"missing {path}")
    records = []
    with tempfile.TemporaryDirectory(prefix="deerflow-pyz-bench-") as directory:
        root = Path(directory)
        snapshot = root / "snapshot"
        make_snapshot(bundle, config_bundle, snapshot)
        initial_hash, file_count, byte_count = tree_hash(snapshot)
        pyz = root / "dependencies.pyz"
        zip_info = make_pyz(snapshot, pyz)
        print(json.dumps({"snapshot_sha256": initial_hash, "snapshot_files": file_count,
                          "snapshot_bytes": byte_count, **zip_info}), flush=True)
        order = ["flat", "pyz", "pyz", "flat"][: args.pairs * 2]
        for index, variant in enumerate(order, 1):
            with tempfile.TemporaryDirectory(prefix=f"acp-pyz-{index}-", dir=root) as run_directory:
                record = run_daemon(snapshot, variant, pyz, Path(run_directory))
            record["index"] = index
            records.append(record)
            print(json.dumps(record, ensure_ascii=False), flush=True)
            if record["error"]:
                break
        if all(record["error"] is None for record in records) and len(records) == len(order):
            for index, variant in enumerate(order, 1):
                record = run_model_probe(snapshot, variant, pyz)
                record["index"] = index
                records.append(record)
                print(json.dumps(record, ensure_ascii=False), flush=True)
                if record["error"]:
                    break
        final_hash, _, _ = tree_hash(snapshot)
        print(json.dumps({"snapshot_unchanged": final_hash == initial_hash,
                          "snapshot_final_sha256": final_hash}), flush=True)
    if records and all(record["error"] is None for record in records) and final_hash == initial_hash:
        for kind in ("daemon", "model_probe"):
            for variant in ("flat", "pyz"):
                subset = [r for r in records if r["kind"] == kind and r["variant"] == variant]
                if subset:
                    metrics = ("endpoint_seconds", "ready_seconds", "warmup_seconds", "python_and_import_seconds") if kind == "daemon" else ("process_seconds", "import_seconds", "construct_seconds", "total_seconds")
                    print(json.dumps({"kind": "summary", "subject": kind, "variant": variant,
                                      **{f"median_{key}": round(statistics.median(r[key] for r in subset if r[key] is not None), 3)
                                         for key in metrics if any(r[key] is not None for r in subset)}}), flush=True)
        return 0
    return 1


if __name__ == "__main__":
    sys.exit(main())
