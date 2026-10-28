//! Ordered, bounded catch-up before a subscription consumes live events.
use std::collections::{HashMap, VecDeque};
use std::time::{Duration, Instant};

use anyhow::{bail, ensure};
use crossbeam_channel::{Receiver, Sender, unbounded};
use uuid::Uuid;
use waku_protocol::{
    Command, ReplayCursor, Request, ResponseOutcome, ResponsePayload, SequencedEvent,
};

type Key = (Uuid, Uuid);
const PAGE_SIZE: usize = 256;
const MAX_LIVE_EVENTS: usize = 4096;
const MAX_LIVE_BYTES: usize = 64 * 1024 * 1024;
const REPLAY_TIMEOUT: Duration = Duration::from_secs(120);

struct Subscription {
    events: Sender<SequencedEvent>,
    catching_up: bool,
    through: Option<(Uuid, u64)>,
    live: VecDeque<SequencedEvent>,
    live_bytes: usize,
}

struct PageRequest {
    key: Key,
    epoch: Option<Uuid>,
    after: u64,
    through: Option<u64>,
    started: Instant,
}

#[derive(Default)]
pub(super) struct Delivery {
    subscriptions: HashMap<Key, Subscription>,
    cursors: HashMap<Key, ReplayCursor>,
    requests: HashMap<Uuid, PageRequest>,
}

impl Delivery {
    pub(super) fn new(cursors: Vec<ReplayCursor>) -> Self {
        Self {
            cursors: cursors
                .into_iter()
                .map(|c| ((c.session_id, c.runtime_id), c))
                .collect(),
            ..Self::default()
        }
    }

    pub(super) fn subscribe(
        &mut self,
        session: Uuid,
        runtime: Uuid,
    ) -> (Receiver<SequencedEvent>, Request) {
        let key = (session, runtime);
        self.unsubscribe(session, runtime);
        let (events, receiver) = unbounded();
        self.subscriptions.insert(
            key,
            Subscription {
                events,
                catching_up: true,
                through: None,
                live: VecDeque::new(),
                live_bytes: 0,
            },
        );
        let cursor = self.cursors.get(&key);
        let (epoch, after) = cursor
            .map(|c| (Some(c.epoch), c.sequence))
            .unwrap_or((None, 0));
        (receiver, self.page_request(key, epoch, after, None))
    }

    fn page_request(
        &mut self,
        key: Key,
        epoch: Option<Uuid>,
        after: u64,
        through: Option<u64>,
    ) -> Request {
        let request_id = Uuid::new_v4();
        self.requests.insert(
            request_id,
            PageRequest {
                key,
                epoch,
                after,
                through,
                started: Instant::now(),
            },
        );
        Request {
            request_id,
            session_id: key.0,
            runtime_id: key.1,
            command: Command::ReplayEvents {
                epoch,
                after_sequence: after,
                through_sequence: through,
                limit: PAGE_SIZE,
            },
        }
    }

    pub(super) fn unsubscribe(&mut self, session: Uuid, runtime: Uuid) {
        let key = (session, runtime);
        self.subscriptions.remove(&key);
        self.requests.retain(|_, page| page.key != key);
    }

    pub(super) fn cursors(&self) -> Vec<ReplayCursor> {
        self.cursors.values().cloned().collect()
    }
    pub(super) fn is_replay(&self, id: Uuid) -> bool {
        self.requests.contains_key(&id)
    }
    pub(super) fn expired(&self) -> bool {
        self.requests
            .values()
            .any(|p| p.started.elapsed() >= REPLAY_TIMEOUT)
    }
    pub(super) fn close(&mut self) {
        self.subscriptions.clear();
        self.requests.clear();
    }

    pub(super) fn live(&mut self, event: SequencedEvent) -> anyhow::Result<()> {
        let key = (event.session_id, event.runtime_id);
        let Some(subscription) = self.subscriptions.get_mut(&key) else {
            // The journal is authoritative. An absent consumer must not advance
            // a cursor or silently evict a pre-subscription replay buffer.
            return Ok(());
        };
        if !event.replayable {
            // PTY bytes are meaningful only to this live emulator, including
            // bytes racing its initial empty replay page. They carry no
            // transcript acknowledgement and must not wait behind catch-up.
            if subscription.events.send(event).is_err() {
                self.unsubscribe(key.0, key.1);
            }
            return Ok(());
        }
        if subscription.catching_up {
            if let Some((epoch, through)) = subscription.through {
                ensure!(event.epoch == epoch, "runtime epoch changed during replay");
                if event.sequence <= through {
                    return Ok(());
                }
            }
            let bytes = serde_json::to_vec(&event)?.len();
            ensure!(
                subscription.live.len() < MAX_LIVE_EVENTS
                    && subscription.live_bytes.saturating_add(bytes) <= MAX_LIVE_BYTES,
                "live events exceeded the catch-up buffer; reconnect from the confirmed cursor"
            );
            subscription.live_bytes += bytes;
            subscription.live.push_back(event);
            return Ok(());
        }
        self.deliver(event);
        Ok(())
    }

    fn deliver(&mut self, event: SequencedEvent) {
        let key = (event.session_id, event.runtime_id);
        if self
            .cursors
            .get(&key)
            .is_some_and(|c| c.epoch == event.epoch && c.sequence >= event.sequence)
        {
            return;
        }
        let cursor = ReplayCursor {
            session_id: key.0,
            runtime_id: key.1,
            epoch: event.epoch,
            sequence: event.sequence,
        };
        if self
            .subscriptions
            .get(&key)
            .is_some_and(|s| s.events.send(event).is_ok())
        {
            self.cursors.insert(key, cursor);
        } else {
            self.unsubscribe(key.0, key.1);
        }
    }

    pub(super) fn page(
        &mut self,
        id: Uuid,
        outcome: ResponseOutcome,
    ) -> anyhow::Result<Option<Request>> {
        let Some(page) = self.requests.remove(&id) else {
            return Ok(None);
        };
        let ResponseOutcome::Ok {
            payload:
                ResponsePayload::ReplayEvents {
                    epoch,
                    events,
                    through_sequence,
                    next_sequence,
                    has_more,
                },
        } = outcome
        else {
            bail!("daemon could not supply the complete runtime replay; cursor retained");
        };
        let after = if page.epoch == Some(epoch) {
            page.after
        } else {
            ensure!(
                page.through.is_none(),
                "runtime epoch changed between replay pages"
            );
            0
        };
        ensure!(
            page.through
                .is_none_or(|through| through == through_sequence),
            "replay boundary changed"
        );
        ensure!(
            events.len() <= PAGE_SIZE
                && after <= next_sequence
                && next_sequence <= through_sequence,
            "invalid replay page bounds"
        );
        ensure!(
            !has_more || next_sequence > after && next_sequence < through_sequence,
            "replay page made no progress"
        );
        ensure!(
            !has_more
                || events
                    .last()
                    .is_some_and(|event| event.sequence == next_sequence),
            "replay page acknowledged an event it did not supply"
        );
        ensure!(
            has_more || next_sequence == through_sequence,
            "replay ended before its boundary"
        );
        let mut previous = after;
        for event in &events {
            ensure!(
                event.replayable
                    && (event.session_id, event.runtime_id) == page.key
                    && event.epoch == epoch
                    && event.sequence > previous
                    && event.sequence <= next_sequence,
                "replay event identity or order mismatch"
            );
            previous = event.sequence;
        }
        // Validate the entire page before exposing any of it to the consumer.
        if !self.subscriptions.contains_key(&page.key) {
            return Ok(None);
        }
        for event in events {
            self.deliver(event);
        }
        let Some(subscription) = self.subscriptions.get_mut(&page.key) else {
            return Ok(None);
        };
        self.cursors.insert(
            page.key,
            ReplayCursor {
                session_id: page.key.0,
                runtime_id: page.key.1,
                epoch,
                sequence: next_sequence,
            },
        );
        subscription.through = Some((epoch, through_sequence));
        // Everything through the fixed snapshot is supplied by the journal;
        // non-replayable terminal bytes intentionally do not survive reconnect.
        subscription
            .live
            .retain(|e| e.epoch != epoch || e.sequence > through_sequence);
        subscription.live_bytes = subscription
            .live
            .iter()
            .map(|e| serde_json::to_vec(e).map_or(MAX_LIVE_BYTES, |b| b.len()))
            .sum();
        if has_more {
            return Ok(Some(self.page_request(
                page.key,
                Some(epoch),
                next_sequence,
                Some(through_sequence),
            )));
        }
        subscription.catching_up = false;
        subscription.live_bytes = 0;
        let live = std::mem::take(&mut subscription.live);
        for event in live {
            ensure!(event.epoch == epoch, "runtime epoch changed during replay");
            self.deliver(event);
        }
        Ok(None)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use waku_protocol::WireDriverEvent;
    fn event(key: Key, epoch: Uuid, sequence: u64) -> SequencedEvent {
        SequencedEvent {
            session_id: key.0,
            runtime_id: key.1,
            epoch,
            sequence,
            replayable: true,
            event: WireDriverEvent::new("textDelta", serde_json::json!("x")),
        }
    }
    fn response(
        epoch: Uuid,
        events: Vec<SequencedEvent>,
        through: u64,
        next: u64,
        more: bool,
    ) -> ResponseOutcome {
        ResponseOutcome::Ok {
            payload: ResponsePayload::ReplayEvents {
                epoch,
                events,
                through_sequence: through,
                next_sequence: next,
                has_more: more,
            },
        }
    }
    #[test]
    fn ephemeral_events_arriving_before_initial_page_are_delivered_immediately() {
        let key = (Uuid::new_v4(), Uuid::new_v4());
        let epoch = Uuid::new_v4();
        let mut delivery = Delivery::default();
        let (rx, request) = delivery.subscribe(key.0, key.1);
        for sequence in 1..=2 {
            let mut terminal = event(key, epoch, sequence);
            terminal.replayable = false;
            terminal.event = WireDriverEvent::new("terminalOutput", serde_json::json!("ready"));
            delivery.live(terminal).unwrap();
            assert_eq!(rx.try_recv().unwrap().sequence, sequence);
            assert!(delivery.cursors().is_empty());
        }
        assert!(
            delivery
                .page(request.request_id, response(epoch, vec![], 2, 2, false))
                .unwrap()
                .is_none()
        );
        assert!(rx.is_empty());
        let mut terminal = event(key, epoch, 3);
        terminal.replayable = false;
        delivery.live(terminal).unwrap();
        assert_eq!(rx.try_recv().unwrap().sequence, 3);
        assert_eq!(delivery.cursors()[0].sequence, 2);
    }

    #[test]
    fn replay_rejects_empty_partial_pages_and_ephemeral_events_without_advancing() {
        let key = (Uuid::new_v4(), Uuid::new_v4());
        let epoch = Uuid::new_v4();
        let mut delivery = Delivery::default();
        let (rx, request) = delivery.subscribe(key.0, key.1);
        assert!(
            delivery
                .page(request.request_id, response(epoch, vec![], 10, 5, true))
                .is_err()
        );
        assert!(rx.is_empty());
        assert!(delivery.cursors().is_empty());
        let (rx, request) = delivery.subscribe(key.0, key.1);
        let mut terminal = event(key, epoch, 1);
        terminal.replayable = false;
        assert!(
            delivery
                .page(
                    request.request_id,
                    response(epoch, vec![terminal], 1, 1, false)
                )
                .is_err()
        );
        assert!(rx.is_empty());
        assert!(delivery.cursors().is_empty());
    }

    #[test]
    fn replay_recovers_more_than_both_old_buffers_before_live_events() {
        let key = (Uuid::new_v4(), Uuid::new_v4());
        let epoch = Uuid::new_v4();
        let mut delivery = Delivery::default();
        for n in 1..=10000 {
            delivery.live(event(key, epoch, n)).unwrap();
        }
        assert!(delivery.cursors().is_empty());
        let (rx, mut request) = delivery.subscribe(key.0, key.1);
        delivery.live(event(key, epoch, 10001)).unwrap();
        assert!(rx.is_empty());
        let mut after = 0;
        loop {
            let next = (after + 256).min(10000);
            let events = (after + 1..=next).map(|n| event(key, epoch, n)).collect();
            let following = delivery
                .page(
                    request.request_id,
                    response(epoch, events, 10000, next, next < 10000),
                )
                .unwrap();
            after = next;
            match following {
                Some(page) => request = page,
                None => break,
            }
        }
        assert_eq!(
            rx.try_iter().map(|e| e.sequence).collect::<Vec<_>>(),
            (1..=10001).collect::<Vec<_>>()
        );
        assert_eq!(delivery.cursors()[0].sequence, 10001);
    }
    #[test]
    fn replay_validates_scope_and_preserves_cursor_on_failure() {
        let key = (Uuid::new_v4(), Uuid::new_v4());
        let epoch = Uuid::new_v4();
        let mut delivery = Delivery::new(vec![ReplayCursor {
            session_id: key.0,
            runtime_id: key.1,
            epoch,
            sequence: 7,
        }]);
        let (rx, request) = delivery.subscribe(key.0, key.1);
        delivery.live(event(key, epoch, 20)).unwrap();
        let wrong = event((key.0, Uuid::new_v4()), epoch, 8);
        assert!(
            delivery
                .page(
                    request.request_id,
                    response(epoch, vec![wrong], 10, 10, false)
                )
                .is_err()
        );
        assert_eq!(delivery.cursors()[0].sequence, 7);
        assert!(rx.is_empty());
    }
    #[test]
    fn replay_accepts_ephemeral_holes_and_new_epochs_without_duplicates() {
        let key = (Uuid::new_v4(), Uuid::new_v4());
        let epoch = Uuid::new_v4();
        let mut delivery = Delivery::new(vec![ReplayCursor {
            session_id: key.0,
            runtime_id: key.1,
            epoch: Uuid::new_v4(),
            sequence: 999,
        }]);
        let (rx, request) = delivery.subscribe(key.0, key.1);
        delivery.live(event(key, epoch, 3)).unwrap();
        delivery
            .page(
                request.request_id,
                response(
                    epoch,
                    vec![event(key, epoch, 1), event(key, epoch, 3)],
                    10,
                    10,
                    false,
                ),
            )
            .unwrap();
        delivery.live(event(key, epoch, 3)).unwrap();
        delivery.live(event(key, epoch, 12)).unwrap();
        assert_eq!(
            rx.try_iter().map(|e| e.sequence).collect::<Vec<_>>(),
            vec![1, 3, 12]
        );
    }
    #[test]
    fn replay_overflow_is_explicit_and_never_advances_the_cursor() {
        let key = (Uuid::new_v4(), Uuid::new_v4());
        let epoch = Uuid::new_v4();
        let mut delivery = Delivery::default();
        let (rx, _) = delivery.subscribe(key.0, key.1);
        for n in 1..=MAX_LIVE_EVENTS {
            delivery.live(event(key, epoch, n as u64)).unwrap();
        }
        assert!(delivery.live(event(key, epoch, 5000)).is_err());
        assert!(rx.is_empty());
        assert!(delivery.cursors().is_empty());
    }
}
