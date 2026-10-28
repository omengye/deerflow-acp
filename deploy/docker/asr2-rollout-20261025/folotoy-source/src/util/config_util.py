from __future__ import annotations

import copy
import logging
import math

from config import Config

LOGGER = logging.getLogger(__name__)


def get_role_stt_config(stt_type: str, role_config: dict, config: Config):
    stt_config = role_config.get('stt_config') or {}
    stt_merged = {
        'openai-whisper': {
            'model': stt_config.get('model', config.OPENAI_WHISPER_MODEL),
            'prompt': stt_config.get('prompt', config.OPENAI_WHISPER_PROMPT),
            'api_base': stt_config.get('api_base', config.OPENAI_WHISPER_API_BASE),
            'api_type': 'openai',
            'api_key': config.OPENAI_WHISPER_KEY,
            'api_version': config.OPENAI_WHISPER_KEY,
            'response_format': 'json',
            'language': role_config.get('language', stt_config.get('language', config.OPENAI_WHISPER_KEY)),
            'timeout': stt_config.get('timeout', config.HTTPX_TIMEOUT),
        },
        'azure-stt': {
            'language': role_config.get('language', stt_config.get('language', config.AZURE_STT_LANGUAGE)),
            'api_key': config.AZURE_STT_KEY,
            'service_region': stt_config.get('service_region', config.AZURE_STT_SERVICE_REGION),
            'timeout': stt_config.get('timeout', config.HTTPX_TIMEOUT),
        },
        'azure-whisper': {
            'api_version': stt_config.get('api_version', config.AZURE_WHISPER_API_VERSION),
            'api_key': config.AZURE_WHISPER_API_BASE,
            'deployment_name': stt_config.get('deployment_name', config.AZURE_WHISPER_DEPLOYMENT_NAME),
            'api_base': stt_config.get('api_base', config.AZURE_WHISPER_API_BASE),
            'timeout': stt_config.get('timeout', config.HTTPX_TIMEOUT),
        },
        'cf-workers-ai-asr': {
            'account_id': stt_config.get('account_id', config.CF_WORKERS_AI_ASR_ACCOUNT_ID),
            'api_token': stt_config.get('api_token', config.CF_WORKERS_AI_ASR_API_TOKEN),
            'model_id': stt_config.get('model_id', config.CF_WORKERS_AI_ASR_MODEL_ID),
            'timeout': stt_config.get('timeout', config.HTTPX_TIMEOUT),
        },
        'dify-stt': {
            'api_base': stt_config.get('api_base', config.DIFY_STT_API_BASE),
            'api_key': config.DIFY_STT_KEY,
            'timeout': stt_config.get('timeout', config.HTTPX_TIMEOUT),
        },
        'groq-stt': {
            'model': stt_config.get('model', config.GROQ_STT_MODEL),
            'prompt': stt_config.get('prompt', config.GROQ_STT_PROMPT),
            'api_base': stt_config.get('api_base', config.GROQ_STT_API_BASE),
            'api_type': 'openai',
            'api_key': config.GROQ_STT_KEY,
            'api_version': config.GROQ_STT_API_BASE,
            'response_format': 'json',
            'language': role_config.get('language', stt_config.get('language', config.GROQ_STT_API_BASE)),
            'timeout': stt_config.get('timeout', config.HTTPX_TIMEOUT),
        },
        'aliyun-asr': {
            'access_key_id': stt_config.get('access_key_id', config.ALIYUN_ASR_ACCESS_KEY_ID),
            'access_key_secret': stt_config.get('access_key_secret', config.ALIYUN_ASR_ACCESS_KEY_SECRET),
            'region_id': stt_config.get('region_id', config.ALIYUN_ASR_REGION_ID),
            'app_key': stt_config.get('app_key', config.ALIYUN_ASR_APP_KEY),
            'timeout': stt_config.get('timeout', config.HTTPX_TIMEOUT),
        },
        # The service owns the ASR 2.0 credential and resource selection.
        # Do not copy keys, endpoints, hotwords or context from role JSON.
        'volcengine-asr2': {
            'api_key': config.VOLCENGINE_ASR2_API_KEY,
            'resource_id': config.VOLCENGINE_ASR2_RESOURCE_ID,
            'timeout': config.VOLCENGINE_ASR2_TIMEOUT,
        },
        'volcengine-asr': {
            'app_id': stt_config.get('app_id', config.VOLCENGINE_ASR_APP_ID),
            'cluster_id': stt_config.get('cluster_id', config.VOLCENGINE_ASR_CLUSTER_ID),
            'access_token': stt_config.get('access_token', config.VOLCENGINE_ASR_ACCESS_TOKEN),
            'timeout': stt_config.get('timeout', config.HTTPX_TIMEOUT),
        },
    }
    result = stt_merged.get(stt_type)
    if result is None:
        LOGGER.warning('Unsupported stt type: %s', stt_type)
    return result


def _get_deerflow_config(role_config: dict, llm_config: dict, config: Config):
    # One process owns one gateway connection. A role cannot redirect that
    # connection, select another token, or relax its permission policy.
    transport = str(config.DEERFLOW_TRANSPORT or '').strip().lower()
    if transport not in ('acp', 'ws'):
        raise ValueError('DEERFLOW_TRANSPORT must be acp or ws')
    ws_mode = str(config.DEERFLOW_WS_MODE or '').strip()
    if transport == 'ws' and ws_mode not in ('ask', 'fullAccess'):
        raise ValueError('DEERFLOW_WS_MODE must be ask or fullAccess')
    approval = config.DEERFLOW_APPROVAL
    expected_approval = ('ask' if ws_mode == 'ask' else 'allow_always') if transport == 'ws' else 'read_only'
    if approval != expected_approval:
        raise ValueError('DEERFLOW_APPROVAL must be %s for the %s voice adapter' % (expected_approval, transport))

    result = {
        'transport': transport,
        'bridge_path': config.DEERFLOW_BRIDGE_PATH,
        'gateway_url': config.DEERFLOW_GATEWAY_URL,
        'ws_url': config.DEERFLOW_WS_URL,
        'ws_mode': ws_mode,
        'server_binary': config.DEERFLOW_SERVER_BINARY,
        'token_env': config.DEERFLOW_TOKEN_ENV,
        'workspace': config.DEERFLOW_WORKSPACE,
        'state_path': config.DEERFLOW_STATE_PATH,
        'model': role_config.get('model', llm_config.get('model', config.DEERFLOW_MODEL)),
        'prompt': role_config.get('prompt', ''),
        'approval': approval,
        'sentence_len_base': config.SENTENCE_LEN_BASE,
        'sentence_separators': config.SENTENCE_SEPARATORS,
        'sentence_strip': config.SENTENCE_STRIP,
        'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
        'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
        'first_sentence_len': config.FIRST_SENTENCE_LEN,
    }
    for key, value in (
        ('connect_timeout', config.DEERFLOW_CONNECT_TIMEOUT),
        ('turn_timeout', config.DEERFLOW_TURN_TIMEOUT),
        ('cancel_timeout', config.DEERFLOW_CANCEL_TIMEOUT),
    ):
        try:
            numeric = float(value)
        except (TypeError, ValueError):
            raise ValueError('DEERFLOW_' + key.upper() + ' must be a positive number') from None
        if isinstance(value, bool) or not math.isfinite(numeric) or numeric <= 0:
            raise ValueError('DEERFLOW_' + key.upper() + ' must be a positive number')
        result[key] = numeric

    queue_size = config.DEERFLOW_EVENT_QUEUE_SIZE
    try:
        numeric = int(queue_size)
    except (TypeError, ValueError):
        raise ValueError('DEERFLOW_EVENT_QUEUE_SIZE must be a positive integer') from None
    if isinstance(queue_size, bool) or numeric <= 0 or str(numeric) != str(queue_size).strip():
        raise ValueError('DEERFLOW_EVENT_QUEUE_SIZE must be a positive integer')
    result['event_queue_size'] = numeric
    return result


def get_role_llm_config(llm_type: str, role_config: dict, config: Config):
    llm_config = role_config.get('llm_config') or {}
    if llm_type == 'deerflow':
        return _get_deerflow_config(role_config, llm_config, config)
    llm_merged = {
        'openai': {
            'model': role_config.get('model', llm_config.get('model', config.OPENAI_OPENAI_MODEL)),
            'api_base': llm_config.get('api_base', config.OPENAI_OPENAI_API_BASE),
            'api_key': config.OPENAI_OPENAI_KEY,
            'max_tokens': role_config.get('max_tokens', llm_config.get('max_tokens', config.MAX_TOKENS)),
            'temperature': role_config.get('temperature', llm_config.get('temperature', config.TEMPERATURE)),
            'top_p': role_config.get('top_p', llm_config.get('top_p', config.TOP_P)),
            'frequency_penalty': role_config.get('frequency_penalty', llm_config.get('frequency_penalty', config.FREQUENCY_PENALTY)),
            'presence_penalty': role_config.get('presence_penalty', llm_config.get('presence_penalty', config.PRESENCE_PENALTY)),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'azure-openai': {
            'model': role_config.get('model', llm_config.get('model', config.AZURE_OPENAI_MODEL)),
            'api_base': config.AZURE_OPENAI_ENDPOINT,
            'api_key': config.AZURE_OPENAI_KEY,
            'api_version': llm_config.get('api_version', config.AZURE_OPENAI_API_VERSION),
            'max_tokens': role_config.get('max_tokens', llm_config.get('max_tokens', config.MAX_TOKENS)),
            'temperature': role_config.get('temperature', llm_config.get('temperature', config.TEMPERATURE)),
            'top_p': role_config.get('top_p', llm_config.get('top_p', config.TOP_P)),
            'frequency_penalty': role_config.get('frequency_penalty', llm_config.get('frequency_penalty', config.FREQUENCY_PENALTY)),
            'presence_penalty': role_config.get('presence_penalty', llm_config.get('presence_penalty', config.PRESENCE_PENALTY)),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'anthropic': {
            'api_base': llm_config.get('api_base', config.ANTHROPIC_API_BASE),
            'api_key': config.ANTHROPIC_KEY,
            'model': llm_config.get('model', config.ANTHROPIC_MODEL),
            'max_tokens': role_config.get('max_tokens', llm_config.get('max_tokens', config.ANTHROPIC_MAX_TOKENS)),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'aws-bedrock': {
            'model': role_config.get('model', llm_config.get('model', config.AWS_BEDROCK_MODEL)),
            'access_key_id': llm_config.get('access_key_id', config.AWS_BEDROCK_ACCESS_KEY_ID),
            'access_key_secret': llm_config.get('access_key_secret', config.AWS_BEDROCK_ACCESS_KEY_SECRET),
            'max_tokens': role_config.get('max_tokens', llm_config.get('max_tokens', config.AWS_BEDROCK_MAX_TOKENS)),
            'temperature': role_config.get('temperature', llm_config.get('temperature', config.AWS_BEDROCK_TEMPERATURE)),
            'top_p': role_config.get('top_p', llm_config.get('top_p', config.AWS_BEDROCK_TOP_P)),
            'region_name': role_config.get('region_name', llm_config.get('region_name', config.AWS_BEDROCK_REGION_NAME)),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'dashscope': {
            'model': role_config.get('model', llm_config.get('model', config.DASHSCOPE_MODEL)),
            'api_base': llm_config.get('api_base', config.DASHSCOPE_API_BASE),
            'api_key': config.DASHSCOPE_API_BASE,
            'max_tokens': role_config.get('max_tokens', llm_config.get('max_tokens', config.DASHSCOPE_MAX_TOKENS)),
            'temperature': role_config.get('temperature', llm_config.get('temperature', config.DASHSCOPE_TEMPERATURE)),
            'top_p': role_config.get('top_p', llm_config.get('top_p', config.DASHSCOPE_TOP_P)),
            'enable_search': llm_config.get('enable_search', config.DASHSCOPE_ENABLE_SEARCH),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'deepseek': {
            'model': role_config.get('model', llm_config.get('model', config.DEEPSEEK_MODEL)),
            'api_base': llm_config.get('api_base', config.DEEPSEEK_API_BASE),
            'api_key': config.DEEPSEEK_KEY,
            'max_tokens': role_config.get('max_tokens', llm_config.get('max_tokens', config.DEEPSEEK_MAX_TOKENS)),
            'temperature': role_config.get('temperature', llm_config.get('temperature', config.DEEPSEEK_TEMPERATURE)),
            'read_thought_enabled': role_config.get('read_thought_enabled', llm_config.get('read_thought_enabled', config.DEEPSEEK_READ_THOUGHT_ENABLED)),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'dify': {
            'api_base': llm_config.get('api_base', config.DIFY_API_BASE),
            'api_key': config.DIFY_KEY,
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'fastgpt': {
            'api_base': llm_config.get('api_base', config.FASTGPT_API_BASE),
            'api_key': config.FASTGPT_CHAT_ID,
            'chat_id': llm_config.get('chat_id', config.FASTGPT_CHAT_ID),
            'variables': llm_config.get('variables', {}),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'gemini': {
            'model': role_config.get('model', llm_config.get('model', config.GEMINI_MODEL)),
            'api_base': llm_config.get('api_base', config.GEMINI_API_BASE),
            'api_version': llm_config.get('api_version', config.GEMINI_API_VERSION),
            'api_key': config.GEMINI_KEY,
            'top_p': role_config.get('top_p', llm_config.get('top_p', config.TOP_P)),
            'top_k': role_config.get('top_k', llm_config.get('top_k', 10.0)),
            'max_tokens': role_config.get('max_tokens', llm_config.get('max_tokens', config.MAX_TOKENS)),
            'temperature': role_config.get('temperature', llm_config.get('temperature', config.TEMPERATURE)),
            'safety_settings': role_config.get('safety_settings', llm_config.get('safety_settings', [{'category': 'HARM_CATEGORY_HARASSMENT', 'threshold': 'BLOCK_MEDIUM_AND_ABOVE'},
                {'category': 'HARM_CATEGORY_HATE_SPEECH', 'threshold': 'BLOCK_MEDIUM_AND_ABOVE'},
                {'category': 'HARM_CATEGORY_SEXUALLY_EXPLICIT', 'threshold': 'BLOCK_MEDIUM_AND_ABOVE'},
                {'category': 'HARM_CATEGORY_DANGEROUS_CONTENT', 'threshold': 'BLOCK_MEDIUM_AND_ABOVE'}])),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'groq': {
            'api_base': llm_config.get('api_base', config.GROQ_API_BASE),
            'api_key': config.GROQ_KEY,
            'model': llm_config.get('model', config.GROQ_MODEL),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'lingyiwanwu': {
            'model': role_config.get('model', llm_config.get('model', config.LINGYIWANWU_MODEL)),
            'api_base': llm_config.get('api_base', config.LINGYIWANWU_API_BASE),
            'api_key': config.LINGYIWANWU_KEY,
            'max_tokens': role_config.get('max_tokens', llm_config.get('max_tokens', config.LINGYIWANWU_MAX_TOKENS)),
            'temperature': role_config.get('temperature', llm_config.get('temperature', config.LINGYIWANWU_TEMPERATURE)),
            'top_p': role_config.get('top_p', llm_config.get('top_p', config.LINGYIWANWU_TOP_P)),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'minimax': {
            'model': role_config.get('model', llm_config.get('model', config.MINIMAX_MODEL)),
            'api_base': llm_config.get('api_base', config.MINIMAX_API_BASE),
            'api_key': config.MINIMAX_KEY,
            'max_tokens': role_config.get('max_tokens', llm_config.get('max_tokens', config.MINIMAX_MAX_TOKENS)),
            'temperature': role_config.get('temperature', llm_config.get('temperature', config.MINIMAX_TEMPERATURE)),
            'top_p': role_config.get('top_p', llm_config.get('top_p', config.MINIMAX_TOP_P)),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'moonshot': {
            'model': role_config.get('model', llm_config.get('model', config.MOONSHOT_MODEL)),
            'api_base': llm_config.get('api_base', config.MOONSHOT_API_BASE),
            'api_key': config.MOONSHOT_KEY,
            'max_tokens': role_config.get('max_tokens', llm_config.get('max_tokens', config.MOONSHOT_MAX_TOKENS)),
            'temperature': role_config.get('temperature', llm_config.get('temperature', config.MOONSHOT_TEMPERATURE)),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'ollama': {
            'api_base': llm_config.get('api_base', config.OLLAMA_API_BASE),
            'model': llm_config.get('model', config.OLLAMA_MODEL),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'qianfan': {
            'client_id': llm_config.get('client_id', config.QIANFAN_CLIENT_ID),
            'secret': llm_config.get('secret', config.QIANFAN_SECRET),
            'model': role_config.get('model', llm_config.get('model', config.QIANFAN_MODEL)),
            'max_tokens': role_config.get('max_tokens', llm_config.get('max_tokens', config.MAX_TOKENS)),
            'temperature': role_config.get('temperature', llm_config.get('temperature', config.TEMPERATURE)),
            'extra_parameters': role_config.get('extra_parameters', llm_config.get('extra_parameters', config.QIANFAN_CLIENT_ID)),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'spark-desk': {
            'api_base': llm_config.get('api_base', config.SPARK_DESK_API_BASE),
            'api_key': config.SPARK_DESK_KEY,
            'model': role_config.get('model', llm_config.get('model', config.SPARK_DESK_MODEL)),
            'max_tokens': role_config.get('max_tokens', llm_config.get('max_tokens', config.SPARK_DESK_MAX_TOKENS)),
            'temperature': role_config.get('temperature', llm_config.get('temperature', config.SPARK_DESK_TEMPERATURE)),
            'top_p': role_config.get('top_p', llm_config.get('top_p', config.SPARK_DESK_TOP_P)),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'volcengine': {
            'api_version': role_config.get('api_version', llm_config.get('api_version', config.VOLCENGINE_API_VERSION)),
            'host': role_config.get('host', llm_config.get('host', config.VOLCENGINE_HOST)),
            'region': role_config.get('region', llm_config.get('region', config.VOLCENGINE_REGION)),
            'access_key': llm_config.get('access_key', config.VOLCENGINE_ACCESS_KEY),
            'secret_key': llm_config.get('secret_key', config.VOLCENGINE_SECRET_KEY),
            'model': role_config.get('model', llm_config.get('model', config.VOLCENGINE_MODEL)),
            'personal_api_key': llm_config.get('personal_api_key', config.VOLCENGINE_PERSONAL_API_KEY),
            'bot_id': llm_config.get('bot_id', config.VOLCENGINE_BOT_ID),
            'max_tokens': role_config.get('max_tokens', llm_config.get('max_tokens', config.VOLCENGINE_MAX_TOKENS)),
            'temperature': role_config.get('temperature', llm_config.get('temperature', config.VOLCENGINE_TEMPERATURE)),
            'top_p': role_config.get('top_p', llm_config.get('top_p', config.VOLCENGINE_TOP_P)),
            'top_k': role_config.get('top_k', llm_config.get('top_k', config.VOLCENGINE_TOP_K)),
            'api_base': llm_config.get('api_base', config.VOLCENGINE_API_BASE),
            'api_key': config.VOLCENGINE_ACCESS_KEY,
            'auth_type': llm_config.get('auth_type', config.VOLCENGINE_AUTH_TYPE),
            'stream': llm_config.get('stream', True),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'yandex': {
            'model': role_config.get('model', llm_config.get('model', config.YANDEX_MODEL)),
            'api_base': llm_config.get('api_base', config.YANDEX_API_BASE),
            'api_key': config.YANDEX_KEY,
            'max_tokens': role_config.get('max_tokens', llm_config.get('max_tokens', config.YANDEX_MAX_TOKENS)),
            'temperature': role_config.get('temperature', llm_config.get('temperature', config.YANDEX_TEMPERATURE)),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'zhipu': {
            'model': role_config.get('model', llm_config.get('model', config.ZHIPU_MODEL)),
            'api_base': llm_config.get('api_base', config.ZHIPU_API_BASE),
            'api_key': config.ZHIPU_API_BASE,
            'max_tokens': role_config.get('max_tokens', llm_config.get('max_tokens', config.ZHIPU_MAX_TOKENS)),
            'temperature': role_config.get('temperature', llm_config.get('temperature', config.ZHIPU_TEMPERATURE)),
            'top_p': role_config.get('top_p', llm_config.get('top_p', config.ZHIPU_TOP_P)),
            'tools': role_config.get('tools', llm_config.get('tools', config.ZHIPU_TOOLS)),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'zkdn': {
            'api_base': llm_config.get('api_base', config.ZKDN_API_BASE),
            'api_key': config.ZKDN_KEY,
            'sensi_filter': llm_config.get('sensi_filter', 1),
            'plugin': llm_config.get('plugin', 3),
            'role_play': llm_config.get('role_play', 'normal'),
            'output_style': llm_config.get('output_style', config.ZKDN_KEY),
            'query_route': llm_config.get('query_route', config.BG_MUSIC),
            'query_weather': llm_config.get('query_weather', config.BG_MUSIC),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'coze': {
            'api_base': llm_config.get('api_base', config.COZE_API_BASE),
            'personal_access_token': llm_config.get('personal_access_token', config.COZE_PERSONAL_ACCESS_TOKEN),
            'bot_id': llm_config.get('bot_id', config.COZE_BOT_ID),
            'conversation_id': llm_config.get('conversation_id', config.COZE_CONVERSATION_ID),
            'enable_history': llm_config.get('enable_history', config.COZE_ENABLE_HISTORY),
            'custom_variables': llm_config.get('custom_variables', {}),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'bailian': {
            'app_id': llm_config.get('app_id', config.BAILIAN_APP_ID),
            'api_key': config.BAILIAN_APP_ID,
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'cece': {
            'api_base': llm_config.get('api_base', config.CECE_API_BASE),
            'api_key': config.CECE_API_BASE,
            'content_type': llm_config.get('content_type', config.CECE_CONTENT_TYPE),
            'voice_https_enabled': llm_config.get('voice_https_enabled', config.CECE_VOICE_HTTPS_ENABLED),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'cf-workers-ai': {
            'account_id': llm_config.get('account_id', config.CF_WORKERS_AI_ACCOUNT_ID),
            'api_token': llm_config.get('api_token', config.CF_WORKERS_AI_API_TOKEN),
            'model_id': llm_config.get('model_id', config.CF_WORKERS_AI_MODEL_ID),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'qanything': {
            'api_base': llm_config.get('api_base', config.QANYTHING_API_BASE),
            'user_id': llm_config.get('user_id', 'folotoy'),
            'kb_ids': [''],
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
        'xiaodu': {
            'client_id': llm_config.get('client_id', config.XIAODU_CLIENT_ID),
            'secret': llm_config.get('secret', config.XIAODU_SECRET),
            'max_tokens': role_config.get('max_tokens', llm_config.get('max_tokens', config.MAX_TOKENS)),
            'timeout': llm_config.get('timeout', config.HTTPX_TIMEOUT),
            'sentence_len_base': config.SENTENCE_LEN_BASE,
            'sentence_separators': config.SENTENCE_SEPARATORS,
            'sentence_strip': config.SENTENCE_STRIP,
            'first_sentence_config_enabled': config.FIRST_SENTENCE_CONFIG_ENABLED,
            'first_sentence_separators': config.FIRST_SENTENCE_SEPARATORS,
            'first_sentence_len': config.FIRST_SENTENCE_LEN,
        },
    }
    result = llm_merged.get(llm_type)
    if result is None:
        LOGGER.warning('Unsupported llm type: %s', llm_type)
    return result


def get_role_tts_config(tts_type: str, role_config: dict, config: Config):
    tts_config = role_config.get('tts_config') or {}
    tts_merged = {
        'openai-tts': {
            'api_base': tts_config.get('api_base', config.OPENAI_TTS_API_BASE),
            'api_key': config.OPENAI_TTS_KEY,
            'model': tts_config.get('model', config.OPENAI_TTS_MODEL),
            'speed': tts_config.get('speed', config.OPENAI_TTS_SPEED),
            'voice_name': role_config.get('voice_name', tts_config.get('voice_name', config.OPENAI_TTS_VOICE_NAME)),
            'bg_music': tts_config.get('bg_music', config.BG_MUSIC),
            'bg_music_fname': tts_config.get('bg_music_fname', config.BG_MUSIC_FNAME),
            'timeout': tts_config.get('timeout', config.HTTPX_TIMEOUT),
        },
        'azure-tts': {
            'api_key': config.AZURE_TTS_KEY,
            'service_region': tts_config.get('service_region', config.AZURE_TTS_SERVICE_REGION),
            'endpoint': tts_config.get('endpoint', config.AZURE_TTS_ENDPOINT),
            'voice_name': role_config.get('voice_name', tts_config.get('voice_name', config.AZURE_TTS_VOICE_NAME)),
            'dialect_name': role_config.get('dialect_name', tts_config.get('dialect_name', config.AZURE_TTS_ENDPOINT)),
            'style': role_config.get('style', tts_config.get('style', config.AZURE_TTS_ENDPOINT)),
            'styledegree': role_config.get('styledegree', tts_config.get('styledegree', config.AZURE_TTS_ENDPOINT)),
            'role': role_config.get('role', tts_config.get('role', config.AZURE_TTS_ENDPOINT)),
            'prosody_rate': role_config.get('prosody_rate', tts_config.get('prosody_rate', config.AZURE_TTS_ENDPOINT)),
            'prosody_pitch': role_config.get('prosody_pitch', tts_config.get('prosody_pitch', config.AZURE_TTS_ENDPOINT)),
            'prosody_volume': role_config.get('prosody_volume', tts_config.get('prosody_volume', config.AZURE_TTS_ENDPOINT)),
            'debug': tts_config.get('debug', config.BG_MUSIC),
            'input_ssml': tts_config.get('input_ssml', config.BG_MUSIC),
            'force_ssml': tts_config.get('force_ssml', config.BG_MUSIC),
            'bg_music': tts_config.get('bg_music', config.BG_MUSIC),
            'bg_music_fname': tts_config.get('bg_music_fname', config.BG_MUSIC_FNAME),
            'timeout': tts_config.get('timeout', config.HTTPX_TIMEOUT),
        },
        'edge-tts': {
            'voice_name': role_config.get('voice_name', tts_config.get('voice_name', config.EDGE_TTS_VOICE_NAME)),
            'bg_music': tts_config.get('bg_music', config.BG_MUSIC),
            'bg_music_fname': tts_config.get('bg_music_fname', config.BG_MUSIC_FNAME),
            'timeout': tts_config.get('timeout', config.HTTPX_TIMEOUT),
        },
        'elevenlabs': {
            'api_key': config.ELEVENLABS_TTS_KEY,
            'model': tts_config.get('model', config.ELEVENLABS_TTS_MODEL),
            'voice_name': role_config.get('voice_name', tts_config.get('voice_name', config.ELEVENLABS_TTS_VOICE_ID)),
            'bg_music': tts_config.get('bg_music', config.BG_MUSIC),
            'bg_music_fname': tts_config.get('bg_music_fname', config.BG_MUSIC_FNAME),
            'timeout': tts_config.get('timeout', config.HTTPX_TIMEOUT),
        },
        'dify-tts': {
            'api_base': tts_config.get('api_base', config.DIFY_TTS_API_BASE),
            'api_key': config.DIFY_TTS_KEY,
            'bg_music': tts_config.get('bg_music', config.BG_MUSIC),
            'bg_music_fname': tts_config.get('bg_music_fname', config.BG_MUSIC_FNAME),
            'timeout': tts_config.get('timeout', config.HTTPX_TIMEOUT),
        },
        'groq-tts': {
            'api_base': tts_config.get('api_base', config.GROQ_TTS_API_BASE),
            'api_key': config.GROQ_TTS_KEY,
            'model': tts_config.get('model', config.GROQ_TTS_MODEL),
            'speed': tts_config.get('speed', config.GROQ_TTS_SPEED),
            'voice_name': role_config.get('voice_name', tts_config.get('voice_name', config.GROQ_TTS_VOICE_NAME)),
            'bg_music': tts_config.get('bg_music', config.BG_MUSIC),
            'bg_music_fname': tts_config.get('bg_music_fname', config.BG_MUSIC_FNAME),
            'timeout': tts_config.get('timeout', config.HTTPX_TIMEOUT),
        },
        'minimax-tts': {
            'version': tts_config.get('version', config.MINIMAX_TTS_API_VERSION),
            'api_base': tts_config.get('api_base', config.MINIMAX_TTS_API_BASE),
            'key': tts_config.get('key', config.MINIMAX_TTS_KEY),
            'group_id': tts_config.get('group_id', config.MINIMAX_TTS_GROUP_ID),
            'model': tts_config.get('model', config.MINIMAX_TTS_MODEL),
            'voice_id': tts_config.get('voice_id', config.MINIMAX_TTS_VOICE_ID),
            'vol': tts_config.get('vol', config.MINIMAX_TTS_VOL),
            'speed': tts_config.get('speed', config.MINIMAX_TTS_SPEED),
            'pitch': tts_config.get('pitch', config.MINIMAX_TTS_PITCH),
            'char_to_pitch': tts_config.get('char_to_pitch', config.MINIMAX_TTS_CHAR_TO_PITCH),
            'timber_weights': tts_config.get('timber_weights', config.MINIMAX_TTS_TIMBER_WEIGHTS),
            'voice_setting': tts_config.get('voice_setting', config.MINIMAX_TTS_VOICE_SETTING),
            'audio_setting': tts_config.get('audio_setting', config.MINIMAX_TTS_AUDIO_SETTING),
            'pronunciation_dict': tts_config.get('pronunciation_dict', config.MINIMAX_TTS_PRONUNCIATION_DICT),
            'language_boost': tts_config.get('language_boost', config.MINIMAX_TTS_LANGUAGE_BOOST),
            'bg_music': tts_config.get('bg_music', config.BG_MUSIC),
            'bg_music_fname': tts_config.get('bg_music_fname', config.BG_MUSIC_FNAME),
            'timeout': tts_config.get('timeout', config.HTTPX_TIMEOUT),
        },
        'volcengine-tts': {
            'api_base': tts_config.get('api_base', config.VOLCENGINE_TTS_API_BASE),
            'app_id': tts_config.get('app_id', config.VOLCENGINE_TTS_APP_ID),
            'cluster_id': tts_config.get('cluster_id', config.VOLCENGINE_TTS_CLUSTER_ID),
            'access_token': tts_config.get('access_token', config.VOLCENGINE_TTS_ACCESS_TOKEN),
            'voice_type': tts_config.get('voice_type', config.VOLCENGINE_TTS_VOICE_TYPE),
            'speed_ratio': tts_config.get('speed_ratio', config.VOLCENGINE_TTS_SPEED_RATIO),
            'volume_ratio': tts_config.get('volume_ratio', config.VOLCENGINE_TTS_VOLUME_RATIO),
            'pitch_ratio': tts_config.get('pitch_ratio', config.VOLCENGINE_TTS_PITCH_RATIO),
            'emotion': tts_config.get('emotion', config.VOLCENGINE_TTS_EMOTION),
            'language': tts_config.get('language', config.VOLCENGINE_TTS_LANGUAGE),
            'bg_music': tts_config.get('bg_music', config.BG_MUSIC),
            'bg_music_fname': tts_config.get('bg_music_fname', config.BG_MUSIC_FNAME),
            'timeout': tts_config.get('timeout', config.HTTPX_TIMEOUT),
        },
        'aliyun-tts': {
            'access_key_id': tts_config.get('access_key_id', config.ALIYUN_TTS_ACCESS_KEY_ID),
            'access_key_secret': tts_config.get('access_key_secret', config.ALIYUN_TTS_ACCESS_KEY_SECRET),
            'region_id': tts_config.get('region_id', config.ALIYUN_TTS_REGION_ID),
            'app_key': tts_config.get('app_key', config.ALIYUN_TTS_APP_KEY),
            'voice_name': config.ALIYUN_TTS_ACCESS_KEY_ID,
            'volume': tts_config.get('volume', config.ALIYUN_TTS_ACCESS_KEY_ID),
            'speech_rate': tts_config.get('speech_rate', config.ALIYUN_TTS_ACCESS_KEY_ID),
            'pitch_rate': tts_config.get('pitch_rate', config.ALIYUN_TTS_ACCESS_KEY_ID),
            'bg_music': tts_config.get('bg_music', config.BG_MUSIC),
            'bg_music_fname': tts_config.get('bg_music_fname', config.BG_MUSIC_FNAME),
            'timeout': tts_config.get('timeout', config.HTTPX_TIMEOUT),
        },
        'inworld-tts': {
            'api_base': tts_config.get('api_base', config.INWORLD_TTS_API_BASE),
            'api_key': config.INWORLD_TTS_KEY,
            'model_id': tts_config.get('model_id', config.INWORLD_TTS_MODEL_ID),
            'voice_id': role_config.get('voice_name', tts_config.get('voice_id', config.INWORLD_TTS_VOICE_ID)),
            'bg_music': tts_config.get('bg_music', config.BG_MUSIC),
            'bg_music_fname': tts_config.get('bg_music_fname', config.BG_MUSIC_FNAME),
            'timeout': tts_config.get('timeout', config.HTTPX_TIMEOUT),
        },
        'siliconflow-tts': {
            'api_base': tts_config.get('api_base', config.SILICONFLOW_TTS_API_BASE),
            'api_key': config.SILICONFLOW_TTS_KEY,
            'model': tts_config.get('model', config.SILICONFLOW_TTS_MODEL),
            'voice_name': role_config.get('voice_name', tts_config.get('voice_name', config.SILICONFLOW_TTS_VOICE_NAME)),
            'bg_music': tts_config.get('bg_music', config.BG_MUSIC),
            'bg_music_fname': tts_config.get('bg_music_fname', config.BG_MUSIC_FNAME),
            'timeout': tts_config.get('timeout', config.HTTPX_TIMEOUT),
        },
    }
    result = tts_merged.get(tts_type)
    if result is None:
        LOGGER.warning('Unsupported tts type: %s', tts_type)
    return result


def merge_config(default_config, config) -> dict:
    if default_config is None:
        default_config = {}
    if config is None:
        config = {}
    if not isinstance(default_config, dict):
        raise TypeError('Expected dict, got %s' % type(default_config).__name__)
    return _merge(default_config, config)


def _merge(default_config, config):
    if default_config is None:
        default_config = {}
    if config is None:
        config = {}
    merged_config = copy.deepcopy(default_config)
    for key, value in config.items():
        if isinstance(value, dict):
            merged_config[key] = _merge(merged_config.get(key), value)
        else:
            merged_config[key] = value
    return merged_config
