from __future__ import annotations

import os
import threading
import time
from concurrent.futures import ThreadPoolExecutor

import pytest

from deerflow.models import credential_loader
from deerflow.models.credential_loader import _read_secret_from_file_descriptor


@pytest.fixture(autouse=True)
def _isolate_fd_cache(monkeypatch) -> None:
    monkeypatch.setattr(credential_loader, "_fd_secret_cache", {})


def _pipe_with_secret(secret: bytes) -> int:
    read_fd, write_fd = os.pipe()
    os.write(write_fd, secret)
    os.close(write_fd)
    return read_fd


def test_file_descriptor_secret_is_reused_after_first_read(monkeypatch) -> None:
    read_fd = _pipe_with_secret(b"secret-one")
    monkeypatch.setenv("TEST_SECRET_FD", str(read_fd))
    try:
        first = _read_secret_from_file_descriptor("TEST_SECRET_FD")
        second = _read_secret_from_file_descriptor("TEST_SECRET_FD")
    finally:
        os.close(read_fd)

    assert first == second == "secret-one"


def test_cached_secret_survives_closed_descriptor(monkeypatch) -> None:
    read_fd = _pipe_with_secret(b"secret-two")
    monkeypatch.setenv("TEST_SECRET_FD", str(read_fd))

    assert _read_secret_from_file_descriptor("TEST_SECRET_FD") == "secret-two"
    os.close(read_fd)

    assert _read_secret_from_file_descriptor("TEST_SECRET_FD") == "secret-two"


def test_different_descriptors_have_independent_cache_entries(monkeypatch) -> None:
    first_fd = _pipe_with_secret(b"first")
    second_fd = _pipe_with_secret(b"second")
    try:
        monkeypatch.setenv("TEST_SECRET_FD", str(first_fd))
        assert _read_secret_from_file_descriptor("TEST_SECRET_FD") == "first"
        monkeypatch.setenv("TEST_SECRET_FD", str(second_fd))
        assert _read_secret_from_file_descriptor("TEST_SECRET_FD") == "second"
    finally:
        os.close(first_fd)
        os.close(second_fd)


def test_concurrent_first_reads_drain_descriptor_once(monkeypatch) -> None:
    read_fd = _pipe_with_secret(b"shared")
    monkeypatch.setenv("TEST_SECRET_FD", str(read_fd))
    real_read = os.read
    reads: list[int] = []

    def slow_read(fd: int, length: int) -> bytes:
        if fd == read_fd:
            reads.append(fd)
            time.sleep(0.05)
        return real_read(fd, length)

    monkeypatch.setattr(os, "read", slow_read)
    barrier = threading.Barrier(2)

    def load() -> str | None:
        barrier.wait()
        return _read_secret_from_file_descriptor("TEST_SECRET_FD")

    try:
        with ThreadPoolExecutor(max_workers=2) as pool:
            values = list(pool.map(lambda _index: load(), range(2)))
    finally:
        os.close(read_fd)

    assert values == ["shared", "shared"]
    assert reads == [read_fd]


def test_failed_or_empty_reads_are_not_cached(monkeypatch) -> None:
    read_fd, write_fd = os.pipe()
    os.close(write_fd)
    monkeypatch.setenv("TEST_SECRET_FD", str(read_fd))
    try:
        assert _read_secret_from_file_descriptor("TEST_SECRET_FD") is None
        assert credential_loader._fd_secret_cache == {}
    finally:
        os.close(read_fd)
