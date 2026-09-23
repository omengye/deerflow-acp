from abc import ABC, abstractmethod

from deerflow.sandbox.search import GrepMatch


class Sandbox(ABC):
    """Abstract base class for sandbox environments"""

    _id: str

    def __init__(self, id: str):
        self._id = id

    @property
    def id(self) -> str:
        return self._id

    @abstractmethod
    def execute_command(self, command: str) -> str:
        """Execute bash command in sandbox.

        Args:
            command: The command to execute.

        Returns:
            The standard or error output of the command.
        """
        pass

    def execute_command_result(self, command: str, *, cancel_event=None):
        """Return a trustworthy execution outcome when the provider supports it.

        Legacy providers still expose their output, but a text-only response
        cannot prove successful completion and must not pass command acceptance.
        """
        from deerflow.sandbox.command import CommandResult

        if cancel_event is not None and cancel_event.is_set():
            return CommandResult("Command cancelled before execution.", None, "cancelled", True)
        return CommandResult(self.execute_command(command), None, "unknown", False)

    @abstractmethod
    def read_file(
        self,
        path: str,
        start_line: int | None = None,
        end_line: int | None = None,
    ) -> str:
        """Read the content of a file.

        Args:
            path: The absolute path of the file to read.
            start_line: Optional starting line number (1-indexed, inclusive).
            end_line: Optional ending line number (1-indexed, inclusive).

        Returns:
            The content of the file.
        """
        pass

    @abstractmethod
    def list_dir(self, path: str, max_depth=2) -> list[str]:
        """List the contents of a directory.

        Args:
            path: The absolute path of the directory to list.
            max_depth: The maximum depth to traverse. Default is 2.

        Returns:
            The contents of the directory.
        """
        pass

    def read_file_chunk(self, path: str, *, offset: int = 0, max_bytes: int = 65536,
                        expected_version: str | None = None, start_line: int | None = None,
                        end_line: int | None = None):
        """Optional bounded UTF-8 read; offsets and continuations refer to bytes."""
        raise NotImplementedError("This sandbox does not support bounded file reads")

    def list_dir_page(self, path: str, *, max_depth: int = 2, limit: int = 200,
                      cursor: str | None = None):
        """Optional bounded directory page with a change-detecting cursor."""
        raise NotImplementedError("This sandbox does not support directory pages")

    def glob_result(self, path: str, pattern: str, *, include_dirs: bool = False, max_results: int = 200):
        """Optional search with explicit coverage; legacy providers may opt out."""
        raise NotImplementedError("This sandbox does not report search coverage")

    def grep_result(self, path: str, pattern: str, *, glob: str | None = None,
                    literal: bool = False, case_sensitive: bool = False, max_results: int = 100):
        """Optional text search with explicit coverage."""
        raise NotImplementedError("This sandbox does not report search coverage")

    @abstractmethod
    def write_file(self, path: str, content: str, append: bool = False) -> None:
        """Write content to a file.

        Args:
            path: The absolute path of the file to write to.
            content: The text content to write to the file.
            append: Whether to append the content to the file. If False, the file will be created or overwritten.
        """
        pass

    @abstractmethod
    def delete_path(self, path: str, *, recursive: bool = False) -> None:
        """Delete a file, symlink, or directory inside the sandbox."""

        pass

    @abstractmethod
    def move_path(
        self,
        source: str,
        destination: str,
        *,
        overwrite: bool = False,
    ) -> None:
        """Move or rename a file, symlink, or directory inside the sandbox."""

        pass

    @abstractmethod
    def glob(self, path: str, pattern: str, *, include_dirs: bool = False, max_results: int = 200) -> tuple[list[str], bool]:
        """Find paths that match a glob pattern under a root directory."""
        pass

    @abstractmethod
    def grep(
        self,
        path: str,
        pattern: str,
        *,
        glob: str | None = None,
        literal: bool = False,
        case_sensitive: bool = False,
        max_results: int = 100,
    ) -> tuple[list[GrepMatch], bool]:
        """Search for matches inside a text file or files under a directory."""
        pass

    @abstractmethod
    def update_file(self, path: str, content: bytes) -> None:
        """Update a file with binary content.

        Args:
            path: The absolute path of the file to update.
            content: The binary content to write to the file.
        """
        pass

    @abstractmethod
    def download_file(self, path: str) -> bytes:
        """Download the binary content of a file.

        Args:
            path: The absolute path of the file to download.

        Returns:
            Raw file bytes.

        Raises:
            PermissionError: If path traversal is detected or the path is
                outside the allowed virtual prefix.
            OSError: If the file cannot be read or does not exist. Both local
                and remote implementations must raise ``OSError`` so callers
                have a single exception type to handle.
        """
        pass
