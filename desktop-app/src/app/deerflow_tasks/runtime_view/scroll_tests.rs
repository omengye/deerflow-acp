use super::*;
use gpui::{ScrollDelta, ScrollWheelEvent, TestAppContext, size};
use serde_json::json;

fn update_plan(view: &mut RuntimeView, contents: &[&str], completed: bool) {
    let entries = contents
        .iter()
        .map(|content| {
            json!({"content": content, "status": if completed { "completed" } else { "in_progress" }})
        })
        .collect::<Vec<_>>();
    view.observe(
        &ActivityItem::new(Some("plan".into()), ActivityKind::Plan, "Plan", None, true)
            .with_arguments(Some(json!({"entries": entries}).to_string())),
    );
}

fn assert_anchor(view: &RuntimeView, row: &OverviewRow, offset: Pixels) {
    let top = view.list.logical_scroll_top();
    assert_eq!(&view.rows[top.item_ix], row);
    assert_eq!(top.offset_in_item, offset);
}

#[test]
fn todo_progress_and_insertions_preserve_the_visible_todo() {
    let mut view = RuntimeView::default();
    update_plan(&mut view, &["Inspect", "Review", "Review", "Verify"], false);
    let anchor = OverviewRow::Todo("Review".into(), 1);
    view.list.scroll_to(ListOffset {
        item_ix: view.rows.iter().position(|row| row == &anchor).unwrap(),
        offset_in_item: px(19.0),
    });
    update_plan(&mut view, &["Inspect", "Review", "Review", "Verify"], true);
    assert_anchor(&view, &anchor, px(19.0));
    update_plan(
        &mut view,
        &["New task", "Inspect", "Review", "Review", "Verify"],
        true,
    );
    assert_anchor(&view, &anchor, px(19.0));
    update_plan(&mut view, &["Review", "Review", "Verify"], false);
    assert_anchor(&view, &anchor, px(19.0));
}

#[test]
fn changing_todos_does_not_move_the_visible_agent_or_another_session() {
    let mut first = RuntimeView::default();
    first.agent_rows = vec![("run".into(), "worker".into())];
    first.sync_rows();
    let anchor = OverviewRow::Agent(first.agent_rows[0].clone());
    first.list.scroll_to(ListOffset {
        item_ix: first.rows.len() - 1,
        offset_in_item: px(23.0),
    });
    let mut second = RuntimeView::default();
    update_plan(&mut second, &["Other session"], false);
    update_plan(&mut first, &["Inspect", "Write", "Verify"], false);
    assert_anchor(&first, &anchor, px(23.0));
    assert_eq!(second.list.logical_scroll_top().item_ix, 0);
    update_plan(&mut first, &[], true);
    assert_anchor(&first, &anchor, px(23.0));
}

struct OverviewHarness {
    sessions: [(Uuid, RuntimeView); 2],
    selected: usize,
    show_subpage: bool,
    settings_height: f32,
    painted: Rc<RefCell<HashSet<usize>>>,
}

impl Render for OverviewHarness {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let (session, view) = &self.sessions[self.selected];
        let rows = view.rows.clone();
        let settings_height = self.settings_height;
        let painted = self.painted.clone();
        let body = if self.show_subpage {
            div().size_full().into_any_element()
        } else {
            overview_surface(
                *session,
                view,
                Theme::light(),
                cx.entity_id(),
                move |index, _, _| {
                    painted.borrow_mut().insert(index);
                    let height = match &rows[index] {
                        OverviewRow::Settings => settings_height,
                        OverviewRow::Status => 180.0,
                        OverviewRow::Todos | OverviewRow::Agents => 80.0,
                        OverviewRow::Todo(..) => 62.0,
                        OverviewRow::Agent(_) => 132.0,
                    };
                    div()
                        .h(px(height))
                        .w_full()
                        .when(index + 1 == rows.len(), |row| {
                            row.debug_selector(|| "overview-last-row".into())
                        })
                        .into_any_element()
                },
            )
            .into_any_element()
        };
        div()
            .size_full()
            .flex()
            .flex_col()
            .child(div().h(px(32.0)).flex_none())
            .child(div().flex_1().min_h_0().overflow_hidden().child(body))
    }
}

fn harness(cx: &mut Context<OverviewHarness>) -> OverviewHarness {
    let sessions = std::array::from_fn(|_| {
        let mut view = RuntimeView::default();
        view.focus = Some(cx.focus_handle().tab_index(0).tab_stop(true));
        view.agent_rows = (0..120)
            .map(|index| ("run".into(), format!("worker-{index}")))
            .collect();
        update_plan(&mut view, &["Inspect", "Write", "Verify"], false);
        (Uuid::new_v4(), view)
    });
    OverviewHarness {
        sessions,
        selected: 0,
        show_subpage: false,
        settings_height: 220.0,
        painted: Rc::default(),
    }
}

#[gpui::test]
fn whole_overview_scrolls_in_short_windows_and_keeps_refresh_position(cx: &mut TestAppContext) {
    let (entity, cx) = cx.add_window_view(|_, cx| harness(cx));
    for (width, height) in [(280.0, 240.0), (360.0, 600.0), (500.0, 900.0)] {
        cx.simulate_resize(size(px(width), px(height)));
        let scroll = cx.read_entity(&entity, |h, _| h.sessions[0].1.list.clone());
        entity.update(cx, |h, cx| {
            h.painted.borrow_mut().clear();
            scroll.scroll_to(ListOffset::default());
            cx.notify();
        });
        cx.run_until_parked();
        assert!(scroll.viewport_bounds().bottom() <= px(height));
        assert!(scroll.viewport_bounds().size.height > px(0.0));
        cx.read_entity(&entity, |h, _| {
            assert!(
                h.painted.borrow().len() < 30,
                "overview must remain virtualized"
            );
        });

        cx.simulate_event(ScrollWheelEvent {
            position: point(px(width / 2.0), px(100.0)),
            delta: ScrollDelta::Pixels(point(px(0.0), px(-160.0))),
            ..Default::default()
        });
        cx.run_until_parked();
        assert!(scroll.logical_scroll_top().offset_in_item > px(0.0));

        // Enter the agent section, then simulate an automatic snapshot refresh
        // that changes earlier sections as well as adding a new plan item.
        entity.update(cx, |h, cx| {
            scroll.scroll_to(ListOffset {
                item_ix: h.sessions[0].1.agent_heading_index() + 5,
                offset_in_item: px(17.0),
            });
            cx.notify();
        });
        cx.run_until_parked();
        let anchor = cx.read_entity(&entity, |h, _| {
            h.sessions[0].1.rows[scroll.logical_scroll_top().item_ix].clone()
        });
        entity.update(cx, |h, cx| {
            h.settings_height += 40.0;
            update_plan(
                &mut h.sessions[0].1,
                &["New task", "Inspect", "Write", "Verify"],
                true,
            );
            cx.notify();
        });
        cx.run_until_parked();
        cx.read_entity(&entity, |h, _| {
            assert_anchor(&h.sessions[0].1, &anchor, px(17.0))
        });

        // Unmounting for task details or another session must keep the same list.
        for (selected, subpage) in [(1, false), (0, true), (0, false)] {
            entity.update(cx, |h, cx| {
                h.selected = selected;
                h.show_subpage = subpage;
                cx.notify();
            });
            cx.run_until_parked();
        }
        cx.read_entity(&entity, |h, _| {
            assert_anchor(&h.sessions[0].1, &anchor, px(17.0))
        });

        let focus = cx.read_entity(&entity, |h, _| h.sessions[0].1.focus.clone().unwrap());
        cx.update(|window, cx| window.focus(&focus, cx));
        cx.simulate_keystrokes("end");
        cx.run_until_parked();
        let bounds = cx.debug_bounds("overview-last-row").unwrap();
        assert!(bounds.top() >= scroll.viewport_bounds().top());
        assert!(bounds.bottom() <= scroll.viewport_bounds().bottom());
        cx.simulate_keystrokes("home");
        cx.run_until_parked();
        assert_eq!(scroll.logical_scroll_top().item_ix, 0);
        assert_eq!(scroll.logical_scroll_top().offset_in_item, px(0.0));
    }
}
