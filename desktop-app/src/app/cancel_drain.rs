#[derive(Default)]
pub(super) struct CancelDrain {
    pending: bool,
    followups: usize,
}

impl CancelDrain {
    pub(super) fn begin(&mut self) {
        self.pending = true;
        self.followups = 0;
    }

    pub(super) fn is_pending(&self) -> bool {
        self.pending
    }

    pub(super) fn queue_followup(&mut self) -> usize {
        let position = self.followups;
        self.followups += 1;
        position
    }

    pub(super) fn finish(&mut self) -> bool {
        let followup = self.pending && self.followups > 0;
        self.pending = false;
        self.followups = 0;
        followup
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn stop_waits_for_cleanup_without_resuming_old_queue() {
        let mut drain = CancelDrain::default();
        drain.begin();
        assert!(drain.is_pending());
        assert!(!drain.finish());
        assert!(!drain.is_pending());
    }

    #[test]
    fn a_new_submission_waits_for_the_cancelled_turn_to_finish() {
        let mut drain = CancelDrain::default();
        drain.begin();
        assert_eq!(drain.queue_followup(), 0);
        assert_eq!(drain.queue_followup(), 1);
        assert!(drain.is_pending());
        assert!(drain.finish());
        assert!(!drain.is_pending());
        assert!(!drain.finish());
    }

    #[test]
    fn another_stop_revokes_the_pending_followup() {
        let mut drain = CancelDrain::default();
        drain.begin();
        drain.queue_followup();
        drain.begin();
        assert!(!drain.finish());
    }
}
