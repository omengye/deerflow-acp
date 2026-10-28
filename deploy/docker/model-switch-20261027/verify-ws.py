"""Isolated WS text/weather acceptance for any explicitly selected model.

Run inside the installed FoloToy container's /opt/folotoy directory. --run makes
real provider requests; the default dry run reads no environment or network.
The reviewed music-opencli/verify-ws.py is reused for bounded streaming, private
token handling, temporary state and cleanup. SOURCE_FILES may inject that base
under the key 'verify-ws.py', or BASE_SOURCE may contain its source text.
Native session/run and tool receipts still require a separate host-side audit.
No physical toy command or conversation mapping is created by this verifier.
"""
import argparse
import json
from pathlib import Path
import re


def base_source():
    if isinstance(globals().get('BASE_SOURCE'), str):
        return globals()['BASE_SOURCE']
    injected = globals().get('SOURCE_FILES')
    if injected is not None:
        if not isinstance(injected, dict):
            raise ValueError('reviewed_ws_verifier_source_missing')
        value = injected.get('verify-ws-base.py', injected.get('verify-ws.py'))
        if not isinstance(value, str):
            raise ValueError('reviewed_ws_verifier_source_missing')
        return value
    return (Path(__file__).resolve().parents[1] / 'music-opencli' / 'verify-ws.py').read_text(encoding='utf-8-sig')


def patch_once(source, old, new):
    if source.count(old) != 1:
        raise ValueError('reviewed_ws_verifier_anchor_changed')
    return source.replace(old, new)


def load_verifier(expected_model):
    source = base_source()
    source = patch_once(source,
        "'model': os.environ.get('DEERFLOW_MODEL') or None,",
        "'model': EXPECTED_MODEL,")
    source = patch_once(source,
        '                cancellation = threading.Event()\n',
        '''                cancellation = threading.Event()
                from sdk.llm_sdk.deerflow import _configure
                _configure(client, session_id, generation, info,
                    {'transport': 'ws', 'ws_mode': 'fullAccess', 'approval': 'allow_always',
                     'model': EXPECTED_MODEL, 'connect_timeout': 25},
                    cancellation, time.monotonic() + 30)
                report['configured_model'] = client._load(session_id).model
                if report['configured_model'] != EXPECTED_MODEL:
                    raise ProbeError('configured_model_not_confirmed')
''')
    source = patch_once(source,
        "    report['elapsed_ms'] = round((time.monotonic() - started) * 1000)\n",
        '''    report['elapsed_ms'] = round((time.monotonic() - started) * 1000)
    report['expected_model'] = EXPECTED_MODEL
    report['model_verified'] = (report.get('configured_model') == EXPECTED_MODEL
                                and report.get('server_model') == EXPECTED_MODEL)
    report['native_model_receipts_verified'] = False
    report['requires_native_model_audit'] = True
    report['production_session_database_opened'] = False
    report['physical_toy_audio_published'] = False
    if report['status'] == 'conversation_completed' and not report['model_verified']:
        report.update(status='failed', failure_code='server_model_mismatch')
    if args.case == 'text' and report.get('reply', '').strip() != 'MODEL_OK':
        report.update(status='failed', failure_code='deterministic_text_reply_mismatch')
    report['successful_tool_activity_observed'] = any(
        item.get('name') == 'skill_run' and item.get('complete') is True
        and item.get('failed') is False for item in report['actual_tool_activities'])
    if args.case == 'weather' and not report['successful_tool_activity_observed']:
        report.update(status='failed', failure_code='weather_execution_not_observed')
    report['ws_protocol_passed'] = bool(report['status'] == 'conversation_completed'
        and report['model_verified'] and report.get('cleanup', {}).get('passed'))
''')
    namespace = {'__name__': '_isolated_ws_model_verifier', '__file__': globals().get('__file__', '/tmp/verify-model-ws.py'),
                 'EXPECTED_MODEL': expected_model}
    exec(compile(source, 'reviewed-ws-verifier.py', 'exec'), namespace)
    namespace['CASES'] = ('text', 'weather')
    def prompt(case, media=None):
        if case == 'text':
            return '这是独立模型连通性测试。不要调用工具，只回复 MODEL_OK，不加标点或其他内容。'
        if case == 'weather':
            return ('请加载 weather-cma 技能，按技能说明使用 skill_run 实际查询明天北京的天气。'
                    '给出具体日期、温度和降水预报及来源。只做这一项公开天气查询，不修改配置、不下载、不播放音频。'
                    '工具失败应如实说明，不编造成功。')
        raise namespace['ProbeError']('unknown_model_verifier_case')
    namespace['prompt_for'] = prompt
    return namespace


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run', action='store_true')
    parser.add_argument('--expected-model', required=True)
    parser.add_argument('--case', choices=('text', 'weather'), default='text')
    parser.add_argument('--timeout', type=float, default=180)
    parser.add_argument('--report', type=Path)
    args = parser.parse_args(argv)
    if not re.fullmatch(r'[A-Za-z0-9_.:/-]{1,160}', args.expected_model):
        parser.error('expected-model must be a public model identifier without whitespace')
    if not 15 <= args.timeout <= 240:
        parser.error('timeout must be between 15 and 240 seconds')
    if not args.run:
        report = {'status': 'not_run', 'expected_model': args.expected_model, 'case': args.case,
                  'network_requests_made': False, 'physical_toy_audio_published': False}
        print(json.dumps(report), flush=True)
        return 0
    try:
        namespace = load_verifier(args.expected_model)
    except (OSError, ValueError, SyntaxError) as error:
        print(json.dumps({'status': 'failed', 'failure_code': 'reviewed_ws_verifier_unavailable',
                          'error_type': type(error).__name__}), flush=True)
        return 1
    forwarded = ['--case', args.case, '--timeout', str(args.timeout)]
    if args.report:
        forwarded += ['--report', str(args.report)]
    return namespace['main'](forwarded)


if __name__ == '__main__':
    raise SystemExit(main())
