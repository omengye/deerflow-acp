"""Install the fixed wrapper into an existing skills deployment without rebuilding.

Run on the Docker host after draining active requests, with the daemon still
running for identity checks. This only stages
the wrapper and updates Compose; the caller controls coordinated recreation and
verification. It never opens, moves, or merges a conversation database.
"""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import uuid


MOUNT = './runtime/deerflow-acpd-wrapper.sh:/opt/deerflow/bin/deerflow-acpd:ro'
TARGET = '/opt/deerflow/bin/deerflow-acpd'


def bound_data_directory(root):
    """Require an established selection; wrapper updates never choose a DB."""
    user_data = (root / 'data/user-data').resolve(strict=True)
    raw = (user_data / 'config/runtime-data.json').read_bytes()
    if len(raw) > 8192:
        raise ValueError('Data directory binding is too large')
    pairs = json.loads(raw, object_pairs_hook=list)
    if (not isinstance(pairs, list) or len(pairs) != 1 or
            not isinstance(pairs[0], tuple) or pairs[0][0] != 'data_dir' or
            not isinstance(pairs[0][1], str)):
        raise ValueError('Expected exactly one data_dir binding')
    value = pairs[0][1]
    relative = PurePosixPath(value)
    if (not value or relative.is_absolute() or '..' in relative.parts or
            ':' in value or '\\' in value or relative == PurePosixPath('.')):
        raise ValueError('Data directory must be relative to user-data')
    selected = (user_data / relative).resolve(strict=True)
    if user_data not in selected.parents or not (selected / 'harness.db').is_file():
        raise ValueError('Bound database is absent or outside user-data')
    return str(PurePosixPath('/var/lib/deerflow/user-data') / relative)


def verify_native_binding(container, selected):
    """Reject incompatible images or a live mismatch before changing files."""
    command = ['docker', 'exec', container, '/opt/deerflow/bin/deerflow-acpd.real',
               '--check-paths', '--config', '/var/lib/deerflow/user-data/config/config.yaml',
               '--data-dir', selected]
    result = subprocess.run(command, capture_output=True, text=True, timeout=20)
    if result.returncode or json.loads(result.stdout).get('data_dir') != selected:
        raise RuntimeError('Active image does not support this database binding')
    result = subprocess.run(
        ['docker', 'exec', '-i', container, '/opt/deerflow/bin/deerflow-control-go',
         '--runtime-dir', '/var/lib/deerflow/user-data/runtime/acp-go'],
        input='{"operation":"daemon.status"}', capture_output=True, text=True, timeout=20)
    status = json.loads(result.stdout) if result.returncode == 0 else {}
    if not status.get('ok') or status.get('runtime', {}).get('data_dir') != selected:
        raise RuntimeError('Running daemon uses another database or cannot be verified')


def update_compose(text):
    """Preserve all existing values and interpolation; add one short-syntax bind."""
    lines = text.splitlines(keepends=True)
    services = [i for i, line in enumerate(lines)
                if re.fullmatch(r'  deerflow:[ \t]*', line.rstrip('\r\n'))]
    if len(services) != 1:
        raise ValueError('Expected one two-space-indented deerflow service')
    start = services[0] + 1
    end = next((i for i in range(start, len(lines))
                if re.fullmatch(r'  [^\s#][^:]*:[ \t]*', lines[i].rstrip('\r\n'))), len(lines))
    block = ''.join(lines[start:end])
    if TARGET in block:
        if MOUNT in block:
            return text
        raise ValueError('A different daemon wrapper mount already exists')
    volumes = [i for i in range(start, end)
               if re.fullmatch(r'    volumes:[ \t]*', lines[i].rstrip('\r\n'))]
    if len(volumes) != 1:
        raise ValueError('Expected one four-space-indented volumes block')
    index = volumes[0]
    newline = '\r\n' if lines[index].endswith('\r\n') else '\n'
    indent = '      '
    for line in lines[index + 1:end]:
        if not line.strip() or line.lstrip().startswith('#'):
            continue
        sequence = re.match(r'^( {4,6})-\s', line)
        if sequence:
            indent = sequence[1]
        break
    if not lines[index].endswith('\n'):
        lines[index] += newline
    lines.insert(index + 1, indent + '- ' + MOUNT + newline)
    return ''.join(lines)


def write_private(path, text, mode):
    temporary = path.with_name('.' + path.name + '.' + uuid.uuid4().hex + '.tmp')
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(fd, 'w', newline='') as output:
            output.write(text)
            output.flush()
            os.fsync(output.fileno())
        temporary.chmod(mode)
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, default=Path('/home/gen/deerflow-deploy'))
    parser.add_argument('--wrapper-source', required=True, type=Path)
    parser.add_argument('--container', default='deerflow-deploy-deerflow-1')
    parser.add_argument('--apply', action='store_true')
    args = parser.parse_args()
    root = args.root.resolve(strict=True)
    compose = root / 'compose.yaml'
    with compose.open(newline='') as source:
        original = source.read()
    updated = update_compose(original)
    wrapper = args.wrapper_source.read_text()
    if ('exec /opt/deerflow/bin/deerflow-acpd.real "$@"' not in wrapper or
            'DEERFLOW_PERSISTENT_DATA_DIR' not in wrapper or 'go-harness-state' in wrapper):
        raise ValueError('Wrapper must use the native database binding')
    destination = root / 'runtime' / 'deerflow-acpd-wrapper.sh'
    if root not in destination.parent.resolve().parents:
        raise ValueError('Runtime destination escaped the deployment root')
    if not args.apply:
        print(json.dumps({'status': 'not_applied', 'compose_change_required': updated != original,
                          'mount': MOUNT, 'requires_recreate': True}))
        return 0
    selected = bound_data_directory(root)
    verify_native_binding(args.container, selected)
    stage = root / 'runtime-backups' / (
        datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ') + '-' + uuid.uuid4().hex[:8])
    stage.mkdir(parents=True, mode=0o700)
    stage.parent.chmod(0o700)
    write_private(stage / 'compose.yaml.before', original, 0o600)
    previous_wrapper = destination.read_text() if destination.is_file() else None
    if previous_wrapper is not None:
        write_private(stage / 'wrapper.before', previous_wrapper, 0o600)
    destination.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    destination.parent.chmod(0o700)
    write_private(destination, wrapper, 0o755)
    candidate = root / ('.compose-runtime-' + uuid.uuid4().hex + '.yaml')
    try:
        write_private(candidate, updated, 0o600)
        check = subprocess.run(
            ['docker', 'compose', '--project-directory', str(root), '-f', str(candidate), 'config', '--quiet'],
            capture_output=True, text=True, timeout=30)
        if check.returncode:
            if previous_wrapper is not None:
                write_private(destination, previous_wrapper, 0o755)
            else:
                destination.unlink()
            raise RuntimeError('Compose validation failed; no Compose change was made')
        with compose.open(newline='') as source:
            still_original = source.read() == original
        if not still_original:
            raise RuntimeError('Compose changed concurrently; preserve the backup and retry')
        write_private(compose, updated, 0o600)
    finally:
        candidate.unlink(missing_ok=True)
    report = {'status': 'installed', 'backup': str(stage), 'mount': MOUNT,
              'wrapper_sha256': hashlib.sha256(wrapper.encode()).hexdigest(),
              'compose_changed': updated != original, 'requires_recreate': True,
              'persistent_data_dir': selected,
              'database_modified': False}
    write_private(stage / 'installation.json', json.dumps(report, indent=2) + '\n', 0o600)
    print(json.dumps(report))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
