use super::*;

fn diagnostic_fixture() -> Value {
    json!({"session_id":"session-a","consent_epoch":2,"contribute":true,"running":true,
        "queue":{"pending":1,"queued":0,"ready":0,"blocked":1,"skipped":0,"failed":1,
            "failed_jobs":1,"capture_limited_sessions":0,"reasons":[{"code":"tree_unsettled","count":1}]},
        "inputs":[{"run_id":"run-a","root_id":"run-a","episode_id":"lep_a","state":"pending",
            "reason":"tree_unsettled","ended_at":"2026-10-28T01:00:00Z","job_id":"","run_status":"paused"}],
        "jobs":[{"id":"smj_extract","episode_id":"lep_a","kind":"experience_extract","state":"failed",
            "state_version":4,"attempts":1,"error_code":"temporary","deadline":"2026-10-29T01:00:00Z",
            "retryable":true,"retry_reason":""}],"truncated":false})
}

fn diagnostic_browser() -> LearningBrowser {
    let mut browser = ready();
    read(
        &mut browser,
        json!({"operation":"skill.learning.control.get","session_id":"session-a"}),
        control("session-a", 2, true, false),
    );
    let query = browser.diagnostics_request().unwrap();
    read(&mut browser, query, diagnostic_fixture());
    browser
}

#[test]
fn learning_diagnostics_keep_counts_metadata_and_unknown_reasons_visible() {
    let browser = diagnostic_browser();
    let diagnostics = browser.diagnostics.unwrap();
    assert!(diagnostics.queue.summary().contains("失败作业 1"));
    assert!(
        diagnostics
            .queue
            .reasons_summary()
            .contains("原任务或子任务尚未结算")
    );
    assert!(learning_diagnostics::reason("future_blocker").contains("future_blocker"));
    let old = ready();
    assert!(old.status.as_ref().unwrap().queue.is_none());
    assert!(old.status_summary.unwrap().contains("状态未知"));
}

#[test]
fn learning_diagnostics_reject_foreign_overlarge_and_invalid_metadata_atomically() {
    let mut browser = diagnostic_browser();
    let query = browser.diagnostics_request().unwrap();
    for field in ["session_id", "consent_epoch", "inputs", "jobs"] {
        let mut bad = diagnostic_fixture();
        bad[field] = match field {
            "session_id" => json!("session-b"),
            "consent_epoch" => json!(u64::MAX),
            "inputs" => json!(vec![
                bad["inputs"][0].clone();
                learning_diagnostics::LIMIT + 1
            ]),
            _ => {
                let mut job = bad["jobs"][0].clone();
                job["state"] = json!("completed");
                json!([job])
            }
        };
        browser.begin(&query);
        assert!(browser.accept(&query, bad).is_err(), "{field}");
        assert_eq!(
            browser.diagnostics.as_ref().unwrap().session_id,
            "session-a"
        );
    }
}

#[test]
fn learning_diagnostics_stale_session_or_selection_cannot_authorize_retry() {
    let mut browser = diagnostic_browser();
    let query = browser.diagnostics_request().unwrap();
    browser.begin(&query);
    browser.choose_pane(Pane::Jobs);
    browser.diagnostics = None;
    browser.accept(&query, diagnostic_fixture()).unwrap();
    assert!(browser.diagnostics.is_none());
    browser.begin(&query);
    browser.set_session(Some("session-b"));
    browser.accept(&query, diagnostic_fixture()).unwrap();
    assert!(browser.diagnostics.is_none());
    assert!(!browser.can_begin(&query));
}

#[test]
fn extraction_retry_requires_exact_job_and_current_consent_versions() {
    let mut browser = diagnostic_browser();
    let request = browser.extraction_retry_request("smj_extract").unwrap();
    assert_eq!(
        request,
        json!({"operation":"skill.learning.extraction.retry","session_id":"session-a",
        "job_id":"smj_extract","expected_epoch":2,"expected_state_version":4})
    );
    assert!(mutation(&request));
    assert!(browser.can_begin(&request));
    for field in [
        "session_id",
        "job_id",
        "expected_epoch",
        "expected_state_version",
    ] {
        let mut changed = request.clone();
        changed[field] = if field.starts_with("expected_") {
            json!(9)
        } else {
            json!("other")
        };
        assert!(!browser.can_begin(&changed), "{field}");
    }
    browser.control.as_mut().unwrap().consent_epoch = 3;
    assert!(browser.extraction_retry_request("smj_extract").is_none());
    assert!(!browser.can_begin(&request));
}

#[test]
fn extraction_retry_unknown_transport_uses_read_only_diagnostics_without_replay() {
    let mut browser = diagnostic_browser();
    let request = browser.extraction_retry_request("smj_extract").unwrap();
    browser.begin(&request);
    browser.invalidate();
    assert!(!browser.can_begin(&request));
    let query = browser.reconciliation_request().unwrap();
    assert_eq!(query["operation"], "skill.learning.diagnostics");
    browser.set_session(Some("session-b"));
    assert!(browser.can_begin(&query));
    browser.begin(&query);
    browser.accept(&query, diagnostic_fixture()).unwrap();
    assert!(browser.diagnostics.is_none());
    assert_eq!(browser.pending_request(), Some(&request));
    assert!(
        browser
            .reconciliation
            .as_ref()
            .unwrap()
            .contains("不是原重试请求的回执")
    );
}

#[test]
fn extraction_retry_receipt_must_match_original_job_and_version() {
    let mut browser = diagnostic_browser();
    let request = browser.extraction_retry_request("smj_extract").unwrap();
    browser.begin(&request);
    let mut job = diagnostic_fixture()["jobs"][0].clone();
    job["state"] = json!("queued");
    assert!(browser.accept(&request, json!({"job":job})).is_err());
    job["state_version"] = json!(5);
    browser.accept(&request, json!({"job":job})).unwrap();
    assert!(browser.pending_request().is_none());
    assert!(browser.diagnostics.is_none());
    assert!(browser.last_result.as_ref().unwrap().contains("已重新排队"));
}

fn status() -> Value {
    json!({"version":"procedural-experience-v1", "enabled":true, "mode":"shadow", "idle_seconds":300,
        "pending_inputs":0, "experiences":1, "consolidation_enabled":true, "patterns":1,
        "matching_enabled":true, "cross_session_enabled":true, "matches":1, "proposal_discovery_enabled":false})
}

fn control(session: &str, epoch: u64, contribute: bool, shared: bool) -> Value {
    json!({"session_id":session, "contribute":contribute, "workspace_shared":shared, "consent_epoch":epoch})
}

fn ready() -> LearningBrowser {
    let mut browser = LearningBrowser::default();
    browser.set_session(Some("session-a"));
    read(
        &mut browser,
        json!({"operation":"skill.learning.status"}),
        status(),
    );
    read(
        &mut browser,
        json!({"operation":"skill.learning.control.get", "session_id":"session-a"}),
        control("session-a", 0, true, false),
    );
    browser
}

fn read(browser: &mut LearningBrowser, request: Value, data: Value) {
    assert!(browser.can_begin(&request));
    browser.begin(&request);
    browser.accept(&request, data).unwrap();
}

fn recipe() -> Value {
    json!({"kind":"proven_recipe", "goal":"Inspect a JSON file", "applicability":"PowerShell JSON validation",
        "resolution":"Read complete text, parse it, then check required fields", "steps":["Read full text", "Parse JSON", "Check fields"],
        "verification":"A local tool receipt is saved", "outcome":"observed_execution", "references":["input/accepted", "receipt/local"]})
}

fn experience(i: usize) -> Value {
    json!({"id":format!("lexp_{i:064x}"), "session_id":"session-a", "episode_id":"lep_a", "revision":1,
        "origin":"model_extracted", "quality":"unassessed", "experience":recipe(), "created_at":"2026-10-09T08:10:20.123456789Z"})
}

fn pattern() -> Value {
    json!({"id":format!("lpat_{}", "a".repeat(64)), "session_id":"session-a", "revision":2,
        "origin":"model_extracted", "quality":"unassessed", "representative_id":experience(1)["id"], "experience":recipe(),
        "experience_ids":[experience(1)["id"], experience(2)["id"]], "episode_ids":["lep_a", "lep_b"],
        "support_episodes":2, "observed_episodes":2, "evidence_state":"observed_execution", "source_state":"not_revalidated",
        "conflicts_with":[], "updated_at":"2026-10-09T08:20:00Z"})
}

fn matched() -> Value {
    json!({"id":format!("lmatch_{}", "c".repeat(64)), "session_id":"session-a", "origin":"model_matched", "quality":"unassessed",
        "pattern_id":pattern()["id"], "pattern_revision":2, "relation":"body_gap", "reason":"The recipe covers a missing parsing check",
        "target":{"source_id":"project", "source_identity":"d".repeat(64), "name":"json-audit", "scope":"workspace", "workspace":"D:\\workspace"},
        "head":{"version":"e".repeat(64), "state_version":2, "exists":true, "enabled":true, "managed":true},
        "actual_reads":[{"sourceId":"project", "sourceIdentity":"d".repeat(64), "name":"json-audit", "version":"f".repeat(64), "hash":"f".repeat(64)}],
        "evidence_state":"observed_execution", "source_state":"not_revalidated", "created_at":"2026-10-09T08:30:00Z"})
}

fn job() -> Value {
    json!({"id":"smj_a", "episode_id":"", "pattern_id":pattern()["id"], "pattern_revision":2,
        "kind":"skill_match", "state":"queued", "state_version":3, "attempts":0, "deadline":"0001-01-01T00:00:00Z"})
}

#[test]
fn learning_status_is_runtime_truth_not_an_automatic_publication_claim() {
    let request = json!({"operation":"skill.learning.status"});
    let mut browser = LearningBrowser::default();
    read(&mut browser, request.clone(), status());
    assert!(
        browser
            .status_summary
            .as_ref()
            .unwrap()
            .contains("仅积累经验")
    );
    let mut candidate = status();
    candidate["mode"] = json!("review_candidates");
    candidate["proposal_discovery_enabled"] = json!(true);
    read(&mut browser, request.clone(), candidate);
    assert!(
        browser
            .status_summary
            .as_ref()
            .unwrap()
            .contains("仍需人工发布")
    );
    for field in ["version", "mode", "idle_seconds", "cross_session_enabled"] {
        let mut bad = status();
        bad[field] = json!(null);
        browser.begin(&request);
        assert!(browser.accept(&request, bad).is_err(), "{field}");
    }
    let mut bad = status();
    bad["consolidation_enabled"] = json!(false);
    browser.begin(&request);
    assert!(browser.accept(&request, bad).is_err());
}

#[test]
fn learning_control_uses_observed_epoch_and_revocation_works_when_host_off() {
    let mut browser = ready();
    let request = browser.control_request(false, false).unwrap();
    assert_eq!(request["control"]["expected_epoch"], 0);
    let mut forged = request.clone();
    forged["control"]["expected_epoch"] = json!(3);
    assert!(!browser.can_begin(&forged));
    forged = request.clone();
    forged["control"]["workspace_shared"] = json!(true);
    assert!(!browser.can_begin(&forged));
    read(&mut browser, request, control("session-a", 1, false, false));
    assert_eq!(browser.control.as_ref().unwrap().consent_epoch, 1);
    assert!(browser.episode_request().is_none());
    assert!(browser.last_result.as_ref().unwrap().contains("费用"));
    browser.status.as_mut().unwrap().enabled = false;
    assert!(browser.control_request(true, false).is_none());
    browser.control = Some(serde_json::from_value(control("session-a", 5, true, true)).unwrap());
    browser.status.as_mut().unwrap().cross_session_enabled = false;
    assert!(browser.control_request(true, false).is_some());
    assert!(browser.control_request(false, false).is_some());
    assert!(browser.control_request(true, true).is_none());
}

#[test]
fn learning_read_fencing_rejects_native_local_pane_and_service_changes() {
    for change in 0..4 {
        let mut browser = ready();
        let request = browser.page_request().unwrap();
        browser.begin(&request);
        match change {
            0 => browser.set_session(Some("session-b")),
            1 => browser.invalidate(), // Same native session, different local view.
            2 => browser.choose_pane(Pane::Patterns),
            _ => browser.invalidate(), // Applied service generation changed.
        }
        browser
            .accept(&request, json!({"experiences":[experience(1)]}))
            .unwrap();
        assert!(browser.rows.is_empty());
        assert!(!browser.loaded);
        if change != 2 {
            assert!(!browser.can_begin(&json!({"operation":"skill.learning.control.set", "session_id":"session-a", "control":{"contribute":false,"workspace_shared":false,"expected_epoch":0}})));
        }
    }
    let mut browser = ready();
    browser.set_session(Some("session-b"));
    let status_request = json!({"operation":"skill.learning.status"});
    browser.begin(&status_request);
    browser.set_session(Some("session-a"));
    browser.accept(&status_request, status()).unwrap();
    assert!(browser.status.is_none());
}

#[test]
fn learning_successful_old_mutation_clears_pending_without_rebinding_current_session() {
    for same_native in [false, true] {
        let mut browser = ready();
        let request = browser.control_request(false, false).unwrap();
        browser.begin(&request);
        if same_native {
            browser.invalidate();
        } else {
            browser.set_session(Some("session-b"));
        }
        assert_eq!(browser.pending_request(), Some(&request));
        browser
            .accept(&request, control("session-a", 1, false, false))
            .unwrap();
        assert!(browser.pending_request().is_none());
        assert!(browser.control.is_none());
        assert!(browser.last_result.is_none());
    }
}

#[test]
fn learning_current_state_read_does_not_reauthorize_an_older_mutation_receipt() {
    let mut browser = ready();
    let original = browser.control_request(false, false).unwrap();
    browser.begin(&original);
    browser.invalidate();
    let query = browser.reconciliation_request().unwrap();
    read(&mut browser, query, control("session-a", 7, true, false));
    assert_eq!(browser.control.as_ref().unwrap().consent_epoch, 7);
    browser
        .accept(&original, control("session-a", 1, false, false))
        .unwrap();
    assert_eq!(browser.control.as_ref().unwrap().consent_epoch, 7);
    assert!(browser.control.as_ref().unwrap().contribute);
    assert!(browser.pending_request().is_none());
    assert!(browser.last_result.is_none());
}

#[test]
fn learning_workspace_history_requires_host_and_explicit_session_sharing() {
    let mut browser = ready();
    browser.choose_pane(Pane::Patterns);
    assert!(!browser.choose_scope(Scope::Workspace));
    let control_get = json!({"operation":"skill.learning.control.get", "session_id":"session-a"});
    read(
        &mut browser,
        control_get.clone(),
        control("session-a", 2, true, true),
    );
    assert!(browser.choose_scope(Scope::Workspace));
    let request = browser.page_request().unwrap();
    assert_eq!(request["operation"], "skill.learning.workspace.patterns");
    let mut foreign = pattern();
    foreign["session_id"] = json!("same-workspace-member");
    read(&mut browser, request, json!({"patterns":[foreign.clone()]}));
    assert!(browser.rows[0].detail.contains("same-workspace-member"));
    browser.choose_scope(Scope::Session);
    let request = browser.page_request().unwrap();
    browser.begin(&request);
    assert!(
        browser
            .accept(&request, json!({"patterns":[foreign]}))
            .is_err()
    );
    browser.choose_scope(Scope::Workspace);
    browser.begin(&control_get);
    browser
        .accept(&control_get, control("session-a", 3, true, false))
        .unwrap();
    assert_eq!(browser.scope, Scope::Session);
    assert!(browser.rows.is_empty());
    browser.choose_pane(Pane::Jobs);
    assert!(!browser.choose_scope(Scope::Workspace));
}

#[test]
fn learning_experience_pages_are_bounded_ordered_and_commit_atomically() {
    let mut browser = ready();
    let request = browser.page_request().unwrap();
    let entries = (1..=20).map(experience).collect::<Vec<_>>();
    read(
        &mut browser,
        request,
        json!({"experiences":entries, "next_cursor":experience(20)["id"]}),
    );
    assert_eq!(browser.rows.len(), 20);
    let next = browser.nav.request(true).unwrap();
    read(
        &mut browser,
        next.clone(),
        json!({"experiences":[experience(21)]}),
    );
    assert_eq!(browser.nav.page_number(), 2);
    assert_eq!(browser.rows.len(), 1);
    for invalid in [
        json!({"experiences":[experience(22)], "next_cursor":"invented"}),
        json!({"experiences":[experience(23), experience(22)]}),
        json!({"experiences":[experience(22), experience(22)]}),
        json!({"experiences":(1..=21).map(experience).collect::<Vec<_>>()}),
        json!({"experiences":[], "next_cursor":false}),
    ] {
        browser.begin(&next);
        assert!(browser.accept(&next, invalid).is_err());
        assert_eq!(browser.rows[0].id, experience(21)["id"].as_str().unwrap());
        assert_eq!(browser.nav.page_number(), 2);
    }
}

#[test]
fn learning_history_preserves_unassessed_sources_and_independent_episode_counts() {
    let mut browser = ready();
    browser.choose_pane(Pane::Patterns);
    let request = browser.page_request().unwrap();
    read(
        &mut browser,
        request.clone(),
        json!({"patterns":[pattern()]}),
    );
    assert!(browser.rows[0].detail.contains("历史，未重新核验"));
    assert!(browser.rows[0].detail.contains("质量未评估"));
    for (field, bad) in [
        ("quality", json!("passed")),
        ("source_state", json!("checked_at_consolidation")),
        ("support_episodes", json!(3)),
        ("observed_episodes", json!(1)),
        (
            "conflicts_with",
            json!([format!("lpat_{}", "b".repeat(64))]),
        ),
        ("representative_id", experience(3)["id"].clone()),
    ] {
        let mut entry = pattern();
        entry[field] = bad;
        browser.begin(&request);
        assert!(
            browser
                .accept(&request, json!({"patterns":[entry]}))
                .is_err(),
            "{field}"
        );
    }
    browser.choose_pane(Pane::Experiences);
    let request = browser.page_request().unwrap();
    for (field, bad) in [
        ("session_id", json!("foreign")),
        ("origin", json!("operator")),
        ("revision", json!(0)),
    ] {
        let mut entry = experience(1);
        entry[field] = bad;
        browser.begin(&request);
        assert!(
            browser
                .accept(&request, json!({"experiences":[entry]}))
                .is_err(),
            "{field}"
        );
    }
}

#[test]
fn learning_match_keeps_old_actual_reads_separate_from_fixed_head() {
    let mut browser = ready();
    browser.choose_pane(Pane::Matches);
    let request = browser.page_request().unwrap();
    read(
        &mut browser,
        request.clone(),
        json!({"matches":[matched()]}),
    );
    let detail = &browser.rows[0].detail;
    assert!(detail.contains(&format!("固定 head {}", "e".repeat(64))));
    assert!(detail.contains(&format!("version {}", "f".repeat(64))));
    for (field, bad) in [
        ("source_state", json!("checked_at_matching")),
        ("relation", json!("new_capability")),
        ("head", json!(null)),
        ("suggested_name", json!("other-name")),
    ] {
        let mut entry = matched();
        entry[field] = bad;
        browser.begin(&request);
        assert!(
            browser
                .accept(&request, json!({"matches":[entry]}))
                .is_err(),
            "{field}"
        );
    }
    let mut unknown = matched();
    unknown["relation"] = json!("new_capability");
    unknown.as_object_mut().unwrap().remove("target");
    unknown.as_object_mut().unwrap().remove("head");
    unknown["actual_reads"] = json!([]);
    read(
        &mut browser,
        request.clone(),
        json!({"matches":[unknown.clone()]}),
    );
    assert!(browser.rows[0].detail.contains("无实际读取记录"));
    for name in [
        "json-audit",
        "",
        "json-12",
        "json-abcdefabcdef",
        "../json-audit",
        "Json-audit",
    ] {
        unknown["suggested_name"] = json!(name);
        browser.begin(&request);
        assert_eq!(
            browser
                .accept(&request, json!({"matches":[unknown.clone()]}))
                .is_ok(),
            matches!(name, "json-audit" | ""),
            "{name}"
        );
    }
}

#[test]
fn learning_jobs_pin_pattern_not_single_episode_and_cancel_with_observed_version() {
    let mut browser = ready();
    browser.choose_pane(Pane::Jobs);
    let request = browser.page_request().unwrap();
    read(&mut browser, request.clone(), json!({"jobs":[job()]}));
    let cancel = browser.rows[0].cancel.clone().unwrap();
    assert_eq!(cancel["expected_state_version"], 3);
    assert!(browser.can_begin(&cancel));
    let mut forged = cancel.clone();
    forged["expected_state_version"] = json!(4);
    assert!(!browser.can_begin(&forged));
    let mut bad = job();
    bad["episode_id"] = json!("lep_a");
    browser.begin(&request);
    assert!(browser.accept(&request, json!({"jobs":[bad]})).is_err());
    read(&mut browser, cancel, json!({"cancelled":true}));
    assert!(!browser.loaded);
    assert!(
        browser
            .last_result
            .as_ref()
            .unwrap()
            .contains("已发生用量保留")
    );
    let mut terminal = job();
    terminal["state"] = json!("completed");
    read(&mut browser, request, json!({"jobs":[terminal]}));
    assert!(browser.rows[0].cancel.is_none());
}

#[test]
fn learning_uncertain_mutations_are_never_replayed_or_cleared_by_current_state_reads() {
    let mut browser = ready();
    let request = browser.control_request(false, false).unwrap();
    browser.begin(&request);
    browser.invalidate();
    assert!(!browser.can_begin(&request));
    browser.set_session(Some("session-b"));
    let query = browser.reconciliation_request().unwrap();
    assert_eq!(query["session_id"], "session-a");
    assert!(browser.can_begin(&query));
    read(&mut browser, query, control("session-a", 1, false, false));
    assert_eq!(browser.pending_request(), Some(&request));
    assert!(
        browser
            .reconciliation
            .as_ref()
            .unwrap()
            .contains("不是原操作的幂等回执")
    );
    assert!(browser.control.is_none());
    browser.definitive_failure(
        &json!({"operation":"skill.learning.control.set", "session_id":"session-b"}),
    );
    assert!(browser.pending_request().is_some());
    browser.definitive_failure(&request);
    assert!(browser.pending_request().is_none());
    assert!(browser.reconciliation.is_none());
}

#[test]
fn learning_episode_start_unknown_result_has_no_retry_or_fake_reconciliation() {
    let mut browser = ready();
    let request = browser.episode_request().unwrap();
    assert!(browser.can_begin(&request));
    browser.begin(&request);
    assert!(!browser.pending_sources_changed());
    browser.invalidate();
    assert_eq!(browser.pending_request(), Some(&request));
    assert!(browser.reconciliation_request().is_none());
    assert!(!browser.can_begin(&request));
    assert!(
        browser
            .accept(&request, json!({"episode_id":"forged"}))
            .is_err()
    );
    assert!(browser.pending_request().is_some());
    browser
        .accept(&request, json!({"episode_id":"lep_actual"}))
        .unwrap();
    assert!(browser.pending_request().is_none());
    assert!(browser.last_result.is_none()); // The old receipt never attaches to the new view.
}

#[test]
fn learning_cancel_reconciliation_is_bounded_and_absence_is_not_failure() {
    let mut browser = ready();
    browser.choose_pane(Pane::Jobs);
    let list = browser.page_request().unwrap();
    read(&mut browser, list, json!({"jobs":[job()]}));
    let cancel = browser.rows[0].cancel.clone().unwrap();
    browser.begin(&cancel);
    browser.invalidate();
    browser.set_session(Some("session-b"));
    let query = browser.reconciliation_request().unwrap();
    read(&mut browser, query.clone(), json!({"jobs":[]}));
    assert!(
        browser
            .reconciliation
            .as_ref()
            .unwrap()
            .contains("不代表作业不存在")
    );
    assert_eq!(browser.pending_request(), Some(&cancel));
    let mut cancelled = job();
    cancelled["state"] = json!("cancelled");
    cancelled["state_version"] = json!(4);
    read(&mut browser, query, json!({"jobs":[cancelled]}));
    assert!(browser.reconciliation.as_ref().unwrap().contains("已取消"));
    assert_eq!(browser.pending_request(), Some(&cancel));
    assert!(browser.rows.is_empty());
}

#[test]
fn learning_control_mutation_validates_success_before_releasing_unknown_outcome() {
    let mut browser = ready();
    let request = browser.control_request(false, false).unwrap();
    browser.begin(&request);
    for invalid in [
        control("foreign", 1, false, false),
        control("session-a", 3, false, false),
        control("session-a", 1, true, false),
    ] {
        assert!(browser.accept(&request, invalid).is_err());
        assert_eq!(browser.pending_request(), Some(&request));
    }
    browser
        .accept(&request, control("session-a", 1, false, false))
        .unwrap();
    assert!(browser.pending_request().is_none());
}

#[test]
fn learning_projection_bounds_utf8_titles_and_keeps_complete_details() {
    let mut browser = ready();
    let request = browser.page_request().unwrap();
    let mut long = experience(1);
    let goal = "核对 JSON 配置\n".repeat(30);
    long["experience"]["goal"] = json!(goal);
    long["experience"]["references"] = json!([format!("receipt/run/{}", "x".repeat(1024))]);
    read(&mut browser, request.clone(), json!({"experiences":[long]}));
    assert!(browser.rows[0].title.chars().count() <= 83);
    assert!(!browser.rows[0].title.contains('\n'));
    assert!(browser.rows[0].detail.contains(&goal));
    let mut extra = experience(2);
    extra["untrusted_extra"] = json!("x".repeat(64 << 10));
    browser.begin(&request);
    assert!(
        browser
            .accept(&request, json!({"experiences":[extra]}))
            .is_err()
    );
    assert_eq!(browser.rows[0].id, experience(1)["id"].as_str().unwrap());
}

#[test]
fn learning_status_separates_auto_publication_grant_from_shadow_and_review() {
    let mut observed = status();
    observed["proposal_discovery_enabled"] = json!(true);
    observed["auto_patch_enabled"] = json!(true);
    observed["mode"] = json!("guarded_auto_patch");
    let parsed: Status = serde_json::from_value(observed.clone()).unwrap();
    parsed.validate().unwrap();
    assert!(parsed.summary().contains("受限自动补丁"));
    assert!(parsed.summary().contains("描述修改另需正负触发评测"));
    assert!(parsed.summary().contains("新技能/其他元数据"));
    for (key, value) in [
        ("mode", json!("review_candidates")),
        ("proposal_discovery_enabled", json!(false)),
        ("matching_enabled", json!(false)),
        ("auto_patch_enabled", json!(false)),
    ] {
        let mut invalid = observed.clone();
        invalid[key] = value;
        assert!(
            serde_json::from_value::<Status>(invalid)
                .unwrap()
                .validate()
                .is_err()
        );
    }
    observed["mode"] = json!("review_candidates");
    observed
        .as_object_mut()
        .unwrap()
        .remove("auto_patch_enabled");
    let legacy: Status = serde_json::from_value(observed).unwrap();
    legacy.validate().unwrap();
    assert!(legacy.summary().contains("仍需人工发布"));
}

fn pattern_review(state_version: u64, ignored: bool, adjudication: &str) -> Value {
    json!({"pattern_id":pattern()["id"], "pattern_revision":2, "state_version":state_version, "ignored":ignored,
        "adjudication_revision":if adjudication.is_empty() { 0 } else { 2 }, "adjudication_id":adjudication})
}

fn review_ready() -> LearningBrowser {
    let mut browser = ready();
    browser.choose_pane(Pane::Patterns);
    let mut value = pattern();
    value["review"] = pattern_review(0, false, "");
    let query = browser.page_request().unwrap();
    read(&mut browser, query, json!({"patterns":[value]}));
    browser.selected = Some(pattern()["id"].as_str().unwrap().to_owned());
    browser
}

#[test]
fn learning_review_uses_loaded_scope_revision_and_control_version() {
    let mut browser = review_ready();
    let request = browser.ignore_request().unwrap();
    assert!(browser.can_begin(&request));
    assert_eq!(request["review"]["expected_state_version"], 0);
    for key in [
        "pattern_id",
        "expected_pattern_revision",
        "expected_state_version",
        "scope",
        "ignored",
    ] {
        let mut wrong = request.clone();
        wrong["review"][key] = json!(null);
        assert!(!browser.can_begin(&wrong), "{key}");
    }
    browser.begin(&request);
    assert!(browser.pending_sources_changed());
    browser
        .accept(&request, json!({"review":pattern_review(1, true, "")}))
        .unwrap();
    assert!(!browser.loaded);
    assert!(browser.last_result.as_ref().unwrap().contains("消费记录"));
    assert!(browser.ignore_request().is_none());
    let legacy = pattern();
    let query = browser.page_request().unwrap();
    read(&mut browser, query, json!({"patterns":[legacy]}));
    browser.selected = Some(pattern()["id"].as_str().unwrap().to_owned());
    assert!(
        browser.ignore_request().is_none(),
        "old host without review cannot authorize mutation"
    );
}

#[test]
fn learning_review_unknown_outcome_replays_only_complete_original() {
    let mut browser = review_ready();
    let request = browser.ignore_request().unwrap();
    browser.begin(&request);
    browser.invalidate();
    assert!(browser.can_begin(&request));
    assert!(browser.reconciliation_request().is_none());
    for key in [
        "request_key",
        "pattern_id",
        "expected_state_version",
        "ignored",
    ] {
        let mut wrong = request.clone();
        wrong["review"][key] = json!("changed");
        assert!(!browser.can_begin(&wrong));
    }
    browser.set_session(Some("session-b"));
    browser.begin(&request);
    browser
        .accept(&request, json!({"review":pattern_review(1, true, "")}))
        .unwrap();
    assert!(browser.pending_request().is_none());
    assert!(browser.last_result.is_none());
    assert!(browser.control.is_none());
    assert_eq!(browser.session_id.as_deref(), Some("session-b"));
}

#[test]
fn learning_review_adjudication_is_not_model_or_quality_authority() {
    let mut browser = review_ready();
    let source = matched();
    let request = browser
        .adjudication_request(
            source["target"].clone(),
            source["head"].clone(),
            Value::Null,
            "body_gap",
        )
        .unwrap();
    assert!(browser.can_begin(&request));
    assert!(
        browser
            .adjudication_request(
                source["target"].clone(),
                source["head"].clone(),
                Value::Null,
                "new_capability"
            )
            .is_none()
    );
    browser.begin(&request);
    let id = format!("ladj_{}", "b".repeat(64));
    let review = pattern_review(1, false, &id);
    let mut decision = matched();
    decision["id"] = json!(id);
    decision["origin"] = json!("host_adjudicated");
    decision["source_state"] = json!("checked_at_adjudication");
    decision["head"] = request["review"]["expected_head"].clone();
    let mut invalid = decision.clone();
    invalid["origin"] = json!("model_matched");
    assert!(
        browser
            .accept(&request, json!({"review":review, "match":invalid}))
            .is_err()
    );
    assert!(browser.pending_request().is_some());
    browser
        .accept(&request, json!({"review":review, "match":decision}))
        .unwrap();
    browser.choose_pane(Pane::Matches);
    decision["source_state"] = json!("not_revalidated");
    decision["review"] = review;
    let query = browser.page_request().unwrap();
    read(&mut browser, query, json!({"matches":[decision]}));
    assert!(browser.rows[0].detail.contains("人工目标裁决"));
    assert!(browser.rows[0].detail.contains("不是模型判断或质量确认"));
    assert!(browser.rows[0].detail.contains("候选需人工发布"));
}

#[test]
fn learning_review_ignored_or_unresolved_pattern_cannot_be_adjudicated() {
    for (ignored, evidence) in [(true, "observed_execution"), (false, "unresolved")] {
        let mut browser = ready();
        browser.choose_pane(Pane::Patterns);
        let mut value = pattern();
        value["review"] = pattern_review(1, ignored, "");
        value["evidence_state"] = json!(evidence);
        let query = browser.page_request().unwrap();
        read(&mut browser, query, json!({"patterns":[value]}));
        browser.selected = Some(pattern()["id"].as_str().unwrap().to_owned());
        let source = matched();
        assert!(
            browser
                .adjudication_request(
                    source["target"].clone(),
                    source["head"].clone(),
                    Value::Null,
                    "body_gap"
                )
                .is_none()
        );
    }
}

#[test]
fn learning_review_old_match_cannot_authorize_new_pattern_revision() {
    let mut browser = ready();
    browser.choose_pane(Pane::Matches);
    let mut value = matched();
    let mut review = pattern_review(1, false, "");
    review["pattern_revision"] = json!(3);
    value["review"] = review;
    let query = browser.page_request().unwrap();
    read(&mut browser, query, json!({"matches":[value]}));
    browser.selected = Some(matched()["id"].as_str().unwrap().to_owned());
    assert!(browser.ignore_request().is_none());
    assert!(browser.rows[0].detail.contains("建议控制版本"));
}

fn creation_retry_ready() -> LearningBrowser {
    let mut browser = review_ready();
    browser.status.as_mut().unwrap().proposal_discovery_enabled = true;
    browser.status.as_mut().unwrap().mode = "review_candidates".into();
    browser
}

fn creation_retry_prepared(request: &Value) -> Value {
    let mut fields = request["retry"].clone();
    fields["request_key"] = json!("");
    fields["expected_generation_state_version"] = json!(3);
    fields["previous_proposal_id"] = json!("");
    fields["expected_proposal_state_version"] = json!(0);
    fields["target"] = json!({"source_id":"project", "source_identity":"d".repeat(64), "name":"json-audit", "scope":"workspace", "workspace":"D:\\workspace"});
    fields["expected_head"] =
        json!({"version":"", "state_version":0, "exists":false, "enabled":false, "managed":false});
    fields["overlay_base"] = Value::Null;
    fields["evidence_reservation_id"] = json!("sd_original");
    fields["expected_retry_number"] = json!(1);
    json!({"request":fields, "previous_generation_state":"cancelled", "previous_proposal_status":"",
        "retry":{"previous_generation_id":request["retry"]["previous_generation_id"], "evidence_reservation_id":"sd_original", "retry_number":1}, "max_retries":3})
}

#[test]
fn learning_creation_retry_requires_fresh_complete_observation_and_explicit_key() {
    let mut browser = creation_retry_ready();
    assert!(browser.creation_retry_request().is_none());
    let query = browser
        .creation_retry_prepare_request("smj_previous")
        .unwrap();
    let prepared = creation_retry_prepared(&query);
    browser.begin(&query);
    for (key, value) in [
        ("max_retries", json!(4)),
        ("previous_generation_state", json!("running")),
    ] {
        let mut invalid = prepared.clone();
        invalid[key] = value;
        assert!(browser.accept(&query, invalid).is_err());
    }
    let mut invalid = prepared.clone();
    invalid["request"]["expected_head"]["managed"] = json!(true);
    assert!(browser.accept(&query, invalid).is_err());
    let mut invalid = prepared.clone();
    invalid["request"]["expected_pattern_revision"] = json!(3);
    assert!(browser.accept(&query, invalid).is_err());
    browser.accept(&query, prepared).unwrap();
    let request = browser.creation_retry_request().unwrap();
    assert!(browser.can_begin(&request));
    for key in [
        "target",
        "previous_generation_id",
        "expected_generation_state_version",
        "expected_state_version",
        "overlay_base",
    ] {
        let mut invalid = request.clone();
        invalid["retry"][key] = json!("different");
        assert!(!browser.can_begin(&invalid), "{key}");
    }
    assert!(
        browser
            .creation_retry_caption()
            .unwrap()
            .contains("cooldown")
    );
    browser.invalidate();
    assert!(!browser.can_begin(&request));
    assert!(browser.creation_retry_request().is_none());
}

#[test]
fn learning_creation_retry_unknown_result_replays_only_original_and_fences_late_receipt() {
    let mut browser = creation_retry_ready();
    let query = browser
        .creation_retry_prepare_request("smj_previous")
        .unwrap();
    read(&mut browser, query.clone(), creation_retry_prepared(&query));
    let request = browser.creation_retry_request().unwrap();
    browser.begin(&request);
    browser.invalidate();
    assert!(browser.can_begin(&request));
    let mut changed = request.clone();
    changed["retry"]["request_key"] = json!(uuid::Uuid::new_v4().to_string());
    assert!(!browser.can_begin(&changed));
    browser.set_session(Some("session-b"));
    browser.begin(&request);
    let result = json!({"id":format!("lretry_{}", "a".repeat(64)), "review":pattern_review(1,false,""),
        "generation_id":"smj_retry", "discovery_id":"sd_retry", "retry":{"previous_generation_id":"smj_previous","evidence_reservation_id":"sd_original","retry_number":1}});
    let mut invalid = result.clone();
    invalid["retry"]["previous_generation_id"] = json!("smj_another");
    assert!(browser.accept(&request, invalid).is_err());
    assert!(browser.pending_request().is_some());
    browser.accept(&request, result).unwrap();
    assert!(browser.pending_request().is_none());
    assert!(browser.last_result.is_none());
    assert!(browser.control.is_none());
}

#[test]
fn learning_creation_retry_observation_does_not_survive_a_different_pattern_selection() {
    let mut browser = creation_retry_ready();
    let query = browser
        .creation_retry_prepare_request("smj_previous")
        .unwrap();
    read(&mut browser, query.clone(), creation_retry_prepared(&query));
    browser.selected = Some("different-pattern".into());
    assert!(browser.creation_retry_request().is_none());
    browser.begin(&query);
    assert!(
        browser
            .accept(&query, creation_retry_prepared(&query))
            .is_err()
    );
}

fn source_ready() -> LearningBrowser {
    let mut browser = ready();
    let query = browser.page_request().unwrap();
    read(
        &mut browser,
        query,
        json!({"experiences":[experience(1),experience(2)]}),
    );
    browser.select_row(experience(1)["id"].as_str().unwrap());
    browser
}

fn source_response(request: &Value) -> Value {
    json!({"session_id":request["session_id"], "source":request["source"], "source_session_id":"session-a",
        "episode_id":"lep_a", "consent_epoch":2, "extraction_id":format!("lex_{}", "a".repeat(64)),
        "input_sha256":"b".repeat(64), "result_sha256":"c".repeat(64),
        "policy_version":"procedural-experience-v1", "external_context_policy":"hints_only",
        "source_state":"checked_at_read", "quality":"unassessed",
        "references":[{"id":"input/accepted","run_id":"run-a","input_id":"accepted","sequence":1,"kind":"user","text":"Read structured JSON"},
            {"id":"receipt/run-a/local","run_id":"run-a","tool_call_id":"local","version":3,"kind":"tool_observation","state":"completed",
                "tool_name":"read_file","text":"Arguments: {path: configuration.json}\nObserved result: {verified: true}"}]})
}

#[test]
fn learning_source_selected_member_is_read_only_bounded_and_attributed() {
    let mut browser = source_ready();
    let member = experience(1)["id"].as_str().unwrap().to_owned();
    let request = browser.source_request(&member).unwrap();
    assert!(!mutation(&request));
    assert!(!review_mutation(&request));
    assert!(!sources_changed(&request));
    assert!(session_bound(&request));
    assert_eq!(request["source"]["expected_revision"], 1);
    assert!(browser.source_request("lexp_uncached").is_none());
    for key in ["kind", "id", "scope", "expected_revision", "experience_id"] {
        let mut invalid = request.clone();
        invalid["source"][key] = json!(null);
        assert!(!browser.can_begin(&invalid), "{key}");
    }
    read(&mut browser, request.clone(), source_response(&request));
    let detail = browser.source_detail().unwrap();
    assert!(detail.contains("读取时重建"));
    assert!(detail.contains("不增加独立票"));
    assert!(detail.contains("verified: true"));
    assert!(browser.pending_request().is_none());
    browser.choose_pane(Pane::Jobs);
    assert!(browser.source_members().is_empty());
    assert!(browser.source_detail().is_none());
}

#[test]
fn learning_source_response_rejects_wrong_binding_external_body_and_corruption() {
    let mut browser = source_ready();
    let request = browser
        .source_request(experience(1)["id"].as_str().unwrap())
        .unwrap();
    let valid = source_response(&request);
    let cases: Vec<Box<dyn Fn(&mut Value)>> = vec![
        Box::new(|v| v["raw_receipt"] = json!("unbounded original")),
        Box::new(|v| v["references"][1]["raw_result"] = json!("PRIVATE_REMOTE_BODY")),
        Box::new(|v| v["session_id"] = json!("session-b")),
        Box::new(|v| v["source_session_id"] = json!("session-b")),
        Box::new(|v| v["source"]["expected_revision"] = json!(2)),
        Box::new(|v| v["quality"] = json!("passed")),
        Box::new(|v| v["source_state"] = json!("not_revalidated")),
        Box::new(|v| v["external_context_policy"] = json!("full")),
        Box::new(|v| v["input_sha256"] = json!("invalid")),
        Box::new(|v| v["references"][1]["version"] = json!(0)),
        Box::new(|v| v["references"][1]["state"] = json!("uncertain")),
        Box::new(|v| v["references"][1]["tool_name"] = json!("web_fetch")),
        Box::new(|v| v["references"][1]["tool_call_id"] = json!("other")),
        Box::new(|v| v["references"][0]["kind"] = json!("reasoning")),
        Box::new(|v| v["references"] = json!(vec![v["references"][0].clone(); 33])),
        Box::new(|v| v["references"][0]["text"] = json!("x".repeat(8193))),
        Box::new(|v| v["references"] = json!([])),
    ];
    for (i, change) in cases.iter().enumerate() {
        let mut invalid = valid.clone();
        change(&mut invalid);
        browser.begin(&request);
        assert!(browser.accept(&request, invalid).is_err(), "{i}");
        assert!(browser.source_detail().is_none());
    }
    let mut external = valid.clone();
    external["references"][1]["tool_name"] = json!("web_fetch");
    external["references"][1]["text"] = json!("Argument shape: {query: string}");
    external["references"][1]["truncated"] = json!(true);
    read(&mut browser, request, external);
    assert!(browser.source_detail().unwrap().contains("无结果正文"));
    assert!(browser.source_detail().unwrap().contains("已截断"));
}

#[test]
fn learning_source_selection_and_authority_changes_discard_late_or_cached_body() {
    let member = experience(1)["id"].as_str().unwrap().to_owned();
    for change in 0..7 {
        let mut browser = source_ready();
        let request = browser.source_request(&member).unwrap();
        browser.begin(&request);
        match change {
            0 => browser.select_row(experience(2)["id"].as_str().unwrap()),
            1 => browser.set_session(Some("session-b")),
            2 => browser.choose_pane(Pane::Patterns),
            3 => browser.invalidate(),
            4 => {
                let query = browser.page_request().unwrap();
                read(&mut browser, query, json!({"experiences":[experience(1)]}));
            }
            5 => {
                let query = browser.page_request().unwrap();
                browser.begin(&query);
            }
            _ => {
                let control = browser.control_request(false, false).unwrap();
                browser.begin(&control);
            }
        }
        browser.accept(&request, source_response(&request)).unwrap();
        assert!(browser.source_detail().is_none(), "late {change}");
    }
    let mut browser = source_ready();
    let request = browser.source_request(&member).unwrap();
    read(&mut browser, request.clone(), source_response(&request));
    browser.select_row(experience(2)["id"].as_str().unwrap());
    assert!(browser.source_detail().is_none());
    browser.select_row(&member);
    assert!(
        browser.source_detail().is_none(),
        "back-navigation resurrected cached source"
    );
}

#[test]
fn learning_source_workspace_member_read_does_not_grant_private_or_match_access() {
    let mut browser = ready();
    read(
        &mut browser,
        json!({"operation":"skill.learning.control.get","session_id":"session-a"}),
        control("session-a", 3, true, true),
    );
    browser.choose_pane(Pane::Patterns);
    assert!(browser.choose_scope(Scope::Workspace));
    let query = browser.page_request().unwrap();
    read(&mut browser, query, json!({"patterns":[pattern()]}));
    browser.select_row(pattern()["id"].as_str().unwrap());
    assert_eq!(browser.source_members().len(), 2);
    let request = browser
        .source_request(experience(2)["id"].as_str().unwrap())
        .unwrap();
    assert_eq!(request["source"]["kind"], "pattern");
    assert_eq!(request["source"]["scope"], "workspace");
    let mut data = source_response(&request);
    data["source_session_id"] = json!("session-b");
    read(&mut browser, request.clone(), data);
    assert!(browser.source_detail().unwrap().contains("session-b"));
    browser.choose_scope(Scope::Session);
    assert!(!browser.can_begin(&request));
    assert!(browser.source_detail().is_none());
    browser.choose_pane(Pane::Matches);
    let query = browser.page_request().unwrap();
    read(&mut browser, query, json!({"matches":[matched()]}));
    browser.select_row(matched()["id"].as_str().unwrap());
    assert!(
        browser.source_members().is_empty(),
        "historical match silently rebased to a current pattern"
    );
}

#[test]
fn learning_source_skill_read_keeps_exact_version_not_quality_or_causality() {
    let mut browser = source_ready();
    let request = browser
        .source_request(experience(1)["id"].as_str().unwrap())
        .unwrap();
    let mut data = source_response(&request);
    data["references"].as_array_mut().unwrap().push(json!({"id":"skill_read/7","run_id":"run-a","kind":"skill_read",
        "skill":matched()["actual_reads"][0], "path":"SKILL.md", "sha256":"a".repeat(64), "role_id":"lead"}));
    read(&mut browser, request.clone(), data.clone());
    assert!(browser.source_detail().unwrap().contains(&"f".repeat(64)));
    assert!(browser.source_detail().unwrap().contains("不证明因果贡献"));
    data["references"][2]["skill"]["hash"] = json!("a".repeat(64));
    browser.begin(&request);
    assert!(browser.accept(&request, data).is_err());
}
