from __future__ import annotations

import os
import subprocess
import sys
import textwrap
from pathlib import Path


def _run_tracing_script(code: str, config_path: Path) -> None:
    environment = os.environ.copy()
    environment["DEER_FLOW_CONFIG_PATH"] = str(config_path)
    result = subprocess.run(
        [sys.executable, "-c", textwrap.dedent(code), str(config_path)],
        cwd=Path(__file__).resolve().parents[1],
        env=environment,
        capture_output=True,
        text=True,
        encoding="utf-8",
        timeout=30,
    )
    assert result.returncode == 0, result.stdout + result.stderr


def test_cold_tracing_load_and_app_config_reload_finish(tmp_path: Path) -> None:
    config_path = tmp_path / "config.yaml"
    config_path.write_text(
        "sandbox:\n  use: test\ntracing:\n  langfuse:\n"
        "    enabled: true\n    public_key: initial-key\n"
        "    secret_key: test-secret\n    host: https://tracing.test\n",
        encoding="utf-8",
    )
    _run_tracing_script(
        """
        import sys
        from pathlib import Path
        from deerflow.config.app_config import reload_app_config, reset_app_config
        from deerflow.config.tracing_config import get_tracing_config, reset_tracing_config

        reset_app_config()
        reset_tracing_config()
        first = get_tracing_config()
        assert first.langfuse.public_key == "initial-key"
        assert first.langfuse.is_configured
        assert get_tracing_config() is first

        path = Path(sys.argv[1])
        path.write_text(path.read_text(encoding="utf-8").replace("initial-key", "reloaded-key"), encoding="utf-8")
        reload_app_config()
        second = get_tracing_config()
        assert second.langfuse.public_key == "reloaded-key"
        assert second is not first
        assert get_tracing_config() is second
        """,
        config_path,
    )


def test_reset_during_tracing_load_discards_stale_snapshot(tmp_path: Path) -> None:
    _run_tracing_script(
        """
        import threading
        import deerflow.config.tracing_config as tracing

        read_started = threading.Event()
        release_read = threading.Event()
        reset_finished = threading.Event()
        results = []
        errors = []
        reads = 0

        def read_section():
            global reads
            reads += 1
            if reads == 1:
                snapshot = {"langfuse": {"public_key": "old-key"}}
                read_started.set()
                assert release_read.wait(10), "The blocked read was not released"
                return snapshot
            return {"langfuse": {"public_key": "new-key"}}

        def load():
            try:
                results.append(tracing.get_tracing_config())
            except BaseException as error:
                errors.append(error)

        def invalidate():
            tracing.reset_tracing_config()
            reset_finished.set()

        tracing._config_section = read_section
        tracing.reset_tracing_config()
        reader = threading.Thread(target=load, daemon=True)
        resetter = threading.Thread(target=invalidate, daemon=True)
        reader.start()
        assert read_started.wait(5), "Tracing never reached configuration loading"
        resetter.start()
        try:
            assert reset_finished.wait(5), "Tracing reset waits on configuration loading"
        finally:
            release_read.set()
            reader.join(timeout=5)
            resetter.join(timeout=5)

        assert not reader.is_alive() and not resetter.is_alive()
        assert not errors, errors
        assert len(results) == 1
        assert results[0].langfuse.public_key == "new-key"
        assert tracing.get_tracing_config() is results[0]
        assert reads == 2
        """,
        tmp_path / "unused-config.yaml",
    )
