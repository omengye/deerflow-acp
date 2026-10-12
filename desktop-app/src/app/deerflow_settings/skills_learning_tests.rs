use super::*;

fn store(path: &str, generation: u64) -> Value {
    json!({"config_path":format!("C:/{path}/config.yaml"), "user_data":format!("C:/{path}/data"), "applied_generation":generation})
}

fn control(session: &str, epoch: u64, contribute: bool) -> Value {
    json!({"session_id":session, "consent_epoch":epoch, "contribute":contribute, "workspace_shared":false})
}

fn browser() -> SkillBrowser {
    let mut browser = SkillBrowser::default();
    let service = store("first", 1);
    browser.set_learning_session(Some("session-a"));
    let status = json!({"operation":"skill.learning.status"});
    browser.begin_at(&status, &service);
    browser.accept_learning(&status, json!({"version":"procedural-experience-v1", "enabled":true,
        "mode":"shadow", "idle_seconds":300, "pending_inputs":0, "experiences":0, "consolidation_enabled":false,
        "patterns":0, "matching_enabled":false, "cross_session_enabled":false, "matches":0, "proposal_discovery_enabled":false})).unwrap();
    let get = json!({"operation":"skill.learning.control.get", "session_id":"session-a"});
    browser.begin_at(&get, &service);
    browser
        .accept_learning(&get, control("session-a", 2, true))
        .unwrap();
    browser
}

#[test]
fn learning_transport_fences_unknown_operations_to_original_store_without_replay() {
    let first = store("first", 1);
    let mut browser = browser();
    let request = browser.learning.control_request(false, false).unwrap();
    assert!(browser.can_begin_at(&request, &first));
    browser.begin_at(&request, &first);
    assert!(browser.review_stale);
    browser.transport_failed(&request);
    assert!(browser.mutations_pending());
    assert!(browser.is_original_retry(&request));
    assert_eq!(browser.learning.pending_request(), Some(&request));
    assert!(browser.learning.pending_sources_changed());
    assert!(!browser.can_begin_at(&request, &first));
    assert!(!browser.can_begin_at(&json!({"operation":"proposal.reject", "proposal_id":"another", "expected_state_version":1}), &first));
    let query = browser.learning.reconciliation_request().unwrap();
    assert!(browser.can_begin_at(&query, &first));
    assert!(browser.can_begin_at(&query, &store("first", 12)));
    assert!(!browser.can_begin_at(&query, &store("second", 1)));
    browser.set_learning_session(Some("session-b"));
    assert!(browser.can_begin_at(&query, &first));
    browser.begin_at(&query, &first);
    browser
        .accept_learning(&query, control("session-a", 3, false))
        .unwrap();
    assert!(browser.learning.control.is_none());
    assert!(browser.mutations_pending());
    assert_eq!(browser.retry_service, Some(service_identity(&first)));
    assert!(
        browser
            .learning
            .reconciliation
            .as_ref()
            .unwrap()
            .contains("不是原操作的幂等回执")
    );
}

#[test]
fn learning_source_transport_read_failure_clears_body_without_pending_receipt() {
    let mut browser = browser();
    let first = store("first", 1);
    let member = format!("lexp_{}", "a".repeat(64));
    let query = browser.learning.page_request().unwrap();
    browser.begin_at(&query, &first);
    browser.accept_learning(&query, json!({"experiences":[{
        "id":member,"session_id":"session-a","episode_id":"lep_a","revision":1,"origin":"model_extracted","quality":"unassessed",
        "created_at":"2026-10-09T08:20:00Z","experience":{"kind":"proven_recipe","goal":"Read JSON","applicability":"Structured input",
            "resolution":"Read full input","steps":["Read"],"verification":"Local receipt saved","outcome":"observed_execution","references":["input/accepted"]}
    }]})).unwrap();
    browser.learning.select_row(&member);
    let request = browser.learning.source_request(&member).unwrap();
    assert!(browser.can_begin_at(&request, &first));
    browser.begin_at(&request, &first);
    let changed = browser.accept_learning(&request, json!({"session_id":"session-a","source":request["source"],"source_session_id":"session-a",
        "episode_id":"lep_a","consent_epoch":2,"extraction_id":format!("lex_{}", "a".repeat(64)),"input_sha256":"b".repeat(64),
        "result_sha256":"c".repeat(64),"policy_version":"procedural-experience-v1","external_context_policy":"hints_only",
        "source_state":"checked_at_read","quality":"unassessed","references":[{"id":"input/accepted","run_id":"run-a",
            "input_id":"accepted","sequence":1,"kind":"user","text":"Read JSON"}]})).unwrap();
    assert!(!changed, "source read revoked unrelated generation caches");
    assert!(browser.learning.source_detail().is_some());
    assert!(!browser.mutations_pending());
    assert!(browser.retry.is_none());
    assert!(browser.retry_service.is_none());
    browser.transport_failed(&request);
    assert!(browser.learning.source_detail().is_none());
    assert!(!browser.mutations_pending());
    assert!(browser.retry.is_none());
    assert!(browser.retry_service.is_none());
    assert!(
        !browser.can_begin_at(&request, &first),
        "failed read reused stale member selection"
    );
    let query = browser.learning.page_request().unwrap();
    assert!(browser.can_begin_at(&query, &first));
}

#[test]
fn learning_transport_episode_result_unknown_never_becomes_a_generic_retry() {
    let first = store("first", 1);
    let mut browser = browser();
    let request = browser.learning.episode_request().unwrap();
    browser.begin_at(&request, &first);
    browser.transport_failed(&request);
    assert!(!browser.can_begin_at(&request, &first));
    assert!(browser.learning.reconciliation_request().is_none());
    assert!(browser.mutations_pending());
    browser.abandon_retry();
    assert!(!browser.mutations_pending());
    assert!(browser.retry_service.is_none());
    assert!(browser.learning.pending_request().is_none());
    assert!(browser.learning.control.is_none());
    assert!(browser.learning.episode_request().is_none());
}

#[test]
fn learning_review_transport_retains_original_key_store_and_late_receipt_fence() {
    let mut browser = browser();
    let first = store("first", 1);
    browser.learning.choose_pane(learning_state::Pane::Patterns);
    let query = browser.learning.page_request().unwrap();
    let pattern_id = format!("lpat_{}", "a".repeat(64));
    let experience_id = format!("lexp_{}", "b".repeat(64));
    let review = json!({"pattern_id":pattern_id, "pattern_revision":1, "state_version":0, "ignored":false, "adjudication_revision":0, "adjudication_id":""});
    let pattern = json!({"id":pattern_id,"session_id":"session-a","revision":1,"origin":"model_extracted","quality":"unassessed",
        "representative_id":experience_id,"experience_ids":[experience_id],"episode_ids":["lep_a"],"support_episodes":1,"observed_episodes":1,
        "evidence_state":"insufficient_evidence","source_state":"not_revalidated","conflicts_with":[],"updated_at":"2026-10-09T08:20:00Z",
        "review":review,"experience":{"kind":"proven_recipe","goal":"Read JSON","applicability":"JSON text","resolution":"Read full file",
            "steps":["Read"],"verification":"Saved receipt","outcome":"observed_execution","references":["receipt/local"]}});
    browser.begin_at(&query, &first);
    browser
        .accept_learning(&query, json!({"patterns":[pattern]}))
        .unwrap();
    browser.learning.selected = Some(pattern_id.clone());
    let entry = json!({"target":{"source_id":"project","source_identity":"c".repeat(64),"name":"json-check","scope":"global"},
        "head":{"version":"d".repeat(64),"state_version":2,"exists":true,"enabled":true,"managed":false},
        "record":{"description":"Inspect structured input"},"builtin":false,"managed_source":false,"can_refresh_import":false});
    let list = json!({"operation":"skill.list", "limit":PAGE_SIZE});
    browser
        .accept_catalog(
            &list,
            json!({"skills":[entry.clone()],"sources":[],"catalog_version":5}),
        )
        .unwrap();
    let get = json!({"operation":"skill.get", "target":entry["target"]});
    browser
        .accept_detail(&get, json!({"skill":entry.clone(),"catalog_version":5}))
        .unwrap();
    let parsed = serde_json::from_value::<learning_state::Status>(json!({"version":"procedural-experience-v1","enabled":true,"mode":"shadow",
        "idle_seconds":300,"pending_inputs":0,"experiences":1,"consolidation_enabled":true,"patterns":1,"matching_enabled":true,
        "cross_session_enabled":false,"matches":0,"proposal_discovery_enabled":false})).unwrap();
    browser.learning.status = Some(parsed);
    let adjudication = browser.learning_adjudication_request("body_gap").unwrap();
    assert!(browser.can_begin_at(&adjudication, &first));
    let mut changed = adjudication.clone();
    changed["review"]["expected_head"]["state_version"] = json!(3);
    assert!(!browser.can_begin_at(&changed, &first));
    browser.catalog_stale = true;
    assert!(browser.learning_adjudication_request("body_gap").is_none());
    assert!(!browser.can_begin_at(&adjudication, &first));
    browser.catalog_stale = false;
    let request = browser.learning.ignore_request().unwrap();
    assert!(browser.can_begin_at(&request, &first));
    browser.begin_at(&request, &first);
    browser.transport_failed(&request);
    assert!(browser.can_begin_at(&request, &store("first", 10)));
    assert!(!browser.can_begin_at(&request, &store("second", 1)));
    let mut altered = request.clone();
    altered["review"]["request_key"] = json!("fresh-key");
    assert!(!browser.can_begin_at(&altered, &first));
    browser.set_learning_session(Some("session-b"));
    browser.begin_at(&request, &first);
    let mut receipt = review;
    receipt["state_version"] = json!(1);
    receipt["ignored"] = json!(true);
    browser
        .accept_learning(&request, json!({"review":receipt}))
        .unwrap();
    assert!(!browser.mutations_pending());
    assert!(browser.retry_service.is_none());
    assert!(browser.learning.last_result.is_none());
    assert!(browser.learning.control.is_none());
}

#[test]
fn learning_transport_definitive_errors_clear_only_the_original_mutation() {
    for code in [
        "conflict",
        "permission_denied",
        "not_found",
        "invalid_request",
        "unsupported_operation",
        "validation_failed",
        "resource_limit",
    ] {
        let mut browser = browser();
        let request = browser.learning.control_request(false, false).unwrap();
        browser.begin_at(&request, &store("first", 1));
        browser.backend_failed(&request, code);
        assert!(!browser.mutations_pending(), "{code}");
        assert!(browser.learning.pending_request().is_none(), "{code}");
        assert!(browser.retry_service.is_none(), "{code}");
        assert!(browser.learning.control.is_none(), "{code}");
    }
    let mut browser = browser();
    let request = browser.learning.control_request(false, false).unwrap();
    browser.begin_at(&request, &store("first", 1));
    browser.backend_failed(&request, "internal_error");
    assert!(browser.mutations_pending());
    let query = browser.learning.reconciliation_request().unwrap();
    browser.backend_failed(&query, "not_found");
    assert!(browser.mutations_pending());
    assert_eq!(browser.learning.pending_request(), Some(&request));
}

#[test]
fn learning_transport_success_invalidates_dependent_projections_and_releases_guard() {
    let mut browser = browser();
    browser.review_stale = false;
    let request = browser.learning.control_request(false, false).unwrap();
    browser.begin_at(&request, &store("first", 1));
    assert!(
        browser
            .accept_learning(&request, control("session-a", 3, false))
            .unwrap()
    );
    assert!(browser.review_stale);
    assert!(!browser.mutations_pending());
    assert!(browser.retry_service.is_none());
    assert!(browser.learning.pending_request().is_none());
    assert!(!browser.learning.control.as_ref().unwrap().contribute);
    let unchanged = json!({"operation":"skill.learning.control.get", "session_id":"session-a"});
    browser.begin_at(&unchanged, &store("first", 1));
    assert!(
        !browser
            .accept_learning(&unchanged, control("session-a", 3, false))
            .unwrap()
    );
    browser.begin_at(&unchanged, &store("first", 1));
    assert!(
        browser
            .accept_learning(&unchanged, control("session-a", 4, true))
            .unwrap()
    );
}

#[test]
fn learning_transport_keyed_nonlearning_retries_remain_available_in_original_store() {
    let mut browser = SkillBrowser::default();
    let service = store("first", 1);
    let request =
        json!({"operation":"proposal.reject", "proposal_id":"pending", "expected_state_version":1});
    assert!(browser.can_begin_at(&request, &service));
    browser.begin_at(&request, &service);
    browser.transport_failed(&request);
    assert!(browser.can_begin_at(&request, &store("first", 2)));
    assert!(!browser.can_begin_at(&request, &store("second", 1)));
    browser.complete_mutation(&request);
    assert!(!browser.mutations_pending());
    assert!(browser.learning.pending_request().is_none());
}

#[test]
fn learning_creation_retry_transport_pins_selected_job_store_and_unknown_receipt() {
    let mut browser = browser();
    let first = store("first", 1);
    let status = json!({"operation":"skill.learning.status"});
    browser.begin_at(&status, &first);
    browser.accept_learning(&status,json!({"version":"procedural-experience-v1","enabled":true,"mode":"review_candidates",
        "idle_seconds":300,"pending_inputs":0,"experiences":2,"consolidation_enabled":true,"patterns":1,"matching_enabled":true,
        "cross_session_enabled":false,"matches":1,"proposal_discovery_enabled":true})).unwrap();
    browser.learning.choose_pane(learning_state::Pane::Patterns);
    let pattern_id = format!("lpat_{}", "a".repeat(64));
    let experience_id = format!("lexp_{}", "b".repeat(64));
    let review = json!({"pattern_id":pattern_id,"pattern_revision":2,"state_version":0,"ignored":false,"adjudication_revision":0,"adjudication_id":""});
    let query = browser.learning.page_request().unwrap();
    browser.begin_at(&query, &first);
    browser.accept_learning(&query,json!({"patterns":[{"id":pattern_id,"session_id":"session-a","revision":2,"origin":"model_extracted",
        "quality":"unassessed","representative_id":experience_id,"experience_ids":[experience_id,format!("lexp_{}","e".repeat(64))],"episode_ids":["lep_a","lep_b"],
        "support_episodes":2,"observed_episodes":2,"evidence_state":"observed_execution","source_state":"not_revalidated","conflicts_with":[],
        "updated_at":"2026-10-09T08:20:00Z","review":review,"experience":{"kind":"proven_recipe","goal":"Read JSON","applicability":"JSON text",
        "resolution":"Read full file","steps":["Read"],"verification":"Saved receipt","outcome":"observed_execution","references":["receipt/local"]}}]})).unwrap();
    browser.learning.selected = Some(pattern_id.clone());
    let prepare = browser
        .learning
        .creation_retry_prepare_request("smj_previous")
        .unwrap();
    assert!(!browser.can_begin_at(&prepare, &first));
    let target = json!({"source_id":"project","source_identity":"c".repeat(64),"name":"json-audit","scope":"global"});
    let head =
        json!({"version":"","state_version":0,"exists":false,"enabled":false,"managed":false});
    browser.generation.accept(&json!({"operation":"skill.generation.get","job_id":"smj_previous"}),json!({"id":"smj_previous","target":target,"base":head,
        "state":"cancelled","phase":"generating","state_version":3,"attempts":0,"discovery_id":"sd_original","created_at":"2026-10-09T08:20:00Z"})).unwrap();
    assert!(browser.can_begin_at(&prepare, &first));
    browser.begin_at(&prepare, &first);
    let mut fixed = prepare["retry"].clone();
    fixed["request_key"] = json!("");
    fixed["expected_generation_state_version"] = json!(3);
    fixed["previous_proposal_id"] = json!("");
    fixed["expected_proposal_state_version"] = json!(0);
    fixed["target"] = target;
    fixed["expected_head"] = head;
    fixed["overlay_base"] = Value::Null;
    fixed["evidence_reservation_id"] = json!("sd_original");
    fixed["expected_retry_number"] = json!(1);
    let reference = json!({"previous_generation_id":"smj_previous","evidence_reservation_id":"sd_original","retry_number":1});
    browser.accept_learning(&prepare,json!({"request":fixed,"previous_generation_state":"cancelled","previous_proposal_status":"","retry":reference,"max_retries":3})).unwrap();
    let request = browser.learning.creation_retry_request().unwrap();
    assert!(browser.can_begin_at(&request, &first));
    browser.begin_at(&request, &first);
    assert!(browser.review_stale);
    browser.transport_failed(&request);
    assert!(browser.can_begin_at(&request, &store("first", 9)));
    assert!(!browser.can_begin_at(&request, &store("second", 1)));
    let mut changed = request.clone();
    changed["retry"]["request_key"] = json!(Uuid::new_v4().to_string());
    assert!(!browser.can_begin_at(&changed, &first));
    browser.set_learning_session(Some("session-b"));
    browser.begin_at(&request, &first);
    let mut receipt = review;
    receipt["state_version"] = json!(1);
    browser
        .accept_learning(
            &request,
            json!({"id":format!("lretry_{}","d".repeat(64)),"review":receipt,
        "generation_id":"smj_retry","discovery_id":"sd_retry","retry":reference}),
        )
        .unwrap();
    assert!(!browser.mutations_pending());
    assert!(browser.retry_service.is_none());
    assert!(browser.learning.last_result.is_none());
}
