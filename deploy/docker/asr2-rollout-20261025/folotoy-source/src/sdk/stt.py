import logging
import time

from config import Config

from .stt_sdk import (
    aliyun_asr,
    azure_stt,
    azure_whisper,
    cf_workers_ai_asr,
    dify_stt,
    groq_stt,
    openai_whisper,
    volcengine_asr,
)

from .stt_sdk.aliyun_asr import AliyunAsrEngine
from .stt_sdk.azure_stt import AzureSttEngine
from .stt_sdk.azure_whisper import AzureWhisperEngine
from .stt_sdk.cf_workers_ai_asr import CfWorkersAiAsrEngine
from .stt_sdk.dify_stt import DifySttEngine
from .stt_sdk.groq_stt import GroqSttEngine
from .stt_sdk.openai_whisper import OpenaiWhisperEngine
from .stt_sdk.volcengine_asr import VolcengineAsrEngine
from .stt_sdk.volcengine_asr2 import VolcengineAsr2Engine

LOGGER = logging.getLogger(__name__)

__test__ = {}


class SttSdk:
    def __init__(self, config):
        self.config = config
        self.azure_whisper_engine = AzureWhisperEngine(config)
        self.azure_stt_engine = AzureSttEngine(config)
        self.dify_stt_engine = DifySttEngine(config)
        self.openai_whisper_engine = OpenaiWhisperEngine(config)
        self.aliyun_asr_engine = AliyunAsrEngine(config)
        self.cf_workers_ai_asr_engine = CfWorkersAiAsrEngine(config)
        self.volcengine_asr_engine = VolcengineAsrEngine(config)
        self.volcengine_asr2_engine = VolcengineAsr2Engine(config)
        self.groq_stt_engine = GroqSttEngine(config)

    def openai_whisper(self, file, stt_config=None):
        return self.openai_whisper_engine.sound_to_text(file, stt_config)

    def azure_stt(self, file, stt_config=None):
        return self.azure_stt_engine.sound_to_text(file, stt_config)

    def azure_whisper(self, file, stt_config=None):
        return self.azure_whisper_engine.sound_to_text(file, stt_config)

    def dify_stt(self, file, stt_config=None):
        return self.dify_stt_engine.sound_to_text(file, stt_config)

    def groq_stt(self, file, stt_config=None):
        return self.groq_stt_engine.sound_to_text(file, stt_config)

    def cf_workers_ai_asr(self, file, stt_config=None):
        return self.cf_workers_ai_asr_engine.sound_to_text(file, stt_config)

    def volcengine_asr(self, file, stt_config=None):
        return self.volcengine_asr_engine.sound_to_text(file, stt_config)

    def volcengine_asr2(self, file, stt_config=None):
        return self.volcengine_asr2_engine.sound_to_text(file, stt_config)

    def aliyun_asr(self, file, stt_config=None):
        return self.aliyun_asr_engine.sound_to_text(file, stt_config)

    def transcribe(self, file_path, stt_type=None, stt_config=None):
        handlers = {
            'openai-whisper': self.openai_whisper,
            'azure-stt': self.azure_stt,
            'azure-whisper': self.azure_whisper,
            'dify-stt': self.dify_stt,
            'groq-stt': self.groq_stt,
            'groq_stt': self.groq_stt,
            'cf-workers-ai-asr': self.cf_workers_ai_asr,
            'volcengine-asr': self.volcengine_asr,
            'volcengine-asr2': self.volcengine_asr2,
            'aliyun-asr': self.aliyun_asr,
        }
        handler = handlers.get(stt_type)
        if handler is None:
            LOGGER.warning('Unknown stt type found: %s', stt_type)
            return None

        # [FIX] volcengine-asr 与 azure-stt 需要文件路径（内部自行 open），
        # 其余引擎需要 file object（OpenAI SDK 的 file= 参数）。
        # 依据 oracle 实测：这两种类型传 str，其余传 BufferedReader。
        if stt_type in ('volcengine-asr', 'volcengine-asr2', 'azure-stt'):
            return handler(file_path, stt_config)

        with open(file_path, 'rb') as f:
            return handler(f, stt_config)
