"""Single-threaded durable worker for automatic Skill proposal generation."""

from __future__ import annotations

import asyncio
import logging
import queue
import threading
import time
from typing import TYPE_CHECKING, Any

from .store import FileEvolutionStore, utc_now_iso

if TYPE_CHECKING:
    from .service import SkillEvolutionService

logger = logging.getLogger(__name__)


class EvolutionWorker:
    """Serialize model-backed evolution jobs for the single-user deployment."""

    def __init__(
        self,
        store: FileEvolutionStore | None = None,
        service: SkillEvolutionService | None = None,
    ):
        self.store = store or FileEvolutionStore()
        if service is None:
            from .service import SkillEvolutionService

            service = SkillEvolutionService(self.store)
        self.service = service
        self._queue: queue.Queue[str | None] = queue.Queue()
        self._stop_event = threading.Event()
        self._thread: threading.Thread | None = None
        self._state_lock = threading.Lock()
        self._processing_signal_id: str | None = None
        self._last_error: str | None = None
        self._queued_ids: set[str] = set()
        self._cancelled_ids: set[str] = set()

    def start(self, *, recover: bool = True) -> None:
        with self._state_lock:
            if self._thread is not None and self._thread.is_alive():
                return
            self._stop_event.clear()
            self._thread = threading.Thread(target=self._run, name="skill-evolution-worker", daemon=True)
            self._thread.start()
        if recover:
            self.recover()

    def recover(self) -> int:
        recovered = 0
        for signal in reversed(self.store.list_signals()):
            if signal.status not in {"pending", "processing"}:
                continue
            if signal.status == "processing":
                signal.status = "pending"
                signal.updated_at = utc_now_iso()
                signal.error = "Recovered after worker restart."
                self.store.save_signal(signal)
            if self.enqueue(signal.id, lazy_start=False):
                recovered += 1
        return recovered

    def enqueue(self, signal_id: str, *, lazy_start: bool = True) -> bool:
        if lazy_start:
            self.start(recover=False)
        with self._state_lock:
            if signal_id in self._queued_ids or signal_id == self._processing_signal_id:
                return False
            self._cancelled_ids.discard(signal_id)
            self._queued_ids.add(signal_id)
        self._queue.put(signal_id)
        return True

    def cancel(self, signal_id: str) -> bool:
        """Cancel a queued signal; return False once processing has started."""
        with self._state_lock:
            if signal_id == self._processing_signal_id:
                return False
            if signal_id in self._queued_ids:
                self._queued_ids.discard(signal_id)
                self._cancelled_ids.add(signal_id)
            return True

    def _run(self) -> None:
        while not self._stop_event.is_set():
            try:
                signal_id = self._queue.get(timeout=0.5)
            except queue.Empty:
                continue
            if signal_id is None:
                self._queue.task_done()
                break
            with self._state_lock:
                cancelled = signal_id in self._cancelled_ids
                self._cancelled_ids.discard(signal_id)
                self._queued_ids.discard(signal_id)
                if not cancelled:
                    self._processing_signal_id = signal_id
            if cancelled:
                self._queue.task_done()
                continue
            try:
                asyncio.run(self.service.process_signal(signal_id))
                with self._state_lock:
                    self._last_error = None
            except Exception as exc:
                logger.exception("Evolution signal %s failed", signal_id)
                with self._state_lock:
                    self._last_error = str(exc)
            finally:
                with self._state_lock:
                    self._processing_signal_id = None
                self._queue.task_done()

    def stop(self, *, timeout: float = 10.0) -> None:
        with self._state_lock:
            thread = self._thread
            if thread is None or not thread.is_alive():
                self._thread = None
                return
            self._stop_event.set()
        self._queue.put(None)
        thread.join(timeout=max(0.0, timeout))
        with self._state_lock:
            if not thread.is_alive():
                self._thread = None

    def wait_until_idle(self, *, timeout: float = 10.0) -> bool:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            with self._state_lock:
                processing = self._processing_signal_id
                queued = bool(self._queued_ids)
            if not processing and not queued and self._queue.unfinished_tasks == 0:
                return True
            time.sleep(0.02)
        return False

    def status(self) -> dict[str, Any]:
        with self._state_lock:
            thread = self._thread
            return {
                "running": bool(thread and thread.is_alive()),
                "queue_depth": len(self._queued_ids),
                "processing_signal_id": self._processing_signal_id,
                "last_error": self._last_error,
            }


_WORKER_LOCK = threading.Lock()
_WORKER: EvolutionWorker | None = None


def get_evolution_worker() -> EvolutionWorker:
    global _WORKER
    expected_store = FileEvolutionStore()
    with _WORKER_LOCK:
        if _WORKER is None or _WORKER.store.root != expected_store.root:
            if _WORKER is not None:
                _WORKER.stop()
            _WORKER = EvolutionWorker(expected_store)
        return _WORKER
