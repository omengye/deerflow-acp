"""Remove only this successful rollout's temporary build/test artifacts."""
import argparse
import importlib.util
import json
from pathlib import Path
import shutil
import subprocess

TASK = Path('/home/gen/deerflow-deploy/search-plugins-20261103-204617')


def target_for_cleanup(task, path):
    task, path = Path(task), Path(path)
    if task.is_symlink() or path.is_symlink() or path.parent.resolve() != task.resolve():
        raise RuntimeError('cleanup target escaped this release directory')
    if path.name in ('build', 'go-artifacts.tar.gz', '__pycache__'):
        return path
    if path.is_dir() and path.name.startswith(('image-', 'stage-')):
        return path
    raise RuntimeError('not a temporary release artifact')


def logical_size(path):
    if path.is_file():
        return path.stat().st_size
    return sum(p.stat().st_size for p in path.rglob('*') if p.is_file() and not p.is_symlink())


def main(expected_builder):
    spec = importlib.util.spec_from_file_location('search_rollout_cleanup', TASK / 'release.py')
    release = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(release)
    release.initialize()
    util, storage = release.util, release.storage
    report = release.read('production-report.json')
    build = release.read('build-report.json')
    release.require(report.get('passed') and release.read('deployment-report.json').get('passed'), 'deployment has not passed')
    release.require(util.inspect()['Image'] == build['image_id'], 'active image changed')
    release.require(release.digest(storage.BINDING) == release.read('baseline.json')['binding_sha256'], 'database binding changed')
    release.require(storage.others() == release.read('other-containers.json'), 'other containers changed')
    builder = util.inspect(release.BUILDER)
    release.require(builder['Id'] == expected_builder and not builder['State']['Running'], 'unexpected or active builder')
    release.require(builder['Image'] == release.read('deployment-spec.json')['builder_image_id'], 'builder image differs')
    release.require(any(x.get('Source') == str(TASK / 'build') and x.get('Destination') == '/build' for x in builder['Mounts']),
                    'builder does not belong to this release')
    # Stage and probe containers must already have been removed by their owner.
    remaining = util.run(['docker', 'ps', '-aq', '--filter', 'name=^/' + release.STAGE + '$']).strip()
    release.require(not remaining, 'stage container still exists')
    remaining = util.run(['docker', 'ps', '-aq', '--filter', 'name=^/' + release.PROBE + '$']).strip()
    release.require(not remaining, 'search probe still exists')
    free_before = shutil.disk_usage(TASK).free
    util.run(['docker', 'rm', expected_builder])
    paths = [TASK / 'build', TASK / 'go-artifacts.tar.gz', TASK / '__pycache__']
    paths += [p for p in TASK.iterdir() if p.is_dir() and p.name.startswith(('image-', 'stage-'))]
    removed, size = [], 0
    for path in paths:
        if not path.exists():
            continue
        checked = target_for_cleanup(TASK, path)
        size += logical_size(checked)
        if checked.is_dir():
            shutil.rmtree(checked)
        else:
            checked.unlink()
        removed.append(checked.name)
    # Remove only our temporary alias; keep the rollback image and active tag.
    base_info = json.loads(util.run(['docker', 'image', 'inspect', release.BASE_TAG]))[0]
    alias_removed = False
    if len(base_info.get('RepoTags') or []) > 1:
        util.run(['docker', 'image', 'rm', release.BASE_TAG])
        alias_removed = True
    release.require(util.inspect()['Image'] == build['image_id'], 'cleanup affected the active image')
    release.require(release.digest(storage.BINDING) == release.read('baseline.json')['binding_sha256'], 'cleanup changed database binding')
    release.require(storage.others() == release.read('other-containers.json'), 'cleanup affected other containers')
    release.require(storage.status()['runtime']['build_id'] == release.BUILD_ID, 'active daemon changed')
    release.save('cleanup-report.json', {'passed': True, 'removed_builder': expected_builder,
        'removed_temporary_paths': removed, 'temporary_alias_removed': alias_removed,
        'logical_bytes_removed': size, 'free_bytes_before': free_before, 'free_bytes_after': shutil.disk_usage(TASK).free,
        'active_image_preserved': True, 'backup_preserved': storage.BACKUP.is_dir(),
        'reports_and_source_archive_preserved': (TASK / 'source.tar.gz').is_file(),
        'database_binding_unchanged': True, 'other_containers_preserved': True})
    print(json.dumps(release.read('cleanup-report.json'), indent=2))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--expected-builder', required=True)
    main(parser.parse_args().expected_builder)
