from __future__ import annotations

import io
import zipfile
from types import SimpleNamespace

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from langchain_core.messages import ToolMessage

from app import artifact_archive
from app.dependencies import ClientManager, _record_run_artifacts
from app.routers import runs
from deerflow.client import DeerFlowClient
from deerflow.runtime import RunStatus
from deerflow.tools.builtins.present_file_tool import PRESENTED_ARTIFACTS_KEY


class _FakePaths:
    def __init__(self, outputs_dir):
        self._outputs_dir = outputs_dir

    def sandbox_outputs_dir(self, _thread_id: str):
        return self._outputs_dir

    def sandbox_user_data_dir(self, _thread_id: str):
        return self._outputs_dir.parent


def _archive_client(
    monkeypatch,
    outputs_dir,
    *,
    artifacts,
    status=RunStatus.success,
    run_thread="thread-1",
):
    record = SimpleNamespace(
        run_id="run-1",
        thread_id=run_thread,
        status=status,
        metadata={"artifacts": artifacts},
    )

    class _RunManager:
        def get(self, _run_id):
            return record

        async def has_inflight(self, _thread_id):
            return status in {RunStatus.pending, RunStatus.running}

    class _Manager:
        run_manager = _RunManager()

        def try_reserve_artifact_archive(self, _thread_id):
            return status not in {RunStatus.pending, RunStatus.running}

        def release_artifact_archive(self, _thread_id):
            return None

    manager = _Manager()
    monkeypatch.setattr(runs, "get_client_manager", lambda: manager)
    monkeypatch.setattr(runs, "get_paths", lambda: _FakePaths(outputs_dir))
    monkeypatch.setattr(
        runs,
        "get_app_config",
        lambda: SimpleNamespace(tool_output=SimpleNamespace(storage_subdir=".tool-results")),
    )
    monkeypatch.setattr(
        runs, "_artifact_archive_slots", __import__("asyncio").Semaphore(4)
    )
    app = FastAPI()
    app.include_router(runs.router, prefix="/api")
    return TestClient(app)


def test_run_artifacts_are_recorded_once_from_stream_values() -> None:
    record = SimpleNamespace(metadata={})

    _record_run_artifacts(
        record,
        {
            "type": "tool",
            "name": "present_files",
            "status": "success",
            "presented_artifacts": [
                "/mnt/user-data/outputs/report.txt",
                "/mnt/user-data/outputs/report.txt",
                "/mnt/user-data/uploads/private.txt",
                42,
            ],
        },
    )
    _record_run_artifacts(
        record,
        {
            "type": "tool_result_chunk",
            "name": "present_files",
            "status": "success",
            "presented_artifacts": ["/mnt/user-data/outputs/data.csv"],
        },
    )

    assert record.metadata["artifacts"] == [
        "/mnt/user-data/outputs/report.txt",
        "/mnt/user-data/outputs/data.csv",
    ]


def test_cumulative_values_artifacts_are_not_attributed_to_current_run() -> None:
    record = SimpleNamespace(metadata={})

    _record_run_artifacts(
        record,
        {"artifacts": ["/mnt/user-data/outputs/from-an-older-run.txt"]},
    )

    assert "artifacts" not in record.metadata


def test_present_files_tool_message_exposes_normalized_run_delivery() -> None:
    message = ToolMessage(
        content="Successfully presented files",
        name="present_files",
        tool_call_id="call-1",
        additional_kwargs={
            PRESENTED_ARTIFACTS_KEY: ["/mnt/user-data/outputs/report.txt"]
        },
    )

    event = DeerFlowClient._tool_message_event(message)

    assert event.data["presented_artifacts"] == ["/mnt/user-data/outputs/report.txt"]


def test_archive_download_contains_only_run_recorded_files(
    tmp_path, monkeypatch
) -> None:
    outputs = tmp_path / "outputs"
    (outputs / "reports").mkdir(parents=True)
    (outputs / "reports" / "summary.txt").write_text("summary", encoding="utf-8")
    (outputs / "data.csv").write_text("a,b\n1,2\n", encoding="utf-8")
    (outputs / "not-recorded.txt").write_text("secret", encoding="utf-8")
    client = _archive_client(
        monkeypatch,
        outputs,
        artifacts=[
            "/mnt/user-data/outputs/reports/summary.txt",
            "/mnt/user-data/outputs/data.csv",
            "/mnt/user-data/outputs/data.csv",
        ],
    )

    response = client.post("/api/threads/thread-1/runs/run-1/artifacts/archive")

    assert response.status_code == 200
    assert response.headers["content-type"] == "application/zip"
    assert response.headers["cache-control"] == "private, no-store"
    assert response.headers["x-content-type-options"] == "nosniff"
    with zipfile.ZipFile(io.BytesIO(response.content)) as archive:
        assert archive.namelist() == ["reports/summary.txt", "data.csv"]
        assert archive.read("reports/summary.txt") == b"summary"
        assert "not-recorded.txt" not in archive.namelist()


def test_archive_manifest_reports_deduplicated_run_files(tmp_path, monkeypatch) -> None:
    outputs = tmp_path / "outputs"
    outputs.mkdir()
    client = _archive_client(
        monkeypatch,
        outputs,
        artifacts=[
            "/mnt/user-data/outputs/report.txt",
            "/mnt/user-data/outputs/report.txt",
        ],
    )

    response = client.get("/api/threads/thread-1/runs/run-1/artifacts/archive")

    assert response.status_code == 200
    assert response.json() == {"file_count": 1}


@pytest.mark.parametrize(
    "recorded_path",
    [
        "/mnt/user-data/uploads/private.txt",
        "/mnt/user-data/outputs/../uploads/private.txt",
        "/mnt/user-data/outputs/.tool-results/raw.txt",
        "/mnt/user-data/outputs/.browser-frames/frame.png",
    ],
)
def test_archive_rejects_non_public_output_paths(
    tmp_path, monkeypatch, recorded_path
) -> None:
    outputs = tmp_path / "outputs"
    outputs.mkdir()
    client = _archive_client(monkeypatch, outputs, artifacts=[recorded_path])

    response = client.post("/api/threads/thread-1/runs/run-1/artifacts/archive")

    assert response.status_code == 409


def test_archive_rejects_symlink_and_hardlink_members(tmp_path) -> None:
    outputs = tmp_path / "outputs"
    internal = outputs / ".tool-results"
    internal.mkdir(parents=True)
    secret = internal / "secret.txt"
    secret.write_text("secret", encoding="utf-8")

    link = outputs / "link.txt"
    try:
        link.symlink_to(secret)
    except OSError:
        pass
    else:
        with pytest.raises(artifact_archive.ArtifactArchiveError):
            artifact_archive.build_artifact_archive(
                outputs,
                ["/mnt/user-data/outputs/link.txt"],
                user_data_dir=outputs.parent,
            )

    hardlink = outputs / "hardlink.txt"
    try:
        hardlink.hardlink_to(secret)
    except OSError:
        return
    with pytest.raises(artifact_archive.ArtifactArchiveError):
        artifact_archive.build_artifact_archive(
            outputs,
            ["/mnt/user-data/outputs/hardlink.txt"],
            user_data_dir=outputs.parent,
        )


@pytest.mark.parametrize(
    ("status", "run_thread", "expected"),
    [
        (RunStatus.running, "thread-1", 409),
        (RunStatus.success, "another-thread", 404),
    ],
)
def test_archive_requires_terminal_run_bound_to_thread(
    tmp_path, monkeypatch, status, run_thread, expected
) -> None:
    outputs = tmp_path / "outputs"
    outputs.mkdir()
    client = _archive_client(
        monkeypatch,
        outputs,
        artifacts=["/mnt/user-data/outputs/report.txt"],
        status=status,
        run_thread=run_thread,
    )

    response = client.post("/api/threads/thread-1/runs/run-1/artifacts/archive")

    assert response.status_code == expected


def test_archive_enforces_count_and_size_limits(tmp_path, monkeypatch) -> None:
    outputs = tmp_path / "outputs"
    outputs.mkdir()
    paths = []
    for name in ("one.txt", "two.txt"):
        (outputs / name).write_bytes(b"x")
        paths.append(f"/mnt/user-data/outputs/{name}")

    monkeypatch.setattr(artifact_archive, "MAX_FILES", 1)
    with pytest.raises(artifact_archive.ArtifactArchiveError) as count_error:
        artifact_archive.build_artifact_archive(
            outputs, paths, user_data_dir=outputs.parent
        )
    assert count_error.value.status_code == 413

    monkeypatch.setattr(artifact_archive, "MAX_FILES", 2)
    monkeypatch.setattr(artifact_archive, "MAX_TOTAL_BYTES", 1)
    with pytest.raises(artifact_archive.ArtifactArchiveError) as size_error:
        artifact_archive.build_artifact_archive(
            outputs, paths, user_data_dir=outputs.parent
        )
    assert size_error.value.status_code == 413


def test_archive_reservation_excludes_run_writers_atomically() -> None:
    manager = ClientManager()

    assert manager.try_reserve_artifact_archive("thread-1") is True
    assert manager.mark_thread_running("thread-1") is False
    assert manager.is_thread_running("thread-1") is True

    manager.release_artifact_archive("thread-1")
    assert manager.is_thread_running("thread-1") is False
    assert manager.mark_thread_running("thread-1") is True
    assert manager.try_reserve_artifact_archive("thread-1") is False
    manager.mark_thread_done("thread-1")
