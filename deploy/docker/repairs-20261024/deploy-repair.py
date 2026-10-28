"""Apply the reviewed weather, clock, SSH-skill and daemon-directory repair.

Run on Gentoo with an extracted, hash-verified payload directory. Credentials
remain in the existing private bind mount and never enter this payload.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import time

DEER = 'deerflow-deploy-deerflow-1'
FOLO = 'folotoy-ws-folotoy-1'
DATA = '/var/lib/deerflow/user-data/config/go-harness-state'


def run(args, *, cwd=None, timeout=60, payload=None):
    result = subprocess.run(args, cwd=cwd, input=payload, capture_output=True,
                            text=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError('Command failed: ' + args[0] + ' (exit ' + str(result.returncode) + ')')
    return result.stdout


def control(operation, **fields):
    raw = run(['docker', 'exec', '-i', DEER, '/var/lib/deerflow/deerflow-control-go',
               '--runtime-dir', '/var/lib/deerflow/user-data/runtime/acp-go'],
              payload=json.dumps({'operation': operation, **fields}), timeout=25)
    value = json.loads(raw)
    if not value.get('ok'):
        raise RuntimeError('Management operation failed: ' + operation)
    return value.get('data') or {}


def container(name):
    value = json.loads(run(['docker', 'inspect', name], timeout=15))[0]
    return {'name': name, 'status': value['State']['Status'],
            'health': value['State'].get('Health', {}).get('Status'),
            'image': value['Config']['Image']}


def healthy(names, timeout=75):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        states = [container(name) for name in names]
        if all(s['status'] == 'running' and s['health'] == 'healthy' for s in states):
            return states
        time.sleep(2)
    raise RuntimeError('Container health deadline exceeded')


def wait_idle(timeout=90):
    deadline = time.monotonic() + timeout
    announced = False
    while time.monotonic() < deadline:
        try:
            count = control('daemon.status').get('active_operations', 0)
        except RuntimeError:
            # A bridge may have already shut down an idle native process.
            probe = "import pathlib,sqlite3; p=pathlib.Path('" + DATA + "/harness.db'); d=sqlite3.connect(p.as_uri()+'?mode=ro',uri=True); print(d.execute(\"SELECT count(*) FROM harness_runs WHERE status IN ('running','pending','in_progress')\").fetchone()[0])"
            count = int(run(['docker', 'exec', DEER, '/opt/deerflow-skill-python/bin/python', '-c', probe]).strip())
        if count == 0:
            return
        if not announced:
            print('waiting_for_active_requests', count, flush=True)
            announced = True
        time.sleep(2)
    raise RuntimeError('Requests did not become idle; services were not stopped')


def start_and_verify_native(root):
    # Deliberately supply the conflicting Waku default: the wrapper must win.
    run(['docker', 'exec', '-e', 'DEERFLOW_GO_DATA_DIR=/var/lib/deerflow/user-data/data/go-harness',
         DEER, '/var/lib/deerflow/deerflow-acp', '--config',
         '/var/lib/deerflow/user-data/config/config.yaml', '--daemon',
         '/var/lib/deerflow/deerflow-acpd', '--runtime-dir',
         '/var/lib/deerflow/user-data/runtime/acp-go', '--start-daemon'], timeout=35)
    path = root / 'data/user-data/runtime/acp-go/endpoint.json'
    endpoint = json.loads(path.read_text())
    if endpoint.get('data_dir') != DATA:
        raise RuntimeError('Native service selected a different conversation directory')
    diagnostics = control('session.diagnostics', session_id='WWL4PNRSI65AS6WOKUF3WTM5IO')
    if not diagnostics:
        raise RuntimeError('Original production conversation was not accessible')
    return {'data_dir': endpoint['data_dir'], 'original_session_accessible': True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--payload', required=True, type=Path)
    parser.add_argument('--root', type=Path, default=Path('/home/gen/deerflow-deploy'))
    parser.add_argument('--folotoy-root', type=Path,
                        default=Path('/home/gen/Documents/folotoy/deerflow-ws/src'))
    args = parser.parse_args()
    payload = args.payload.resolve(strict=True)
    root = args.root.resolve(strict=True)
    folo_root = args.folotoy_root.resolve(strict=True)
    manifest = json.loads((payload / 'manifest.json').read_text())
    for relative, expected in manifest['files'].items():
        path = (payload / relative).resolve(strict=True)
        if not path.is_relative_to(payload) or hashlib.sha256(path.read_bytes()).hexdigest() != expected:
            raise RuntimeError('Payload digest mismatch: ' + relative)
    stage = root / 'repair-20261024'
    stage.mkdir(mode=0o700, exist_ok=True)
    stage.chmod(0o700)
    report_path = stage / 'deployment.json'
    if report_path.exists():
        raise RuntimeError('A deployment report already exists; inspect before reapplying')
    roles_path = folo_root / 'roles-ws.json'
    roles_digest = hashlib.sha256(roles_path.read_bytes()).digest()
    config_path = root / 'data/user-data/config/config.yaml'
    config_digest = hashlib.sha256(config_path.read_bytes()).digest()
    report = {'status': 'preparing', 'credential_values_in_payload': False}
    # This makes a tiny image and updates host source/Compose only; the current
    # container still runs its old image until the coordinated recreation below.
    clock = json.loads(run(['python3', str(payload / 'folotoy/install-folotoy-clock.py'),
                            '--root', str(folo_root), '--provider-source',
                            str(payload / 'folotoy/deerflow.py'), '--apply'], timeout=180).splitlines()[-1])
    report['folotoy_installation'] = clock
    wait_idle()
    run(['docker', 'stop', FOLO], timeout=45)
    folo_stopped = True
    try:
        wait_idle()
        backup = stage / 'backup'
        backup.mkdir(mode=0o700, exist_ok=False)
        for relative in ('public/weather-cma', 'custom/huaweicloud-linux', 'custom/huaweicloud-sandbox'):
            target = root / 'skills' / relative
            if target.is_dir():
                shutil.copytree(target, backup / 'skills' / relative)
        for source in (payload / 'skills').rglob('*'):
            if not source.is_file():
                continue
            relative = source.relative_to(payload / 'skills')
            target = root / 'skills' / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            temporary = target.with_name('.' + target.name + '.repair')
            temporary.write_bytes(source.read_bytes())
            temporary.chmod(0o644)
            os.replace(temporary, target)
        runtime = json.loads(run(['python3', str(payload / 'runtime/install-runtime-override.py'),
                                  '--root', str(root), '--wrapper-source',
                                  str(payload / 'runtime/deerflow-acpd-wrapper.sh'), '--apply'],
                                 timeout=50).splitlines()[-1])
        report['runtime_installation'] = runtime
        run(['docker', 'compose', 'up', '-d', '--no-build', '--force-recreate', 'deerflow'],
            cwd=root, timeout=100)
        healthy([DEER])
        report['after_recreate'] = start_and_verify_native(root)
        # Exercise an ordinary restart before reopening the voice service.
        run(['docker', 'restart', DEER], timeout=70)
        healthy([DEER])
        report['after_restart'] = start_and_verify_native(root)
        report['skills'] = {}
        for name in ('weather-cma', 'huaweicloud-linux', 'huaweicloud-sandbox'):
            data = control('skill.list', name=name, status='current', limit=10)
            report['skills'][name] = data
        run(['docker', 'compose', '--env-file', '.env.ws', '-p', 'folotoy-ws',
             '-f', 'compose.ws.yaml', 'up', '-d', '--no-build', '--force-recreate', 'folotoy'],
            cwd=folo_root, timeout=70)
        folo_stopped = False
        report['containers'] = healthy([DEER, FOLO])
        report['roles_unchanged'] = hashlib.sha256(roles_path.read_bytes()).digest() == roles_digest
        report['deerflow_model_and_tool_config_unchanged'] = hashlib.sha256(config_path.read_bytes()).digest() == config_digest
        if not report['roles_unchanged'] or not report['deerflow_model_and_tool_config_unchanged']:
            raise RuntimeError('An unrelated configuration changed during deployment')
        report['status'] = 'deployed'
        report['private_ssh_files_mode'] = {
            name: oct((root / 'data/user-data/private/skills/huaweicloud' / name).stat().st_mode & 0o777)
            for name in ('huaweicloud_ed25519', 'known_hosts')}
        # Reports contain only public capability metadata, never config snapshots.
        report_path.write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n')
        report_path.chmod(0o600)
        print(json.dumps({key: value for key, value in report.items() if key != 'skills'}, ensure_ascii=False), flush=True)
    finally:
        if folo_stopped:
            # Always bring back the voice container even when a verification fails.
            subprocess.run(['docker', 'start', FOLO], capture_output=True, timeout=30)


if __name__ == '__main__':
    main()
