import concurrent.futures
import logging
import os
import time
import uuid
import wave

from config import Config
from core.conversation_control import get_conversation_control
from core.folotoy_app import FolotoyApp
from util.config_util import (
    get_role_llm_config,
    get_role_stt_config,
    get_role_tts_config,
)

LOGGER = logging.getLogger(__name__)

default_config = Config()

__test__ = {}


class SpeechWavProcessor:
    CHANNELS = 1
    SAMPLE_WIDTH = 2
    FIRST_RATE = 32000
    SECOND_RATE = 16000
    THIRD_RATE = 8000

    VOICE_GENERATED_RESULT_CODE_SUCCESS = 1
    VOICE_GENERATED_RESULT_CODE_EMPTY_FRAME = 1001
    VOICE_GENERATED_RESULT_CODE_AUDIO_TOO_SHORT = 1002
    VOICE_GENERATED_RESULT_CODE_EMPTY_TRANSCRIPT = 1003
    VOICE_GENERATED_RESULT_CODE_ROLE_CONFIG_ERROR = 1004
    VOICE_GENERATED_RESULT_CODE_FAILED = 1099

    def __init__(self, folotoy_app: 'FolotoyApp'):
        self.folotoy_app = folotoy_app
        self.config = folotoy_app.config
        self.stt_sdk = folotoy_app.stt_sdk
        self.tts_sdk = folotoy_app.tts_sdk
        self.chat_sdk = folotoy_app.llm_sdk
        self.voice_executor = concurrent.futures.ThreadPoolExecutor(
            max_workers=self.config.VOICE_EXECUTOR_MAX_WORKERS)

    def write_wav(self, frames, token, recording_id):
        session = self.folotoy_app.sessions.get(token)
        if session is None:
            raise KeyError(token)
        dkey = session['dkey']

        audio_data = b''.join(frames)
        if not audio_data:
            return None, False

        recording_dir = self.config.AUDIO_SAVE_PATH
        recording_filename = 'recording-%s.wav' % uuid.uuid4().hex
        file_path = '%s/%s' % (recording_dir, recording_filename)

        wf = wave.open(file_path, 'wb')
        device = self.folotoy_app.devices[dkey]
        rate = session['rate']
        audio_duration_seconds = len(audio_data) / (rate * self.SAMPLE_WIDTH)

        with wf:
            wf.setnchannels(self.CHANNELS)
            wf.setsampwidth(self.SAMPLE_WIDTH)
            wf.setframerate(rate)
            wf.writeframes(audio_data)

        recording_valid = True
        if audio_duration_seconds < 0.1:
            LOGGER.warning(
                'Dkey %s: Audio file is too short. Minimum audio length is 0.1'
                ' seconds. Now is %s seconds', dkey, audio_duration_seconds)
            recording_valid = False

        return recording_filename, recording_valid

    def call_stt(self, dkey, role, recording_id, recording_filename, file_path,
                 stt_type=None, stt_config=None):
        start_time = time.time()
        transcript = self.stt_sdk.transcribe(file_path, stt_type=stt_type,
                                             stt_config=stt_config)
        LOGGER.info('Dkey=%s: Transcribed:%s', dkey, transcript)
        response_time = time.time() - start_time
        # [FIX] STT 失败时返回 None，原实现直接 .get 会 AttributeError，
        # 导致 recording_executor 线程静默吞掉异常、设备收不到任何回应。
        if transcript is None:
            return None, None
        return transcript, transcript.get('text')

    def call_llm(self, dkey, role, recording_id, messages, llm_type=None,
                 llm_config=None, tts_type=None, tts_config=None,
                 conversation_id=None, conversation_round=None):
        start_time = time.time()
        chunks = self.chat_sdk.chat(
            messages=messages, llm_type=llm_type, llm_config=llm_config,
            stream=True, conversation_id=conversation_id,
            conversation_round=conversation_round, dkey=dkey, role=role)
        sentences = []
        extra_data = {}
        content = ''
        for chunk in chunks:
            chunk_type = chunk['type']
            if chunk_type == 'sentence':
                sentence = chunk['content']
                order = chunk['order']
                extra_data = chunk['extra_data']
                sentences.append(sentence)
                self.call_tts(dkey, tts_type, tts_config, recording_id,
                              sentence, order, role)
                content += sentence
        response_time = time.time() - start_time
        return content, extra_data

    def call_tts(self, dkey, tts_type, tts_config, recording_id, content, order, role):
        status, wav_name = self.tts_sdk.text_to_sound(content, tts_type, tts_config)
        if status:
            url = '%s/%s' % (self.config.AUDIO_DOWNLOAD_URL, wav_name)
            self.send_url(dkey, recording_id, order, url)

    def send_audio_play_cmd_msg(self, dkey, recording_id, result_code=0, total=0):
        msg = {
            'msgId': self.folotoy_app.get_and_increase_msg_id(dkey),
            'identifier': 'audioplay_cmd',
            'inputParams': {
                'recordingId': recording_id,
                'total': total,
            },
        }
        self.folotoy_app.command_call(dkey, msg)

    def send_url(self, dkey, recording_id, order, url):
        msg = {
            'msgId': self.folotoy_app.get_and_increase_msg_id(dkey),
            'identifier': 'audioplay',
            'inputParams': {
                'recordingId': recording_id,
                'order': order,
                'url': url,
            },
        }
        self.folotoy_app.command_call(dkey, msg)

    def make_failed_answer_voice(self, token, dkey, tts_provider_type, tts_config,
                                 recording_id, failed_answer_text, role):
        role_failed_answer_voice = self.config.ROLES.get(str(role), {}).get(
            'failed_answer_voice')
        failed_answer_voice = role_failed_answer_voice or failed_answer_text
        status, wav_name = self.tts_sdk.text_to_sound(
            failed_answer_voice, tts_provider_type, tts_config)
        if status:
            url = '%s/%s' % (self.config.AUDIO_DOWNLOAD_URL, wav_name)
            self.send_url(dkey, recording_id, 1, url)
        self.send_audio_play_cmd_msg(dkey, recording_id, 0, 1)

    def _is_current(self, turn):
        device = self.folotoy_app.devices.get(turn.dkey, {})
        return (device.get('token') == turn.token
                and turn.token in self.folotoy_app.sessions
                and get_conversation_control(self.folotoy_app).is_current(turn))

    def _apply_current(self, turn, action):
        def guarded_action():
            if self.folotoy_app.devices.get(turn.dkey, {}).get('token') == turn.token:
                action()
        return get_conversation_control(self.folotoy_app).apply(turn, guarded_action)

    def _save_deerflow_metadata(self, turn, chunk):
        extra_data = chunk.get('extra_data') or {}
        conversation_id = extra_data.get('conversation_id')
        if conversation_id:
            def save():
                session = self.folotoy_app.sessions.get(turn.token)
                if session is not None:
                    session.setdefault('conversation_ids', {})[turn.role] = conversation_id
            self._apply_current(turn, save)

    def _send_deerflow_sentence(self, turn, content, tts_type, tts_config):
        if not content or not self._is_current(turn):
            return False
        status, wav_name = self.tts_sdk.text_to_sound(content, tts_type, tts_config)
        if not status or not wav_name or not self._is_current(turn):
            return False
        url = '%s/%s' % (self.config.AUDIO_DOWNLOAD_URL, wav_name)
        sent = []
        def send():
            if turn.terminal_sent:
                return
            self.send_url(turn.dkey, turn.recording_id, turn.sent_count + 1, url)
            turn.sent_count += 1
            sent.append(True)
        self._apply_current(turn, send)
        return bool(sent)

    def _finish_deerflow(self, turn):
        music = getattr(self.folotoy_app, 'music_playback', None)
        if music is not None:
            music.finish_turn(turn, self.send_url)
        def finish():
            if not turn.terminal_sent:
                self.send_audio_play_cmd_msg(
                    turn.dkey, turn.recording_id, total=turn.sent_count)
                turn.terminal_sent = True
        self._apply_current(turn, finish)

    def _fail_deerflow(self, turn, role_config, tts_type, tts_config):
        if not self._is_current(turn):
            return
        if turn.sent_count == 0:
            text = (role_config.get('failed_answer_voice')
                    or getattr(self.config, 'FAILED_ANSWER_TEXT', None)
                    or '暂时无法回答，请稍后再试。')
            try:
                self._send_deerflow_sentence(turn, text, tts_type, tts_config)
            except Exception as exc:
                LOGGER.warning('Failure voice failed for device %s (%s)',
                               turn.dkey, type(exc).__name__)
        self._finish_deerflow(turn)

    def _do_deerflow_task(self, token, recording_valid, recording_id,
                          recording_filename, role, role_config, turn):
        control = get_conversation_control(self.folotoy_app)
        session = self.folotoy_app.sessions.get(token)
        if session is None:
            return
        dkey = session.get('dkey')
        if turn is None:
            # Direct callers retain a useful entry point; the UDP server always
            # captures the turn before queueing work so stale jobs cannot start.
            turn = control.begin(dkey, role, token, recording_id)
        if not self._is_current(turn) or not control.claim(turn):
            return
        tts_type = role_config.get('tts_type', self.config.TTS_TYPE)
        tts_config = None
        chunks = None
        try:
            tts_config = get_role_tts_config(tts_type, role_config, self.config)
            stt_type = role_config.get('stt_type', self.config.STT_TYPE)
            stt_config = get_role_stt_config(stt_type, role_config, self.config)
            llm_config = get_role_llm_config('deerflow', role_config, self.config)
            if getattr(self.folotoy_app, 'music_playback', None) is not None:
                llm_config = dict(llm_config)
                llm_config['_music_voice_context'] = {
                    'device_key': dkey, 'request_id': turn.request_id,
                    'role': str(role),
                }
            if not recording_valid or not recording_filename:
                self._fail_deerflow(turn, role_config, tts_type, tts_config)
                return
            if not self._is_current(turn):
                return
            file_path = os.path.join(self.config.AUDIO_SAVE_PATH, recording_filename)
            _, text = self.call_stt(dkey, role, recording_id, recording_filename,
                                    file_path, stt_type, stt_config)
            if not self._is_current(turn):
                return
            if not text:
                self._fail_deerflow(turn, role_config, tts_type, tts_config)
                return

            # DeerFlow owns its thread history. Send only this user turn.
            conversation_id = session.get('conversation_ids', {}).get(role)
            conversation_round = session.get('conversation_rounds', {}).get(role, 0)
            chunks = self.chat_sdk.chat(
                messages=[{'role': 'user', 'content': text}],
                llm_type='deerflow', llm_config=llm_config, stream=True,
                conversation_id=conversation_id,
                conversation_round=conversation_round, dkey=dkey, role=role,
                cancel_event=turn.cancel_event, request_id=turn.request_id)
            content = []
            for chunk in chunks:
                if not self._is_current(turn):
                    return
                self._save_deerflow_metadata(turn, chunk)
                if chunk.get('type') != 'sentence' or chunk.get('reasoning'):
                    continue
                sentence = chunk.get('content') or ''
                if sentence:
                    content.append(sentence)
                    self._send_deerflow_sentence(turn, sentence, tts_type, tts_config)
            if not self._is_current(turn):
                return
            if not content or turn.sent_count == 0:
                self._fail_deerflow(turn, role_config, tts_type, tts_config)
                return

            def save_history():
                current_session = self.folotoy_app.sessions.get(token)
                if current_session is None:
                    return
                history = list(current_session.get('messages', {}).get(role, []))
                history.extend([{'role': 'user', 'content': text},
                                {'role': 'assistant', 'content': ''.join(content)}])
                # Bounded local display/audit cache, never replayed to DeerFlow.
                limit = max(2, int(role_config.get('max_message_count', 20) or 20))
                current_session.setdefault('messages', {})[role] = history[-limit:]
                current_session.setdefault('conversation_rounds', {})[role] = conversation_round + 1
            self._apply_current(turn, save_history)
            self._finish_deerflow(turn)
        except Exception as exc:
            # Lazy import: the core can still run without loading the optional
            # DeerFlow transport when another provider is selected.
            from sdk.llm_sdk.deerflow import DeerFlowCancelled, DeerFlowError
            if isinstance(exc, DeerFlowCancelled) or not self._is_current(turn):
                return
            kind = 'provider' if isinstance(exc, DeerFlowError) else 'pipeline'
            LOGGER.error('DeerFlow %s failed for device %s request %s (%s)',
                         kind, dkey, turn.request_id, type(exc).__name__)
            self._fail_deerflow(turn, role_config, tts_type, tts_config)
        finally:
            close = getattr(chunks, 'close', None)
            if close is not None:
                try:
                    close()
                except Exception:
                    LOGGER.debug('Closing DeerFlow voice iterator failed')

    def do_task(self, token, recording_valid, recording_id, recording_filename,
                turn=None):
        session = self.folotoy_app.sessions.get(token, {})
        dkey = session.get('dkey')
        device = self.folotoy_app.devices.get(dkey, {})
        role = session.get('role', '1')
        selected_role = str(device.get('role', role))
        selected_config = self.config.ROLES.get(selected_role, {})
        if selected_config.get('llm_type', self.config.LLM_TYPE) == 'deerflow':
            return self._do_deerflow_task(
                token, recording_valid, recording_id, recording_filename,
                selected_role, selected_config, turn)
        role_config = self.config.ROLES.get(str(role), {})

        stt_type = role_config.get('stt_type', self.config.STT_TYPE)
        llm_type = role_config.get('llm_type', self.config.LLM_TYPE)
        tts_type = role_config.get('tts_type', self.config.TTS_TYPE)

        stt_config = get_role_stt_config(stt_type, role_config, self.config)
        llm_config = get_role_llm_config(llm_type, role_config, self.config)
        tts_config = get_role_tts_config(tts_type, role_config, self.config)

        failed_answer_text = role_config.get('failed_answer_voice')

        if not recording_valid:
            self.make_failed_answer_voice(token, dkey, tts_type, tts_config,
                                          recording_id, failed_answer_text, role)
            return

        file_path = os.path.join(self.config.AUDIO_SAVE_PATH, recording_filename)

        transcribed_result, transcribed_text = self.call_stt(
            dkey, role, recording_id, recording_filename, file_path,
            stt_type, stt_config)
        if not transcribed_text:
            LOGGER.info('Dkey %s: Transcript is empty', dkey)
            self.make_failed_answer_voice(token, dkey, tts_type, tts_config,
                                          recording_id, failed_answer_text, role)
            return

        role_key = str(role)
        messages = session.get('messages', {}).get(role_key, [])
        llm_messages = list(messages)
        llm_messages.append({'role': 'user', 'content': transcribed_text})

        conversation_id = session.get('conversation_ids', {}).get(role_key)
        conversation_round = session.get('conversation_rounds', {}).get(role_key, 0)

        assistant_content, extra_data = self.call_llm(
            dkey, role, recording_id, llm_messages, llm_type, llm_config,
            tts_type, tts_config, conversation_id, conversation_round)

        llm_messages.append({'role': 'assistant', 'content': assistant_content})
        session.setdefault('messages', {})[role_key] = llm_messages
        session.setdefault('conversation_rounds', {})[role_key] = \
            conversation_round + 1
        session.setdefault('conversation_ids', {})[role_key] = \
            extra_data.get('conversation_id') if isinstance(extra_data, dict) else None
