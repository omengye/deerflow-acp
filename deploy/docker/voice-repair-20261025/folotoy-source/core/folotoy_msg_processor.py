import logging
from contextlib import nullcontext
import uuid

from core.folotoy_app import FolotoyApp
from core.conversation_control import get_conversation_control
import util.file_util as file_util
from util.config_util import get_role_llm_config, get_role_tts_config, merge_config
from util.format_util import format_m1_card_code, format_rtu_card_code

LOGGER = logging.getLogger(__name__)

__test__ = {}


class FolotoyMsgProcessor:
    def __init__(self, folotoy_app: 'FolotoyApp'):
        self.folotoy_app = folotoy_app
        self.config = folotoy_app.config
        self.chat_sdk = None
        self.tts_sdk = None
        self.stt_sdk = None

    def get_tts_config(self, dkey, role):
        role_config = self.config.ROLES.get(str(role))
        if role_config is None:
            return None
        tts_type = role_config.get('tts_type', self.config.TTS_TYPE)
        tts_config = get_role_tts_config(tts_type, role_config, self.config)
        return tts_type, tts_config, role_config

    def process_system_config(self, json_msg):
        identifier = json_msg.get('identifier')
        LOGGER.info('Received system config: %s', identifier)

    def process_login_event(self, dkey, json_msg, update_config=True):
        token = uuid.uuid4().hex

        old_device = self.folotoy_app.devices.get(dkey, {})
        selected_role = str(old_device.get('role', '1'))
        selected_config = self.config.ROLES.get(selected_role, {})
        is_deerflow = selected_config.get('llm_type', self.config.LLM_TYPE) == 'deerflow'

        def login():
            transaction = getattr(self.folotoy_app, 'device_session_transaction', nullcontext)
            with transaction():
                if is_deerflow:
                    old_token = old_device.get('token')
                    if old_token is not None:
                        self.folotoy_app.sessions.pop(old_token, None)
                self.folotoy_app.devices[dkey] = {
                    'role': selected_role if is_deerflow else 1, 'token': token}
                self.folotoy_app.sessions[token] = {
                    'dkey': dkey,
                    # Reserve the login commands in the same atomic state
                    # commit as the replacement token. No later disk failure
                    # may leave the old token revoked before the new is sent.
                    'msgIdCount': 1 if update_config else 0,
                    'currentRecording': {},
                    'messages': {},
                    'conversation_rounds': {},
                    'conversation_ids': {},
                }

        if is_deerflow:
            # Preserve the provider's durable (device, role) thread mapping.
            # Only the transient login token and currently running turn change.
            get_conversation_control(self.folotoy_app).cancel(dkey, login)
        else:
            login()

        self.folotoy_app.command_call(dkey, {
            'msgId': 0,
            'identifier': 'updatetoken',
            'inputParams': {'token': token},
        })

        if update_config:
            self.folotoy_app.command_call(dkey, {
                'msgId': 1,
                'identifier': 'updateconfig',
                'inputParams': {
                    'speechUdpServerHost': self.config.SPEECH_UDP_SERVER_HOST,
                    'speechUdpServerPort': self.config.SPEECH_UDP_SERVER_PORT,
                },
            })

    def process_press_small_btn(self, dkey, json_msg):
        device = self.folotoy_app.devices.get(dkey)
        if device is None:
            self.process_login_event(dkey, json_msg, update_config=False)
            return
        LOGGER.debug('Role no changed, empty the messages')
        session = self.folotoy_app.sessions[device['token']]
        role = str(device.get('role', session.get('role', '1')))
        role_config = self.config.ROLES.get(role, {})
        is_deerflow = role_config.get('llm_type', self.config.LLM_TYPE) == 'deerflow'

        def clear():
            session.setdefault('messages', {})[role] = []
            session.setdefault('conversation_rounds', {})[role] = 0
            session.setdefault('conversation_ids', {})[role] = None
            if is_deerflow:
                from sdk.llm_sdk.deerflow import reset_conversation
                llm_config = get_role_llm_config('deerflow', role_config, self.config)
                # This operation only updates local mapping/cancellation state;
                # no network request runs on the MQTT callback thread.
                reset_conversation(dkey, role, llm_config)

        if is_deerflow:
            get_conversation_control(self.folotoy_app).cancel(dkey, clear)
        else:
            clear()

    def process_nfc_card(self, dkey, json_msg):
        data = json_msg.get('data', {}) or {}
        card_type = data.get('cardType')
        card_code = data.get('cardCode')
        out_params = {'cardCode': 1, 'statusCode': -1}
        try:
            if card_type == 'm1':
                format_m1_card_code(card_code)
            else:
                format_rtu_card_code(card_code)
        except Exception as e:
            LOGGER.warning('format card code error: %s', e)
        msg_id = self.folotoy_app.get_and_increase_msg_id(dkey)
        self.folotoy_app.command_call(dkey, {
            'msgId': msg_id,
            'identifier': 'updatestartvoice',
            'inputParams': out_params,
        })

    def process_system_command_call_iwantplay(self, dkey, json_msg):
        self.folotoy_app.publish_msg(
            '/sys/%s/%s/thing/command/callAck' % (self.folotoy_app.product_key, dkey),
            {'identifier': 'iwantplay', 'result': 0},
        )

    def process_system_command_call_updatecommonconfig(self, dkey, json_msg):
        self.folotoy_app.publish_msg(
            '/sys/%s/%s/thing/command/callAck' % (self.folotoy_app.product_key, dkey),
            {'identifier': 'updatecommonconfig', 'result': 1},
        )
        msg_id = self.folotoy_app.get_and_increase_msg_id(dkey)
        self.folotoy_app.publish_msg(
            '/user/%s/%s/thing/command/call' % (self.folotoy_app.product_key, dkey),
            {
                'msgId': msg_id,
                'identifier': 'updatecommonconfig',
                'inputParams': {},
            },
        )
