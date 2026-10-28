"""Build the credential-free, offline Gentoo music/OpenCLI deployment payload."""
import argparse
import ast
import hashlib
import io
import json
from pathlib import Path
import tarfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--wheels', required=True, type=Path)
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    source = Path(__file__).resolve().parent
    repo = source.parents[2]
    production = source / 'folotoy-source'
    folo_manifest = json.loads((production / 'manifest.json').read_text(encoding='utf-8'))
    files = {}
    for name in ('deploy.py', 'Dockerfile.deerflow', 'Dockerfile.folotoy'):
        files[name] = (source / name).read_bytes()
    for name in ('host_opencli_bridge.py', 'service.py', 'deerflow-opencli-bridge.service'):
        files['host-bridge/' + name] = (source / 'host-bridge' / name).read_bytes()
    files['proxy/opencli-proxy'] = (source / 'proxy/opencli-proxy').read_bytes()
    files['runtime/deerflow-acpd-wrapper.sh'] = (repo / 'deploy/docker/skills-runtime/runtime/deerflow-acpd-wrapper.sh').read_bytes()
    names = ('toy-playmusic', 'song-download', 'qq-music', 'opencli')
    for name in names:
        folder = source / 'skills/custom' / name
        if not (folder / 'SKILL.md').is_file():
            raise ValueError('Missing skill: ' + name)
        for path in folder.rglob('*'):
            if not path.is_file() or '__pycache__' in path.parts:
                continue
            if path.is_symlink() or any(part in ('private', 'tests', 'verify', '.env') for part in path.relative_to(folder).parts):
                raise ValueError('Unexpected source in skill payload')
            relative = path.relative_to(folder)
            allowed = (relative.as_posix() in ('SKILL.md', 'skill-runtime.json', 'requirements.txt', 'agents/openai.yaml')
                       or relative.parts[0] == 'scripts' and path.suffix == '.py')
            if not allowed or any(part.startswith('.') for part in relative.parts):
                raise ValueError('Unrecognized or private file in skill payload')
            files['skills/custom/' + name + '/' + path.relative_to(folder).as_posix()] = path.read_bytes()
    clean_manifest = {'files': []}
    for record in folo_manifest['files']:
        relative = Path(record['path'])
        if not relative.parts or relative.parts[0] != 'src' or '..' in relative.parts or relative.suffix != '.py':
            raise ValueError('Unexpected Folo production file')
        path = production / relative
        raw = path.read_bytes()
        if hashlib.sha256(raw).hexdigest() != record['after_sha256']:
            raise ValueError('Folo staged file changed after verification: ' + relative.as_posix())
        files['folotoy-source/' + relative.as_posix()] = raw
        clean_manifest['files'].append({key: record.get(key) for key in ('path', 'before_sha256', 'after_sha256')})
    files['folotoy-source/manifest.json'] = (json.dumps(clean_manifest, indent=2) + '\n').encode()
    wheels = sorted(args.wheels.resolve(strict=True).glob('*.whl'))
    if not wheels or not any(path.name.startswith('requests-2.32.5-') for path in wheels):
        raise ValueError('The offline requests==2.32.5 wheel set is missing')
    if len(wheels) > 20 or sum(path.stat().st_size for path in wheels) > 40 * 1024 * 1024:
        raise ValueError('Unexpected wheel payload size')
    for path in wheels:
        if path.is_symlink():
            raise ValueError('Wheel source must be a regular file')
        files['wheels/' + path.name] = path.read_bytes()
    for name, raw in files.items():
        if name.endswith('.py') or name == 'proxy/opencli-proxy':
            ast.parse(raw.decode('utf-8-sig'))
        if b'-----BEGIN OPENSSH PRIVATE KEY-----' in raw or b'-----BEGIN RSA PRIVATE KEY-----' in raw:
            raise ValueError('A private credential was found in the payload')
    manifest = {'format_version': 1, 'credentials_included': False,
                'files': {name: hashlib.sha256(data).hexdigest() for name, data in sorted(files.items())}}
    files['manifest.json'] = (json.dumps(manifest, indent=2) + '\n').encode()
    args.output.parent.mkdir(parents=True, exist_ok=True)
    with tarfile.open(args.output, 'w:gz') as bundle:
        for name, data in sorted(files.items()):
            member = tarfile.TarInfo(name)
            member.mode, member.size = 0o644, len(data)
            bundle.addfile(member, io.BytesIO(data))
    print(json.dumps({'path': str(args.output), 'bytes': args.output.stat().st_size,
                      'files': len(files), 'sha256': hashlib.sha256(args.output.read_bytes()).hexdigest(),
                      'credentials_included': False}))


if __name__ == '__main__':
    main()
