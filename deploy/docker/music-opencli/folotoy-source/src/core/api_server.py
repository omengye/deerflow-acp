import logging
from multiprocessing import Process

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.staticfiles import StaticFiles

from config import Config
from routes.api import api_router

LOGGER = logging.getLogger(__name__)

default_config = Config()

__test__ = {}


def music_fetch_event(method, status, filename, user_agent):
    filename = filename.lstrip('/')
    if (method == 'GET' and status in (200, 206)
            and len(filename) == 42 and filename.startswith('music-')
            and filename.endswith('.mp3')
            and all(c in '0123456789abcdef' for c in filename[6:-4])):
        return {'filename': filename, 'esp32': 'esp32' in user_agent.lower()}
    return None


def create_fastapp(music_events=None):
    fastapp = FastAPI(
        title=default_config.PROJECT_NAME,
        version='0.1.0',
        openapi_url=default_config.API_V1_STR + '/openapi.json',
    )
    fastapp.add_middleware(
        CORSMiddleware,
        allow_origins=default_config.CORS_ORIGINS,
        allow_credentials=True,
        allow_methods=['*'],
        allow_headers=['*'],
    )
    fastapp.include_router(api_router, prefix=default_config.API_V1_STR)
    if music_events is not None:
        @fastapp.middleware('http')
        async def observe_music_fetch(request, call_next):
            response = await call_next(request)
            event = music_fetch_event(request.method, response.status_code,
                                      request.url.path, request.headers.get('user-agent', ''))
            if event is not None:
                try:
                    music_events.put_nowait(event)
                except Exception:
                    pass  # Playback serving never depends on audit-queue capacity.
            return response
    fastapp.mount('', StaticFiles(directory=default_config.AUDIO_SAVE_PATH), name='static')
    return fastapp


app = FastAPI(version='0.1.0')
fast_app = create_fastapp()


def start_server(host, port, music_events=None):
    import uvicorn
    LOGGER.info('API server started(http://%s:%s)', host, port)
    uvicorn.run(create_fastapp(music_events) if music_events is not None else fast_app,
                host=host, port=port)


class ApiServer:
    def __init__(self, config, music_events=None):
        self.config = config
        self.music_events = music_events

    def start(self):
        p = Process(target=start_server, kwargs={
            'host': self.config.API_SERVER_BIND_ADDRESS,
            'port': self.config.API_SERVER_PORT,
            'music_events': self.music_events,
        })
        p.start()
