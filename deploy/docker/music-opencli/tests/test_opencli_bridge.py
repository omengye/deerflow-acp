"""Offline fake-CLI tests for bounded execution, receipts and the proxy protocol."""
import contextlib
import importlib.machinery
import importlib.util
import io
import json
import os
from pathlib import Path
import signal
import sys
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]


def load(name, path):
    loader = importlib.machinery.SourceFileLoader(name, str(path))
    spec = importlib.util.spec_from_loader(name, loader)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    loader.exec_module(module)
    return module


bridge = load('opencli_bridge', ROOT / 'host-bridge/host_opencli_bridge.py')
proxy = load('opencli_proxy', ROOT / 'proxy/opencli-proxy')
TOKEN = 'test-token-' + 'x' * 40
FAKE = '''import json,pathlib,sys,time
a=sys.argv[1:]
if a==['--version']: print('1.8.6')
elif a[0]=='list': print(json.dumps({'commands':['web echo']}))
elif a[0]=='doctor': print('Browser extension connected')
elif len(a)>1 and a[1]=='slow': time.sleep(4); print('done')
elif len(a)>1 and a[1]=='burst': print('x'*20000)
elif len(a)>1 and a[1]=='fail': print('diagnostic',file=sys.stderr); sys.exit(7)
elif len(a)>1 and a[1]=='count':
 p=pathlib.Path(a[2]);p.write_text(str(int(p.read_text())+1) if p.exists() else '1');print('counted')
else: print(json.dumps(a))
'''


class BridgeTests(unittest.TestCase):
    def setUp(self):
        scratch = ROOT.parents[2] / '.tmp/music-opencli/bridge-tests'
        scratch.mkdir(parents=True, exist_ok=True)
        self.temporary = tempfile.TemporaryDirectory(prefix='opencli-bridge-test-', dir=scratch)
        self.root = Path(self.temporary.name)
        self.fake = self.root / 'fake.py'
        self.fake.write_text(FAKE)
        self.config_path = self.root / 'host.json'
        self.config_path.write_text(json.dumps({
            'token': TOKEN, 'executable': sys.executable, 'prefix': [str(self.fake)],
            'home': str(self.root), 'bind': '127.0.0.1', 'port': 0,
            'journal': str(self.root / 'jobs.sqlite3'), 'allowed_sites': ['web'],
            'allowed_cidrs': ['127.0.0.0/8'], 'output_limit_bytes': 2048,
            'max_timeout_seconds': 10, 'workers': 1, 'queue_size': 8,
        }))
        self.config_path.chmod(0o600)
        self.config = bridge.read_config(self.config_path)
        self.jobs = bridge.Jobs(self.config)
        self.server = bridge.Server(('127.0.0.1', 0), self.config, self.jobs)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.url = 'http://127.0.0.1:' + str(self.server.server_port)
        self.proxy_config = self.root / 'proxy.json'
        self.proxy_config.write_text(json.dumps({'url': self.url, 'token': TOKEN,
                                                'timeout_seconds': 2, 'client_timeout_seconds': 7}))
        self.proxy_config.chmod(0o600)
        self.signals = {s: signal.getsignal(s) for s in (signal.SIGINT, signal.SIGTERM)}

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)
        self.jobs.close()
        for number, handler in self.signals.items():
            signal.signal(number, handler)
        self.temporary.cleanup()

    def submit(self, argv, key=None, timeout=2):
        return self.jobs.submit({'idempotency_key': key or os.urandom(16).hex(),
                                 'argv': argv, 'timeout_seconds': timeout})[0]

    def finish(self, receipt):
        deadline = time.monotonic() + 6
        while time.monotonic() < deadline:
            receipt = self.jobs.get(receipt['id'])
            if receipt['state'] in bridge.TERMINAL:
                return receipt
            time.sleep(0.025)
        self.fail('fake job did not terminate')

    def http(self, method, path, data=None, token=TOKEN):
        raw = json.dumps(data).encode() if data is not None else None
        req = urllib.request.Request(self.url + path, data=raw, method=method,
                                     headers={'Authorization': 'Bearer ' + token})
        with urllib.request.urlopen(req, timeout=2) as response:
            return response.status, json.load(response)

    def test_auth_and_source_cidr_fail_before_execution(self):
        with self.assertRaises(urllib.error.HTTPError) as error:
            self.http('GET', '/health', token='wrong')
        self.assertEqual(error.exception.code, 401)
        self.config['_networks'] = []
        with self.assertRaises(urllib.error.HTTPError) as error:
            self.http('GET', '/health')
        self.assertEqual(error.exception.code, 403)
        self.assertEqual(self.jobs.db.execute('SELECT count(*) FROM jobs').fetchone()[0], 0)

    def test_only_fixed_cli_surfaces_and_sites_are_accepted(self):
        for argv in (['daemon', 'stop'], ['plugin', 'install', 'anything'], ['other', 'search'],
                     ['doctor', '--arbitrary'], ['list', '--arbitrary']):
            with self.assertRaises(bridge.RequestError):
                self.submit(argv)
        for argv in (['--version'], ['list', '--format', 'json'], ['doctor'],
                     ['web', '--help'], ['web', 'search', '--help'], ['web', 'search', 'word']):
            self.assertEqual(self.finish(self.submit(argv))['state'], 'completed')

    def test_literal_argv_never_uses_shell(self):
        marker = self.root / 'SHOULD_NOT_EXIST'
        arg = '空 格; $(touch ' + str(marker) + ') & "quotes"'
        receipt = self.finish(self.submit(['web', 'echo', arg]))
        self.assertEqual(json.loads(receipt['stdout']), ['web', 'echo', arg])
        self.assertFalse(marker.exists())
        self.assertTrue(receipt['termination_confirmed'])

    def test_idempotency_survives_completion_and_rejects_conflict(self):
        marker = self.root / 'counter'
        key = 'same-request-key-123456'
        first = self.submit(['web', 'count', str(marker)], key)
        self.finish(first)
        again, created = self.jobs.submit({'idempotency_key': key, 'argv': ['web', 'count', str(marker)], 'timeout_seconds': 2})
        self.assertFalse(created)
        self.assertEqual(first['id'], again['id'])
        self.assertEqual(marker.read_text(), '1')
        with self.assertRaises(bridge.RequestError) as error:
            self.submit(['web', 'echo', 'different'], key)
        self.assertEqual(error.exception.status, 409)

    def test_restart_marks_interrupted_job_unknown_without_replay(self):
        marker = self.root / 'counter'
        receipt = self.finish(self.submit(['web', 'count', str(marker)]))
        receipt['state'] = 'running'
        with self.jobs.lock:
            self.jobs._save(receipt)
        self.jobs.close()
        self.jobs = bridge.Jobs(self.config)
        recovered = self.jobs.get(receipt['id'])
        self.assertEqual(recovered['state'], 'unknown')
        self.assertFalse(recovered['termination_confirmed'])
        self.assertEqual(marker.read_text(), '1')

    def test_timeout_and_output_limit_produce_bounded_receipts(self):
        receipt = self.finish(self.submit(['web', 'slow'], timeout=1))
        self.assertEqual(receipt['state'], 'timed_out')
        self.assertTrue(receipt['termination_confirmed'])
        receipt = self.finish(self.submit(['web', 'burst']))
        self.assertEqual(receipt['state'], 'output_limit')
        self.assertLessEqual(len(receipt['stdout'].encode()), self.config['output_limit_bytes'])
        self.assertTrue(receipt['stdout_truncated'])

    def test_http_cancel_queued_job_never_executes(self):
        first = self.submit(['web', 'slow'], timeout=5)
        marker = self.root / 'counter'
        queued = self.submit(['web', 'count', str(marker)])
        status, cancelled = self.http('DELETE', '/v1/opencli/jobs/' + queued['id'])
        self.assertEqual(status, 200)
        self.assertEqual(cancelled['state'], 'cancelled')
        self.assertTrue(cancelled['termination_confirmed'])
        self.jobs.cancel(first['id'])
        self.finish(first)
        self.assertFalse(marker.exists())

    def test_proxy_transparently_preserves_stdout_and_failure_exit(self):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = proxy.invoke(['--version'], self.proxy_config)
        self.assertEqual(code, 0)
        self.assertEqual(out.getvalue(), '1.8.6' + os.linesep)
        self.assertEqual(err.getvalue(), '')
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = proxy.invoke(['web', 'fail'], self.proxy_config)
        self.assertEqual(code, 7)
        self.assertIn('diagnostic', err.getvalue())

    def test_proxy_retries_lost_submission_only_with_the_same_key(self):
        marker = self.root / 'counter'
        original = proxy.request
        submissions = []
        def lost(config, method, path, value=None):
            result = original(config, method, path, value)
            if method == 'POST':
                submissions.append(dict(value))
                if len(submissions) == 1:
                    raise proxy.BridgeError('simulated_lost_response')
            return result
        with patch.object(proxy, 'request', lost), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(proxy.invoke(['web', 'count', str(marker)], self.proxy_config), 0)
        self.assertEqual(submissions[0], submissions[1])
        self.assertEqual(marker.read_text(), '1')

    def test_proxy_unavailable_never_prints_token(self):
        out, err = io.StringIO(), io.StringIO()
        with patch.object(proxy, 'request', side_effect=proxy.BridgeError('offline')):
            with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
                code = proxy.invoke(['web', 'search', 'hello'], self.proxy_config)
        self.assertEqual(code, 125)
        result = json.loads(err.getvalue())
        self.assertFalse(result['host_termination_confirmed'])
        self.assertEqual(result['host_state'], 'unknown')
        self.assertNotIn(TOKEN, out.getvalue() + err.getvalue())


if __name__ == '__main__':
    unittest.main()
