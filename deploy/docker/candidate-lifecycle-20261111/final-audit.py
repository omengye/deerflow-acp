import datetime
import importlib.util
import json
from pathlib import Path
import sqlite3

task = Path('/home/gen/deerflow-deploy/candidate-lifecycle-20261111-3722ca94e939')
spec = importlib.util.spec_from_file_location('release', task / 'release.py')
r = importlib.util.module_from_spec(spec)
spec.loader.exec_module(r)
r.initialize()
state = r.util.inspect()
status = r.storage.status()
r.require(state['Image'] == r.read('build-report.json')['image_id'], 'image changed')
r.require(state['State']['Health']['Status'] == 'healthy' and state['RestartCount'] == 0, 'container unhealthy/restarted')
r.require(status['data']['draining'] is False, 'daemon still draining')
r.require(r.storage.others() == r.read('other-containers.json'), 'other containers changed')
r.require(r.storage.tree_hashes(r.ARCHIVE) == r.read('alternate-hashes.json'), 'inactive archive changed')
r.require(not r.ALTERNATE.exists(), 'inactive store returned')
with sqlite3.connect(r.util.DB.as_uri() + '?mode=ro', uri=True) as db:
    integrity = db.execute('PRAGMA quick_check').fetchone()[0]
    r.require(integrity == 'ok', 'database integrity failed')
    runs = dict(db.execute('SELECT status,count(*) FROM harness_runs GROUP BY status'))
    channels = [json.loads(row[0]).get('enabled', False) for row in db.execute('SELECT config FROM harness_channels')]
result = {'passed': True, 'checked_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
          'commit': r.COMMIT, 'image_id': state['Image'], 'build_id': status['runtime']['build_id'],
          'health': state['State']['Health']['Status'], 'restart_count': state['RestartCount'],
          'draining': status['data']['draining'], 'activity': {key: status['data'][key] for key in ('active_operations', 'active_runs', 'queued_runs')},
          'candidate_lifecycle': r.lifecycle_probe(r.util.CONTAINER),
          'database_integrity': integrity, 'run_states': runs, 'bundled_channel_enabled': any(channels),
          'other_containers_preserved': True, 'other_container_count': len(r.storage.others()),
          'inactive_archive_preserved': True,
          'backup_files': {p.name: {'bytes': p.stat().st_size, 'sha256': r.digest(p)} for p in sorted(r.BACKUP.iterdir()) if p.is_file()}}
r.save('final-audit.json', result)
print(json.dumps(result, indent=2))
