"""Create a public-source-only deployment bundle; runtime keys use stdin."""
import argparse
import ast
import base64
import hashlib
import json
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    root = Path(__file__).resolve().parent
    manifest = json.loads((root/'folotoy-source/manifest.json').read_text(encoding='utf-8'))
    names = ['deploy.py', 'rollback.py', 'Dockerfile.folotoy', 'folotoy-source/manifest.json']
    for record in manifest['files']:
        name = 'folotoy-source/' + record['path']
        if not name.startswith('folotoy-source/src/') or '..' in Path(name).parts:
            raise ValueError('Invalid production path')
        raw = (root/name).read_bytes()
        if hashlib.sha256(raw).hexdigest() != record['after_sha256']:
            raise ValueError('Source changed after validation')
        names.append(name)
    files = {}
    for name in names:
        path = root/name
        if path.is_symlink():
            raise ValueError('No links in deployment bundle')
        raw = path.read_bytes()
        if name.endswith('.py'):
            ast.parse(raw.decode('utf-8-sig'))
        if b'-----BEGIN OPENSSH PRIVATE KEY' in raw or b'-----BEGIN RSA PRIVATE KEY' in raw:
            raise ValueError('Unexpected private credential in public source')
        files[name] = {'sha256': hashlib.sha256(raw).hexdigest(),
                       'base64': base64.b64encode(raw).decode()}
    value = {'format_version': 1, 'credentials_included': False, 'files': files}
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(value), encoding='utf-8')
    print(json.dumps({'files': len(files), 'bytes': args.output.stat().st_size,
                      'credentials_included': False,
                      'sha256': hashlib.sha256(args.output.read_bytes()).hexdigest()}))


if __name__ == '__main__':
    main()
