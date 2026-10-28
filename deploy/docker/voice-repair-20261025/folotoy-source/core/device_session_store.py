"""Private restart state for device UDP authentication; no voice or LLM history."""

import json
import os
from pathlib import Path
import re
import stat
import threading
import uuid


class DeviceSessionStateError(Exception):
    """Fixed errors deliberately never contain device authentication tokens."""


class DeviceSessionStore:
    MAX_DEVICES = 256
    MAX_BYTES = 65536
    MAX_MSG_ID = (1 << 53) - 1

    def __init__(self, path, namespace):
        self.path = Path(path).expanduser().absolute()
        self.namespace = namespace
        self._lock = threading.RLock()
        if not self._identifier(namespace):
            raise DeviceSessionStateError('invalid device state namespace')

    @classmethod
    def from_config(cls, config):
        explicit = os.environ.get('FOLOTOY_DEVICE_SESSIONS_PATH')
        path = (explicit or Path(getattr(config, 'DEERFLOW_STATE_PATH', './data/deerflow-sessions.sqlite3'))
                .expanduser().absolute().parent / 'private' / 'device-sessions.json')
        return cls(path, config.PROJECT_NAME)

    @staticmethod
    def _identifier(value):
        return isinstance(value, str) and re.fullmatch(r'[A-Za-z0-9_-]{1,64}', value) is not None

    @staticmethod
    def _integer(value, minimum, maximum):
        return type(value) is int and minimum <= value <= maximum

    def _check_parent(self, create):
        # No symlink is accepted for the private file or any ancestor. The
        # configured data root itself may be a Docker bind mount.
        for ancestor in (self.path, *self.path.parents):
            if ancestor.is_symlink():
                raise DeviceSessionStateError('device state symlink rejected')
        if create:
            self.path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        if not self.path.parent.exists():
            return False
        metadata = self.path.parent.stat()
        if not stat.S_ISDIR(metadata.st_mode):
            raise DeviceSessionStateError('device state parent is not a directory')
        if os.name == 'posix' and (stat.S_IMODE(metadata.st_mode) != 0o700 or metadata.st_uid != os.geteuid()):
            raise DeviceSessionStateError('device state parent permissions rejected')
        return True

    @staticmethod
    def _check_file(metadata):
        if not stat.S_ISREG(metadata.st_mode) or metadata.st_nlink != 1:
            raise DeviceSessionStateError('device state must be one regular private file')
        if os.name == 'posix' and (stat.S_IMODE(metadata.st_mode) != 0o600 or metadata.st_uid != os.geteuid()):
            raise DeviceSessionStateError('device state file permissions rejected')

    @staticmethod
    def _unique_object(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise DeviceSessionStateError('duplicate device state field')
            result[key] = value
        return result

    def validate(self, document):
        if (not isinstance(document, dict) or set(document) != {'version', 'namespace', 'devices'}
                or type(document['version']) is not int or document['version'] != 1
                or document['namespace'] != self.namespace
                or not isinstance(document['devices'], list)
                or len(document['devices']) > self.MAX_DEVICES):
            raise DeviceSessionStateError('invalid device state envelope')
        seen_devices, seen_tokens = set(), set()
        for entry in document['devices']:
            if (not isinstance(entry, dict) or set(entry) != {'device', 'token', 'role', 'msg_id'}
                    or not self._identifier(entry['device'])
                    or not isinstance(entry['token'], str)
                    or re.fullmatch(r'[0-9a-f]{32}', entry['token']) is None
                    or not isinstance(entry['role'], str)
                    or re.fullmatch(r'[1-9][0-9]{0,2}', entry['role']) is None
                    or not self._integer(entry['msg_id'], -1, self.MAX_MSG_ID)
                    or entry['device'] in seen_devices or entry['token'] in seen_tokens):
                raise DeviceSessionStateError('invalid device state entry')
            seen_devices.add(entry['device'])
            seen_tokens.add(entry['token'])
        return document

    def load(self):
        with self._lock:
            if not self._check_parent(create=False) or not self.path.exists():
                return []
            descriptor = os.open(self.path, os.O_RDONLY | getattr(os, 'O_NOFOLLOW', 0))
            with os.fdopen(descriptor, 'rb') as stream:
                self._check_file(os.fstat(stream.fileno()))
                data = stream.read(self.MAX_BYTES + 1)
            if len(data) > self.MAX_BYTES:
                raise DeviceSessionStateError('device state is too large')
            try:
                document = json.loads(data.decode('utf-8'), object_pairs_hook=self._unique_object)
            except (ValueError, UnicodeError, RecursionError) as error:
                raise DeviceSessionStateError('device state JSON rejected') from error
            return self.validate(document)['devices']

    def save(self, records):
        document = self.validate({'version': 1, 'namespace': self.namespace, 'devices': records})
        data = (json.dumps(document, ensure_ascii=True, separators=(',', ':')) + '\n').encode('utf-8')
        if len(data) > self.MAX_BYTES:
            raise DeviceSessionStateError('device state is too large')
        with self._lock:
            self._check_parent(create=True)
            if self.path.exists():
                self._check_file(self.path.lstat())
            temporary = self.path.with_name('.' + self.path.name + '.' + uuid.uuid4().hex)
            try:
                descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, 'O_NOFOLLOW', 0), 0o600)
                with os.fdopen(descriptor, 'wb') as stream:
                    stream.write(data)
                    stream.flush()
                    os.fsync(stream.fileno())
                os.replace(temporary, self.path)
                if os.name == 'posix':
                    directory = os.open(self.path.parent, os.O_RDONLY | getattr(os, 'O_DIRECTORY', 0))
                    try:
                        os.fsync(directory)
                    finally:
                        os.close(directory)
            finally:
                temporary.unlink(missing_ok=True)
