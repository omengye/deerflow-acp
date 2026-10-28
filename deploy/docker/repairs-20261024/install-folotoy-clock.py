#!/usr/bin/env python3
"""Build and stage the FoloToy clock repair; service lifecycle is caller-owned."""

import argparse
from dataclasses import dataclass
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import stat
import subprocess
import tempfile

OLD_SHA = '9806612da55e28e6d05026cc0403f8bae72392e3775157fcf62b5aded1dd37bb'
NEW_SHA = 'a5b7cd7f9db22b28c36ddfc8cfe279e9297c842a78a166cf93783e6ff815fcf1'
OLD_IMAGE = 'folotoy-deerflow-ws:20261024'
NEW_IMAGE = 'folotoy-deerflow-ws:tools-fix-20261024'
IMAGE_VARIABLE = re.compile(rb'(?m)^([ \t]*(?:export[ \t]+)?FOLOTOY_WS_IMAGE[ \t]*=)[^\r\n]*(\r?)$')


class RepairError(RuntimeError):
    pass


def digest(data):
    return hashlib.sha256(data).hexdigest()


@dataclass
class Snapshot:
    path: Path
    data: bytes | None
    mode: int | None
    signature: tuple | None

    @classmethod
    def read(cls, path):
        if path.is_symlink():
            raise RepairError('Refusing a symlink at a deployment file')
        try:
            before = path.stat()
        except FileNotFoundError:
            return cls(path, None, None, None)
        if not stat.S_ISREG(before.st_mode):
            raise RepairError('Deployment target is not a regular file')
        data = path.read_bytes()
        after = path.stat()
        signature = (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns, before.st_mode)
        if signature != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns, after.st_mode):
            raise RepairError('Deployment file changed while reading')
        return cls(path, data, stat.S_IMODE(before.st_mode), signature)

    def verify(self):
        current = self.read(self.path)
        if current.signature != self.signature or current.data != self.data:
            raise RepairError('Deployment files changed concurrently; no overwrite is allowed')


def atomic_write(path, data, mode):
    fd, temporary = tempfile.mkstemp(prefix='.folotoy-clock-', dir=path.parent)
    try:
        with os.fdopen(fd, 'wb') as output:
            if hasattr(os, 'fchmod'):
                os.fchmod(output.fileno(), mode)
            else:  # Allows offline verification on a Windows build workstation.
                os.chmod(temporary, mode)
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def command(argv, *, cwd=None):
    # Compose files and env files can contain credentials: never echo its output.
    environment = dict(os.environ)
    environment.pop('FOLOTOY_WS_IMAGE', None)
    result = subprocess.run(argv, cwd=cwd, env=environment, capture_output=True, text=True, timeout=300)
    if result.returncode:
        raise RepairError('Docker build or Compose validation failed (output withheld)')
    return result.stdout.strip()


def compose_validate(root, compose):
    argv = ['docker', 'compose']
    if (root / '.env.ws').is_file():
        argv.extend(['--env-file', str(root / '.env.ws')])
    command(argv + ['--project-directory', str(root), '-f', str(compose),
                    'config', '--quiet'], cwd=root)


def image_override(data):
    if data is None:
        return None
    matches = list(IMAGE_VARIABLE.finditer(data))
    if len(matches) > 1:
        raise RepairError('Duplicate FOLOTOY_WS_IMAGE assignments require review')
    return IMAGE_VARIABLE.sub(lambda match: match[1] + NEW_IMAGE.encode() + match[2], data)


def install(root, provider_source, *, apply=False):
    root = root.resolve(strict=True)
    provider = root / 'sdk/llm_sdk/deerflow.py'
    compose = root / 'compose.ws.yaml'
    paths = [provider, compose, root / '.env', root / '.env.ws']
    if any(not path.resolve().is_relative_to(root) for path in paths):
        raise RepairError('Deployment paths escape the specified root')
    snapshots = [Snapshot.read(path) for path in paths]
    if snapshots[0].data is None or digest(snapshots[0].data) not in (OLD_SHA, NEW_SHA):
        raise RepairError('Deployed provider checksum does not match the reviewed versions')
    new_provider = provider_source.read_bytes()
    if digest(new_provider) != NEW_SHA:
        raise RepairError('Provider source checksum does not match the reviewed repair')
    old_template = ('${FOLOTOY_WS_IMAGE:-' + OLD_IMAGE + '}').encode()
    new_template = ('${FOLOTOY_WS_IMAGE:-' + NEW_IMAGE + '}').encode()
    raw_compose = snapshots[1].data
    if raw_compose is None:
        raise RepairError('compose.ws.yaml is missing')
    if raw_compose.count(old_template) == 1 and new_template not in raw_compose:
        new_compose = raw_compose.replace(old_template, new_template)
    elif raw_compose.count(new_template) == 1 and old_template not in raw_compose:
        new_compose = raw_compose
    else:
        raise RepairError('Compose image default does not match the reviewed deployment')
    updates = [new_provider, new_compose, image_override(snapshots[2].data), image_override(snapshots[3].data)]
    changed = [(old, data) for old, data in zip(snapshots, updates) if old.data != data]
    report = {'status': 'planned', 'image': NEW_IMAGE, 'provider_sha256': NEW_SHA,
              'changed_files': [str(old.path.relative_to(root)) for old, _ in changed],
              'backup': None, 'image_sha256': None, 'services_restarted': False}
    if not apply:
        return report

    dockerfile = Path(__file__).with_name('Dockerfile.folotoy-clock').read_bytes()
    # This context contains exactly these two non-secret files, never the project.
    with tempfile.TemporaryDirectory(prefix='folotoy-clock-build-') as temporary:
        context = Path(temporary)
        (context / 'Dockerfile').write_bytes(dockerfile)
        (context / 'deerflow.py').write_bytes(new_provider)
        command(['docker', 'build', '--pull=false', '--network=none', '-t', NEW_IMAGE, str(context)])
    image_sha = command(['docker', 'image', 'inspect', '--format', '{{.Id}}', NEW_IMAGE])
    if not re.fullmatch(r'sha256:[0-9a-f]{64}', image_sha):
        raise RepairError('Docker returned an invalid image identity')
    report['image_sha256'] = image_sha
    for snapshot in snapshots:
        snapshot.verify()
    with tempfile.TemporaryDirectory(prefix='folotoy-clock-validate-') as temporary:
        draft = Path(temporary) / 'compose.ws.yaml'
        draft.write_bytes(new_compose)
        compose_validate(root, draft)
    for snapshot in snapshots:
        snapshot.verify()
    if not changed:
        compose_validate(root, compose)
        report['status'] = 'already_configured'
        return report

    backup_parent = root.parent / 'backups'
    backup_parent.mkdir(mode=0o700, exist_ok=True)
    backup = backup_parent / ('folotoy-clock-' + datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')
                              + '-' + secrets.token_hex(4))
    backup.mkdir(mode=0o700)
    # Preserve the original provider/Compose even if only an env override changed.
    for index, snapshot in enumerate(snapshots):
        if snapshot.data is not None:
            name = ('provider.before.py', 'compose.ws.yaml.before', '.env.before', '.env.ws.before')[index]
            atomic_write(backup / name, snapshot.data, 0o600)
    report['backup'] = str(backup)
    applied = []
    try:
        for snapshot, data in changed:
            snapshot.verify()
            mode = 0o600 if snapshot.path.name in ('.env', '.env.ws') else snapshot.mode
            atomic_write(snapshot.path, data, mode)
            applied.append((snapshot, data))
        compose_validate(root, compose)
    except BaseException as error:
        rollback_failed = False
        for snapshot, ours in reversed(applied):
            try:
                if snapshot.path.is_symlink() or snapshot.path.read_bytes() != ours:
                    rollback_failed = True
                    continue  # Never overwrite a concurrent writer during rollback.
                atomic_write(snapshot.path, snapshot.data, snapshot.mode)
            except OSError:
                rollback_failed = True
        if rollback_failed:
            raise RepairError('Repair failed; concurrent changes preserved. Inspect private backup: ' + str(backup)) from error
        raise RepairError('Repair failed; original files restored. Private backup: ' + str(backup)) from error
    report['status'] = 'applied'
    return report


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, default=Path('/home/gen/Documents/folotoy/deerflow-ws/src'))
    parser.add_argument('--provider-source', type=Path, required=True)
    parser.add_argument('--apply', action='store_true')
    args = parser.parse_args(argv)
    try:
        result = install(args.root, args.provider_source, apply=args.apply)
    except (OSError, RepairError, subprocess.SubprocessError) as error:
        # Exception bodies from Docker are never included by command().
        print(json.dumps({'status': 'failed', 'error': str(error)}, ensure_ascii=False), flush=True)
        return 1
    print(json.dumps(result, ensure_ascii=False), flush=True)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
