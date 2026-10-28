"""Restore pre-switch model configuration without changing images or state DBs."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--deployment-report', type=Path,
                        default=Path('/home/gen/deerflow-deploy/model-switch-20261027/deployment.json'))
    parser.add_argument('--apply', action='store_true')
    args = parser.parse_args()
    report = json.loads(args.deployment_report.read_text())
    root = Path('/home/gen/deerflow-deploy')
    folo = Path('/home/gen/Documents/folotoy/deerflow-ws/src')
    allowed = {root / 'data/user-data/config/config.yaml', folo / '.env.ws'}
    entries = report['files']
    if len(entries) != 2 or {Path(e['path']) for e in entries} != allowed:
        raise RuntimeError('unexpected rollback targets')
    for entry in entries:
        if hashlib.sha256(Path(entry['path']).read_bytes()).hexdigest() != entry['after_sha256']:
            raise RuntimeError('configuration changed after switch; refusing overwrite')
        if hashlib.sha256(Path(entry['backup']).read_bytes()).hexdigest() != entry['before_sha256']:
            raise RuntimeError('backup checksum mismatch')
    if not args.apply:
        print(json.dumps({'status': 'rollback_ready', 'previous_default': report['previous_default']}))
        return
    for path, query in [(folo / 'data/deerflow-sessions.sqlite3',
                         "SELECT count(*) FROM deerflow_sessions WHERE status NOT IN ('completed','idle','invalidated','failed','cancelled')"),
                        (root / 'data/user-data/config/go-harness-state/harness.db',
                         "SELECT count(*) FROM harness_runs WHERE status NOT IN ('completed','failed','cancelled')")]:
        with sqlite3.connect(path.as_uri() + '?mode=ro', uri=True) as db:
            if db.execute(query).fetchone()[0]:
                raise RuntimeError('active or unconfirmed turn; retry when idle')
    for entry in entries:
        path = Path(entry['path'])
        temporary = path.with_name('.' + path.name + '.rollback-' + uuid.uuid4().hex)
        try:
            temporary.write_bytes(Path(entry['backup']).read_bytes())
            temporary.chmod(entry['mode'])
            with temporary.open('rb') as stream:
                os.fsync(stream.fileno())
            os.replace(temporary, path)
        finally:
            temporary.unlink(missing_ok=True)
    commands = [
        ['docker', 'compose', '-p', 'deerflow-deploy', '-f', str(root / 'compose.yaml'),
         '--project-directory', str(root), 'up', '-d', '--no-build', '--force-recreate', 'deerflow'],
        ['docker', 'compose', '--env-file', str(folo / '.env.ws'), '-p', 'folotoy-ws',
         '-f', str(folo / 'compose.ws.yaml'), '--project-directory', str(folo),
         'up', '-d', '--no-build', '--force-recreate', 'folotoy']]
    for command in commands:
        result = subprocess.run(command, capture_output=True, text=True, timeout=120)
        if result.returncode:
            raise RuntimeError('rollback restart failed')
    print(json.dumps({'status': 'rollback_applied', 'default_model': report['previous_default']}))


if __name__ == '__main__':
    main()
