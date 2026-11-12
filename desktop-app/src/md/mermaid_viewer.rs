//! Window-level Mermaid viewer. Gestures only change cached-image geometry;
//! parsing and rasterization remain on the bounded Mermaid worker queue.

use std::{cell::Cell, rc::Rc, sync::Arc};

use gpui::{
    Action, AnyElement, App, Bounds, Context, CursorStyle, Entity, EventEmitter, FocusHandle,
    KeyDownEvent, MouseButton, MouseDownEvent, MouseMoveEvent, PinchEvent, Pixels, Point, Render,
    ScrollDelta, ScrollWheelEvent, Size, Subscription, Window, canvas, div, point, prelude::*, px,
    size,
};

use super::mermaid::{self, Key, Rendered, ResultState};
use crate::{theme::Theme, ui::tooltip::Tooltip};

#[derive(Clone, PartialEq, Action)]
#[action(namespace = mermaid_viewer, no_json)]
pub(crate) struct OpenPreview {
    pub(super) key: Key,
}

/// Retained by the window root, never by a virtualized Markdown row.
pub(crate) struct Preview {
    viewer: Entity<Viewer>,
    previous_focus: Option<FocusHandle>,
    _dismiss: Subscription,
}

impl Preview {
    pub(crate) fn new<T: 'static>(
        request: &OpenPreview,
        window: &mut Window,
        cx: &mut Context<T>,
        dismiss: impl Fn(&mut T, &mut Window, &mut Context<T>) + 'static,
    ) -> Self {
        let previous_focus = window.focused(cx);
        let viewer = cx.new(|cx| Viewer {
            key: request.key.clone(),
            rendered: None,
            viewport: Rc::new(Cell::new(Viewport::default())),
            drag: None,
            focus: cx.focus_handle(),
            buttons: std::array::from_fn(|_| cx.focus_handle()),
        });
        let subscription =
            cx.subscribe_in(&viewer, window, move |host, _, _: &Dismiss, window, cx| {
                dismiss(host, window, cx);
            });
        // Deferred overlays join the dispatch tree after their first frame.
        // A weak reference prevents a closed/replaced preview stealing focus.
        let weak = viewer.downgrade();
        window.on_next_frame(move |window, _| {
            window.on_next_frame(move |window, cx| {
                if let Some(viewer) = weak.upgrade() {
                    window.focus(&viewer.read(cx).focus.clone(), cx);
                }
            });
        });
        Self {
            viewer,
            previous_focus,
            _dismiss: subscription,
        }
    }

    pub(crate) fn restore_focus(&self, window: &mut Window, cx: &mut App) {
        if let Some(focus) = &self.previous_focus {
            window.focus(focus, cx);
        }
    }

    pub(crate) fn render(&self) -> AnyElement {
        gpui::deferred(div().absolute().inset_0().child(self.viewer.clone()))
            .with_priority(5)
            .into_any_element()
    }
}

#[derive(Clone, Copy, Debug)]
struct Viewport {
    bounds: Bounds<Pixels>,
    image: Size<Pixels>,
    zoom: f32,
    pan: Point<Pixels>,
    fitted: bool,
}

impl Default for Viewport {
    fn default() -> Self {
        Self {
            bounds: Bounds::default(),
            image: size(px(1.0), px(1.0)),
            zoom: 1.0,
            pan: Point::default(),
            fitted: true,
        }
    }
}

impl Viewport {
    fn fit_zoom(&self) -> f32 {
        let width = (self.bounds.size.width - px(40.0)).max(px(1.0));
        let height = (self.bounds.size.height - px(40.0)).max(px(1.0));
        (width / self.image.width)
            .min(height / self.image.height)
            .min(1.0)
    }

    fn fit(&mut self) {
        self.zoom = self.fit_zoom();
        self.pan = Point::default();
        self.fitted = true;
    }

    fn layout(&mut self, bounds: Bounds<Pixels>, image: Size<Pixels>) {
        self.bounds = bounds;
        self.image = image;
        if self.fitted {
            self.fit();
        } else {
            self.clamp_pan();
        }
    }

    fn zoom_at(&mut self, zoom: f32, anchor: Point<Pixels>) {
        if !zoom.is_finite() || zoom <= 0.0 {
            return;
        }
        let zoom = zoom.clamp(self.fit_zoom().min(0.1), 8.0);
        let relative = anchor - self.bounds.center();
        self.pan = relative - (relative - self.pan) * (zoom / self.zoom);
        self.zoom = zoom;
        self.fitted = false;
        self.clamp_pan();
    }

    fn move_by(&mut self, delta: Point<Pixels>) {
        self.pan += delta;
        self.fitted = false;
        self.clamp_pan();
    }

    fn clamp_pan(&mut self) {
        // Leave a visible portion even after a long drag; all four image edges
        // can still be brought into view at every zoom level.
        let limit = size(
            ((self.image.width * self.zoom + self.bounds.size.width) / 2.0 - px(48.0)).max(px(0.0)),
            ((self.image.height * self.zoom + self.bounds.size.height) / 2.0 - px(48.0))
                .max(px(0.0)),
        );
        self.pan.x = self.pan.x.clamp(-limit.width, limit.width);
        self.pan.y = self.pan.y.clamp(-limit.height, limit.height);
    }

    fn image_bounds(&self) -> Bounds<Pixels> {
        let scaled = size(self.image.width * self.zoom, self.image.height * self.zoom);
        Bounds::new(
            self.bounds.center() + self.pan - point(scaled.width, scaled.height) / 2.0,
            scaled,
        )
    }
}

struct Dismiss;

struct Viewer {
    key: Key,
    rendered: Option<Arc<Rendered>>,
    viewport: Rc<Cell<Viewport>>,
    drag: Option<Point<Pixels>>,
    focus: FocusHandle,
    buttons: [FocusHandle; 5],
}

impl EventEmitter<Dismiss> for Viewer {}

#[derive(Clone, Copy)]
enum Command {
    Out,
    In,
    Fit,
    Actual,
    Close,
}

impl Viewer {
    fn command(&mut self, command: Command, cx: &mut Context<Self>) {
        let mut viewport = self.viewport.get();
        match command {
            Command::Out => viewport.zoom_at(viewport.zoom / 1.25, viewport.bounds.center()),
            Command::In => viewport.zoom_at(viewport.zoom * 1.25, viewport.bounds.center()),
            Command::Fit => viewport.fit(),
            Command::Actual => {
                viewport.zoom = 1.0;
                viewport.pan = Point::default();
                viewport.fitted = false;
            }
            Command::Close => {
                cx.emit(Dismiss);
                return;
            }
        }
        self.drag = None;
        self.viewport.set(viewport);
        cx.notify();
    }

    fn key_down(&mut self, event: &KeyDownEvent, window: &mut Window, cx: &mut Context<Self>) {
        let command = match event.keystroke.key.as_str() {
            "escape" => Some(Command::Close),
            "+" | "=" | "add" => Some(Command::In),
            "-" | "subtract" => Some(Command::Out),
            "0" | "home" => Some(Command::Fit),
            "1" => Some(Command::Actual),
            "left" | "right" | "up" | "down" => {
                let delta = match event.keystroke.key.as_str() {
                    "left" => point(px(48.0), px(0.0)),
                    "right" => point(px(-48.0), px(0.0)),
                    "up" => point(px(0.0), px(48.0)),
                    _ => point(px(0.0), px(-48.0)),
                };
                let mut viewport = self.viewport.get();
                viewport.move_by(delta);
                self.viewport.set(viewport);
                cx.notify();
                None
            }
            "tab" => {
                let focus: Vec<_> = std::iter::once(&self.focus)
                    .chain(self.buttons.iter())
                    .collect();
                let current = focus
                    .iter()
                    .position(|handle| handle.is_focused(window))
                    .unwrap_or(0);
                let step = if event.keystroke.modifiers.shift {
                    focus.len() - 1
                } else {
                    1
                };
                window.focus(focus[(current + step) % focus.len()], cx);
                None
            }
            _ => return,
        };
        if let Some(command) = command {
            self.command(command, cx);
        }
        cx.stop_propagation();
    }

    fn button(
        &self,
        index: usize,
        selector: &'static str,
        label: String,
        tooltip: String,
        command: Command,
        theme: Theme,
        cx: &Context<Self>,
    ) -> AnyElement {
        div()
            .id(selector)
            .debug_selector(move || selector.into())
            .track_focus(&self.buttons[index])
            .tab_index(0)
            .border_1()
            .border_color(gpui::transparent_black())
            .h(px(32.0))
            .min_w(px(32.0))
            .px(px(10.0))
            .rounded(px(5.0))
            .flex()
            .items_center()
            .justify_center()
            .cursor_pointer()
            .hover(|style| style.bg(theme.overlay_strong))
            .focus_visible(|style| style.border_color(theme.accent))
            .tooltip(Tooltip::text(tooltip))
            .child(label)
            .on_click(cx.listener(move |this, _, _, cx| {
                this.command(command, cx);
                cx.stop_propagation();
            }))
            .on_key_down(cx.listener(move |this, event: &KeyDownEvent, _, cx| {
                if matches!(event.keystroke.key.as_str(), "enter" | "space") {
                    this.command(command, cx);
                    cx.stop_propagation();
                }
            }))
            .into_any_element()
    }
}

impl Render for Viewer {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = Theme::current(cx);
        let state = self
            .rendered
            .as_ref()
            .map(|image| ResultState::Ready(image.clone()))
            .unwrap_or_else(|| mermaid::request(&self.key, cx.entity_id(), cx));
        let content = match state {
            ResultState::Ready(rendered) => {
                self.rendered = Some(rendered.clone());
                let viewport = self.viewport.clone();
                let owner = cx.entity_id();
                canvas(
                    move |bounds, _, cx| {
                        let mut geometry = viewport.get();
                        let resized = geometry.bounds.size != bounds.size;
                        geometry.layout(bounds, size(px(rendered.width), px(rendered.height)));
                        viewport.set(geometry);
                        if resized {
                            cx.notify(owner);
                        }
                        (geometry.image_bounds(), rendered.image.clone())
                    },
                    |bounds, (image_bounds, image), window, _| {
                        let _ = window.paint_image(
                            bounds,
                            image_bounds,
                            Default::default(),
                            image,
                            0,
                            false,
                        );
                    },
                )
                .size_full()
                .into_any_element()
            }
            ResultState::Pending => div().child(tr!("mermaid.rendering")).into_any_element(),
            ResultState::Failed(error) => div().p(px(24.0)).child(error).into_any_element(),
        };
        let zoom = format!("{:.0}%", self.viewport.get().zoom * 100.0);
        let toolbar = div()
            .flex()
            .flex_wrap()
            .items_center()
            .gap(px(4.0))
            .p(px(8.0))
            .flex_none()
            .bg(theme.raised)
            .child(div().px(px(8.0)).child("Mermaid"))
            .child(div().flex_1())
            .child(self.button(
                0,
                "mermaid-viewer-out",
                "−".into(),
                tr!("mermaid.zoom_out"),
                Command::Out,
                theme,
                cx,
            ))
            .child(div().min_w(px(48.0)).text_center().child(zoom))
            .child(self.button(
                1,
                "mermaid-viewer-in",
                "+".into(),
                tr!("mermaid.zoom_in"),
                Command::In,
                theme,
                cx,
            ))
            .child(self.button(
                2,
                "mermaid-viewer-fit",
                tr!("mermaid.fit"),
                tr!("mermaid.fit"),
                Command::Fit,
                theme,
                cx,
            ))
            .child(self.button(
                3,
                "mermaid-viewer-actual",
                "100%".into(),
                tr!("mermaid.actual_size"),
                Command::Actual,
                theme,
                cx,
            ))
            .child(self.button(
                4,
                "mermaid-viewer-close",
                tr!("common.close"),
                tr!("common.close"),
                Command::Close,
                theme,
                cx,
            ));
        let stage = div()
            .id("mermaid-viewer-stage")
            .debug_selector(|| "mermaid-viewer-stage".into())
            .track_focus(&self.focus)
            .tab_index(0)
            .border_1()
            .border_color(gpui::transparent_black())
            .relative()
            .w_full()
            .flex_1()
            .min_h_0()
            .overflow_hidden()
            .cursor(if self.drag.is_some() {
                CursorStyle::ClosedHand
            } else {
                CursorStyle::OpenHand
            })
            .focus_visible(|style| style.border_color(theme.accent))
            .on_mouse_down(
                MouseButton::Left,
                cx.listener(|this, event: &MouseDownEvent, window, cx| {
                    window.focus(&this.focus, cx);
                    if event.click_count == 2 {
                        this.command(Command::Fit, cx);
                    } else {
                        this.drag = Some(event.position);
                        cx.notify();
                    }
                    cx.stop_propagation();
                }),
            )
            .on_scroll_wheel(cx.listener(|this, event: &ScrollWheelEvent, _, cx| {
                let delta = match event.delta {
                    ScrollDelta::Pixels(delta) => f32::from(delta.y),
                    ScrollDelta::Lines(delta) => delta.y * 24.0,
                };
                let mut viewport = this.viewport.get();
                viewport.zoom_at(
                    viewport.zoom * (delta * 0.01).clamp(-1.0, 1.0).exp(),
                    event.position,
                );
                this.viewport.set(viewport);
                cx.notify();
                cx.stop_propagation();
            }))
            .on_pinch(cx.listener(|this, event: &PinchEvent, _, cx| {
                let mut viewport = this.viewport.get();
                viewport.zoom_at(viewport.zoom * (1.0 + event.delta), event.position);
                this.viewport.set(viewport);
                cx.notify();
                cx.stop_propagation();
            }))
            .child(content);
        div()
            .id("mermaid-viewer")
            .debug_selector(|| "mermaid-viewer".into())
            .size_full()
            .occlude()
            .bg(gpui::black().opacity(0.75))
            .p(px(16.0))
            .text_color(theme.text)
            .text_size(px(13.0))
            .tab_group()
            .tab_stop(false)
            .capture_key_down(cx.listener(Self::key_down))
            .on_mouse_move(cx.listener(|this, event: &MouseMoveEvent, _, cx| {
                if let Some(previous) = this.drag {
                    if event.pressed_button == Some(MouseButton::Left) {
                        let mut viewport = this.viewport.get();
                        viewport.move_by(event.position - previous);
                        this.viewport.set(viewport);
                        this.drag = Some(event.position);
                    } else {
                        this.drag = None;
                    }
                    cx.notify();
                    cx.stop_propagation();
                }
            }))
            .on_mouse_up(
                MouseButton::Left,
                cx.listener(|this, _, _, cx| {
                    if this.drag.take().is_some() {
                        cx.notify();
                    }
                    cx.stop_propagation();
                }),
            )
            .on_mouse_up_out(
                MouseButton::Left,
                cx.listener(|this, _, _, cx| {
                    if this.drag.take().is_some() {
                        cx.notify();
                    }
                }),
            )
            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
            .on_scroll_wheel(|_, _, cx| cx.stop_propagation())
            .child(
                div()
                    .size_full()
                    .flex()
                    .flex_col()
                    .overflow_hidden()
                    .bg(theme.canvas)
                    .child(toolbar)
                    .child(stage)
                    .child(
                        div()
                            .px(px(12.0))
                            .py(px(7.0))
                            .flex_none()
                            .text_size(px(12.0))
                            .text_color(theme.text_secondary)
                            .child(tr!("mermaid.viewer_hint")),
                    ),
            )
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use gpui::{Modifiers, ScrollWheelEvent};

    fn viewport(width: f32, height: f32) -> Viewport {
        let mut viewport = Viewport::default();
        viewport.layout(
            Bounds::new(point(px(30.0), px(50.0)), size(px(800.0), px(600.0))),
            size(px(width), px(height)),
        );
        viewport
    }

    #[test]
    fn fit_contains_wide_and_tall_diagrams_and_tracks_resize() {
        for (width, height) in [(2400.0, 200.0), (200.0, 3200.0), (120.0, 80.0)] {
            let mut viewport = viewport(width, height);
            assert!(viewport.zoom <= 1.0);
            assert!(viewport.bounds.contains(&viewport.image_bounds().origin));
            assert!(
                viewport
                    .bounds
                    .contains(&viewport.image_bounds().bottom_right())
            );
            viewport.layout(
                Bounds::new(Point::default(), size(px(300.0), px(220.0))),
                viewport.image,
            );
            assert!(viewport.bounds.contains(&viewport.image_bounds().origin));
            assert!(
                viewport
                    .bounds
                    .contains(&viewport.image_bounds().bottom_right())
            );
        }
    }

    #[test]
    fn zoom_keeps_the_point_under_the_mouse_and_fit_recovers_from_long_drags() {
        let mut viewport = viewport(2400.0, 1000.0);
        let anchor = viewport.bounds.center() + point(px(75.0), px(-30.0));
        let image_point = (anchor - viewport.image_bounds().origin) / viewport.zoom;
        viewport.zoom_at(1.5, anchor);
        let after = (anchor - viewport.image_bounds().origin) / viewport.zoom;
        assert!(f32::from(after.x - image_point.x).abs() < 0.01);
        assert!(f32::from(after.y - image_point.y).abs() < 0.01);
        viewport.move_by(point(px(100_000.0), px(-100_000.0)));
        let visible = viewport.bounds.intersect(&viewport.image_bounds());
        assert!(visible.size.width >= px(47.0) && visible.size.height >= px(47.0));
        let zoom = viewport.zoom;
        viewport.layout(
            Bounds::new(Point::default(), size(px(600.0), px(400.0))),
            viewport.image,
        );
        assert_eq!(viewport.zoom, zoom, "manual zoom survives resize");
        viewport.fit();
        assert_eq!(viewport.pan, Point::default());
        assert!(
            viewport
                .bounds
                .contains(&viewport.image_bounds().bottom_right())
        );
        viewport.zoom_at(f32::NAN, anchor);
        assert!(viewport.zoom.is_finite());
    }

    fn click(cx: &mut gpui::VisualTestContext, selector: &'static str) {
        cx.run_until_parked();
        let position = cx.debug_bounds(selector).expect("viewer control").center();
        cx.simulate_mouse_move(position, None, Modifiers::none());
        cx.simulate_click(position, Modifiers::none());
        cx.run_until_parked();
    }

    #[gpui::test]
    fn gestures_toolbar_and_keyboard_operate_on_the_rendered_viewer(cx: &mut gpui::TestAppContext) {
        let (view, cx) = cx.add_window_view(|_, cx| Viewer {
            key: Key::new(
                "flowchart LR\nA[Start] --> B[Build] --> C[Test] --> D[Deploy] --> E[Complete]"
                    .into(),
                true,
                16.0,
                1.0,
            ),
            rendered: None,
            viewport: Rc::new(Cell::new(Viewport::default())),
            drag: None,
            focus: cx.focus_handle(),
            buttons: std::array::from_fn(|_| cx.focus_handle()),
        });
        cx.run_until_parked();
        let before = view.read_with(cx, |view, _| view.viewport.get());
        assert!(before.fitted);
        click(cx, "mermaid-viewer-in");
        view.read_with(cx, |view, _| {
            assert!(view.viewport.get().zoom > before.zoom)
        });
        click(cx, "mermaid-viewer-out");
        view.read_with(cx, |view, _| {
            assert!((view.viewport.get().zoom - before.zoom).abs() < 0.001)
        });
        click(cx, "mermaid-viewer-actual");
        view.read_with(cx, |view, _| assert_eq!(view.viewport.get().zoom, 1.0));

        let stage = cx.debug_bounds("mermaid-viewer-stage").unwrap();
        let anchor = stage.center() + point(px(30.0), px(20.0));
        cx.simulate_mouse_move(anchor, None, Modifiers::none());
        cx.simulate_event(ScrollWheelEvent {
            position: anchor,
            delta: ScrollDelta::Lines(point(0.0, 2.0)),
            ..Default::default()
        });
        cx.run_until_parked();
        let zoomed = view.read_with(cx, |view, _| view.viewport.get());
        assert!(zoomed.zoom > 1.0);
        cx.simulate_mouse_down(anchor, MouseButton::Left, Modifiers::none());
        let delta = point(px(60.0), px(35.0));
        cx.simulate_mouse_move(anchor + delta, MouseButton::Left, Modifiers::none());
        cx.simulate_mouse_up(anchor + delta, MouseButton::Left, Modifiers::none());
        cx.run_until_parked();
        view.read_with(cx, |view, _| {
            assert_eq!(view.viewport.get().pan, zoomed.pan + delta);
            assert!(view.drag.is_none());
        });
        cx.simulate_keystrokes("right down");
        view.read_with(cx, |view, _| {
            assert_eq!(
                view.viewport.get().pan,
                zoomed.pan + delta - point(px(48.0), px(48.0))
            )
        });
        cx.simulate_keystrokes("home");
        view.read_with(cx, |view, _| assert!(view.viewport.get().fitted));
        cx.simulate_resize(size(px(740.0), px(520.0)));
        cx.run_until_parked();
        view.read_with(cx, |view, _| {
            let viewport = view.viewport.get();
            assert!(
                viewport
                    .bounds
                    .contains(&viewport.image_bounds().bottom_right())
            );
        });
        // The first Tab enters the toolbar; the sixth returns to the canvas.
        cx.simulate_keystrokes("tab tab tab tab tab tab");
        let focus = view.read_with(cx, |view, _| view.focus.clone());
        cx.update(|window, _| assert!(focus.is_focused(window)));
        cx.simulate_keystrokes("shift-tab");
        let close_focus = view.read_with(cx, |view, _| view.buttons[4].clone());
        cx.update(|window, _| assert!(close_focus.is_focused(window)));

        // Releasing outside the viewport must retire the drag.
        cx.simulate_mouse_down(anchor, MouseButton::Left, Modifiers::none());
        cx.simulate_mouse_up(
            point(px(-5.0), px(-5.0)),
            MouseButton::Left,
            Modifiers::none(),
        );
        cx.run_until_parked();
        view.read_with(cx, |view, _| assert!(view.drag.is_none()));
    }
}
