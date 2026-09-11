use iced::widget::{button, checkbox, column, container, pick_list, row, svg, text, text_input};
use iced::{Background, Border, Color, Element, Fill, Length, Shadow, Theme, Vector, border};

pub const BG_BASE: Color = rgb(0x10, 0x14, 0x1b);
pub const BG_SURFACE: Color = rgb(0x17, 0x1c, 0x25);
pub const BG_ELEVATED: Color = rgb(0x23, 0x2b, 0x37);
pub const BG_INPUT: Color = rgb(0x12, 0x17, 0x20);
pub const BORDER: Color = rgb(0x2a, 0x32, 0x3f);
pub const BORDER_STRONG: Color = rgb(0x40, 0x4b, 0x5d);
pub const TEXT_PRIMARY: Color = rgb(0xf1, 0xf5, 0xf9);
pub const TEXT_SECONDARY: Color = rgb(0xa8, 0xb5, 0xc8);
pub const TEXT_MUTED: Color = rgb(0x7f, 0x8d, 0xa8);
pub const ACCENT: Color = rgb(0x06, 0xb6, 0xd4);
pub const ACCENT_HOVER: Color = rgb(0x22, 0xd3, 0xee);
pub const ACCENT_TEXT: Color = rgb(0x06, 0x2b, 0x36);
pub const SUCCESS: Color = rgb(0x10, 0xb9, 0x81);
pub const SUCCESS_HOVER: Color = rgb(0x34, 0xd3, 0x99);
pub const WARNING: Color = rgb(0xf5, 0x9e, 0x0b);
pub const DANGER: Color = rgb(0xf8, 0x71, 0x71);

const fn rgb(r: u8, g: u8, b: u8) -> Color {
    Color::from_rgb(r as f32 / 255.0, g as f32 / 255.0, b as f32 / 255.0)
}

fn with_alpha(color: Color, alpha: f32) -> Color {
    Color { a: alpha, ..color }
}

pub fn app_theme() -> Theme {
    Theme::custom(
        "DeerFlow",
        iced::theme::Palette {
            background: BG_BASE,
            text: TEXT_PRIMARY,
            primary: ACCENT,
            success: SUCCESS,
            warning: WARNING,
            danger: DANGER,
        },
    )
}

pub fn app_background(_: &Theme) -> container::Style {
    container::Style::default().background(BG_BASE)
}

pub fn sidebar(_: &Theme) -> container::Style {
    container::Style {
        background: Some(Background::Color(BG_SURFACE)),
        border: Border {
            color: Color::TRANSPARENT,
            width: 0.0,
            radius: 0.0.into(),
        },
        ..Default::default()
    }
}

pub fn topbar(_: &Theme) -> container::Style {
    container::Style {
        background: Some(Background::Color(with_alpha(BG_SURFACE, 0.94))),
        border: Border {
            color: BORDER,
            width: 1.0,
            radius: border::radius(10),
        },
        ..Default::default()
    }
}

pub fn card(_: &Theme) -> container::Style {
    container::Style {
        background: Some(Background::Color(BG_SURFACE)),
        border: Border {
            color: BORDER,
            width: 1.0,
            radius: border::radius(12),
        },
        shadow: Shadow {
            color: Color::BLACK.scale_alpha(0.08),
            offset: Vector::new(0.0, 2.0),
            blur_radius: 6.0,
        },
        ..Default::default()
    }
}

pub fn inset_card(_: &Theme) -> container::Style {
    container::Style {
        background: Some(Background::Color(BG_INPUT)),
        border: Border {
            color: BORDER,
            width: 1.0,
            radius: border::radius(10),
        },
        ..Default::default()
    }
}

pub fn accent_card(_: &Theme) -> container::Style {
    container::Style {
        background: Some(Background::Color(with_alpha(ACCENT, 0.08))),
        border: Border {
            color: with_alpha(ACCENT, 0.45),
            width: 1.0,
            radius: border::radius(12),
        },
        ..Default::default()
    }
}

pub fn error_callout(_: &Theme) -> container::Style {
    callout_style(DANGER)
}

pub fn success_callout(_: &Theme) -> container::Style {
    callout_style(SUCCESS)
}

pub fn warning_callout(_: &Theme) -> container::Style {
    callout_style(WARNING)
}

fn callout_style(color: Color) -> container::Style {
    container::Style {
        background: Some(Background::Color(with_alpha(color, 0.10))),
        border: Border {
            color: with_alpha(color, 0.55),
            width: 1.0,
            radius: border::radius(9),
        },
        ..Default::default()
    }
}

fn button_base(
    status: button::Status,
    background: Color,
    hovered: Color,
    text_color: Color,
    border_color: Color,
) -> button::Style {
    let (background, text_color) = match status {
        button::Status::Active => (background, text_color),
        button::Status::Hovered => (hovered, text_color),
        button::Status::Pressed => (with_alpha(hovered, 0.78), text_color),
        button::Status::Disabled => (with_alpha(background, 0.42), with_alpha(text_color, 0.55)),
    };
    button::Style {
        background: Some(Background::Color(background)),
        text_color,
        border: Border {
            color: if matches!(status, button::Status::Disabled) {
                with_alpha(border_color, 0.35)
            } else {
                border_color
            },
            width: 1.0,
            radius: border::radius(8),
        },
        ..Default::default()
    }
}

pub fn primary_button(_: &Theme, status: button::Status) -> button::Style {
    button_base(status, ACCENT, ACCENT_HOVER, ACCENT_TEXT, ACCENT)
}

pub fn success_button(_: &Theme, status: button::Status) -> button::Style {
    button_base(status, SUCCESS, SUCCESS_HOVER, ACCENT_TEXT, SUCCESS)
}

pub fn secondary_button(_: &Theme, status: button::Status) -> button::Style {
    button_base(
        status,
        BG_ELEVATED,
        rgb(0x2d, 0x37, 0x45),
        TEXT_PRIMARY,
        BORDER_STRONG,
    )
}

pub fn action_button<'a, Message: Clone + 'a>(label: &'a str) -> button::Button<'a, Message> {
    button(text(label).size(13))
        .padding([9, 13])
        .style(secondary_button)
}

pub fn danger_button(_: &Theme, status: button::Status) -> button::Style {
    outline_button(status, DANGER)
}

pub fn warning_button(_: &Theme, status: button::Status) -> button::Style {
    outline_button(status, WARNING)
}

fn outline_button(status: button::Status, color: Color) -> button::Style {
    let background = match status {
        button::Status::Hovered | button::Status::Pressed => with_alpha(color, 0.14),
        button::Status::Disabled => with_alpha(BG_ELEVATED, 0.35),
        button::Status::Active => Color::TRANSPARENT,
    };
    button::Style {
        background: Some(Background::Color(background)),
        text_color: if matches!(status, button::Status::Disabled) {
            with_alpha(color, 0.45)
        } else {
            color
        },
        border: Border {
            color: if matches!(status, button::Status::Disabled) {
                with_alpha(color, 0.35)
            } else {
                color
            },
            width: 1.0,
            radius: border::radius(8),
        },
        ..Default::default()
    }
}

pub fn nav_button(active: bool) -> impl Fn(&Theme, button::Status) -> button::Style {
    move |_, status| {
        let background = if matches!(status, button::Status::Pressed) {
            with_alpha(ACCENT, 0.20)
        } else if active && matches!(status, button::Status::Hovered) {
            with_alpha(ACCENT, 0.17)
        } else if active {
            with_alpha(ACCENT, 0.12)
        } else if matches!(status, button::Status::Hovered | button::Status::Pressed) {
            BG_ELEVATED
        } else {
            Color::TRANSPARENT
        };
        button::Style {
            background: Some(Background::Color(background)),
            text_color: if active { TEXT_PRIMARY } else { TEXT_SECONDARY },
            border: Border {
                color: if active {
                    with_alpha(ACCENT, 0.16)
                } else {
                    Color::TRANSPARENT
                },
                width: 1.0,
                radius: border::radius(8),
            },
            ..Default::default()
        }
    }
}

pub fn list_button(active: bool) -> impl Fn(&Theme, button::Status) -> button::Style {
    move |_, status| {
        let background = if active {
            with_alpha(ACCENT, 0.12)
        } else if matches!(status, button::Status::Hovered | button::Status::Pressed) {
            BG_ELEVATED
        } else {
            BG_INPUT
        };
        button::Style {
            background: Some(Background::Color(background)),
            text_color: if active { TEXT_PRIMARY } else { TEXT_SECONDARY },
            border: Border {
                color: if active { ACCENT } else { BORDER },
                width: 1.0,
                radius: border::radius(8),
            },
            ..Default::default()
        }
    }
}

pub fn tab_button(active: bool) -> impl Fn(&Theme, button::Status) -> button::Style {
    move |_, status| {
        let background = if active {
            ACCENT
        } else if matches!(status, button::Status::Hovered | button::Status::Pressed) {
            BG_ELEVATED
        } else {
            Color::TRANSPARENT
        };
        button::Style {
            background: Some(Background::Color(background)),
            text_color: if active { ACCENT_TEXT } else { TEXT_SECONDARY },
            border: Border {
                color: if active { ACCENT } else { BORDER },
                width: 1.0,
                radius: border::radius(8),
            },
            ..Default::default()
        }
    }
}

pub fn input_style(_: &Theme, status: text_input::Status) -> text_input::Style {
    let focused = matches!(status, text_input::Status::Focused { .. });
    let disabled = matches!(status, text_input::Status::Disabled);
    text_input::Style {
        background: Background::Color(if disabled { BG_SURFACE } else { BG_INPUT }),
        border: Border {
            color: if focused { ACCENT } else { BORDER_STRONG },
            width: if focused { 1.5 } else { 1.0 },
            radius: border::radius(7),
        },
        icon: TEXT_MUTED,
        placeholder: TEXT_MUTED,
        value: if disabled { TEXT_MUTED } else { TEXT_PRIMARY },
        selection: with_alpha(ACCENT, 0.35),
    }
}

pub fn pick_list_style(_: &Theme, status: pick_list::Status) -> pick_list::Style {
    let focused = matches!(
        status,
        pick_list::Status::Opened { .. } | pick_list::Status::Hovered
    );
    pick_list::Style {
        text_color: TEXT_PRIMARY,
        placeholder_color: TEXT_MUTED,
        handle_color: if focused { ACCENT } else { TEXT_SECONDARY },
        background: Background::Color(BG_INPUT),
        border: Border {
            color: if focused { ACCENT } else { BORDER_STRONG },
            width: 1.0,
            radius: border::radius(7),
        },
    }
}

pub fn checkbox_style(_: &Theme, status: checkbox::Status) -> checkbox::Style {
    let (checked, hovered, disabled) = match status {
        checkbox::Status::Active { is_checked } => (is_checked, false, false),
        checkbox::Status::Hovered { is_checked } => (is_checked, true, false),
        checkbox::Status::Disabled { is_checked } => (is_checked, false, true),
    };
    let fill = if checked {
        ACCENT
    } else if hovered {
        BG_ELEVATED
    } else {
        BG_INPUT
    };
    checkbox::Style {
        background: Background::Color(if disabled {
            with_alpha(fill, 0.45)
        } else {
            fill
        }),
        icon_color: ACCENT_TEXT,
        border: Border {
            color: if checked || hovered {
                ACCENT
            } else {
                BORDER_STRONG
            },
            width: 1.0,
            radius: border::radius(4),
        },
        text_color: Some(if disabled { TEXT_MUTED } else { TEXT_SECONDARY }),
    }
}

pub fn page_header<'a, Message: 'a>(title: &'a str, description: &'a str) -> Element<'a, Message> {
    column![
        text(title).size(28).color(TEXT_PRIMARY),
        text(description)
            .size(13)
            .color(TEXT_SECONDARY)
            .width(Fill)
            .wrapping(text::Wrapping::WordOrGlyph),
    ]
    .spacing(5)
    .into()
}

pub fn section<'a, Message: 'a>(
    title: &'a str,
    description: &'a str,
    content: impl Into<Element<'a, Message>>,
) -> Element<'a, Message> {
    container(
        column![
            text(title).size(19).color(TEXT_PRIMARY),
            text(description)
                .size(12)
                .color(TEXT_SECONDARY)
                .width(Fill)
                .wrapping(text::Wrapping::WordOrGlyph),
            content.into(),
        ]
        .spacing(12),
    )
    .padding(18)
    .width(Fill)
    .style(card)
    .into()
}

pub fn metric<'a, Message: 'a>(
    icon: Icon,
    label: &'a str,
    value: String,
    hint: &'a str,
) -> Element<'a, Message> {
    container(
        column![
            row![
                icon_view(icon, 17.0, ACCENT),
                text(label).size(13).color(TEXT_SECONDARY),
            ]
            .spacing(8)
            .align_y(iced::Alignment::Center),
            text(value).size(34).color(TEXT_PRIMARY),
            text(hint).size(12).color(TEXT_MUTED),
        ]
        .spacing(7),
    )
    .padding(18)
    .width(Fill)
    .style(card)
    .into()
}

pub fn status_pill<'a, Message: 'a + 'static>(
    label: &'a str,
    color: Color,
    icon: Icon,
) -> Element<'a, Message> {
    container(
        row![
            container(icon_view(icon, 14.0, color))
                .width(Length::Fixed(14.0))
                .height(Length::Fixed(14.0)),
            text(label).size(12).color(TEXT_SECONDARY),
        ]
        .spacing(7)
        .align_y(iced::Alignment::Center),
    )
    .padding([6, 10])
    .style(move |_| container::Style {
        background: Some(Background::Color(with_alpha(color, 0.09))),
        border: Border {
            color: with_alpha(color, 0.45),
            width: 1.0,
            radius: border::radius(999),
        },
        ..Default::default()
    })
    .into()
}

pub fn sidebar_item<'a, Message: Clone + 'a>(
    icon: Icon,
    label: &'a str,
    active: bool,
    message: Message,
) -> Element<'a, Message> {
    sidebar_entry(icon, label, active, None, message)
}

fn sidebar_entry<'a, Message: Clone + 'a>(
    icon: Icon,
    label: &'a str,
    active: bool,
    badge: Option<&'a str>,
    message: Message,
) -> Element<'a, Message> {
    let mut content = row![
        icon_view(icon, 18.0, if active { ACCENT } else { TEXT_SECONDARY }),
        text(label).size(14).width(Fill),
    ]
    .spacing(10)
    .align_y(iced::Alignment::Center);
    if let Some(label) = badge {
        content = content.push(
            container(
                text(label)
                    .size(11)
                    .color(if active { ACCENT } else { TEXT_SECONDARY }),
            )
            .padding([3, 7])
            .style(move |_| container::Style {
                background: Some(Background::Color(if active {
                    with_alpha(ACCENT, 0.10)
                } else {
                    BG_ELEVATED
                })),
                border: Border {
                    radius: border::radius(5),
                    ..Default::default()
                },
                ..Default::default()
            }),
        );
    } else if active {
        content = content.push(icon_view(Icon::ChevronRight, 14.0, ACCENT));
    }
    button(content)
        .padding([10, 12])
        .height(42)
        .width(Fill)
        .style(nav_button(active))
        .on_press(message)
        .into()
}

#[derive(Debug, Clone, Copy)]
pub enum Icon {
    Dashboard,
    Models,
    Agents,
    Memory,
    Skills,
    Tools,
    Runtime,
    Diagnostics,
    Activity,
    Refresh,
    Save,
    Play,
    Stop,
    Restart,
    Check,
    Alert,
    Folder,
    ChevronDown,
    ChevronRight,
}

pub fn icon_view<'a>(icon: Icon, size: f32, color: Color) -> svg::Svg<'a> {
    svg(svg::Handle::from_memory(icon.svg().as_bytes()))
        .width(Length::Fixed(size))
        .height(Length::Fixed(size))
        .style(move |_, _| svg::Style { color: Some(color) })
}

impl Icon {
    fn svg(self) -> &'static str {
        match self {
            Self::ChevronDown => {
                r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m6 9 6 6 6-6"/></svg>"#
            }
            Self::ChevronRight => {
                r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m9 6 6 6-6 6"/></svg>"#
            }
            Self::Dashboard => {
                r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="3" width="7" height="7"/><rect x="14" y="3" width="7" height="7"/><rect x="3" y="14" width="7" height="7"/><rect x="14" y="14" width="7" height="7"/></svg>"#
            }
            Self::Models => {
                r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2 2 7l10 5 10-5-10-5Z"/><path d="m2 17 10 5 10-5"/><path d="m2 12 10 5 10-5"/></svg>"#
            }
            Self::Agents => {
                r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="11" width="18" height="10" rx="2"/><circle cx="8" cy="16" r="1"/><circle cx="16" cy="16" r="1"/><path d="M12 2v4M8 7h8"/></svg>"#
            }
            Self::Memory => {
                r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><ellipse cx="12" cy="5" rx="8" ry="3"/><path d="M4 5v7c0 1.7 3.6 3 8 3s8-1.3 8-3V5"/><path d="M4 12v7c0 1.7 3.6 3 8 3s8-1.3 8-3v-7"/></svg>"#
            }
            Self::Skills => {
                r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m12 3-1.9 4.6L5 9.5l4 3.1L7.8 18l4.2-2.7 4.2 2.7-1.2-5.4 4-3.1-5.1-1.9L12 3Z"/></svg>"#
            }
            Self::Tools => {
                r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14.7 6.3a4 4 0 0 0-5-5L12 3.6 9.6 6 7.3 3.7a4 4 0 0 0 5 5l-8.6 8.6a2 2 0 1 0 3 3l8.6-8.6a4 4 0 0 0 5-5L18 9l-2.4-2.4 2.3-2.3a4 4 0 0 0-3.2 2Z"/></svg>"#
            }
            Self::Runtime => {
                r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="m7 9 3 3-3 3M13 15h4"/></svg>"#
            }
            Self::Diagnostics => {
                r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12h4l2-5 4 10 2-5h6"/></svg>"#
            }
            Self::Activity => {
                r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M8 12h8"/></svg>"#
            }
            Self::Refresh => {
                r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20 7v5h-5"/><path d="M4 17v-5h5"/><path d="M6.1 9A7 7 0 0 1 18 6l2 6M4 12l2 6a7 7 0 0 0 11.9-3"/></svg>"#
            }
            Self::Save => {
                r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 3h13l3 3v15H4V3Z"/><path d="M8 3v6h8V3M8 21v-7h8v7"/></svg>"#
            }
            Self::Play => {
                r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m7 4 13 8-13 8V4Z"/></svg>"#
            }
            Self::Stop => {
                r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="5" y="5" width="14" height="14" rx="2"/></svg>"#
            }
            Self::Restart => {
                r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20 11a8 8 0 1 0-2 5.3"/><path d="M20 4v7h-7"/></svg>"#
            }
            Self::Check => {
                r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="m8 12 2.5 2.5L16 9"/></svg>"#
            }
            Self::Alert => {
                r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M10.3 3.5 2.4 18a2 2 0 0 0 1.8 3h15.6a2 2 0 0 0 1.8-3L13.7 3.5a2 2 0 0 0-3.4 0Z"/><path d="M12 9v4M12 17h.01"/></svg>"#
            }
            Self::Folder => {
                r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 6h7l2 2h9v11H3V6Z"/></svg>"#
            }
        }
    }
}
