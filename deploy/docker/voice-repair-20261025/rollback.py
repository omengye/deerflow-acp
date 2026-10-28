"""Restore the pre-repair FoloToy source and image, with checksum guards."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import uuid


def run(command):
    result = subprocess.run(command, text=True, capture_output=True, timeout=120)
    if result.returncode:
        raise RuntimeError('rollback command failed')
    return result.stdout


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--deployment-report', type=Path,
                        default=Path('/home/gen/deerflow-deploy/voice-repair-20261025/deployment.json'))
    parser.add_argument('--apply', action='store_true')
    args = parser.parse_args()
    report = json.loads(args.deployment_report.read_text())
    root = Path('/home/gen/Documents/folotoy/deerflow-ws/src')
    allowed = {root / name for name in ('core/device_session_store.py', 'core/folotoy_app.py',
                                        'core/folotoy_msg_processor.py', 'compose.ws.yaml')}
    if len(report['files']) != 4 or {Path(e['path']) for e in report['files']} != allowed:
        raise RuntimeError('unexpected rollback targets')
    for entry in report['files']:
        if hashlib.sha256(Path(entry['path']).read_bytes()).hexdigest() != entry['after_sha256']:
            raise RuntimeError('source changed since repair; refusing overwrite')
        if entry['backup'] and hashlib.sha256(Path(entry['backup']).read_bytes()).hexdigest() != entry['before_sha256']:
            raise RuntimeError('backup checksum mismatch')
    if not args.apply:
        print(json.dumps({'status': 'rollback_ready', 'restores_image': report['old_image'],
                          'asr_rollback_order': 'Revert this device repair before earlier ASR image rollbacks.'}))
        return
    with sqlite3.connect((root / 'data/deerflow-sessions.sqlite3').as_uri() + '?mode=ro', uri=True) as db:
        if db.execute("SELECT count(*) FROM deerflow_sessions WHERE status NOT IN ('completed','idle','invalidated','failed','cancelled')").fetchone()[0]:
            raise RuntimeError('voice turn active')
    for entry in report['files']:
        path = Path(entry['path'])
        if not entry['backup']:
            path.unlink()
            continue
        temporary = path.with_name('.' + path.name + '.rollback-' + uuid.uuid4().hex)
        try:
            temporary.write_bytes(Path(entry['backup']).read_bytes())
            temporary.chmod(entry['mode'])
            with temporary.open('rb') as stream:
                os.fsync(stream.fileno())
            os.replace(temporary, path)
        finally:
            temporary.unlink(missing_ok=True)
    run(['docker', 'compose', '--env-file', str(root / '.env.ws'), '-p', 'folotoy-ws',
         '-f', str(root / 'compose.ws.yaml'), '--project-directory', str(root),
         'up', '-d', '--no-build', '--force-recreate', 'folotoy'])
    print(json.dumps({'status': 'rollback_applied', 'restored_image': report['old_image'],
                      'device_relogin_needed': True}))


if __name__ == '__main__':
    main()
