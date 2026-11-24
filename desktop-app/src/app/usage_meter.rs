//! Context-window usage for the current DeerFlow session.
use gpui::{PathBuilder, relative};

use super::*;
use crate::usage::format_tokens;

const USAGE_METER_MENU_ID: &str = "usage-meter";

impl Waku {
    /// Whether the footer shows the gauge. Always true with a session
    /// selected — an empty ring is the honest "nothing measured yet" state,
    /// and hiding it would make the control feel intermittent.
    pub(super) fn usage_meter_available(&self) -> bool {
        self.selected_session().is_some()
    }

    /// Primary modifier + U: toggle the usage panel as if its footer trigger were clicked.
    pub(super) fn toggle_usage_panel_action(
        &mut self,
        _: &ToggleUsagePanel,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if self.settings_page.is_some() || !self.usage_meter_available() {
            return;
        }
        let menus = self.menus.borrow();
        let Some(handle) = menus.get(USAGE_METER_MENU_ID).cloned() else {
            return;
        };
        // A keyboard toggle produces no mouse-down for another open menu's
        // dismiss-on-down-out to see, so close the rest here.
        let other_open: Vec<_> = menus
            .iter()
            .filter(|(id, other)| id.as_ref() != USAGE_METER_MENU_ID && other.is_open())
            .map(|(_, other)| other.clone())
            .collect();
        drop(menus);
        window.defer(cx, move |window, cx| {
            for menu in other_open {
                menu.close(window, cx);
            }
            crate::ui::menu::toggle_popover(&handle, MenuAlign::AboveRight, window, cx);
        });
    }

    /// The footer's circular context gauge plus its anchored panel. `None`
    /// while there is nothing to show for the selected session's provider.
    pub(super) fn render_usage_meter(&self, cx: &mut Context<Self>) -> Option<AnyElement> {
        if !self.usage_meter_available() {
            return None;
        }
        let session = self.selected_session()?;
        let context = session.context_usage;
        let theme = Theme::current(cx);

        let weak = cx.entity().downgrade();
        let handle = self.menu_handle_with(USAGE_METER_MENU_ID, cx, move |open, window, cx| {
            if open {
                let mut card_focus = None;
                let _ = weak.update(cx, |this, cx| {
                    card_focus = this
                        .menus
                        .borrow()
                        .get(USAGE_METER_MENU_ID)
                        .map(|handle| handle.focus_handle().clone());
                    cx.notify();
                });
                // The card is deferred, so its focus handle joins the
                // dispatch tree only after the deferred draw — the same
                // two-frame wait the menus use. Focused, the card's menu
                // context is what lets `escape` dismiss it.
                if let Some(focus) = card_focus {
                    window.on_next_frame(move |window, _| {
                        window.on_next_frame(move |window, cx| window.focus(&focus, cx));
                    });
                }
            } else {
                let mut composer_focus = None;
                let _ = weak.update(cx, |this, cx| {
                    composer_focus = Some(this.composer.read(cx).focus());
                    cx.notify();
                });
                if let Some(focus) = composer_focus {
                    window.focus(&focus, cx);
                }
            }
        });

        let percent = context.and_then(context_percent);
        let fill = match percent {
            Some(percent) if percent >= 95.0 => theme.danger,
            Some(percent) if percent >= 80.0 => theme.warning,
            _ => theme.gauge,
        };
        let tooltip = match percent {
            Some(percent) => SharedString::from(tr!(
                "usage.context_used",
                percent = format!("{percent:.0}"),
                shortcut = crate::platform::primary_shortcut("⌘U", "Ctrl+U")
            )),
            None => SharedString::from(tr!(
                "usage.shortcut",
                shortcut = crate::platform::primary_shortcut("⌘U", "Ctrl+U")
            )),
        };

        let trigger = div()
            .id("usage-meter")
            .h(px(20.0))
            .px(px(5.0))
            .rounded(px(5.0))
            .flex()
            .items_center()
            .flex_none()
            .cursor_default()
            .hover(|element| element.bg(theme.overlay))
            .when(handle.is_open(), |element| element.bg(theme.overlay_strong))
            .tooltip(Tooltip::text(tooltip))
            .child(context_gauge(percent, theme.border_strong, fill));

        Some(popover(
            trigger,
            &handle,
            MenuAlign::AboveRight,
            move |handle, _, cx| {
                usage_panel(handle, context, cx)
            },
        ))
    }
}

/// Fraction of the window in use, capped at 100%. A provider (notably
/// OpenCode) can report a lifetime/cumulative token counter that exceeds the
/// model's window; the context can never be more than fully used, so anything
/// past the window reads as full rather than an impossible percentage.
fn context_percent(usage: ContextUsage) -> Option<f64> {
    usage
        .window
        .filter(|window| *window > 0)
        .map(|window| (usage.tokens as f64 * 100.0 / window as f64).min(100.0))
}

/// The trigger glyph: a ring whose arc fills clockwise from 12 o'clock as the
/// context window does, over a faint full ring. An unknown fraction draws the
/// track alone. This is Zed's `CircularProgress` drawing sized for the footer
/// — `PathBuilder::stroke` arcs, which lyon tessellates correctly where a
/// hand-built annulus fill does not survive GPUI's fill rule.
fn context_gauge(percent: Option<f64>, track: Hsla, fill: Hsla) -> impl IntoElement {
    const SIZE: f32 = 13.0;
    const STROKE: f32 = 2.5;
    canvas(
        |_, _, _| (),
        move |bounds, _, window, _| {
            let center = bounds.center();
            let radius = px((SIZE - STROKE) / 2.0);

            // A full circle is two 180° arcs; lyon rejects a single
            // zero-length one.
            let full_circle = |builder: &mut PathBuilder| {
                builder.move_to(point(center.x + radius, center.y));
                builder.arc_to(
                    point(radius, radius),
                    px(0.0),
                    false,
                    true,
                    point(center.x - radius, center.y),
                );
                builder.arc_to(
                    point(radius, radius),
                    px(0.0),
                    false,
                    true,
                    point(center.x + radius, center.y),
                );
                builder.close();
            };

            let mut track_builder = PathBuilder::stroke(px(STROKE));
            full_circle(&mut track_builder);
            if let Ok(path) = track_builder.build() {
                window.paint_path(path, track);
            }

            let Some(percent) = percent else {
                return;
            };
            // Keep a visible sliver for a nearly-empty context.
            let fraction = ((percent / 100.0) as f32).clamp(0.0, 1.0).max(0.05);
            let mut arc_builder = PathBuilder::stroke(px(STROKE));
            if fraction >= 0.999 {
                full_circle(&mut arc_builder);
            } else {
                let start = -std::f32::consts::FRAC_PI_2;
                let angle = start + fraction * std::f32::consts::TAU;
                arc_builder.move_to(point(center.x, center.y - radius));
                arc_builder.arc_to(
                    point(radius, radius),
                    px(0.0),
                    fraction > 0.5,
                    true,
                    point(
                        center.x + radius * angle.cos(),
                        center.y + radius * angle.sin(),
                    ),
                );
            }
            if let Ok(path) = arc_builder.build() {
                window.paint_path(path, fill);
            }
        },
    )
    .w(px(SIZE))
    .h(px(SIZE))
    .flex_none()
}

fn usage_panel(
    handle: &ContextMenuHandle,
    context: Option<ContextUsage>,
    cx: &App,
) -> AnyElement {
    let theme = Theme::current(cx);
    let mut panel = div()
        // Focused on open so the surrounding menu context sees `escape`.
        .track_focus(handle.focus_handle())
        .w(px(320.0))
        .p(px(14.0))
        .rounded(px(10.0))
        .border_1()
        .border_color(theme.border_strong)
        .bg(theme.raised)
        .shadow_lg()
        .flex()
        .flex_col()
        .gap(px(12.0))
        .text_size(sp(12.5));

    // The context row always renders; a session with nothing measured yet
    // reads "0" over an empty track, exactly like the CLI's own panel.
    let usage = context.unwrap_or_default();
    let percent = context_percent(usage);
    let value = match (usage.window, percent) {
        (Some(window), Some(percent)) => format!(
            "{} / {} ({percent:.0}%)",
            format_tokens(usage.tokens.min(window)),
            format_tokens(window)
        ),
        // The transport reports occupancy but not the window size.
        _ => format_tokens(usage.tokens),
    };
    panel = panel.child(
        div()
            .flex()
            .flex_col()
            .gap(px(7.0))
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(8.0))
                    .child(
                        div()
                            .text_color(theme.text)
                            .child(tr!("usage.context_window")),
                    )
                    .child(div().flex_1())
                    .child(
                        div()
                            .text_size(sp(12.5))
                            .text_color(theme.text_tertiary)
                            .child(SharedString::from(value)),
                    ),
            )
            .child(meter_bar(&theme, percent.unwrap_or(0.0))),
    );
    panel.into_any_element()
}

fn meter_bar(theme: &Theme, percent: f64) -> Div {
    let fraction = (percent / 100.0).clamp(0.0, 1.0) as f32;
    let fraction = if fraction > 0.0 {
        fraction.max(0.015)
    } else {
        0.0
    };
    let fill = if percent >= 95.0 {
        theme.danger
    } else if percent >= 80.0 {
        theme.warning
    } else {
        theme.gauge
    };
    div()
        .h(px(3.0))
        .w_full()
        .flex_none()
        .rounded_full()
        .bg(theme.overlay_strong)
        .child(div().h_full().w(relative(fraction)).rounded_full().bg(fill))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn context_percent_caps_at_full_when_provider_over_reports() {
        // OpenCode reports lifetime/cumulative session counters that can run
        // far past the model's window; the meter must read "full", never an
        // impossible percentage like 472%.
        let usage = ContextUsage {
            tokens: 4_718_002,
            window: Some(1_000_000),
        };
        assert_eq!(context_percent(usage), Some(100.0));
    }

    #[test]
    fn context_percent_reports_partial_usage_verbatim() {
        let usage = ContextUsage {
            tokens: 250_000,
            window: Some(1_000_000),
        };
        assert_eq!(context_percent(usage), Some(25.0));
    }

    #[test]
    fn context_percent_is_none_without_a_positive_window() {
        assert_eq!(
            context_percent(ContextUsage {
                tokens: 4_718_002,
                window: None,
            }),
            None
        );
        assert_eq!(
            context_percent(ContextUsage {
                tokens: 4_718_002,
                window: Some(0),
            }),
            None
        );
    }
}
