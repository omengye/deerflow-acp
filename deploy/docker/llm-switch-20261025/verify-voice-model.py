"""Add confirmed LLM model evidence to the existing isolated ASR2 voice probe.

All original CLI arguments, ASR receipt auditing and synthetic actor cleanup
remain in verify-rate-voice.py. Only role 1 runs the complete voice pipeline.
The first --run still leaves passed=false until --audit-main-log succeeds.

An orchestrator may inject SOURCE_FILES={'verify-rate-voice.py': source_text}
when this wrapper runs inside the FoloToy container. Otherwise the sibling
asr2-audiofix-20261025 source is read locally. No production source is changed.
"""

import argparse
from contextlib import closing
import json
from pathlib import Path
import re
import sqlite3
import sys


DEFAULT_MODEL = 'gemini-3.8-flash-high'
BASE_NAME = 'verify-rate-voice.py'


def load_base():
    injected = globals().get('SOURCE_FILES')
    source_path = (Path(globals().get('__file__', str(Path.cwd() / 'verify-voice-model.py')))
                   .resolve().parent.parent / 'asr2-audiofix-20261025' / BASE_NAME)
    if injected is not None:
        if not isinstance(injected, dict) or not isinstance(injected.get(BASE_NAME), str):
            raise ValueError('original_voice_verifier_source_missing')
        source = injected[BASE_NAME]
    else:
        source = source_path.read_text(encoding='utf-8-sig')
    namespace = {'__name__': '_rate_voice_model_base', '__file__': str(source_path)}
    exec(compile(source, str(source_path), 'exec'), namespace)
    return namespace


def model_evidence_valid(report, expected_model):
    session = report.get('waku_session_id')
    return bool(
        report.get('role') == 1
        and report.get('expected_model') == expected_model
        and report.get('effective_role_model') == expected_model
        and report.get('saved_ws_model') == expected_model
        and isinstance(session, str) and session
        and report.get('saved_ws_model_session_id') == session
        and report.get('conversation_state') == 'completed'
        and report.get('pipeline_completed') is True
    )


def add_model_checks(namespace, expected_model):
    original_run = namespace['run']
    original_state = namespace['state_for_device']
    original_audit = namespace['audit_main_log']
    failure = namespace['VoiceFailure']
    active_report = [None]

    def state_with_model(path, device):
        state = original_state(path, device)
        if not state or not state.get('session_id'):
            return state
        if not re.fullmatch(r'ASR2TEST[0-9A-F]{16}', device):
            raise failure('model_evidence_device_not_synthetic')
        location = Path(path).expanduser().resolve()
        try:
            with closing(sqlite3.connect(location.as_uri() + '?mode=ro', uri=True, timeout=1)) as database:
                rows = database.execute(
                    'SELECT model FROM deerflow_ws_sessions WHERE session_id=?',
                    (state['session_id'],)).fetchall()
        except sqlite3.OperationalError:
            return state
        if len(rows) == 1 and isinstance(rows[0][0], str):
            state = {**state, 'saved_ws_model': rows[0][0]}
            report = active_report[0]
            if report is not None and state.get('status') == 'completed':
                report.update(saved_ws_model=rows[0][0],
                              saved_ws_model_session_id=state['session_id'])
        return state

    def run_with_model(args, report):
        report.update(expected_model=expected_model, llm_model_verified=False)
        if args.role != 1:
            raise failure('llm_voice_model_verification_requires_role1')
        # Match the original probe's source discovery, before importing Config.
        namespace['logging'].disable(namespace['logging'].CRITICAL)
        project = namespace['PROJECT']
        source = next((path for path in (project / 'src', project, Path.cwd())
                       if (path / 'config.py').is_file()), project)
        sys.path.insert(0, str(source))
        from config import Config
        from util.config_util import get_role_llm_config
        config = Config()
        role = config.ROLES.get(str(args.role), {})
        if role.get('llm_type', config.LLM_TYPE) != 'deerflow':
            raise failure('device_default_role_is_not_deerflow')
        selected = get_role_llm_config('deerflow', role, config)
        report['effective_role_model'] = selected.get('model')
        if report['effective_role_model'] != expected_model:
            raise failure('effective_role_model_does_not_match_expected')
        active_report[0] = report
        try:
            original_run(args, report)
            report['llm_model_verified'] = model_evidence_valid(report, expected_model)
            if not report['llm_model_verified']:
                raise failure('confirmed_ws_model_evidence_missing_or_mismatched')
        finally:
            active_report[0] = None

    def audit_with_model(report, log_text):
        result = original_audit(report, log_text)
        verified = model_evidence_valid(result, expected_model)
        result['llm_model_verified'] = verified
        result['passed'] = result.get('passed') is True and verified
        if not verified:
            result.update(status='failed', failure_code='confirmed_ws_model_evidence_missing_or_mismatched')
        return result

    namespace.update(run=run_with_model, state_for_device=state_with_model,
                     audit_main_log=audit_with_model)
    return namespace


def main(argv=None):
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument('--expected-model', default=DEFAULT_MODEL)
    args, forwarded = parser.parse_known_args(argv)
    if not re.fullmatch(r'[A-Za-z0-9_.:/-]{1,160}', args.expected_model):
        parser.error('expected-model must be a public model identifier')
    try:
        namespace = add_model_checks(load_base(), args.expected_model)
    except (OSError, ValueError, SyntaxError) as error:
        print(json.dumps({'passed': False, 'status': 'failed',
                          'failure_code': 'original_voice_verifier_source_unavailable',
                          'error_type': type(error).__name__}), flush=True)
        return 1
    last_report = [None]
    original_emit = namespace['emit']

    def emit_and_remember(value):
        if isinstance(value, dict) and 'status' in value:
            last_report[0] = value
        original_emit(value)

    namespace['emit'] = emit_and_remember
    code = namespace['main'](forwarded)
    # The original allows exit 0 for a completed pipeline awaiting ASR audit.
    # A model mismatch after that pipeline must still exit nonzero.
    if last_report[0] is not None and last_report[0].get('status') == 'failed':
        return 1
    return code


if __name__ == '__main__':
    raise SystemExit(main())
