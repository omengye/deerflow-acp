"""Apply reviewed public FoloToy sources locally, preserving unrelated changes."""
import hashlib
import json
import os
from pathlib import Path
import tempfile


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest() if path.is_file() else None


def main():
    stage = Path(__file__).resolve().parent
    manifest = json.loads((stage / 'manifest.json').read_text(encoding='utf8'))
    project = Path(manifest['project']).resolve(strict=True)
    entries = manifest['files'] + manifest.get('test_files', [])
    snapshots = []
    for entry in entries:
        target = (project / entry['path']).resolve()
        source = (stage / entry['path']).resolve(strict=True)
        assert target.is_relative_to(project) and source.is_relative_to(stage)
        assert not target.is_symlink() and not source.is_symlink()
        assert digest(source) == entry['after_sha256'], 'Staged source changed'
        current = digest(target)
        assert current in (entry['before_sha256'], entry['after_sha256']), 'Source changed: ' + entry['path']
        if current != entry['after_sha256']:
            snapshots.append((entry, target, target.read_bytes() if target.exists() else None))
    applied = []
    try:
        for entry, target, before in snapshots:
            assert digest(target) == entry['before_sha256'], 'Concurrent source change'
            target.parent.mkdir(parents=True, exist_ok=True)
            fd, temporary = tempfile.mkstemp(prefix='.music-source-', dir=target.parent)
            try:
                with os.fdopen(fd, 'wb') as output:
                    output.write((stage / entry['path']).read_bytes())
                    output.flush()
                    os.fsync(output.fileno())
                os.replace(temporary, target)
            finally:
                Path(temporary).unlink(missing_ok=True)
            applied.append((entry, target, before))
    except BaseException:
        for entry, target, before in reversed(applied):
            if digest(target) == entry['after_sha256']:
                if before is None:
                    target.unlink()
                else:
                    target.write_bytes(before)
        raise
    print(json.dumps({'applied': [entry['path'] for entry, _, _ in applied],
                      'production_files': len(manifest['files']),
                      'validation': manifest['validation']}, ensure_ascii=False))


if __name__ == '__main__':
    main()
