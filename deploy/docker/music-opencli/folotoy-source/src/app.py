import logging
from multiprocessing import Queue
import os
import traceback

from config import Config
from core.api_server import ApiServer
from core.folotoy_app import FolotoyApp
from core.folotoy_msg_processor import FolotoyMsgProcessor
from core.music_playback import MusicPlaybackService
from core.speech_server import SpeechServer
from sdk.llm import LlmSdk
from sdk.stt import SttSdk
from sdk.tts import TtsSdk

LOGGER = logging.getLogger(__name__)

default_config = Config()

del Config

__test__ = {}


def run():
    stt_sdk = SttSdk(default_config)
    llm_sdk = LlmSdk(default_config)
    tts_sdk = TtsSdk(default_config)

    folotoy_app = FolotoyApp(default_config, llm_sdk, tts_sdk, stt_sdk)
    msg_processor = FolotoyMsgProcessor(folotoy_app)
    folotoy_app.set_message_processor(msg_processor)

    LOGGER.info('FoloToy Server Version: v%s', default_config.VERSION)
    folotoy_app.start()

    music_events = Queue(maxsize=1024) if os.environ.get('MUSIC_CONTROL_CONFIG') else None
    if default_config.API_SERVER:
        api_server = ApiServer(default_config, music_events=music_events)
        api_server.start()

    # This HTTP server shares the real device/turn state. Start its threads only
    # after the legacy HTTP worker has forked, not in the separate API process.
    music = MusicPlaybackService.from_environment(folotoy_app, fetch_events=music_events)
    folotoy_app.music_playback = music
    if music is not None:
        music.start()

    speech_server = SpeechServer(folotoy_app)
    try:
        speech_server.start()
    finally:
        # Lazy backend initialization keeps non-DeerFlow deployments unchanged.
        from sdk.llm_sdk.deerflow import close_all
        try:
            speech_server.stop()
        finally:
            try:
                if music is not None:
                    music.stop()
            finally:
                close_all()


def main():
    try:
        run()
    except Exception:
        LOGGER.error(traceback.format_exc())
