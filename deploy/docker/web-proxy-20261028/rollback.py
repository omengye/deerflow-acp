"""Restore the exact pre-proxy config; restart only the DeerFlow container.

The default invocation checks readiness without changing files or services.
Use --apply on Gentoo after reviewing deployment.json. No keys are printed.
"""
import argparse
from contextlib import closing
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import stat
import subprocess
import time
import uuid


ROOT = Path('/home/gen/deerflow-deploy')
STAGE = ROOT / 'web-proxy-20261028'
CONFIG = ROOT / 'data/user-data/config/config.yaml'
FOLO_DB = Path('/home/gen/Documents/folotoy/deerflow-ws/src/data/deerflow-sessions.sqlite3')
CONTAINER = 'deerflow-deploy-deerflow-1'
RUNTIME = '/var/lib/deerflow/user-data/runtime/acp-go'
CONTROL = '/var/lib/deerflow/deerflow-control-go'
MAX_CONFIG_BYTES = 4 * 1024 * 1024


class RollbackError(Exception):
    """Fixed diagnostic codes; never provider error text or configuration data."""


def command(argv, data=None, timeout=45):
    result = subprocess.run(argv, input=data, text=True, capture_output=True, timeout=timeout)
    if result.returncode:
        raise RollbackError('rollback_command_failed')
    return result.stdout


def control(operation):
    result = json.loads(command(['docker', 'exec', '-i', CONTAINER, CONTROL,
                                 '--runtime-dir', RUNTIME],
                                json.dumps({'operation': operation}), timeout=30))
    if not result.get('ok') or not isinstance(result.get('data'), dict):
        raise RollbackError('native_control_rejected')
    return result['data']


def idle_native(activity):
    if type(activity.get('active_operations')) is not int or activity['active_operations'] != 0:
        raise RollbackError('native_operations_active')


def idle_folotoy():
    # Historical needs_reconciliation rows in the native DB are deliberately
    # not inspected or altered. The daemon owns active-operation accounting.
    with closing(sqlite3.connect(FOLO_DB.as_uri() + '?mode=ro', uri=True, timeout=3)) as database:
        count = database.execute(
            "SELECT count(*) FROM deerflow_sessions WHERE status IN ('running','creating','pending')"
        ).fetchone()[0]
    if count:
        raise RollbackError('folotoy_turn_active')


def read_config(path):
    if path.is_symlink() or path.resolve() != path:
        raise RollbackError('config_symlink_rejected')
    metadata = path.stat()
    if not stat.S_ISREG(metadata.st_mode) or metadata.st_size > MAX_CONFIG_BYTES:
        raise RollbackError('invalid_config_file')
    return path.read_bytes()


def digest(data):
    return hashlib.sha256(data).hexdigest()


def readiness(report):
    config = Path(report['config_path'])
    backup = Path(report['backup_config'])
    if config != CONFIG or config.resolve() != CONFIG:
        raise RollbackError('unexpected_config_target')
    if (not backup.is_absolute() or backup.is_symlink() or backup.resolve() != backup
            or STAGE not in backup.parents):
        raise RollbackError('unexpected_backup_target')
    mode = report['original_mode']
    if type(mode) is not int or mode < 0 or mode > 0o777 or mode & 0o077:
        raise RollbackError('original_private_mode_rejected')
    original, current = read_config(backup), read_config(config)
    if digest(original) != report['before_sha256']:
        raise RollbackError('backup_checksum_mismatch')
    if digest(current) != report['after_sha256']:
        raise RollbackError('config_changed_since_deployment')
    return original, current


def atomic_restore(path, data, mode):
    metadata = path.stat()
    temporary = path.with_name('.' + path.name + '.proxy-rollback-' + uuid.uuid4().hex)
    try:
        descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, mode)
        with os.fdopen(descriptor, 'wb') as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        if os.name == 'posix':
            os.chown(temporary, metadata.st_uid, metadata.st_gid)
        temporary.chmod(mode)
        os.replace(temporary, path)
        if os.name == 'posix':
            descriptor = os.open(path.parent, os.O_RDONLY | getattr(os, 'O_DIRECTORY', 0))
            try:
                os.fsync(descriptor)
            finally:
                os.close(descriptor)
    finally:
        temporary.unlink(missing_ok=True)


def wait_healthy(timeout=90):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        objects = json.loads(command(['docker', 'inspect', CONTAINER], timeout=20))
        state = objects[0]['State']
        if not state.get('Running'):
            raise RollbackError('deerflow_container_stopped')
        if state.get('Health', {}).get('Status') == 'healthy':
            return
        time.sleep(2)
    raise RollbackError('deerflow_health_timeout')


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--deployment-report', type=Path, default=STAGE / 'deployment.json')
    parser.add_argument('--apply', action='store_true')
    args = parser.parse_args(argv)
    report = json.loads(args.deployment_report.read_text(encoding='utf-8'))
    original, current = readiness(report)
    if not args.apply:
        print(json.dumps({'status': 'rollback_ready', 'config_path': str(CONFIG),
                          'apply_required': True, 'backup_checksum_verified': True,
                          'current_checksum_verified': True}), flush=True)
        return 0
    idle_native(control('daemon.status'))
    idle_folotoy()
    drained = False
    restored = False
    resume_confirmed = False
    try:
        # Set this before the RPC: if an acknowledgement is lost after drain
        # was applied, finally still attempts to restore normal admission.
        drained = True
        activity = control('daemon.drain')
        idle_native(activity)
        if activity.get('draining') is not True:
            raise RollbackError('native_drain_unconfirmed')
        idle_folotoy()
        latest_original, latest_current = readiness(report)
        if latest_original != original or latest_current != current:
            raise RollbackError('concurrent_configuration_change')
        atomic_restore(CONFIG, original, report['original_mode'])
        restored = True
        command(['docker', 'restart', '--time', '30', CONTAINER], timeout=120)
        wait_healthy()
        resumed = control('daemon.resume')
        if resumed.get('draining') is not False:
            raise RollbackError('native_resume_unconfirmed')
        resume_confirmed = True
        if digest(read_config(CONFIG)) != report['before_sha256']:
            raise RollbackError('restored_config_checksum_mismatch')
    finally:
        if drained and not resume_confirmed:
            try:
                resumed = control('daemon.resume')
                resume_confirmed = resumed.get('draining') is False
            except Exception:
                # Main emits a fixed diagnostic, never raw native output.
                resume_confirmed = False
        if drained and not resume_confirmed:
            print(json.dumps({'status': 'rollback_needs_attention',
                              'config_restored': restored,
                              'failure_code': 'native_resume_unconfirmed'}), flush=True)
    print(json.dumps({'status': 'rollback_applied', 'config_restored': restored,
                      'native_resumed': resume_confirmed, 'deerflow_healthy': True,
                      'folotoy_restarted': False}), flush=True)
    return 0


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except Exception as error:
        result = {'status': 'rollback_failed', 'error_type': type(error).__name__}
        if isinstance(error, RollbackError):
            result['failure_code'] = str(error)
        print(json.dumps(result), flush=True)
        raise SystemExit(1)
