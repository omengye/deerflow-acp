"""Coordinate device turns without holding a lock across STT, LLM or TTS calls."""

from dataclasses import dataclass, field
import threading
import uuid


@dataclass
class VoiceTurn:
    dkey: str
    role: str
    token: str
    recording_id: int
    generation: int
    request_id: str = field(default_factory=lambda: uuid.uuid4().hex)
    cancel_event: threading.Event = field(default_factory=threading.Event)
    sent_count: int = 0
    terminal_sent: bool = False
    claimed: bool = False
    music_closed: bool = False


class ConversationControl:
    def __init__(self):
        self._lock = threading.RLock()
        self._turns = {}
        self._generations = {}

    def _cancel_locked(self, dkey):
        previous = self._turns.pop(dkey, None)
        if previous is not None:
            previous.cancel_event.set()
        self._generations[dkey] = self._generations.get(dkey, 0) + 1

    def begin(self, dkey, role, token, recording_id):
        with self._lock:
            self._cancel_locked(dkey)
            turn = VoiceTurn(dkey, str(role), token, recording_id,
                             self._generations[dkey])
            self._turns[dkey] = turn
            return turn

    def cancel(self, dkey, action=None):
        """Cancel and optionally reset local state atomically with new turns.

        action must perform local work only; it must never wait for a network.
        """
        with self._lock:
            self._cancel_locked(dkey)
            if action is not None:
                return action()

    def cancel_all(self):
        with self._lock:
            for dkey in list(self._turns):
                self._cancel_locked(dkey)

    def _is_current_locked(self, turn):
        return (turn is not None and not turn.cancel_event.is_set()
                and self._turns.get(turn.dkey) is turn)

    def is_current(self, turn):
        with self._lock:
            return self._is_current_locked(turn)

    def generation(self, dkey):
        with self._lock:
            return self._generations.get(dkey, 0)

    def current(self, dkey=None, request_id=None):
        with self._lock:
            values = [self._turns.get(dkey)] if dkey else list(self._turns.values())
            return next((turn for turn in values if self._is_current_locked(turn)
                         and (request_id is None or turn.request_id == request_id)), None)

    def apply_idle_generation(self, dkey, generation, action):
        """An admin music job may follow an ended turn, never a new one."""
        with self._lock:
            if self._generations.get(dkey, 0) != generation:
                return False
            turn = self._turns.get(dkey)
            if self._is_current_locked(turn) and not turn.terminal_sent:
                return False
            action()
            return True

    def claim(self, turn):
        """A duplicate recording END must not submit the same turn twice."""
        with self._lock:
            if not self._is_current_locked(turn) or turn.claimed:
                return False
            turn.claimed = True
            return True

    def apply(self, turn, action):
        """Run a short state update or MQTT publish only for the active turn."""
        with self._lock:
            if not self._is_current_locked(turn):
                return False
            action()
            return True


_creation_lock = threading.Lock()


def get_conversation_control(app):
    with _creation_lock:
        control = getattr(app, 'conversation_control', None)
        if control is None:
            control = ConversationControl()
            app.conversation_control = control
        return control
