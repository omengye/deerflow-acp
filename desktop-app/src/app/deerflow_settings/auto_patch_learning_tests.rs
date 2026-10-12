use super::*;
use serde_json::json;

fn options() -> Value {
    json!({"experience_patterns_enabled":true, "max_changed_lines":40, "max_daily_publications":2,
        "observation_hours":72, "min_observations":3, "rollback_failure_threshold":2})
}

fn observation() -> Value {
    json!({"proposal_id":"p_learning", "target":{"source_id":"project", "source_identity":"a".repeat(64),
        "name":"json-check", "scope":"workspace", "workspace":"D:\\workspace"},
        "publication_revision":3, "baseline_revision":2,
        "head":{"version":"b".repeat(64), "exists":true, "enabled":true, "managed":true, "state_version":3},
        "options":options(), "status":"observing", "started_at":"2026-10-09T08:00:00Z",
        "deadline":"2026-10-12T08:00:00Z", "updated_at":"2026-10-09T08:01:00Z",
        "successes":0, "failures":0, "consecutive_failures":0, "ignored":0,
        "learning_source":{"generation_id":"smj_learning", "discovery_id":"sd_learning",
            "discovery_sha256":"c".repeat(64), "pattern_id":"lpat_learning", "pattern_revision":2,
            "match_id":"lmatch_learning", "policy_identity":"d".repeat(64), "evidence_sha256":"e".repeat(64),
            "source_state":"valid_at_publication"}})
}

#[test]
fn auto_patch_learning_status_preserves_old_disabled_grant() {
    let request = json!({"operation":"skill.auto_patch.status"});
    let mut opts = options();
    opts.as_object_mut()
        .unwrap()
        .remove("experience_patterns_enabled");
    let mut browser = AutoPatchBrowser::default();
    browser
        .accept(
            &request,
            json!({"enabled":true, "running":true, "options":opts,
        "today":"2026-10-09", "published_today":0, "observing":0}),
        )
        .unwrap();
    assert!(
        browser
            .status
            .as_ref()
            .unwrap()
            .1
            .contains("学习来源自动补丁未授权")
    );
    browser
        .accept(
            &request,
            json!({"enabled":true, "running":true, "options":options(),
        "today":"2026-10-09", "published_today":0, "observing":0}),
        )
        .unwrap();
    assert!(
        browser
            .status
            .as_ref()
            .unwrap()
            .1
            .contains("学习来源自动补丁已授权")
    );
}

#[test]
fn auto_patch_learning_observation_is_historical_and_keeps_withdrawal_distinct() {
    let request = json!({"operation":"skill.auto_patch.list", "limit":PAGE_SIZE});
    let mut browser = AutoPatchBrowser::default();
    for (state, phrase) in [
        ("valid_at_publication", "不是当前来源授权"),
        ("withdrawn", "已采用版本保留"),
        ("unavailable", "不作为质量判断"),
    ] {
        let mut item = observation();
        item["learning_source"]["source_state"] = json!(state);
        browser
            .accept(&request, json!({"observations":[item]}))
            .unwrap();
        assert!(browser.rows[0].1.contains(phrase));
        assert!(browser.rows[0].1.contains("不把经验摘要当质量票"));
    }
}

#[test]
fn auto_patch_learning_invalid_source_cannot_replace_cached_history() {
    let request = json!({"operation":"skill.auto_patch.list", "limit":PAGE_SIZE});
    let mut browser = AutoPatchBrowser::default();
    browser
        .accept(&request, json!({"observations":[observation()]}))
        .unwrap();
    let original = browser.rows.clone();
    for (field, value) in [
        ("source_state", json!("quality_passed")),
        ("pattern_revision", json!(0)),
        ("policy_identity", json!("untrusted")),
        ("generation_id", json!("foreign")),
        ("evidence_sha256", json!("untrusted")),
    ] {
        let mut item = observation();
        item["learning_source"][field] = value;
        assert!(
            browser
                .accept(&request, json!({"observations":[item]}))
                .is_err()
        );
        assert_eq!(browser.rows, original);
    }
    let mut item = observation();
    item["options"]["experience_patterns_enabled"] = json!(false);
    assert!(
        browser
            .accept(&request, json!({"observations":[item]}))
            .is_err()
    );
    assert_eq!(browser.rows, original);
}
