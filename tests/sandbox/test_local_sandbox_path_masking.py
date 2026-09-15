from __future__ import annotations

from deerflow.sandbox.local.local_sandbox import LocalSandbox, PathMapping


def _sandbox(tmp_path):
    workspace = tmp_path / "workspace"
    workspace.mkdir()
    return LocalSandbox(
        "masking",
        [PathMapping("/mnt/user-data/workspace", str(workspace))],
    ), workspace


def test_reverse_resolve_nested_path_accepts_native_and_forward_separators(tmp_path) -> None:
    sandbox, workspace = _sandbox(tmp_path)
    nested = workspace / "private" / "result.txt"

    expected = "/mnt/user-data/workspace/private/result.txt"
    assert sandbox._reverse_resolve_path(str(nested)) == expected
    assert sandbox._reverse_resolve_path(str(nested).replace("\\", "/")) == expected


def test_output_masks_native_and_forward_slash_host_paths(tmp_path) -> None:
    sandbox, workspace = _sandbox(tmp_path)
    native = str(workspace / "private" / "result.txt")
    forward = native.replace("\\", "/")

    output = sandbox._reverse_resolve_paths_in_output(f'first="{native}" second="{forward}"')

    assert output == (
        'first="/mnt/user-data/workspace/private/result.txt" '
        'second="/mnt/user-data/workspace/private/result.txt"'
    )
    assert str(workspace) not in output
    assert str(workspace).replace("\\", "/") not in output


def test_output_does_not_mask_similarly_prefixed_sibling(tmp_path) -> None:
    sandbox, workspace = _sandbox(tmp_path)
    sibling = f"{workspace}-other"

    assert sandbox._reverse_resolve_paths_in_output(sibling) == sibling
