"""Protocol 12 handshake and optional config apply; never submit a prompt."""
import json
import os
import time
import uuid


def connection(url):
    try:
        import websocket
    except ModuleNotFoundError:
        # FoloToy's existing image includes websockets 12, while the Gentoo
        # host has websocket-client. Both transports exercise the same probe.
        from websockets.sync.client import connect
        ws = connect(url, open_timeout=30, close_timeout=5,
                     compression=None, max_size=16 * 1024 * 1024)
        return ws, lambda: ws.recv(timeout=30)
    ws = websocket.create_connection(url, timeout=30, suppress_origin=True, http_no_proxy=['*'])
    return ws, ws.recv


def probe(token, url='ws://127.0.0.1:18787/v1', apply=False):
    ws, receive = connection(url)
    nil = str(uuid.UUID(int=0))
    try:
        ws.send(json.dumps({'type': 'hello', 'protocolVersion': 12, 'token': token,
                            'clientId': str(uuid.uuid4()), 'resumeFrom': []}))
        hello = json.loads(receive())
        assert hello.get('type') == 'hello' and hello.get('protocolVersion') == 12

        def request(command):
            identifier = str(uuid.uuid4())
            ws.send(json.dumps({'type': 'request', 'requestId': identifier,
                                'sessionId': nil, 'runtimeId': nil, 'command': command}))
            deadline = time.monotonic() + 40
            while time.monotonic() < deadline:
                reply = json.loads(receive())
                if reply.get('type') == 'response' and reply.get('requestId') == identifier:
                    assert reply['outcome']['status'] == 'ok', 'RPC failed'
                    return reply['outcome']['payload']
            raise TimeoutError('RPC timeout')

        assert request({'type': 'getSettings'})['type'] == 'settings'
        tasks = request({'type': 'loadTaskState'})
        assert tasks['type'] == 'taskState'
        report = {'passed': True, 'protocol': 12, 'sessions': len(tasks.get('sessions', [])),
                  'prompts_submitted': 0, 'config_applied': False}
        if apply:
            def status():
                return request({'type': 'deerFlow', 'operation': 'status', 'input': None})['data']
            before = status()
            request({'type': 'deerFlow', 'operation': 'apply', 'input': None})
            deadline = time.monotonic() + 60
            while time.monotonic() < deadline:
                after = status()
                assert not after.get('apply_error'), 'Configuration apply failed'
                if after.get('running') and not after.get('applying') and after.get('applied_generation', 0) > before.get('applied_generation', 0):
                    report['config_applied'] = True
                    break
                time.sleep(0.5)
            assert report['config_applied'], 'Configuration apply timeout'
        return report
    finally:
        ws.close()


if __name__ == '__main__':
    print(json.dumps(probe(os.environ['WAKU_DAEMON_TOKEN'], apply=True)))
