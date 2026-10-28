import json
from contextlib import contextmanager
import logging
import threading
import time
import traceback

import paho.mqtt.client as mqtt

from config import Config
from core.device_session_store import DeviceSessionStore
from sdk.llm import LlmSdk
from sdk.stt import SttSdk
from sdk.tts import TtsSdk
import util.topic_util as topic_util

LOGGER = logging.getLogger(__name__)

__test__ = {}


class FolotoyApp:
    def __init__(self, config: 'Config', llm_sdk: 'LlmSdk', tts_sdk: 'TtsSdk', stt_sdk: 'SttSdk'):
        self.sessions = {}
        self.devices = {}
        self.config = config
        self.mqtt_config = config.MQTT_CONFIG
        self.ip = self.mqtt_config['ip']
        self.port = self.mqtt_config['port']
        self.cid = self.mqtt_config['cid']
        self.keep_alive = self.mqtt_config['keep_alive']
        self.qos = self.mqtt_config['qos']
        self.connected_flag = False
        self.llm_sdk = llm_sdk
        self.tts_sdk = tts_sdk
        self.stt_sdk = stt_sdk
        self.start_voices = {}
        self.product_key = config.PROJECT_NAME
        self._device_session_lock = threading.RLock()
        self._device_session_store = DeviceSessionStore.from_config(config)
        self._restore_device_sessions()
        self.command_call_ack_topic_filter = '/user/%s/+/thing/command/callAck' % self.product_key
        self.event_post_topic_filter = '/user/%s/+/thing/event/post' % self.product_key
        self.data_post_topic_filter = '/user/%s/+/thing/data/post' % self.product_key
        self.sys_config_topic_filter = '/sys/config'
        self.sys_command_call_topic_filter = '/sys/%s/+/thing/command/call' % self.product_key
        self.message_processor = None
        self.mqtt_client = mqtt.Client(
            callback_api_version=mqtt.CallbackAPIVersion.VERSION2,
            client_id=self.cid,
        )

    def set_message_processor(self, message_processor):
        self.message_processor = message_processor

    def start(self):
        def on_connect(client, userdata, flags, reason_code, properties):
            self.process_mqtt_connect(userdata, flags, reason_code, properties)

        def on_disconnect(client, userdata, disconnect_flags, reason_code, properties):
            self.process_mqtt_disconnect(userdata, disconnect_flags, reason_code, properties)

        def on_message(client, userdata, message):
            # [DEPLOY PATCH] 原实现无异常保护：任何畸形 payload（非法 JSON、
            # 非 UTF-8 等）都会让 paho 的 loop_forever 线程抛出并永久退出，
            # 表现为进程仍 active 但再也收不到任何 MQTT 消息。
            # 这里兜底捕获，仅记录日志，保证接收线程存活。
            try:
                self.process_mqtt_message(userdata, message)
            except Exception:
                LOGGER.error("Handle MQTT message failed on topic %s",
                             getattr(message, "topic", "?"))
                LOGGER.error(traceback.format_exc())

        self.mqtt_client.on_connect = on_connect
        self.mqtt_client.on_disconnect = on_disconnect
        self.mqtt_client.on_message = on_message
        self.mqtt_client.username_pw_set(self.mqtt_config['username'],
                                         self.mqtt_config['password'])
        self.mqtt_client.connect(host=self.ip, port=self.port, keepalive=self.keep_alive)
        self.mqtt_client.loop_start()

    def publish_msg(self, topic, msg):
        try:
            mqtt_payload = json.dumps(msg)
            if self.connected_flag:
                self.mqtt_client.publish(topic, mqtt_payload, qos=self.qos)
        except Exception as e:
            LOGGER.error('Publish error: %s', e)
            LOGGER.error(traceback.format_exc())

    def process_mqtt_connect(self, userdata, flags, reason_code, properties):
        self.connected_flag = True
        LOGGER.info('Connected to MQTT Broker(%s:%s)!', self.ip, self.port)
        self.mqtt_client.subscribe(self.command_call_ack_topic_filter, self.qos)
        self.mqtt_client.subscribe(self.event_post_topic_filter, self.qos)
        self.mqtt_client.subscribe(self.data_post_topic_filter, self.qos)
        self.mqtt_client.subscribe(self.sys_config_topic_filter, self.qos)
        self.mqtt_client.subscribe(self.sys_command_call_topic_filter, self.qos)

    def process_mqtt_disconnect(self, userdata, disconnect_flags, reason_code, properties):
        self.connected_flag = False
        LOGGER.warning('Disconnected from MQTT Broker(%s:%s)!', self.ip, self.port)
        for _ in range(10):
            LOGGER.info('Sleep 2s to reconnect to %s:%s', self.ip, self.port)
            time.sleep(2)
            try:
                self.mqtt_client.reconnect()
                break
            except Exception as e:
                LOGGER.warning('Reconnect failed: %s', e)

    def process_mqtt_message(self, userdata, msg):
        LOGGER.debug('Received message on topic %s: %s', msg.topic, msg.payload)
        topic = msg.topic
        json_msg = json.loads(msg.payload)

        command_call_ack_matched, command_call_ack_tokens = \
            topic_util.matches(self.command_call_ack_topic_filter, topic)
        if command_call_ack_matched:
            return
        event_post_matched, event_post_topic_tokens = \
            topic_util.matches(self.event_post_topic_filter, topic)
        if event_post_matched:
            dkey = event_post_topic_tokens['+'][0]
            event_name = json_msg.get('identifier')
            if event_name == 'login':
                self.message_processor.process_login_event(dkey, json_msg)
            elif event_name == 'press_small_btn':
                self.message_processor.process_press_small_btn(dkey, json_msg)
            elif event_name == 'nfc_card':
                self.message_processor.process_nfc_card(dkey, json_msg)
            else:
                LOGGER.warning('Not supported identifier=%s found: %s', event_name, json_msg)
            return
        sys_config_matched, sys_config_topic_tokens = \
            topic_util.matches(self.sys_config_topic_filter, topic)
        if sys_config_matched:
            self.message_processor.process_system_config(json_msg)
            return
        sys_command_call_matched, sys_command_call_topic_tokens = \
            topic_util.matches(self.sys_command_call_topic_filter, topic)
        if sys_command_call_matched:
            dkey = sys_command_call_topic_tokens['+'][0]
            cmd_name = json_msg.get('identifier')
            if cmd_name == 'iwantplay':
                self.message_processor.process_system_command_call_iwantplay(dkey, json_msg)
            elif cmd_name == 'updatecommonconfig':
                self.message_processor.process_system_command_call_updatecommonconfig(dkey, json_msg)
            else:
                LOGGER.warning('Not supported identifier=%s found: %s', cmd_name, json_msg)
            return
        if json_msg.get('identifier') is None:
            LOGGER.warning('No event found')

    def _restore_device_sessions(self):
        try:
            records = self._device_session_store.load()
        except Exception as error:
            LOGGER.error('Device login state restore rejected (%s)', type(error).__name__)
            return
        configured_roles = getattr(self.config, 'ROLES', {})
        if any(entry['role'] not in configured_roles for entry in records):
            LOGGER.error('Device login state restore rejected (unconfigured role)')
            return
        for entry in records:
            dkey, token = entry['device'], entry['token']
            self.devices[dkey] = {'role': entry['role'], 'token': token}
            self.sessions[token] = {
                'dkey': dkey, 'msgIdCount': entry['msg_id'], 'currentRecording': {},
                'messages': {}, 'conversation_rounds': {}, 'conversation_ids': {},
            }
        LOGGER.info('Restored %s device login sessions', len(records))

    def persist_device_sessions(self):
        with self._device_session_lock:
            records = []
            for dkey, device in sorted(self.devices.items()):
                token = device['token']
                session = self.sessions[token]
                if session.get('dkey') != dkey:
                    raise ValueError('inconsistent device session ownership')
                records.append({'device': dkey, 'token': token,
                                'role': str(device['role']), 'msg_id': session['msgIdCount']})
            self._device_session_store.save(records)

    @contextmanager
    def device_session_transaction(self):
        # Login/role/removal and outgoing message IDs share one write order.
        # Persist before publishing so restart never reuses an issued message ID.
        with self._device_session_lock:
            devices, sessions = self.devices.copy(), self.sessions.copy()
            try:
                yield
                self.persist_device_sessions()
            except Exception:
                self.devices, self.sessions = devices, sessions
                raise

    def set_device_role(self, dkey, role):
        role = str(role)
        if role not in self.config.ROLES:
            raise ValueError('unconfigured device role')
        def update():
            with self.device_session_transaction():
                self.devices[dkey] = {**self.devices[dkey], 'role': role}
        control = getattr(self, 'conversation_control', None)
        return control.cancel(dkey, update) if control is not None else update()

    def forget_device_session(self, dkey):
        def remove():
            with self.device_session_transaction():
                device = self.devices.pop(dkey, None)
                if device is not None:
                    self.sessions.pop(device['token'], None)
        control = getattr(self, 'conversation_control', None)
        return control.cancel(dkey, remove) if control is not None else remove()

    def get_and_increase_msg_id(self, dkey):
        with self._device_session_lock:
            device = self.devices.get(dkey)
            if device is None:
                LOGGER.warning('Toy=%s is offline', dkey)
                return 0
            token = device.get('token')
            if token is None:
                LOGGER.warning('Token not found, Toy=%s is offline', dkey)
                return 0
            session = self.sessions[token]
            previous = session['msgIdCount']
            session['msgIdCount'] = previous + 1
            try:
                self.persist_device_sessions()
            except Exception:
                session['msgIdCount'] = previous
                raise
            return session['msgIdCount']

    def system_event_post(self, dkey, msg):
        topic = '/sys/%s/%s/thing/event/post' % (self.product_key, dkey)
        self.publish_msg(topic, msg)

    def command_call(self, dkey, msg):
        topic = '/user/%s/%s/thing/command/call' % (self.product_key, dkey)
        self.publish_msg(topic, msg)
