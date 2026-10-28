"""Deploy the reviewed music/OpenCLI payload on Gentoo with private backups.

The payload contains no credentials. Existing private runtime JSON supplies the
music and browser bridge authorizations. Docker and YAML outputs stay captured.
"""
import argparse
from dataclasses import dataclass
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import time
import urllib.request
import uuid

DEER = 'deerflow-deploy-deerflow-1'
FOLO = 'folotoy-ws-folotoy-1'
DEER_IMAGE = 'deerflow-server:music-opencli-20261024'
FOLO_IMAGE = 'folotoy-deerflow-ws:music-20261024'
DATA_DIR = '/var/lib/deerflow/user-data/config/go-harness-state'
UNIT = 'deerflow-opencli-bridge.service'
SKILLS = ('toy-playmusic', 'song-download', 'qq-music', 'opencli')


class DeploymentError(Exception):
    pass


def digest(data):
    return hashlib.sha256(data).hexdigest()


def command(argv, *, cwd=None, payload=None, timeout=60, check=True):
    environment = dict(os.environ)
    environment.pop('DEERFLOW_IMAGE', None)
    environment.pop('FOLOTOY_WS_IMAGE', None)
    result = subprocess.run(argv, cwd=cwd, input=payload, env=environment,
                            capture_output=True, text=True, timeout=timeout)
    if result.returncode and check:
        raise DeploymentError('Command failed: ' + argv[0] + ' (exit ' + str(result.returncode) + ')')
    return result


def atomic_write(path, data, mode=0o600):
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name('.' + path.name + '.' + uuid.uuid4().hex + '.tmp')
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(fd, 'wb') as output:
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
        temporary.chmod(mode)
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


@dataclass
class Change:
    path: Path
    before: bytes | None
    mode: int
    after: bytes
    after_mode: int
    applied: bool = False

    @classmethod
    def prepare(cls, path, data, mode=0o600):
        if path.is_symlink():
            raise DeploymentError('Refusing a symlink at a deployment target')
        before = path.read_bytes() if path.is_file() else None
        original_mode = path.stat().st_mode & 0o777 if before is not None else mode
        return cls(path, before, original_mode, data, mode)

    def apply(self):
        current = self.path.read_bytes() if self.path.is_file() else None
        if self.path.is_symlink() or current != self.before:
            raise DeploymentError('A deployment file changed concurrently')
        atomic_write(self.path, self.after, self.after_mode)
        self.applied = True

    def restore(self):
        if not self.applied:
            return True
        if self.path.is_symlink() or not self.path.is_file() or self.path.read_bytes() != self.after:
            return False
        if self.before is None:
            self.path.unlink()
        else:
            atomic_write(self.path, self.before, self.mode)
        return True


def update_image_env(raw, key, image):
    pattern = re.compile(rb'(?m)^([ \t]*(?:export[ \t]+)?' + key.encode() + rb'[ \t]*=)[^\r\n]*(\r?)$')
    if len(list(pattern.finditer(raw))) > 1:
        raise DeploymentError('Duplicate image override requires review')
    return pattern.sub(lambda match: match[1] + image.encode() + match[2], raw)


YAML_TRANSFORM = r'''
import copy,json,sys,yaml
r=json.load(sys.stdin);before=yaml.safe_load(r['deer_config']);after=copy.deepcopy(before)
after.setdefault('sandbox',{})['allow_host_tools']=True
tools=after.setdefault('tools',[]);matches=[t for t in tools if t.get('name')=='host_opencli']
if len(matches)>1:raise ValueError('duplicate host_opencli tool')
if matches:tool=matches[0]
else:tool={'name':'host_opencli'};tools.append(tool)
tool.update(executable='/opt/deerflow-host-tools/opencli-proxy',timeout=100,allowed_sites=r['sites'])
deer=yaml.safe_load(r['deer_compose']);newdeer=copy.deepcopy(deer)
newdeer['services']['deerflow']['image']='${DEERFLOW_IMAGE:-deerflow-server:music-opencli-20261024}'
folo=yaml.safe_load(r['folo_compose']);newfolo=copy.deepcopy(folo);service=newfolo['services']['folotoy']
service['image']='${FOLOTOY_WS_IMAGE:-folotoy-deerflow-ws:music-20261024}'
environment=service.setdefault('environment',{})
if not isinstance(environment,dict):raise ValueError('expected environment mapping')
environment.update(MUSIC_CONTROL_CONFIG='/run/deerflow-music-control.json',DEERFLOW_TURN_TIMEOUT='240')
volumes=service.setdefault('volumes',[])
for desired,dest in [(r['music_mount'],'/music'),('./private/music-control.json:/run/deerflow-music-control.json:ro','/run/deerflow-music-control.json')]:
 found=[]
 for value in volumes:
  target=value.get('target') if isinstance(value,dict) else value.split(':')[1] if ':' in value else None
  if target==dest:found.append(value)
 if found and found!=[desired]:raise ValueError('conflicting music mount')
 if not found:volumes.append(desired)
result={}
for name,value in [('deer_config',after),('deer_compose',newdeer),('folo_compose',newfolo)]:
 raw=yaml.safe_dump(value,allow_unicode=True,sort_keys=False)
 if yaml.safe_load(raw)!=value:raise ValueError('serialization changed values')
 result[name]=raw
# Constructing each candidate from an untouched deepcopy confines changes to
# the exact fields above; no model, role, voice or existing volume is edited.
result['expected_config']=after
print(json.dumps(result,ensure_ascii=False))
'''


def yaml_candidates(root, folo, sites):
    raw = {'deer_config': (root / 'data/user-data/config/config.yaml').read_text(),
           'deer_compose': (root / 'compose.yaml').read_text(),
           'folo_compose': (folo / 'compose.ws.yaml').read_text(), 'sites': sites,
           'music_mount': '/home/gen/deerflow-workspaces/folotoy/music:/music:ro'}
    result = command(['docker', 'exec', '-i', FOLO, 'python3', '-c', YAML_TRANSFORM],
                     payload=json.dumps(raw), timeout=30)
    try:
        return json.loads(result.stdout)
    except ValueError:
        raise DeploymentError('YAML candidate construction failed') from None


def control(operation, **fields):
    result = command(['docker', 'exec', '-i', DEER, '/var/lib/deerflow/deerflow-control-go',
                      '--runtime-dir', '/var/lib/deerflow/user-data/runtime/acp-go'],
                     payload=json.dumps({'operation': operation, **fields}), timeout=25)
    value = json.loads(result.stdout)
    if value.get('ok') is not True:
        raise DeploymentError('Native management call failed')
    return value.get('data') or {}


def wait_idle(timeout=120):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            status = control('daemon.status')
            count = status.get('active_operations')
            if not isinstance(count, int):
                raise DeploymentError('Invalid activity response')
        except DeploymentError:
            code = "import pathlib,sqlite3; p=pathlib.Path('" + DATA_DIR + "/harness.db'); d=sqlite3.connect(p.as_uri()+'?mode=ro',uri=True); print(d.execute(\"SELECT count(*) FROM harness_runs WHERE status IN ('running','pending','in_progress')\").fetchone()[0])"
            count = int(command(['docker', 'exec', DEER, '/opt/deerflow-skill-python/bin/python', '-c', code]).stdout.strip())
        if count == 0:
            return
        time.sleep(2)
    raise DeploymentError('Active voice requests did not become idle')


def container_state(name):
    value = json.loads(command(['docker', 'inspect', name], timeout=15).stdout)[0]
    return {'name': name, 'status': value['State']['Status'],
            'health': value['State'].get('Health', {}).get('Status'),
            'image': value['Config']['Image'], 'image_id': value['Image']}


def healthy(names, timeout=90):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        states = [container_state(name) for name in names]
        if all(v['status'] == 'running' and v['health'] == 'healthy' for v in states):
            return states
        time.sleep(2)
    raise DeploymentError('Container health deadline exceeded')


def native_identity(root):
    command(['docker', 'exec', '-e', 'DEERFLOW_GO_DATA_DIR=/var/lib/deerflow/user-data/data/go-harness',
             DEER, '/var/lib/deerflow/deerflow-acp', '--config',
             '/var/lib/deerflow/user-data/config/config.yaml', '--daemon',
             '/var/lib/deerflow/deerflow-acpd', '--runtime-dir',
             '/var/lib/deerflow/user-data/runtime/acp-go', '--start-daemon'], timeout=40)
    endpoint = json.loads((root / 'data/user-data/runtime/acp-go/endpoint.json').read_text())
    if endpoint.get('data_dir') != DATA_DIR:
        raise DeploymentError('The original conversation directory was not selected')
    if not control('session.diagnostics', session_id='WWL4PNRSI65AS6WOKUF3WTM5IO'):
        raise DeploymentError('The original conversation was not accessible')
    return {'data_dir': DATA_DIR, 'original_session_accessible': True}


def bridge_healthy(config):
    host = config.get('bind', '172.27.0.1')
    if host in ('0.0.0.0', '::'):
        host = '127.0.0.1'
    request = urllib.request.Request('http://' + host + ':' + str(config.get('port', 18990)) + '/health',
                                     headers={'Authorization': 'Bearer ' + config['token']})
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(request, timeout=3) as response:
            value = json.load(response)
        return value.get('ok') is True and value.get('service') == 'opencli-host-bridge'
    except Exception:
        return False


def compose_validate(folder, raw, *, folo=False):
    candidate = folder / ('.compose-music-' + uuid.uuid4().hex + '.yaml')
    try:
        atomic_write(candidate, raw.encode())
        argv = ['docker', 'compose', '--project-directory', str(folder)]
        if folo:
            argv += ['--env-file', str(folder / '.env.ws')]
        command([*argv, '-f', str(candidate), 'config', '--quiet'], cwd=folder, timeout=30)
    finally:
        candidate.unlink(missing_ok=True)


def compose_up(root, folo):
    if folo:
        argv = ['docker', 'compose', '--env-file', '.env.ws', '-p', 'folotoy-ws',
                '-f', 'compose.ws.yaml', 'up', '-d', '--no-build', '--force-recreate', 'folotoy']
    else:
        argv = ['docker', 'compose', 'up', '-d', '--no-build', '--force-recreate', 'deerflow']
    command(argv, cwd=root, timeout=110)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--payload', required=True, type=Path)
    parser.add_argument('--root', type=Path, default=Path('/home/gen/deerflow-deploy'))
    parser.add_argument('--folotoy-root', type=Path, default=Path('/home/gen/Documents/folotoy/deerflow-ws/src'))
    parser.add_argument('--allow-redeploy', action='store_true')
    args = parser.parse_args()
    os.umask(0o077)
    payload, root, folo = (p.resolve(strict=True) for p in (args.payload, args.root, args.folotoy_root))
    manifest = json.loads((payload / 'manifest.json').read_text())
    for name, expected in manifest['files'].items():
        path = (payload / name).resolve(strict=True)
        if not path.is_relative_to(payload) or digest(path.read_bytes()) != expected:
            raise DeploymentError('Payload digest mismatch')
    stage = root / 'music-opencli-20261024'
    stage.mkdir(mode=0o700, exist_ok=True)
    stage.chmod(0o700)
    report_path = stage / 'deployment.json'
    if report_path.exists() and not args.allow_redeploy:
        raise DeploymentError('An existing deployment report requires review before reapply')
    host_config_path = root / 'private/opencli-bridge.json'
    proxy_config_path = root / 'data/user-data/private/host-tools/opencli.json'
    music_config_path = folo / 'private/music-control.json'
    for path in (host_config_path, proxy_config_path, music_config_path):
        if not path.is_file() or path.stat().st_mode & 0o077:
            raise DeploymentError('Expected private runtime configuration is missing or not 0600')
    host_config = json.loads(host_config_path.read_text())
    proxy_config = json.loads(proxy_config_path.read_text())
    if host_config.get('token') != proxy_config.get('token'):
        raise DeploymentError('Host and proxy credentials do not match')
    if not (root / 'data/user-data/config/go-harness-state/harness.db').is_file():
        raise DeploymentError('The original conversation database is absent')
    candidates = yaml_candidates(root, folo, host_config['allowed_sites'])
    compose_validate(root, candidates['deer_compose'])
    compose_validate(folo, candidates['folo_compose'], folo=True)
    changes = [
        Change.prepare(root / 'data/user-data/config/config.yaml', candidates['deer_config'].encode()),
        Change.prepare(root / 'compose.yaml', candidates['deer_compose'].encode()),
        Change.prepare(folo / 'compose.ws.yaml', candidates['folo_compose'].encode()),
    ]
    for folder, filename, key, image in ((root, '.env', 'DEERFLOW_IMAGE', DEER_IMAGE),
                                        (folo, '.env', 'FOLOTOY_WS_IMAGE', FOLO_IMAGE),
                                        (folo, '.env.ws', 'FOLOTOY_WS_IMAGE', FOLO_IMAGE)):
        path = folder / filename
        if path.is_file():
            raw = path.read_bytes()
            after = update_image_env(raw, key, image)
            if after != raw:
                changes.append(Change.prepare(path, after))
    source_manifest = json.loads((payload / 'folotoy-source/manifest.json').read_text())
    for record in source_manifest['files']:
        relative = Path(record['path'])
        if relative.parts[0] != 'src' or '..' in relative.parts:
            raise DeploymentError('Invalid Folo source path')
        target = folo.joinpath(*relative.parts[1:])
        current = target.read_bytes() if target.is_file() else None
        actual = digest(current) if current is not None else None
        if actual not in (record.get('before_sha256'), record['after_sha256']):
            raise DeploymentError('Folo source changed outside the reviewed versions')
        data = (payload / 'folotoy-source' / relative).read_bytes()
        if digest(data) != record['after_sha256']:
            raise DeploymentError('Folo source digest mismatch')
        changes.append(Change.prepare(target, data, 0o644))
    for source in (payload / 'skills').rglob('*'):
        if source.is_file():
            relative = source.relative_to(payload / 'skills')
            if len(relative.parts) < 3 or relative.parts[:1] != ('custom',) or relative.parts[1] not in SKILLS:
                raise DeploymentError('Unexpected skill destination')
            changes.append(Change.prepare(root / 'skills' / relative, source.read_bytes(), 0o644))
    changes.append(Change.prepare(root / 'runtime/deerflow-acpd-wrapper.sh',
                                  (payload / 'runtime/deerflow-acpd-wrapper.sh').read_bytes(), 0o755))
    for name in ('host_opencli_bridge.py', 'service.py'):
        changes.append(Change.prepare(root / 'host-tools' / name, (payload / 'host-bridge' / name).read_bytes(), 0o755))
    unit_path = Path.home() / '.config/systemd/user' / UNIT
    changes.append(Change.prepare(unit_path, (payload / 'host-bridge' / UNIT).read_bytes(), 0o644))
    old_containers = [container_state(name) for name in (DEER, FOLO)]
    bridge_was_active = bridge_healthy(host_config)
    unit_existed = unit_path.is_file()
    roles_hash = digest((folo / 'roles-ws.json').read_bytes())
    original_deer_mappings = candidates['expected_config']
    backup = stage / ('backup-' + datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ') + '-' + uuid.uuid4().hex[:8])
    backup.mkdir(mode=0o700)
    backup_index = []
    for index, change in enumerate(changes):
        name = str(index).zfill(3) + '-' + change.path.name + '.before'
        if change.before is not None:
            atomic_write(backup / name, change.before)
        backup_index.append({'target': str(change.path), 'backup': name if change.before is not None else None,
                             'before_sha256': digest(change.before) if change.before is not None else None,
                             'before_mode': oct(change.mode)})
    atomic_write(backup / 'index.json', (json.dumps(backup_index, indent=2) + '\n').encode())
    report = {'status': 'preparing', 'backup': str(backup), 'credentials_in_payload': False,
              'new_skills': list(SKILLS), 'previous_containers': old_containers}
    stopped = False
    bridge_started = False
    try:
        print('building_thin_images', flush=True)
        for dockerfile, image in (('Dockerfile.deerflow', DEER_IMAGE), ('Dockerfile.folotoy', FOLO_IMAGE)):
            command(['docker', 'build', '--pull=false', '--network=none', '-f', str(payload / dockerfile),
                     '-t', image, str(payload)], timeout=300)
        wait_idle()
        command(['docker', 'stop', FOLO], timeout=45)
        stopped = True
        wait_idle()
        for change in changes:
            change.apply()
        # The existing shared workspace is already mounted into DeerFlow.
        Path('/home/gen/deerflow-workspaces/folotoy/music').mkdir(mode=0o755, parents=True, exist_ok=True)
        bridge_started = True
        command(['systemctl', '--user', 'daemon-reload'], check=False, timeout=20)
        service = command(['systemctl', '--user', 'enable', '--now', UNIT], check=False, timeout=30)
        if service.returncode == 0:
            command(['systemctl', '--user', 'restart', UNIT], timeout=30)
            report['host_bridge_supervisor'] = 'systemd-user'
        else:
            command(['python3', str(root / 'host-tools/service.py'), 'stop',
                     '--config', str(host_config_path)], timeout=20)
            launcher = command(['python3', str(root / 'host-tools/service.py'), 'start',
                                '--config', str(host_config_path)], timeout=20)
            if not json.loads(launcher.stdout).get('healthy') and not bridge_healthy(host_config):
                raise DeploymentError('Host bridge fallback launch failed')
            report['host_bridge_supervisor'] = 'user-launcher'
        compose_up(root, False)
        healthy([DEER])
        report['native_identity'] = native_identity(root)
        compose_up(folo, True)
        report['containers'] = healthy([DEER, FOLO])
        if not bridge_healthy(host_config):
            raise DeploymentError('Host bridge health check failed')
        if digest((folo / 'roles-ws.json').read_bytes()) != roles_hash:
            raise DeploymentError('Role/voice configuration changed concurrently')
        # Validate the private configuration semantically after the Go launcher
        # has used it; never return its contents or model credentials.
        verify_code = "import json,sys,yaml; p=json.load(sys.stdin); d=yaml.safe_load(p['raw']); print(json.dumps({'equal':d==p['expected']}))"
        checked = command(['docker', 'exec', '-i', FOLO, 'python3', '-c', verify_code],
                          payload=json.dumps({'raw': (root / 'data/user-data/config/config.yaml').read_text(),
                                              'expected': original_deer_mappings}), timeout=20)
        if json.loads(checked.stdout).get('equal') is not True:
            raise DeploymentError('Unexpected native configuration mutation')
        report.update(status='deployed', host_bridge_healthy=True, roles_unchanged=True,
                      only_requested_native_fields_changed=True, original_images_retained=True,
                      playback_functionality_pending_live_validation=True,
                      browser_functionality_pending_live_validation=True)
        atomic_write(report_path, (json.dumps(report, ensure_ascii=False, indent=2) + '\n').encode())
        print(json.dumps(report, ensure_ascii=False), flush=True)
        return 0
    except Exception as error:
        failures = []
        if bridge_started:
            command(['systemctl', '--user', 'stop', UNIT], check=False, timeout=20)
            command(['python3', str(root / 'host-tools/service.py'), 'stop', '--config', str(host_config_path)],
                    check=False, timeout=20)
            if not unit_existed:
                command(['systemctl', '--user', 'disable', UNIT], check=False, timeout=20)
        for change in reversed(changes):
            try:
                if not change.restore():
                    failures.append('concurrent_file_preserved')
            except Exception:
                failures.append('file_restore_failed')
        command(['systemctl', '--user', 'daemon-reload'], check=False, timeout=20)
        if bridge_was_active:
            restored_bridge = command(['systemctl', '--user', 'start', UNIT], check=False, timeout=30)
            if restored_bridge.returncode or not bridge_healthy(host_config):
                failures.append('previous_bridge_restore_failed')
        if stopped:
            for folder, is_folo in ((root, False), (folo, True)):
                try:
                    compose_up(folder, is_folo)
                except Exception:
                    failures.append('container_restore_failed')
        report.update(status='rollback_incomplete' if failures else 'rolled_back',
                      failure_type=type(error).__name__,
                      failure_message=str(error) if isinstance(error, DeploymentError) else 'Deployment failed; private backup retained',
                      rollback_errors=failures)
        atomic_write(report_path, (json.dumps(report, ensure_ascii=False, indent=2) + '\n').encode())
        print(json.dumps(report, ensure_ascii=False), flush=True)
        return 1


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except (DeploymentError, OSError, ValueError, KeyError) as error:
        print(json.dumps({'status': 'not_deployed', 'error_type': type(error).__name__,
                          'error': str(error) if isinstance(error, DeploymentError) else 'Preflight failed; inspect local paths/configuration privately'}))
        raise SystemExit(1)
