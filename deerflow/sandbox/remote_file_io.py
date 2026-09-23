"""Build an offline-testable protocol for the shared filesystem algorithms."""

from __future__ import annotations

import inspect
from functools import lru_cache

from deerflow.sandbox import file_io, search


@lru_cache(maxsize=1)
def remote_file_io_script() -> str:
    """Send only application-owned code; requests are separate JSON argv data."""
    modules = {"sandbox_file_io": inspect.getsource(file_io), "sandbox_search": inspect.getsource(search)}
    bootstrap = "import sys, types, json, dataclasses, re\nfrom pathlib import Path\n"
    for name, source in modules.items():
        bootstrap += (
            f"module = types.ModuleType({name!r})\n"
            f"sys.modules[{name!r}] = module\n"
            f"exec(compile({source!r}, {name!r}, 'exec'), module.__dict__)\n"
        )
    return bootstrap + '''
request = json.loads(sys.argv[1])
operation, path, options = request["operation"], request["path"], request["options"]
fs, search = sys.modules["sandbox_file_io"], sys.modules["sandbox_search"]
try:
    if operation == "read_file_chunk":
        result = fs.read_file_chunk(path, **options)
    elif operation == "list_dir_page":
        result = fs.list_dir_page(path, ignore_patterns=search.IGNORE_PATTERNS, **options)
    elif operation == "glob_result":
        result = search.find_glob_result(Path(path), **options)
    elif operation == "grep_result":
        result = search.find_grep_result(Path(path), **options)
    else:
        raise ValueError("Unknown filesystem operation")
    print(json.dumps(dataclasses.asdict(result), ensure_ascii=True))
except Exception as error:
    if isinstance(error, fs.FileVersionMismatchError):
        code = 7
    elif isinstance(error, FileNotFoundError):
        code = 2
    elif isinstance(error, NotADirectoryError):
        code = 3
    elif isinstance(error, PermissionError):
        code = 4
    elif isinstance(error, re.error):
        code = 6
    elif isinstance(error, IsADirectoryError):
        code = 9
    elif isinstance(error, ValueError):
        code = 8
    else:
        code = 5
    print(str(error), file=sys.stderr)
    sys.exit(code)
'''
