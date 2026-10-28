"""Restore the two private configuration files from this model rollout.

Run on Gentoo. Both current files must still match the deployment hashes.
This does not change images, ASR, skills, credentials, or conversation databases.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import uuid


def execute(command, payload=None):
    result = subprocess.run(command, input=payload, text=True, capture_output=True, timeout=120)
    if result.returncode:
        raise RuntimeError('rollback command failed')
    return result.stdout


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--deployment-report', type=Path,
                        default=Path('/home/gen/deerflow-deploy/llm-switch-20261025/deployment.json'))
    parser.add_argument('--apply', action='store_true')
    args = parser.parse_args()
    report = json.loads(args.deployment_report.read_text())
    entries = report['files']
    allowed = {Path('/home/gen/deerflow-deploy/data/user-data/config/config.yaml'),
               Path('/home/gen/Documents/folotoy/deerflow-ws/src/.env.ws')}
    if len(entries) != 2 or {Path(e['path']) for e in entries} != allowed:
        raise RuntimeError('unexpected rollback target')
    for entry in entries:
        current = Path(entry['path']).read_bytes()
        backup = Path(entry['backup']).read_bytes()
        if hashlib.sha256(current).hexdigest() != entry['after_sha256']:
            raise RuntimeError('configuration changed since rollout; refusing to overwrite')
        if hashlib.sha256(backup).hexdigest() != entry['before_sha256']:
            raise RuntimeError('backup checksum mismatch')
    if not args.apply:
        print(json.dumps({'status': 'rollback_ready', 'restores_default': report['previous_default'],
                          'apply_required': True}))
        return
    runtime = '/var/lib/deerflow/user-data/runtime/acp-go'
    status = json.loads(execute(['docker', 'exec', '-i', 'deerflow-deploy-deerflow-1',
                                '/var/lib/deerflow/deerflow-control-go', '--runtime-dir', runtime],
                               json.dumps({'operation': 'daemon.status'})))
    if not status.get('ok') or status.get('data', {}).get('active_operations') not in (0, [], {}):
        raise RuntimeError('native runtime is busy')
    folo = Path('/home/gen/Documents/folotoy/deerflow-ws/src')
    with sqlite3.connect((folo / 'data/deerflow-sessions.sqlite3').as_uri() + '?mode=ro', uri=True) as db:
        statuses = db.execute('SELECT status,count(*) FROM deerflow_sessions GROUP BY status').fetchall()
    if any(count for state, count in statuses if state not in ('idle', 'completed', 'invalidated', 'failed', 'cancelled')):
        raise RuntimeError('voice turn is active')
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
    root = Path('/home/gen/deerflow-deploy')
    execute(['docker', 'compose', '-p', 'deerflow-deploy', '-f', str(root / 'compose.yaml'),
             '--project-directory', str(root), 'up', '-d', '--no-build', '--force-recreate', 'deerflow'])
    execute(['docker', 'compose', '--env-file', str(folo / '.env.ws'), '-p', 'folotoy-ws',
             '-f', str(folo / 'compose.ws.yaml'), '--project-directory', str(folo),
             'up', '-d', '--no-build', '--force-recreate', 'folotoy'])
    print(json.dumps({'status': 'rollback_applied', 'default_model': report['previous_default']}))


if __name__ == '__main__':
    main()
