"""Remove the inspected temporary artifacts of this successful Gentoo release."""
import argparse
import datetime
import importlib.util
import json
import os
from pathlib import Path
import shutil
import stat
import sys

sys.dont_write_bytecode = True
TASK = Path('/home/gen/deerflow-deploy/candidate-lifecycle-20261111-3722ca94e939')
BUILDER_ID = json.loads((TASK / 'builder-report.json').read_text())['container_id']
TEMPORARY = ('build', 'stage', 'go-artifacts.tar.gz', 'image-context')


def require(value, message):
    if not value:
        raise RuntimeError(message)


def target(path):
    require(TASK.resolve() == TASK and not TASK.is_symlink(), 'release directory moved')
    require(path.parent == TASK and path.name in TEMPORARY, 'not an approved temporary artifact')
    require(not path.is_symlink() and path.resolve() == path, 'temporary target redirected')
    require(path.exists() and not os.path.ismount(path), 'missing or mounted temporary target')
    return path


def files(root):
    if root.is_file():
        return [root]
    found = []
    for directory, dirs, names in os.walk(root, followlinks=False):
        for name in dirs:
            child = Path(directory) / name
            require(not os.path.ismount(child), 'nested mounted directory')
        for name in names:
            path = Path(directory) / name
            if stat.S_ISREG(path.lstat().st_mode):
                found.append(path)
    return found


def container_snapshot(items, exclude):
    return sorted([{'id': c['Id'], 'name': c['Name'], 'image': c['Image'],
                    'running': c['State']['Running'], 'started_at': c['State']['StartedAt'],
                    'restart_count': c['RestartCount']}
                   for c in items if c['Id'] != exclude], key=lambda c: c['id'])


def main(apply=False):
    spec = importlib.util.spec_from_file_location('cleanup_release', TASK / 'release.py')
    release = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(release)
    release.initialize()
    util, storage = release.util, release.storage

    def containers():
        ids = util.run(['docker', 'ps', '-aq']).split()
        return json.loads(util.run(['docker', 'inspect', *ids]))

    build = release.read('build-report.json')
    require(release.read('deployment-report.json')['passed'], 'deployment did not pass')
    production = util.inspect()
    require(production['Image'] == build['image_id'], 'active image changed')
    require(production['State']['Health']['Status'] == 'healthy', 'active container unhealthy')
    runtime = storage.status()['runtime']
    require(runtime['build_id'] == release.BUILD_ID, 'active build changed')
    before = containers()
    builder = next((c for c in before if c['Id'] == BUILDER_ID), None)
    require(builder is not None and builder['Name'] == '/' + release.BUILDER, 'unexpected builder')
    require(not builder['State']['Running'] and builder['State']['ExitCode'] == 0, 'builder is active or failed')
    require(builder['Image'] == release.read('deployment-spec.json')['builder_image_id'], 'builder image differs')
    require(any(m['Source'] == str(TASK / 'build') and m['Destination'] == '/build' for m in builder['Mounts']), 'builder belongs to another release')
    require(not any(c['Name'] == '/' + release.STAGE for c in before), 'stage container remains')
    require(all(p.exists() or p.is_symlink() for p in (TASK / name for name in TEMPORARY)), 'temporary inventory changed')
    paths = [target(TASK / name) for name in TEMPORARY]
    for c in before:
        if c['Id'] == BUILDER_ID:
            continue
        for mount in c['Mounts']:
            if mount['Type'] != 'bind':
                continue
            source = Path(mount['Source']).resolve()
            require(not any(source.is_relative_to(p) or p.is_relative_to(source) for p in paths), 'temporary path overlaps another container mount')

    rollback = json.loads(util.run(['docker', 'image', 'inspect', release.BASE_TAG]))[0]
    require(rollback['Id'] == storage.BASE, 'rollback alias changed')
    require(set(rollback.get('RepoTags') or []) - {release.BASE_TAG}, 'rollback image has no permanent tag')
    source_archive = TASK / 'source.tar.gz'
    require(release.digest(source_archive) == release.read('source-manifest.json')['source_archive_sha256'], 'source archive changed')
    require(storage.tree_hashes(release.ARCHIVE) == release.read('alternate-hashes.json'), 'inactive archive changed')
    backup_hashes = storage.tree_hashes(release.BACKUP)
    protected = [util.CONFIG, util.ENV, util.COMPOSE, util.WRAPPER, storage.BINDING,
                 util.CONFIG.with_name('search-providers.json'), source_archive]
    protected += [p for p in TASK.iterdir() if p.is_file() and p.name != 'go-artifacts.tar.gz' and not p.name.startswith('cleanup-')]
    preserved_hashes = {str(p): release.digest(p) for p in protected}
    inventory = [{'name': p.name, 'bytes': sum(f.stat().st_size for f in files(p))} for p in paths]
    plan = {'builder': BUILDER_ID, 'temporary_paths': inventory, 'temporary_alias': release.BASE_TAG,
            'logical_bytes_to_remove': sum(p['bytes'] for p in inventory),
            'backup': str(release.BACKUP), 'source_archive': str(source_archive)}
    if not apply:
        print(json.dumps({'ready': True, **plan}, indent=2))
        return

    free_before = shutil.disk_usage(TASK).free
    util.run(['docker', 'rm', BUILDER_ID])
    require(not util.run(['docker', 'ps', '-aq', '--filter', 'id=' + BUILDER_ID]).strip(), 'builder was not removed')
    for path in paths:
        checked = target(path)
        if checked.is_dir():
            shutil.rmtree(checked)
        else:
            checked.unlink()
    # This removes only a disposable tag, retaining the rollback image's other tag.
    util.run(['docker', 'image', 'rm', release.BASE_TAG])

    require(container_snapshot(containers(), BUILDER_ID) == container_snapshot(before, BUILDER_ID), 'cleanup changed other containers')
    for path, expected in preserved_hashes.items():
        require(release.digest(Path(path)) == expected, 'retained configuration or evidence changed')
    require(storage.tree_hashes(release.BACKUP) == backup_hashes, 'backup changed')
    require(storage.tree_hashes(release.ARCHIVE) == release.read('alternate-hashes.json'), 'inactive archive changed')
    require(storage.image_id(storage.BASE) == storage.BASE, 'rollback image missing')
    require(storage.image_id(build['image_id']) == build['image_id'], 'running image missing')
    require(storage.image_id(builder['Image']) == builder['Image'], 'shared builder image missing')
    current = util.inspect()
    current_runtime = storage.status()['runtime']
    require(current['State']['Health']['Status'] == 'healthy', 'production health changed')
    require(current_runtime['instance_id'] == runtime['instance_id'], 'native daemon restarted')
    require(not any(p.exists() or p.is_symlink() for p in paths), 'temporary paths remain')
    result = {'passed': True, 'checked_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
              **plan, 'logical_bytes_removed': plan['logical_bytes_to_remove'],
              'free_bytes_before': free_before, 'free_bytes_after': shutil.disk_usage(TASK).free,
              'health': current['State']['Health']['Status'], 'image_id': current['Image'],
              'container_unchanged': True, 'daemon_unchanged': True, 'other_containers_preserved': True,
              'configuration_preserved': True, 'backup_and_inactive_archive_preserved': True,
              'source_archive_and_reports_preserved': True, 'shared_builder_image_preserved': True,
              'rollback_image_preserved': True}
    release.save('cleanup-report.json', result)
    print(json.dumps(result, indent=2))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apply', action='store_true')
    main(parser.parse_args().apply)
