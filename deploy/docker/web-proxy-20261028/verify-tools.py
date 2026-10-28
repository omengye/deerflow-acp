"""Isolated WS acceptance for the three native web tools after proxy setup.

Run inside FoloToy's /opt/folotoy directory. --run makes real model/tool
requests. Without it, no environment, SDK or network is accessed. The reviewed
music-opencli/verify-ws.py supplies streaming, private-token handling, a temporary
state DB and actor cleanup. Inject BASE_SOURCE or SOURCE_FILES['verify-ws-base.py']
when running via stdin. Native model/tool receipts require parent-side auditing.
No physical toy input, command, audio playback or production identity is used.
"""
import argparse
import json
import os
from pathlib import Path
import re


CASES = ('web_search', 'web_fetch', 'image_search')
FORBIDDEN = ('skill', 'skill_run', 'host_opencli', 'bash', 'execute', 'run_command',
             'write_file', 'delete_path', 'skill_edit', 'task', 'delegate')


def base_source():
    value = globals().get('BASE_SOURCE')
    if isinstance(value, str):
        return value
    injected = globals().get('SOURCE_FILES')
    if injected is not None:
        if not isinstance(injected, dict) or not isinstance(injected.get('verify-ws-base.py'), str):
            raise ValueError('reviewed_ws_base_source_missing')
        return injected['verify-ws-base.py']
    return (Path(__file__).resolve().parents[1] / 'music-opencli/verify-ws.py').read_text(encoding='utf-8-sig')


def patch_once(source, old, new):
    if source.count(old) != 1:
        raise ValueError('reviewed_ws_base_source_changed')
    return source.replace(old, new)


def check_report(report, expected_model):
    """Reject an answer without an observed successful native tool activity."""
    required = report['case']
    activities = report.get('actual_tool_activities', [])
    observed = any(item.get('name') == required and item.get('complete') is True
                   and item.get('failed') is False for item in activities)
    forbidden = sorted({item.get('name') for item in activities
                        if item.get('name') in FORBIDDEN
                        or item.get('name') in CASES and item.get('name') != required})
    model_ok = report.get('configured_model') == expected_model and report.get('server_model') == expected_model
    report.update(expected_model=expected_model, model_verified=model_ok,
                  required_native_tool=required,
                  required_tool_activity_observed=observed,
                  forbidden_tool_activities=forbidden,
                  native_model_receipts_verified=False,
                  native_tool_receipts_verified=False,
                  requires_native_model_audit=True,
                  requires_native_tool_audit=True,
                  production_session_database_opened=False,
                  physical_toy_audio_published=False)
    if report.get('status') == 'conversation_completed':
        if not model_ok:
            report.update(status='failed', failure_code='runtime_model_mismatch')
        elif forbidden:
            report.update(status='failed', failure_code='substitute_or_unexpected_tool_used')
        elif not observed:
            report.update(status='failed', failure_code='required_native_tool_success_not_observed')
    report['ws_protocol_passed'] = bool(report.get('status') == 'conversation_completed'
                                        and model_ok and observed and not forbidden
                                        and report.get('cleanup', {}).get('passed') is True)
    return report


def prompt_for(case, media=None):
    common = ('这是独立管理验收。只调用本轮指定的内置工具完成一项只读测试。'
              '不要用 skill、skill_run、bash、浏览器或其他网络工具代替；不要读取配置或凭据，'
              '不要下载文件、修改配置或播放音频。根据真实工具结果简短回答，失败时如实说明原因。')
    if case == 'web_search':
        return common + '请调用内置 web_search，query="Linux documentation"，max_results=1；返回第一项标题和来源链接。'
    if case == 'web_fetch':
        return common + '请调用内置 web_fetch，url="https://example.com"；返回实际页面标题和一句内容摘要。'
    if case == 'image_search':
        return common + '请调用内置 image_search，query="red panda"，max_results=1；返回第一项标题和完整 image_url，不下载图片。'
    raise ValueError('unsupported_native_web_tool')


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
                        "    CHECK_REPORT(report, EXPECTED_MODEL)\n"
                        "    report['elapsed_ms'] = round((time.monotonic() - started) * 1000)\n")
    namespace = {'__name__': '_native_web_proxy_ws_base',
                 '__file__': globals().get('__file__', '/tmp/verify-web-proxy-tools.py'),
                 'EXPECTED_MODEL': expected_model, 'CHECK_REPORT': check_report}
    exec(compile(source, 'reviewed-ws-base.py', 'exec'), namespace)
    namespace.update(CASES=CASES, prompt_for=prompt_for)
    return namespace


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run', action='store_true')
    parser.add_argument('--case', required=True, choices=CASES)
    parser.add_argument('--expected-model', help='Optional assertion against installed DEERFLOW_MODEL.')
    parser.add_argument('--timeout', type=float, default=180)
    parser.add_argument('--report', type=Path)
    args = parser.parse_args(argv)
    if not 15 <= args.timeout <= 240:
        parser.error('timeout must be between 15 and 240 seconds')
    if not args.run:
        print(json.dumps({'status': 'not_run', 'case': args.case,
                          'expected_model': args.expected_model,
                          'network_requests_made': False,
                          'physical_toy_audio_published': False}), flush=True)
        return 0
    selected = os.environ.get('DEERFLOW_MODEL', '').strip()
    if not re.fullmatch(r'[A-Za-z0-9_.:/-]{1,160}', selected):
        print(json.dumps({'status': 'failed', 'failure_code': 'installed_model_not_configured'}), flush=True)
        return 1
    if args.expected_model and args.expected_model != selected:
        print(json.dumps({'status': 'failed', 'failure_code': 'installed_model_mismatch'}), flush=True)
        return 1
    try:
        namespace = load_verifier(selected)
    except (OSError, ValueError, SyntaxError) as error:
        print(json.dumps({'status': 'failed', 'failure_code': 'reviewed_ws_base_unavailable',
                          'error_type': type(error).__name__}), flush=True)
        return 1
    forwarded = ['--case', args.case, '--timeout', str(args.timeout)]
    if args.report:
        forwarded += ['--report', str(args.report)]
    return namespace['main'](forwarded)


if __name__ == '__main__':
    raise SystemExit(main())
