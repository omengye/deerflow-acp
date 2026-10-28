"""Run isolated model and readonly weather probes through the installed WS SDK.

BASE_SOURCE may provide the reviewed sibling verify-ws.py when run via stdin.
This verifier also tests the real per-turn configuration path from the retained
old model before dispatching the text probe. It never publishes toy audio.
"""
import pathlib
import sys

source = globals().get('BASE_SOURCE')
if source is None:
    source = (pathlib.Path(__file__).resolve().parents[1] / 'music-opencli/verify-ws.py').read_text(encoding='utf-8')
anchor = '                cancellation = threading.Event()\n'
replacement = '''                cancellation = threading.Event()
                if args.case == 'model_chat':
                    old = client.request('session/set_config_option',
                        {'sessionId': session_id, 'configId': 'model', 'value': 'gpt-6-luna'},
                        generation=generation, timeout=25)
                    client.remember_session(session_id, generation, old)
                    report['seed_model'] = client._load(session_id).model
                    from sdk.llm_sdk.deerflow import _configure
                    _configure(client, session_id, generation, old,
                        {'transport': 'ws', 'ws_mode': 'fullAccess', 'approval': 'allow_always',
                         'model': os.environ['DEERFLOW_MODEL'], 'connect_timeout': 25},
                        cancellation, time.monotonic() + 30)
                    report['configured_model'] = client._load(session_id).model
                    if report['seed_model'] != 'gpt-6-luna' or report['configured_model'] != os.environ['DEERFLOW_MODEL']:
                        raise ProbeError('per_turn_model_switch_not_confirmed')
'''
if source.count(anchor) != 1:
    raise RuntimeError('verifier source changed')
error_anchor = "report.update(status='failed', error_type=type(error).__name__)"
source = source.replace(error_anchor, error_anchor + "\n        report['safe_error'] = re.sub(r'https?://[^\\s]+', '[endpoint]', str(error).replace(os.environ.get('WAKU_DAEMON_TOKEN', '\\0'), '[REDACTED]'))[:300]")
namespace = {'__name__': 'model_probe_base', '__file__': globals().get('__file__', '/tmp/verify-model.py')}
exec(compile(source.replace(anchor, replacement), 'verify-ws.py', 'exec'), namespace)
namespace['CASES'] = ('model_chat', 'model_weather')
def prompt_for(case, media=None):
    if case == 'model_chat':
        return '请只回答“模型测试通过”，不调用任何工具。'
    if case == 'model_weather':
        return ('请实际调用网络天气或搜索工具查询明天北京的天气，给出具体日期、温度及降水情况，并标明实际来源。'
                '只做公开信息查询，不修改配置、不下载或播放文件。若查询失败，直接说明原因。')
    raise namespace['ProbeError']('unknown_model_probe')
namespace['prompt_for'] = prompt_for
if __name__ == '__main__':
    raise SystemExit(namespace['main']())
