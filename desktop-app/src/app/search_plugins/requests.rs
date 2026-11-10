//! Keep transport activity separate from user-visible refresh activity.

#[derive(Default)]
pub(super) struct SearchRequests {
    pending: bool,
    mutating: bool,
    refreshing: bool,
    generation: u64,
}

impl SearchRequests {
    pub(super) fn begin(&mut self, mutating: bool) -> Option<u64> {
        if self.pending && (!mutating || self.mutating) {
            return None;
        }
        self.pending = true;
        self.mutating = mutating;
        self.generation += 1;
        Some(self.generation)
    }

    pub(super) fn begin_refresh(&mut self) -> bool {
        if self.refresh_disabled() {
            return false;
        }
        // An in-flight background read can also satisfy a manual refresh.
        self.refreshing = true;
        true
    }

    pub(super) fn finish(&mut self, generation: u64) -> bool {
        if generation != self.generation {
            return false;
        }
        self.pending = false;
        self.mutating = false;
        self.refreshing = false;
        true
    }

    pub(super) fn mutating(&self) -> bool {
        self.mutating
    }

    pub(super) fn refresh_disabled(&self) -> bool {
        self.mutating || self.refreshing
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn background_polls_keep_refresh_enabled_and_deduplicate_reads() {
        let mut requests = SearchRequests::default();
        for _ in 0..4 {
            let generation = requests.begin(false).unwrap();
            assert!(!requests.refresh_disabled());
            assert_eq!(requests.begin(false), None);
            assert!(!requests.refresh_disabled());
            assert!(requests.finish(generation));
            assert!(!requests.refresh_disabled());
        }
    }

    #[test]
    fn manual_refresh_reuses_a_background_read_and_blocks_duplicate_clicks() {
        let mut requests = SearchRequests::default();
        let generation = requests.begin(false).unwrap();
        assert!(requests.begin_refresh());
        assert!(requests.refresh_disabled());
        assert!(!requests.begin_refresh());
        assert_eq!(requests.begin(false), None);
        assert!(requests.finish(generation));
        assert!(!requests.refresh_disabled());
    }

    #[test]
    fn manual_refresh_without_a_background_read_finishes_normally() {
        let mut requests = SearchRequests::default();
        assert!(requests.begin_refresh());
        let generation = requests.begin(false).unwrap();
        assert!(requests.refresh_disabled());
        assert!(!requests.mutating());
        assert!(requests.finish(generation));
        assert!(!requests.refresh_disabled());
    }

    #[test]
    fn stale_read_cannot_unlock_a_save_that_superseded_it() {
        let mut requests = SearchRequests::default();
        assert!(requests.begin_refresh());
        let read = requests.begin(false).unwrap();
        let save = requests.begin(true).unwrap();
        assert!(!requests.finish(read));
        assert!(requests.refresh_disabled());
        assert!(requests.mutating());
        assert!(!requests.begin_refresh());
        assert_eq!(requests.begin(false), None);
        assert_eq!(requests.begin(true), None);
        assert!(requests.finish(save));
        assert!(!requests.refresh_disabled());
        assert!(!requests.mutating());
    }
}
