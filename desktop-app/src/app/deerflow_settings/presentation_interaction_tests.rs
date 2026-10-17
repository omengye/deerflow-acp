use super::*;
use gpui::{Modifiers, TestAppContext, VisualTestContext, point, size};
use std::cell::Cell;

const HEADERS: [&str; 7] = [
    "df-skill-options-general",
    "df-skill-options-generation",
    "df-skill-options-discovery",
    "df-skill-options-budget",
    "df-skill-options-validation",
    "df-skill-options-behavior",
    "df-skill-options-auto_patch",
];

struct Harness {
    state: DeerFlowSettings,
    scroll: ScrollHandle,
    form_renders: Cell<usize>,
    rendered_fields: std::cell::RefCell<Vec<String>>,
}

impl Harness {
    fn new(window: &mut Window, cx: &mut Context<Self>) -> Self {
        let mut state = DeerFlowSettings::new(window, cx);
        state.section = Section::SkillCenter;
        state.skill_center_tab = SkillCenterTab::Automation;
        state.draft = Some(json!({"skill_evolution": {"enabled": true}}));
        state.fields = evolution::fields();
        Self {
            state,
            scroll: ScrollHandle::new(),
            form_renders: Cell::new(0),
            rendered_fields: Default::default(),
        }
    }
}

impl Render for Harness {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        self.form_renders.set(0);
        self.rendered_fields.borrow_mut().clear();
        let content = compose_settings_content(
            self.state.section,
            self.state.skill_center_tab,
            div(),
            || {
                self.form_renders.set(self.form_renders.get() + 1);
                render_skill_groups(
                    &self.state,
                    Theme::dark(),
                    cx,
                    |this: &mut Self, key, cx| {
                        this.state.toggle_skill_group(key);
                        cx.notify();
                    },
                    |field, _| {
                        self.rendered_fields.borrow_mut().push(field.path.clone());
                        div().h(px(10.0)).into_any_element()
                    },
                )
                .into_any_element()
            },
        );
        div()
            .id("settings-test-scroll")
            .size_full()
            .overflow_y_scroll()
            .track_scroll(&self.scroll)
            .child(content)
    }
}

fn click_header(cx: &mut VisualTestContext, selector: &'static str) {
    let bounds = cx.debug_bounds(selector).expect("group header is rendered");
    cx.simulate_click(bounds.center(), Modifiers::none());
}

#[test]
fn form_fields_have_one_page_owner() {
    // Protect the composition boundary as well as the helper: a second child
    // call would reintroduce duplicate IDs even if the helper tests still pass.
    let outer = include_str!("../deerflow_settings.rs");
    let children = include_str!("presentation.rs");
    let call = "self.render_deerflow_form_fields(";
    assert_eq!(outer.matches(call).count(), 1);
    assert_eq!(children.matches(call).count(), 0);
}

#[gpui::test]
fn form_composition_mounts_once_only_on_settings_tabs(cx: &mut TestAppContext) {
    let (view, cx) = cx.add_window_view(Harness::new);
    for section in [
        Section::Common,
        Section::Advanced,
        Section::SkillCenter,
        Section::Sources,
        Section::MCP,
        Section::Overview,
        Section::Models,
        Section::Agents,
        Section::Memory,
        Section::Skills,
        Section::Tools,
        Section::Runtime,
    ] {
        for tab in [
            SkillCenterTab::Catalog,
            SkillCenterTab::Review,
            SkillCenterTab::Generation,
            SkillCenterTab::History,
            SkillCenterTab::Automation,
        ] {
            view.update(cx, |this, cx| {
                this.state.section = section;
                this.state.skill_center_tab = tab;
                cx.notify();
            });
            let expected =
                usize::from(section != Section::SkillCenter || tab == SkillCenterTab::Automation);
            view.read_with(cx, |this, _| assert_eq!(this.form_renders.get(), expected));
        }
    }
}

#[gpui::test]
fn every_skill_group_opens_and_closes_by_mouse(cx: &mut TestAppContext) {
    let (view, cx) = cx.add_window_view(Harness::new);
    cx.simulate_resize(size(px(1100.0), px(1200.0)));
    assert_eq!(SKILL_GROUPS.len(), HEADERS.len());
    for (&(key, _), selector) in SKILL_GROUPS.iter().zip(HEADERS) {
        click_header(cx, selector);
        view.read_with(cx, |this, _| {
            assert!(this.state.skill_group_selected(key), "{key} should open");
            assert_eq!(this.form_renders.get(), 1);
            let expected: Vec<_> = this
                .state
                .fields
                .iter()
                .filter(|field| skill_group(&field.path) == key)
                .map(|field| field.path.clone())
                .collect();
            assert!(!expected.is_empty(), "{key} must have editable fields");
            assert_eq!(*this.rendered_fields.borrow(), expected);
        });
        click_header(cx, selector);
        view.read_with(cx, |this, _| {
            assert!(this.state.search_target.is_none());
            assert!(this.rendered_fields.borrow().is_empty());
        });
    }
}

#[gpui::test]
fn pending_requests_do_not_block_disclosure_or_unlock_editing(cx: &mut TestAppContext) {
    let (view, cx) = cx.add_window_view(Harness::new);
    cx.simulate_resize(size(px(1100.0), px(1200.0)));
    for operation in ["snapshot", "save", "validate", "manage", "apply"] {
        view.update(cx, |this, cx| {
            this.state.pending = Some(operation.into());
            cx.notify();
        });
        let before = view.read_with(cx, |this, _| this.state.draft.clone());
        for (&(key, _), selector) in SKILL_GROUPS.iter().zip(HEADERS) {
            click_header(cx, selector);
            view.read_with(cx, |this, _| {
                assert!(this.state.skill_group_selected(key));
                assert!(!this.state.can_edit());
                assert!(!this.state.can_edit_field("/skill_evolution/enabled"));
            });
            click_header(cx, selector);
            view.read_with(cx, |this, _| assert!(this.state.search_target.is_none()));
        }
        view.read_with(cx, |this, _| {
            assert_eq!(this.state.draft, before);
            assert!(!this.state.dirty);
            assert_eq!(this.state.pending.as_deref(), Some(operation));
        });
    }
}

#[gpui::test]
fn skill_groups_still_accept_clicks_after_scrolling(cx: &mut TestAppContext) {
    let (view, cx) = cx.add_window_view(Harness::new);
    cx.simulate_resize(size(px(1100.0), px(260.0)));
    let scroll = view.read_with(cx, |this, _| this.scroll.clone());
    for (index, (&(key, _), selector)) in SKILL_GROUPS.iter().zip(HEADERS).enumerate() {
        cx.update(|window, _| {
            scroll.set_offset(point(px(0.0), -px(index as f32 * 76.0)));
            window.refresh();
        });
        click_header(cx, selector);
        view.read_with(cx, |this, _| assert!(this.state.skill_group_selected(key)));
        click_header(cx, selector);
        view.read_with(cx, |this, _| assert!(this.state.search_target.is_none()));
    }
    assert!(scroll.offset().y < px(0.0));
}

#[gpui::test]
fn skill_group_keyboard_activation_toggles_once(cx: &mut TestAppContext) {
    let (view, cx) = cx.add_window_view(Harness::new);
    click_header(cx, HEADERS[0]);
    view.read_with(cx, |this, _| {
        assert!(this.state.skill_group_selected("general"))
    });
    cx.simulate_keystrokes("enter");
    view.read_with(cx, |this, _| assert!(this.state.search_target.is_none()));
    cx.simulate_keystrokes("space");
    view.read_with(cx, |this, _| {
        assert!(this.state.skill_group_selected("general"))
    });
}

#[gpui::test]
fn search_and_validation_still_reveal_fields_without_editing(cx: &mut TestAppContext) {
    let (view, cx) = cx.add_window_view(Harness::new);
    let budget = "/skill_evolution/budget/max_daily_model_calls";
    let generation = "/skill_evolution/generation/timeout_seconds";
    view.update(cx, |this, cx| {
        this.state.search_target = Some(budget.into());
        this.state
            .errors
            .insert(generation.into(), "invalid".into());
        cx.notify();
    });
    view.read_with(cx, |this, _| {
        assert!(this.state.skill_group_selected("budget"));
        assert!(this.rendered_fields.borrow().iter().any(|p| p == budget));
        assert!(
            this.rendered_fields
                .borrow()
                .iter()
                .any(|p| p == generation)
        );
    });
    view.update(cx, |this, cx| {
        this.state.status_pending = true;
        this.state.status = json!({"running": true, "applying": true});
        cx.notify();
    });
    view.read_with(cx, |this, _| {
        assert!(this.state.skill_group_selected("budget"));
        assert!(!this.state.can_edit());
    });
    click_header(cx, HEADERS[0]);
    view.read_with(cx, |this, _| {
        assert!(this.state.skill_group_selected("general"));
        assert!(!this.state.can_edit());
        assert!(
            this.rendered_fields
                .borrow()
                .iter()
                .any(|p| p == generation)
        );
    });
}
