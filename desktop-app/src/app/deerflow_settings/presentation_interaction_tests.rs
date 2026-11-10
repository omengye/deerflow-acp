use super::*;
use std::cell::Cell;

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

#[test]
fn form_composition_mounts_once_only_on_settings_pages() {
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
        Section::Tools,
        Section::Runtime,
    ] {
        let form_renders = Cell::new(0);
        let _content = compose_settings_content(section, div(), || {
            form_renders.set(form_renders.get() + 1);
            div().into_any_element()
        });
        assert_eq!(
            form_renders.get(),
            usize::from(section != Section::SkillCenter)
        );
    }
}
