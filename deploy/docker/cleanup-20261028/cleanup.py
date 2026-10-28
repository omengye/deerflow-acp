"""Remove only the audited historical paths and this host's unused build cache.

The user authorized these exact historical targets and Docker build cache.
No image, container, volume, or system prune is performed. Existing backup and
configuration hashes, container identities, and image references are audited.
"""
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import time
import uuid

ROOT = Path('/home/gen/deerflow-deploy')
STAGE = ROOT / 'cleanup-20261028'
DOCKER = ['docker', '--host', 'unix:///var/run/docker.sock']
TARGETS = {
    'voice-repair-20261025/payload-66c92e3784a7',
    'voice-repair-20261025/payload-dc21966e968c',
    'voice-repair-20261025/revision-464f9311',
    'voice-repair-20261025/revision-5d487745',
    'asr2-diagnosis-20261025',
    'asr2-validation-20261025',
    'skill-migration-20261024/runtime-build-fast.log',
    'skill-migration-20261024/runtime-build.log',
    'build-cache-cleanup-20261024.log',
    'cleanup-20261024.log',
    'build-20261024.log.gz',
}


def run(command, timeout=45):
    result = subprocess.run(command, capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError('cleanup verification command failed: ' + command[0])
    return result.stdout


def sha(path):
    digest = hashlib.sha256()
    with path.open('rb') as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b''):
            digest.update(chunk)
    return digest.hexdigest()


def atomic_json(path, data):
    temporary = path.with_name('.' + path.name + '.' + uuid.uuid4().hex)
    try:
        temporary.write_text(json.dumps(data, ensure_ascii=False, indent=2) + '\n')
        temporary.chmod(0o600)
        with temporary.open('rb') as stream:
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def container_snapshot():
    ids = run(DOCKER + ['ps', '-aq']).split()
    if not ids:
        return []
    return [{'id': obj['Id'], 'name': obj['Name'].lstrip('/'), 'image_id': obj['Image'],
             'state': obj['State']['Status'], 'started_at': obj['State']['StartedAt'],
             'restart_count': obj['RestartCount'], 'health': obj['State'].get('Health', {}).get('Status'),
             'mounts': [{'source': mount.get('Source'), 'destination': mount.get('Destination')}
                        for mount in obj.get('Mounts', [])]}
            for obj in json.loads(run(DOCKER + ['inspect', *ids]))]


def image_snapshot():
    ids = sorted(set(run(DOCKER + ['image', 'ls', '--quiet', '--no-trunc', '--all']).split()))
    if not ids:
        return []
    return [{'id': obj['Id'], 'tags': sorted(obj.get('RepoTags') or []),
             'digests': sorted(obj.get('RepoDigests') or [])}
            for obj in json.loads(run(DOCKER + ['image', 'inspect', *ids]))]


def same_containers(before, after):
    current = {obj['id']: obj for obj in after}
    issues = []
    for prior in before:
        now = current.get(prior['id'])
        if now is None:
            issues.append({'name': prior['name'], 'issue': 'container_removed_or_recreated'})
            continue
        for key in ('image_id', 'state', 'started_at', 'restart_count'):
            if now[key] != prior[key]:
                issues.append({'name': prior['name'], 'issue': key + '_changed'})
        if prior.get('health') == 'healthy' and now.get('health') != 'healthy':
            issues.append({'name': prior['name'], 'issue': 'health_changed'})
    return issues


def validate_entry(entry, containers):
    path = Path(entry['path'])
    relative = path.relative_to(ROOT)
    if str(relative) not in TARGETS or path.resolve() != path or path.is_symlink():
        raise RuntimeError('cleanup target boundary validation failed')
    if entry.get('already_absent'):
        if path.exists():
            raise RuntimeError('previously absent target unexpectedly appeared')
        return
    if not path.exists() or path.is_dir() != entry['directory']:
        raise RuntimeError('cleanup target type changed')
    for container in containers:
        for mount in container.get('mounts', []):
            if mount.get('source'):
                source = Path(mount['source'])
                if source == path or source in path.parents or path in source.parents:
                    raise RuntimeError('cleanup target overlaps a current mount')
    all_paths = list(path.rglob('*')) if path.is_dir() else [path]
    if any(item.is_symlink() for item in all_paths):
        raise RuntimeError('symlink appeared in cleanup target')
    actual = {str(item) for item in all_paths if item.is_file()}
    if actual != {item['path'] for item in entry['files']}:
        raise RuntimeError('cleanup directory contents changed')
    for item in entry['files']:
        file = Path(item['path'])
        info = file.lstat()
        if (not stat.S_ISREG(info.st_mode) or info.st_nlink != 1
                or info.st_ino != item['inode'] or info.st_mtime_ns != item['mtime_ns']
                or sha(file) != item['sha256']):
            raise RuntimeError('historical file changed after preflight')


def no_new_references():
    paths = [str(ROOT / name) for name in TARGETS]
    for current, dirs, files in os.walk(ROOT, followlinks=False):
        current = Path(current)
        dirs[:] = [name for name in dirs
                   if name not in ('.git', 'node_modules', '__pycache__', '.venv')
                   and current / name != STAGE
                   and not any(str(current / name) == p or str(current / name).startswith(p + '/') for p in paths)
                   and not (current == ROOT and name in ('data', 'skills', 'private', 'logs'))]
        for name in files:
            file = current / name
            if file.suffix.lower() not in ('.py', '.sh', '.json', '.yaml', '.yml', '.md', '.service', '.txt'):
                continue
            if file.stat().st_size > 2 * 1024 * 1024:
                continue
            try:
                content = file.read_text(errors='replace')
            except OSError:
                continue
            if any(path in content for path in paths):
                raise RuntimeError('new deployment reference to historical target')


def main():
    plan = json.loads((STAGE / 'plan.json').read_text())
    if ROOT.resolve() != ROOT or {str(Path(e['path']).relative_to(ROOT)) for e in plan['candidates']} != TARGETS:
        raise RuntimeError('plan scope does not match authorized targets')
    if plan['docker_endpoint'] != 'unix:///var/run/docker.sock' or plan['default_context_endpoint'] != plan['docker_endpoint']:
        raise RuntimeError('unexpected Docker endpoint')
    builder = run(DOCKER + ['buildx', 'inspect', 'default'])
    if not re.search(r'^Driver:\s+docker\s*$', builder, re.M) or not re.search(r'^Endpoint:\s+default\s*$', builder, re.M):
        raise RuntimeError('selected builder is not the local Docker builder')
    before = container_snapshot()
    if same_containers(plan['containers_before'], before):
        raise RuntimeError('container state changed since preflight')
    images_before = image_snapshot()
    if sorted(images_before, key=lambda obj: obj['id']) != sorted(plan['images_before'], key=lambda obj: obj['id']):
        raise RuntimeError('image collection changed since preflight')
    for entry in plan['candidates']:
        validate_entry(entry, before)
    no_new_references()
    # Never compete with a visible client-side build. BuildKit independently
    # protects in-use cache records if a build begins after this check.
    for process in Path('/proc').iterdir():
        if not process.name.isdigit():
            continue
        try:
            arguments = [arg.decode(errors='replace') for arg in (process / 'cmdline').read_bytes().split(b'\0') if arg]
        except OSError:
            continue
        if arguments and Path(arguments[0]).name in ('docker', 'docker-buildx', 'buildctl') and 'build' in arguments[1:]:
            raise RuntimeError('active build; cleanup deferred')
    volumes_before = sorted(run(DOCKER + ['volume', 'ls', '--quiet']).splitlines())
    report = {'status': 'in_progress', 'started_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
              'user_authorized': True, 'docker_endpoint': plan['docker_endpoint'],
              'file_deletions': [], 'docker_prune': {'status': 'not_started'},
              'container_restart_commands_used': False, 'image_prune_used': False,
              'system_prune_used': False, 'volume_prune_used': False}
    save = lambda: atomic_json(STAGE / 'cleanup-report.json', report)
    save()
    for entry in plan['candidates']:
        path = Path(entry['path'])
        if entry.get('already_absent'):
            report['file_deletions'].append({'path': str(path), 'status': 'already_absent', 'allocated_bytes': 0})
            continue
        validate_entry(entry, before)
        allocated = path.lstat().st_blocks * 512
        if path.is_dir():
            allocated += sum(p.lstat().st_blocks * 512 for p in path.rglob('*'))
            shutil.rmtree(path)
        else:
            path.unlink()
        report['file_deletions'].append({'path': str(path), 'status': 'removed',
                                         'file_count': len(entry['files']), 'allocated_bytes': allocated})
        save()
    report['historical_allocated_bytes_removed'] = sum(e['allocated_bytes'] for e in report['file_deletions'])
    report['historical_file_count_removed'] = sum(e.get('file_count', 0) for e in report['file_deletions'])
    print(json.dumps({'stage': 'historical_cleanup_completed',
                      'paths_removed': sum(e['status'] == 'removed' for e in report['file_deletions']),
                      'allocated_bytes_removed': report['historical_allocated_bytes_removed']}), flush=True)
    report['docker_prune']['status'] = 'running'
    report['docker_prune']['command'] = DOCKER + ['builder', 'prune', '--builder', 'default', '--all', '--force']
    save()
    print(json.dumps({'stage': 'docker_unused_build_cache_cleanup_started', 'builder': 'default'}), flush=True)
    prune = subprocess.run(report['docker_prune']['command'], capture_output=True, text=True, timeout=360)
    totals = [line.strip() for line in prune.stdout.splitlines()
              if re.search(r'(?i)total.*(?:reclaimed|:)', line)]
    report['docker_prune'].update(status='completed' if prune.returncode == 0 else 'failed',
                                 exit_code=prune.returncode, reported_totals=totals,
                                 output_tail=prune.stdout.splitlines()[-4:])
    save()
    after = container_snapshot()
    issues = same_containers(before, after)
    if any(item['issue'] == 'health_changed' for item in issues):
        time.sleep(5)
        after = container_snapshot()
        issues = same_containers(before, after)
    after_images = image_snapshot()
    after_map = {obj['id']: obj for obj in after_images}
    image_issues = []
    for original in images_before:
        current = after_map.get(original['id'])
        if current is None or current['tags'] != original['tags'] or current['digests'] != original['digests']:
            image_issues.append(original['id'])
    protected_changes = []
    for path, original in plan['protected_files'].items():
        file = Path(path)
        if not file.is_file() or sha(file) != original['sha256']:
            protected_changes.append(path)
    service_after = run(['systemctl', '--user', 'show', 'deerflow-opencli-bridge.service',
                         '--property=ActiveState,MainPID,ActiveEnterTimestampMonotonic,ExecMainStartTimestampMonotonic']).splitlines()
    volumes_after = sorted(run(DOCKER + ['volume', 'ls', '--quiet']).splitlines())
    report.update(containers_after=after, container_issues=issues,
                  image_count_before=len(images_before), image_count_after=len(after_images),
                  image_changes=image_issues, protected_files_checked=len(plan['protected_files']),
                  protected_file_changes=protected_changes, volumes_preserved=all(v in volumes_after for v in volumes_before),
                  opencli_service_unchanged=service_after == plan['opencli_service_before'],
                  opencli_service_after=service_after,
                  docker_disk_usage_before=plan['docker_disk_usage_before'],
                  docker_disk_usage_after=run(DOCKER + ['system', 'df']).splitlines(),
                  filesystem_before=plan['filesystem_before'],
                  filesystem_after={'free_bytes': shutil.disk_usage(ROOT).free},
                  completed_at=datetime.datetime.now(datetime.timezone.utc).isoformat())
    report['filesystem_free_bytes_increase'] = report['filesystem_after']['free_bytes'] - report['filesystem_before']['free_bytes']
    report['passed'] = bool(prune.returncode == 0 and not issues and not image_issues and not protected_changes
                            and report['volumes_preserved'] and report['opencli_service_unchanged'])
    report['status'] = 'completed_verified' if report['passed'] else 'completed_with_verification_issues'
    save()
    print(json.dumps({'stage': report['status'], 'historical_allocated_bytes_removed': report['historical_allocated_bytes_removed'],
                      'docker_prune_reported_totals': totals, 'container_issues': issues,
                      'image_count_before': len(images_before), 'image_count_after': len(after_images),
                      'image_changes': image_issues, 'protected_file_changes': protected_changes,
                      'volumes_preserved': report['volumes_preserved'],
                      'opencli_service_unchanged': report['opencli_service_unchanged'],
                      'filesystem_free_bytes_increase': report['filesystem_free_bytes_increase'],
                      'docker_disk_usage_after': report['docker_disk_usage_after'],
                      'report': str(STAGE / 'cleanup-report.json')}), flush=True)
    if not report['passed']:
        raise RuntimeError('cleanup completed but verification requires attention')


if __name__ == '__main__':
    main()
