"""Measure portable settings startup in fresh, isolated data directories."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import statistics
import subprocess
import tempfile
import time


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--package", required=True, type=Path)
    parser.add_argument("--runs", type=int, default=3)
    args = parser.parse_args()
    if args.runs < 1:
        parser.error("--runs must be positive")
    package = args.package.resolve(strict=True)
    repo = Path(__file__).resolve().parents[1]
    cache = repo / ".build-cache" / "settings-benchmarks"
    cache.mkdir(parents=True, exist_ok=True)
    output = Path(tempfile.mkdtemp(prefix="run-", dir=cache))
    env = {key: value for key, value in os.environ.items() if key.upper() in {
        "SYSTEMROOT", "WINDIR", "COMSPEC", "PATH", "TEMP", "TMP",
    }}
    env.update(PYTHONUTF8="1", PYTHONIOENCODING="utf-8", PYTHONNOUSERSITE="1",
               PYTHONDONTWRITEBYTECODE="1")
    results = []
    for index in range(args.runs):
        user_data = output / str(index) / "user-data"
        command = [str(package / "runtime/python.exe"), "-m", "deerflow.config_tool",
                   "--config", str(user_data / "config/config.yaml"),
                   "--user-data", str(user_data), "--resources", str(package / "resources")]
        timings = {}
        # Match the desktop bootstrap followed by its first and repeated reads.
        for label, operation in (("init", "init"), ("snapshot", "snapshot"),
                                 ("reload", "snapshot")):
            start = time.perf_counter()
            process = subprocess.run(command + [operation], cwd=output, env=env,
                                     capture_output=True, input="null", text=True,
                                     encoding="utf-8", timeout=120,
                                     creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
            elapsed = time.perf_counter() - start
            process.check_returncode()
            envelope = json.loads(process.stdout)
            assert envelope["ok"], "Configuration command failed"
            if operation == "snapshot":
                document = envelope["data"]
                assert document["models"] and document["subagents"]["builtin_agents"]
                assert Path(document["paths"]["user_data"]) == user_data
            timings[label + "_ms"] = round(elapsed * 1000, 1)
        timings["first_open_ms"] = round(timings["init_ms"] + timings["snapshot_ms"], 1)
        results.append(timings)
        print(json.dumps({"run": index + 1, **timings}), flush=True)
    report = {"package": str(package), "runs": results,
              "median_ms": {key: statistics.median(row[key] for row in results)
                            for key in results[0]}}
    (output / "timings.json").write_text(json.dumps(report, indent=2), encoding="utf-8")
    print(json.dumps({"report": str(output / "timings.json"), **report}, indent=2))


if __name__ == "__main__":
    main()
