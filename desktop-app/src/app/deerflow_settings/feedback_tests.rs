use super::*;

#[test]
fn deerflow_feedback_abandoned_retry_cannot_bind_a_new_signal_to_old_identity() {
    let mut skills = super::super::skills::SkillBrowser::default();
    skills.feedback = ready();
    let original = skills
        .feedback
        .submit_request(false, "original-key")
        .unwrap();
    skills.begin(&original);
    skills.transport_failed(&original);
    assert!(skills.mutations_pending());
    assert!(skills.feedback.pending_submit.is_some());
    assert!(skills.can_begin(&request("skill.feedback.get")));
    assert!(
        !skills.can_begin(&json!({"operation":"skill.feedback.submit", "request_key":"new-key"}))
    );
    skills.generation_mutation_completed(&request("skill.feedback.get"));
    assert!(skills.mutations_pending());
    skills.abandon_retry();
    assert!(!skills.mutations_pending());
    assert!(skills.feedback.pending_submit.is_none());
    let mut next = ready();
    next.selected.as_mut().unwrap().id = format!("sig_{}", "f".repeat(64));
    skills.feedback.selected = next.selected;
    skills.feedback.snapshot_ready = true;
    skills.feedback.evidence_ready = true;
    skills.feedback.selected_reads = vec![1];
    let second = skills.feedback.submit_request(false, "second-key").unwrap();
    skills.begin(&second);
    skills
        .feedback
        .accept(
            &second,
            json!({"feedback":feedback(&second), "current_revision":1, "replayed":false}),
        )
        .unwrap();
    skills.generation_mutation_completed(&second);
    assert!(!skills.mutations_pending());
}

fn signal() -> Value {
    json!({"id":format!("sig_{}", "a".repeat(64)),
        "target":{"source_id":"project", "source_identity":"b".repeat(64), "name":"workflow", "scope":"global"},
        "version":"c".repeat(64), "root_id":"root-one", "root_session_id":"session-one",
        "state_version":40, "feedback_revision":0, "attribution":"unassessed", "read_count":40,
        "run_count":2, "role_count":1, "completed_runs":1, "failed_runs":1, "cancelled_runs":0,
        "uncertain_runs":1, "collection_pending":true})
}

fn read(id: u64) -> Value {
    json!({"id":id, "run_id":"run-one", "session_id":"session-one", "attempt_id":"attempt-one",
        "role_id":"reviewer", "kind":"skill_load", "path":"SKILL.md", "hash":"d".repeat(64),
        "bytes":300, "observed_at":"2026-10-02T12:00:00Z", "outcome":"completed", "effects_uncertain":false})
}

fn request(operation: &str) -> Value {
    let mut request = json!({"operation":operation, "signal_id":signal()["id"]});
    if operation != "skill.feedback.get" {
        request["limit"] = json!(20);
    }
    request
}

fn ready() -> FeedbackBrowser {
    let mut browser = FeedbackBrowser::default();
    browser.choose(serde_json::from_value(signal()).unwrap());
    let reads = request("skill.signals.get");
    browser.begin(&reads);
    browser.accept(&reads, json!({"signal":signal(), "reads":(1..=20).map(read).collect::<Vec<_>>(), "next_cursor":"page-two"})).unwrap();
    let snapshot = request("skill.feedback.get");
    browser.begin(&snapshot);
    browser
        .accept(
            &snapshot,
            json!({"signal":signal(), "evidence_current":false}),
        )
        .unwrap();
    browser.summary = "工具调用失败源于网络超时".into();
    browser.kind = "network".into();
    browser.toggle_read(1);
    browser
}

fn feedback(request: &Value) -> Value {
    let signal = signal();
    json!({"signal_id":request["signal_id"], "revision":request["expected_feedback_version"].as_u64().unwrap()+1,
        "target":signal["target"], "skill_version":signal["version"], "root_id":signal["root_id"],
        "root_session_id":signal["root_session_id"], "signal_state_version":request["expected_signal_state_version"],
        "origin":"operator", "kind":request["kind"], "summary":request["summary"], "summary_redacted":false,
        "read_ids":request["read_ids"], "created_at":"2026-10-02T12:00:00Z"})
}

#[test]
fn deerflow_feedback_pins_identity_and_real_reads_across_bounded_pages() {
    let mut browser = ready();
    browser.toggle_read(1000); // Not present in returned evidence.
    assert_eq!(browser.selected_reads, vec![1]);
    let mut next = request("skill.signals.get");
    next["cursor"] = json!("page-two");
    browser.begin(&next);
    browser
        .accept(
            &next,
            json!({"signal":signal(), "reads":(21..=40).map(read).collect::<Vec<_>>()}),
        )
        .unwrap();
    for id in 21..=40 {
        browser.toggle_read(id);
    }
    assert_eq!(browser.reads.len(), 20);
    assert_eq!(browser.selected_reads.len(), 16);
    assert_eq!(browser.selected_reads[0], 1);
    let submit = browser.submit_request(false, "stable-key").unwrap();
    assert_eq!(submit["expected_signal_state_version"], 40);
    assert_eq!(submit["expected_feedback_version"], 0);
    assert_eq!(submit["signal_id"], signal()["id"]);
    assert_eq!(submit["read_ids"].as_array().unwrap().len(), 16);
    assert_eq!(submit["kind"], "network");
    assert!(submit.get("target").is_none());
    assert!(browser.submit_request(true, "key").is_none());
    browser.summary = "界".repeat(667);
    assert!(browser.submit_request(false, "key").is_none());
}

#[test]
fn deerflow_feedback_changed_evidence_and_service_revoke_authority_preserve_draft() {
    let mut browser = ready();
    let draft = browser.summary.clone();
    let mut newer = signal();
    newer["state_version"] = json!(41);
    let get = request("skill.feedback.get");
    browser.begin(&get);
    browser
        .accept(&get, json!({"signal":newer, "evidence_current":false}))
        .unwrap();
    assert!(browser.selected_reads.is_empty());
    assert!(!browser.evidence_ready);
    assert!(browser.snapshot_ready);
    assert!(browser.submit_request(false, "key").is_none());
    browser.invalidate();
    assert!(!browser.snapshot_ready);
    assert_eq!(browser.summary, draft);
    assert!(browser.selected_reads.is_empty());
}

#[test]
fn deerflow_feedback_old_callback_cannot_authorize_edited_or_reselected_draft() {
    let mut browser = ready();
    let get = request("skill.feedback.get");
    browser.begin(&get);
    browser.summary = "edited while callback was queued".into();
    browser.edit_generation += 1;
    browser
        .accept(&get, json!({"signal":signal(), "evidence_current":false}))
        .unwrap();
    assert!(!browser.snapshot_ready);
    let mut foreign = signal();
    foreign["id"] = json!(format!("sig_{}", "e".repeat(64)));
    browser.choose(serde_json::from_value(foreign).unwrap());
    assert!(
        browser
            .accept(&get, json!({"signal":signal(), "evidence_current":false}))
            .is_err()
    );
    assert_eq!(browser.summary, "edited while callback was queued");
}

#[test]
fn deerflow_feedback_replay_receipt_never_becomes_current_judgment() {
    let mut browser = ready();
    let submit = browser.submit_request(false, "stable-key").unwrap();
    browser.begin(&submit);
    browser.invalidate();
    browser.begin(&submit); // Transport replay preserves original identity.
    browser
        .accept(
            &submit,
            json!({"feedback":feedback(&submit), "current_revision":3, "replayed":true}),
        )
        .unwrap();
    assert!(browser.current.is_none());
    assert!(!browser.snapshot_ready);
    assert!(browser.last_result.as_ref().unwrap().contains("当前修订 3"));
    assert!(browser.pending_submit.is_none());
    assert!(!browser.summary.is_empty());
}

#[test]
fn deerflow_feedback_withdrawal_uses_observed_versions_and_explicit_empty_payload() {
    let mut browser = ready();
    let submit = browser.submit_request(false, "first").unwrap();
    let current = feedback(&submit);
    let mut revised = signal();
    revised["feedback_revision"] = json!(1);
    let get = request("skill.feedback.get");
    browser.begin(&get);
    browser
        .accept(
            &get,
            json!({"signal":revised, "feedback":current, "evidence_current":true}),
        )
        .unwrap();
    assert!(browser.evidence_current);
    assert!(browser.selected.as_ref().unwrap().collection_pending);
    assert_eq!(browser.selected.as_ref().unwrap().attribution, "unassessed");
    let withdrawal = browser.submit_request(true, "withdraw-key").unwrap();
    assert_eq!(withdrawal["kind"], "withdrawn");
    assert_eq!(withdrawal["summary"], "");
    assert_eq!(withdrawal["read_ids"], json!([]));
    assert_eq!(withdrawal["expected_feedback_version"], 1);
    assert_eq!(withdrawal["expected_signal_state_version"], 40);
}

#[test]
fn deerflow_feedback_rejects_foreign_and_malformed_evidence_and_history() {
    let mut browser = ready();
    let reads = request("skill.signals.get");
    for invalid in [
        json!({"signal":signal()}),
        json!({"signal":signal(), "reads":null}),
        json!({"signal":signal(), "reads":(1..=21).map(read).collect::<Vec<_>>()}),
        json!({"signal":signal(), "reads":[read(1), read(1)]}),
    ] {
        assert!(browser.accept(&reads, invalid).is_err());
    }
    let submit = browser.submit_request(false, "key").unwrap();
    browser.begin(&submit);
    let mut foreign = feedback(&submit);
    foreign["target"]["source_identity"] = json!("e".repeat(64));
    assert!(
        browser
            .accept(
                &submit,
                json!({"feedback":foreign, "current_revision":1, "replayed":false})
            )
            .is_err()
    );
    assert!(browser.pending_submit.is_some());
    let mut wrong_ids = feedback(&submit);
    wrong_ids["read_ids"] = json!([2]);
    assert!(
        browser
            .accept(
                &submit,
                json!({"feedback":wrong_ids, "current_revision":1, "replayed":false})
            )
            .is_err()
    );
    let history = request("skill.feedback.list");
    browser
        .accept(
            &history,
            json!({"revisions":[feedback(&submit)], "current_revision":1}),
        )
        .unwrap();
    assert!(!browser.snapshot_ready); // A newer current revision revokes old CAS.
    assert!(browser.current.is_none());
    let mut other = history.clone();
    other["signal_id"] = json!(format!("sig_{}", "f".repeat(64)));
    assert!(
        browser
            .accept(&other, json!({"revisions":[], "current_revision":0}))
            .is_err()
    );
}
