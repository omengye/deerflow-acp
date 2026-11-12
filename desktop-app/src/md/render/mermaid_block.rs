//! Mermaid code-block presentation. The existing header/copy/selection path
//! remains shared with ordinary code; this body swaps in a cached native image.

use super::*;
use crate::md::mermaid::{self, Key, ResultState};
use crate::md::mermaid_viewer::OpenPreview;
use gpui::{App, EntityId, FocusHandle, RenderOnce};

#[derive(Default)]
pub(super) struct Controls {
    source: Cell<bool>,
    preview_focus: RefCell<Option<FocusHandle>>,
}

#[derive(IntoElement)]
pub(super) struct Toolbar {
    pub id: SharedString,
    pub source: SharedString,
    pub font_size: f32,
    pub streaming: bool,
    pub controls: Rc<Controls>,
    pub palette: Palette,
}

impl RenderOnce for Toolbar {
    fn render(self, window: &mut Window, _: &mut App) -> impl IntoElement {
        // current_view is only valid while GPUI is rendering this element.
        // Event dispatch runs after the rendered-entity stack has been popped.
        let owner = window.current_view();
        let source = self.controls.source.get();
        let request = OpenPreview {
            key: Key::new(
                self.source,
                self.palette.is_dark,
                self.font_size,
                window.scale_factor(),
            ),
        };
        div()
            .flex()
            .flex_none()
            .items_center()
            .gap(px(4.0))
            .child(toggle_button(
                format!("{}-source", self.id).into(),
                if source {
                    tr!("mermaid.show_diagram")
                } else {
                    tr!("mermaid.show_source")
                },
                self.controls.clone(),
                None,
                self.palette,
                owner,
            ))
            .when(!source && !self.streaming, |toolbar| {
                toolbar.child(toggle_button(
                    format!("{}-zoom", self.id).into(),
                    tr!("mermaid.expand"),
                    self.controls,
                    Some(request),
                    self.palette,
                    owner,
                ))
            })
    }
}

fn toggle_button(
    id: SharedString,
    label: String,
    controls: Rc<Controls>,
    request: Option<OpenPreview>,
    palette: Palette,
    owner: EntityId,
) -> AnyElement {
    let keyboard_controls = controls.clone();
    let open = request.is_some();
    let toggle = move |controls: &Controls, window: &mut Window, cx: &mut App| {
        if let Some(request) = &request {
            window.dispatch_action(Box::new(request.clone()), cx);
        } else {
            controls.source.set(!controls.source.get());
            cx.notify(owner);
        }
    };
    let keyboard_toggle = toggle.clone();
    div()
        .id(id)
        .debug_selector(move || {
            if open {
                "mermaid-open-viewer"
            } else {
                "mermaid-source-toggle"
            }
            .into()
        })
        .tab_index(0)
        .h(px(24.0))
        .px(px(6.0))
        .rounded(px(4.0))
        .flex()
        .items_center()
        .text_size(px(12.0))
        .text_color(palette.secondary)
        .cursor_default()
        .hover(|style| style.bg(palette.overlay))
        .focus_visible(|style| style.border_1().border_color(palette.accent))
        .child(label)
        .on_click(move |_, window, cx| {
            toggle(&controls, window, cx);
            cx.stop_propagation();
        })
        .on_key_down(move |event: &KeyDownEvent, window, cx| {
            if matches!(event.keystroke.key.as_str(), "enter" | "space") {
                keyboard_toggle(&keyboard_controls, window, cx);
                cx.stop_propagation();
            }
        })
        .into_any_element()
}

#[derive(IntoElement)]
pub(super) struct Body {
    pub id: SharedString,
    pub source: SharedString,
    pub controls: Rc<Controls>,
    pub palette: Palette,
    pub font_size: f32,
    pub streaming: bool,
    pub searching: bool,
    pub fallback: AnyElement,
}

impl RenderOnce for Body {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        if self.controls.source.get() || self.searching {
            return self.fallback;
        }
        // An incomplete streamed graph is shown as selectable source. Parse
        // once the message settles, rather than queueing one graph per token.
        if self.streaming {
            return self.with_status(tr!("mermaid.streaming"), None);
        }
        let key = Key::new(
            self.source.clone(),
            self.palette.is_dark,
            self.font_size,
            window.scale_factor(),
        );
        let owner = window.current_view();
        match mermaid::request(&key, owner, cx) {
            ResultState::Pending => self.with_status(tr!("mermaid.rendering"), None),
            ResultState::Failed(error) => self.with_status(tr!("mermaid.failed"), Some(error)),
            ResultState::Ready(rendered) => {
                let request = OpenPreview { key };
                let keyboard_request = request.clone();
                let focus = self
                    .controls
                    .preview_focus
                    .borrow_mut()
                    .get_or_insert_with(|| cx.focus_handle())
                    .clone();
                // Bound both axes without distorting or cropping tall graphs.
                // The aspect-ratio wrapper avoids GPUI Img's auto-height gap.
                let image = div()
                    .relative()
                    .w_full()
                    .max_w(px(rendered
                        .width
                        .min(358.0 * rendered.width / rendered.height)))
                    .max_h(px(358.0))
                    .aspect_ratio(rendered.width / rendered.height)
                    .child(
                        img(rendered.image.clone())
                            .absolute()
                            .size_full()
                            .object_fit(gpui::ObjectFit::Contain),
                    );
                div()
                    .id(self.id)
                    .debug_selector(|| "mermaid-viewport".into())
                    .w_full()
                    .min_w_0()
                    .flex()
                    .justify_center()
                    .track_focus(&focus)
                    .tab_index(0)
                    .border_1()
                    .border_color(gpui::transparent_black())
                    .cursor_pointer()
                    .tooltip(Tooltip::text(tr!("mermaid.expand")))
                    .focus_visible(|style| style.border_color(self.palette.accent))
                    .on_mouse_down(MouseButton::Left, move |_, window, cx| {
                        // Focus explicitly before stopping transcript selection;
                        // stopping propagation also skips GPUI's default focus.
                        window.focus(&focus, cx);
                        cx.stop_propagation();
                    })
                    .on_click(move |_, window, cx| {
                        window.dispatch_action(Box::new(request.clone()), cx);
                        cx.stop_propagation();
                    })
                    .on_key_down(move |event: &KeyDownEvent, window, cx| {
                        if matches!(event.keystroke.key.as_str(), "enter" | "space") {
                            window.dispatch_action(Box::new(keyboard_request.clone()), cx);
                            cx.stop_propagation();
                        }
                    })
                    .child(image)
                    .into_any_element()
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const DOCUMENT: &str =
        "```mermaid\nflowchart LR\nA[开始] --> B{验收}\nB -->|通过| C[完成]\n```";

    struct Harness {
        markdown: MarkdownView,
        selection: TranscriptSelection,
        preview: Option<crate::md::mermaid_viewer::Preview>,
        wheel_events: usize,
    }

    impl gpui::Render for Harness {
        fn render(&mut self, _: &mut Window, cx: &mut gpui::Context<Self>) -> impl IntoElement {
            let palette = Palette::from_theme(&Theme::dark());
            let ctx = Ctx::new(
                "mermaid-test",
                &palette,
                Metrics::BODY,
                self.selection.clone(),
            );
            div()
                .relative()
                .size_full()
                .on_scroll_wheel(cx.listener(|this, _, _, _| this.wheel_events += 1))
                .on_action(cx.listener(|this, request: &OpenPreview, window, cx| {
                    this.preview = Some(crate::md::mermaid_viewer::Preview::new(
                        request,
                        window,
                        cx,
                        |this, window, cx| {
                            if let Some(preview) = this.preview.take() {
                                preview.restore_focus(window, cx);
                                cx.notify();
                            }
                        },
                    ));
                    cx.notify();
                }))
                .child(
                    div()
                        .w(px(500.0))
                        .child(frame_reset(self.selection.clone()))
                        .children(markdown(&self.markdown, &ctx)),
                )
                .children(self.preview.as_ref().map(|preview| preview.render()))
        }
    }

    fn settle(cx: &mut gpui::VisualTestContext) {
        cx.run_until_parked();
        // The test platform has no frame clock. Deliver the two frame callbacks
        // used by native deferred overlays before dispatching keyboard input.
        for _ in 0..2 {
            cx.update(|window, cx| window.simulate_next_frame(cx));
            cx.run_until_parked();
        }
    }

    fn click(cx: &mut gpui::VisualTestContext, selector: &'static str) {
        settle(cx);
        let position = cx
            .debug_bounds(selector)
            .expect("visible Mermaid control")
            .center();
        cx.simulate_mouse_move(position, None, gpui::Modifiers::none());
        cx.simulate_click(position, gpui::Modifiers::none());
        settle(cx);
    }

    #[gpui::test]
    fn streamed_source_becomes_a_diagram_and_source_toggle_preserves_text(
        cx: &mut gpui::TestAppContext,
    ) {
        let mut markdown = MarkdownView::new();
        markdown.set_text(DOCUMENT, true);
        let (view, cx) = cx.add_window_view(|_, _| Harness {
            markdown,
            selection: TranscriptSelection::default(),
            preview: None,
            wheel_events: 0,
        });
        settle(cx);
        view.read_with(cx, |view, _| {
            assert_eq!(view.selection.registry.borrow().entries().len(), 1);
        });
        view.update(cx, |view, cx| {
            view.markdown.set_text(DOCUMENT, false);
            cx.notify();
        });
        settle(cx);
        view.read_with(cx, |view, _| {
            assert!(
                view.selection.registry.borrow().entries().is_empty(),
                "completed Mermaid must display its image, not fallback code"
            );
        });
        // Exercise event dispatch: mutating Controls directly bypasses the
        // callback and cannot detect the current_view() click-time panic.
        click(cx, "mermaid-source-toggle");
        view.read_with(cx, |view, _| {
            let registry = view.selection.registry.borrow();
            assert_eq!(registry.entries().len(), 1);
            assert!(registry.entries()[0].text.contains("A[开始] --> B{验收}"));
        });
        click(cx, "mermaid-source-toggle");
        view.read_with(cx, |view, _| {
            assert!(view.selection.registry.borrow().entries().is_empty());
        });
    }

    #[gpui::test]
    fn preview_opens_outside_the_row_and_restores_focus_after_repeated_close(
        cx: &mut gpui::TestAppContext,
    ) {
        let mut markdown = MarkdownView::new();
        markdown.set_text("```mermaid\nflowchart LR\nA[开始] --> B[准备] --> C[构建] --> D[测试] --> E[备份] --> F[发布] --> G[验证] --> H[完成]\n```", false);
        let (view, cx) = cx.add_window_view(|_, _| Harness {
            markdown,
            selection: TranscriptSelection::default(),
            preview: None,
            wheel_events: 0,
        });
        settle(cx);
        let controls = view.read_with(cx, |view, _| {
            view.markdown
                .mermaid_controls
                .borrow()
                .values()
                .next()
                .unwrap()
                .clone()
        });

        // Real event dispatch guards against the previous click-time panic.
        for _ in 0..3 {
            click(cx, "mermaid-open-viewer");
            assert!(cx.debug_bounds("mermaid-viewer-stage").unwrap().size.width > px(500.0));
            cx.simulate_keystrokes("escape");
            settle(cx);
            view.read_with(cx, |view, _| assert!(view.preview.is_none()));
        }
        // Closing restores the toolbar button, so Enter opens it again.
        cx.simulate_keystrokes("enter");
        settle(cx);
        view.read_with(cx, |view, _| assert!(view.preview.is_some()));
        click(cx, "mermaid-viewer-close");
        click(cx, "mermaid-source-toggle");
        assert!(controls.source.get());
        cx.simulate_keystrokes("enter");
        assert!(!controls.source.get());
        cx.simulate_keystrokes("space");
        assert!(controls.source.get());
        cx.simulate_keystrokes("enter");
        assert!(!controls.source.get());

        let before = cx.debug_bounds("mermaid-viewport").unwrap();
        cx.simulate_event(gpui::ScrollWheelEvent {
            position: before.center(),
            delta: gpui::ScrollDelta::Lines(point(0.0, -1.0)),
            ..Default::default()
        });
        view.read_with(cx, |view, _| assert_eq!(view.wheel_events, 1));
        click(cx, "mermaid-viewport");
        let stage = cx.debug_bounds("mermaid-viewer-stage").unwrap();
        cx.simulate_mouse_move(stage.center(), None, gpui::Modifiers::none());
        cx.simulate_event(gpui::ScrollWheelEvent {
            position: stage.center(),
            delta: gpui::ScrollDelta::Lines(point(0.0, 1.0)),
            ..Default::default()
        });
        view.read_with(cx, |view, _| {
            assert_eq!(view.wheel_events, 1, "viewer wheel must not scroll chat")
        });
        click(cx, "mermaid-viewer-in");
        cx.simulate_keystrokes("right down left up");
        click(cx, "mermaid-viewer-fit");
        cx.simulate_keystrokes("escape");
        settle(cx);
        assert_eq!(cx.debug_bounds("mermaid-viewport").unwrap(), before);
        cx.simulate_keystrokes("space");
        settle(cx);
        view.read_with(cx, |view, _| assert!(view.preview.is_some()));
        cx.simulate_keystrokes("escape");
        settle(cx);
        view.read_with(cx, |view, _| {
            assert!(view.selection.registry.borrow().entries().is_empty())
        });
    }

    #[gpui::test]
    fn failed_diagrams_keep_selectable_source(cx: &mut gpui::TestAppContext) {
        let mut markdown = MarkdownView::new();
        markdown.set_text("```mermaid\nflowchart LR\nA[unfinished\n```", false);
        let (view, cx) = cx.add_window_view(|_, _| Harness {
            markdown,
            selection: TranscriptSelection::default(),
            preview: None,
            wheel_events: 0,
        });
        settle(cx);
        view.read_with(cx, |view, _| {
            let registry = view.selection.registry.borrow();
            assert_eq!(registry.entries().len(), 1);
            assert!(registry.entries()[0].text.contains("A[unfinished"));
        });
    }

    #[gpui::test]
    fn tall_chat_preview_fits_the_height_without_cropping(cx: &mut gpui::TestAppContext) {
        let mut markdown = MarkdownView::new();
        markdown.set_text("```mermaid\nflowchart TD\nA[开始] --> B[准备] --> C[构建] --> D[测试] --> E[备份] --> F[发布] --> G[验证] --> H[完成]\n```", false);
        let (_, cx) = cx.add_window_view(|_, _| Harness {
            markdown,
            selection: TranscriptSelection::default(),
            preview: None,
            wheel_events: 0,
        });
        settle(cx);
        let bounds = cx.debug_bounds("mermaid-viewport").unwrap();
        assert!(
            bounds.size.height <= px(360.1),
            "preview bounds: {bounds:?}"
        );
        assert!(bounds.size.height > px(100.0));
    }
}

impl Body {
    fn with_status(self, label: String, error: Option<SharedString>) -> AnyElement {
        div()
            .w_full()
            .min_w_0()
            .flex()
            .flex_col()
            .gap(px(6.0))
            .child(
                div()
                    .text_size(px(12.0))
                    .text_color(self.palette.secondary)
                    .child(label),
            )
            .when_some(error, |body, error| {
                body.child(
                    div()
                        .text_size(px(12.0))
                        .text_color(self.palette.secondary)
                        .child(error),
                )
            })
            .child(self.fallback)
            .into_any_element()
    }
}
