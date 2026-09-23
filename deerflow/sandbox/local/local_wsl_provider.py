"""Local-family provider that runs bash inside a WSL2 distro on Windows."""

from __future__ import annotations

import logging
import platform
import subprocess
import threading
from collections import OrderedDict
from collections.abc import Collection
from pathlib import Path
from typing import ClassVar

from deerflow.sandbox.local.local_sandbox_provider import build_host_fs_path_mappings
from deerflow.sandbox.local.wsl_exceptions import (
    WslDistroNotFoundError,
    WslUnavailableError,
)
from deerflow.sandbox.local.wsl_sandbox import WslSandbox
from deerflow.sandbox.sandbox import Sandbox
from deerflow.sandbox.sandbox_provider import SandboxProvider

logger = logging.getLogger(__name__)

_singleton_lock = threading.Lock()
_singleton: WslSandbox | None = None


def _decode_wsl_list_output(raw: bytes) -> str:
    """Decode ``wsl.exe -l -q`` output, which is UTF-16 LE by default."""
    try:
        return raw.decode("utf-16-le").replace("\x00", "")
    except UnicodeDecodeError:
        # Newer WSL builds with WSL_UTF8=1 honored at the host level emit UTF-8.
        return raw.decode("utf-8", errors="replace")


class LocalWslProvider(SandboxProvider):
    """Run bash inside a WSL2 distro, keeping file I/O on the Windows host.

    Sibling of :class:`~deerflow.sandbox.local.local_sandbox_provider.LocalSandboxProvider`:
    both store thread data on the Windows filesystem and expose it through
    virtual ``/mnt/user-data`` paths. ``LocalWslProvider`` differs only by
    routing the bash tool through ``wsl.exe`` so commands run inside a Linux
    VM instead of PowerShell/cmd.exe on the host.
    """

    uses_thread_data_mounts: ClassVar[bool] = True
    SANDBOX_ID: ClassVar[str] = "wsl"

    def __init__(self) -> None:
        if platform.system() != "Windows":
            raise RuntimeError(
                "LocalWslProvider is only supported on Windows. Configure "
                "'sandbox.use: local' (or another provider) on non-Windows hosts."
            )

        from deerflow.config import get_app_config

        config = get_app_config()
        sandbox_cfg = config.sandbox
        self._distro = sandbox_cfg.wsl_distro if sandbox_cfg else None
        self._wsl_user = sandbox_cfg.wsl_user if sandbox_cfg else None
        self._wsl_shell = (sandbox_cfg.wsl_shell if sandbox_cfg else "bash") or "bash"
        self._mount_prefix = (sandbox_cfg.wsl_mount_prefix if sandbox_cfg else "/mnt") or "/mnt"
        self._command_timeout_seconds = getattr(sandbox_cfg, "bash_command_timeout", WslSandbox.EXECUTE_TIMEOUT_SECONDS)

        self._verify_wsl_available()
        if self._distro:
            self._verify_distro_exists(self._distro)

        all_mappings = build_host_fs_path_mappings(
            skills_path=Path("__missing_skills_projection__")
        )
        skills_container = getattr(
            getattr(config, "skills", None),
            "container_path",
            "/mnt/skills",
        ).rstrip("/") or "/"
        self._base_path_mappings = [
            mapping
            for mapping in all_mappings
            if (mapping.container_path.rstrip("/") or "/") != skills_container
        ]
        self._sandboxes: OrderedDict[str, WslSandbox] = OrderedDict()
        self._max_cached_sandboxes = 256

    def _verify_wsl_available(self) -> None:
        """Probe ``wsl.exe --status`` to confirm WSL is installed and reachable."""
        try:
            result = subprocess.run(
                ["wsl.exe", "--status"],
                shell=False,
                capture_output=True,
                timeout=15,
            )
        except FileNotFoundError as exc:
            raise WslUnavailableError(
                "wsl.exe was not found on PATH. Install Windows Subsystem for Linux."
            ) from exc
        if result.returncode != 0:
            stderr = _decode_wsl_list_output(result.stderr or b"")
            raise WslUnavailableError(
                f"WSL is not available (exit code {result.returncode}): {stderr.strip() or 'no diagnostic output'}"
            )

    def _verify_distro_exists(self, distro: str) -> None:
        """Run ``wsl.exe -l -q`` and confirm *distro* is present (case-insensitive)."""
        try:
            result = subprocess.run(
                ["wsl.exe", "-l", "-q"],
                shell=False,
                capture_output=True,
                timeout=15,
            )
        except FileNotFoundError as exc:
            raise WslUnavailableError(
                "wsl.exe was not found on PATH. Install Windows Subsystem for Linux."
            ) from exc
        if result.returncode != 0:
            stderr = _decode_wsl_list_output(result.stderr or b"")
            raise WslUnavailableError(
                f"Failed to list WSL distros (exit code {result.returncode}): {stderr.strip() or 'no diagnostic output'}"
            )

        decoded = _decode_wsl_list_output(result.stdout or b"")
        installed = {line.strip().lower() for line in decoded.splitlines() if line.strip()}
        if distro.lower() not in installed:
            raise WslDistroNotFoundError(
                f"Configured WSL distro {distro!r} is not registered. "
                f"Available distros: {sorted(installed) or 'none'}"
            )

    def acquire(
        self,
        thread_id: str | None = None,
        *,
        available_skills: Collection[str] | None = None,
        workspace_path: str | None = None,
    ) -> str:
        global _singleton
        from deerflow.config import get_app_config
        from deerflow.sandbox.local.local_sandbox import PathMapping
        from deerflow.skills.projection import get_skill_projection

        if workspace_path is not None:
            raise ValueError("LocalWslProvider does not support external workspace mounts")

        projection = get_skill_projection(available_skills)
        sandbox_id = f"wsl:{projection.revision}"
        with _singleton_lock:
            sandbox = self._sandboxes.get(sandbox_id)
            if sandbox is None:
                sandbox = WslSandbox(
                    sandbox_id,
                    distro=self._distro,
                    wsl_user=self._wsl_user,
                    wsl_shell=self._wsl_shell,
                    mount_prefix=self._mount_prefix,
                    command_timeout_seconds=self._command_timeout_seconds,
                    path_mappings=[
                        *self._base_path_mappings,
                        PathMapping(
                            container_path=get_app_config().skills.container_path,
                            local_path=str(projection.path),
                            read_only=True,
                        ),
                    ],
                )
                self._sandboxes[sandbox_id] = sandbox
                while len(self._sandboxes) > self._max_cached_sandboxes:
                    evicted_id, _ = self._sandboxes.popitem(last=False)
                    logger.info(
                        "Evicting WslSandbox cache entry %s (cap=%d)",
                        evicted_id,
                        self._max_cached_sandboxes,
                    )
            else:
                self._sandboxes.move_to_end(sandbox_id)
            _singleton = sandbox
        return sandbox.id

    def get(self, sandbox_id: str) -> Sandbox | None:
        if sandbox_id == self.SANDBOX_ID:
            return _singleton
        with _singleton_lock:
            sandbox = self._sandboxes.get(sandbox_id)
            if sandbox is not None:
                self._sandboxes.move_to_end(sandbox_id)
            return sandbox

    def release(self, sandbox_id: str) -> None:
        # Singleton lifecycle, same semantics as LocalSandboxProvider.release().
        pass

    def active_skill_revisions(self) -> set[str]:
        with _singleton_lock:
            return {
                sandbox_id.removeprefix("wsl:")
                for sandbox_id in self._sandboxes
                if sandbox_id.startswith("wsl:")
            }

    def reset(self) -> None:
        global _singleton
        with _singleton_lock:
            self._sandboxes.clear()
            _singleton = None

    def shutdown(self) -> None:
        self.reset()
