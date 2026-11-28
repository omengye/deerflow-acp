use super::ChannelSettings;
use super::conversation_list::can_open_channel_task;
use gpui::TestAppContext;
use serde_json::json;

#[test]
fn channel_tasks_open_for_existing_provider_sessions_and_retained_local_history() {
    for status in ["completed", "running", "waiting_approval", "idle"] {
        assert!(can_open_channel_task(
            &json!({"session_id": "task", "status": status})
        ));
    }
    assert!(can_open_channel_task(
        &json!({"session_id": "task", "status": "history_only", "local_history": true})
    ));
    assert!(!can_open_channel_task(
        &json!({"session_id": "task", "status": "history_only"})
    ));
    assert!(!can_open_channel_task(
        &json!({"session_id": "", "local_history": true})
    ));
    assert!(!can_open_channel_task(&json!({})));
}

#[gpui::test]
fn refreshing_channel_history_keeps_the_current_page_in_bounds(cx: &mut TestAppContext) {
    cx.add_empty_window().update(|window, cx| {
        let mut settings = ChannelSettings::new(window, cx);
        settings.history = (0..9)
            .map(|id| json!({"session_id": id.to_string()}))
            .collect();
        settings.history_page = 1;
        settings.clamp_history_page();
        assert_eq!(settings.history_page, 1);

        settings.history.truncate(8);
        settings.clamp_history_page();
        assert_eq!(settings.history_page, 0);

        settings.history.clear();
        settings.clamp_history_page();
        assert_eq!(settings.history_page, 0);
    });
}
