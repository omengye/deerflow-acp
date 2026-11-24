//! Pure client-side composer matching over daemon-provided command/file lists.

use std::ops::Range;

use nucleo_matcher::pattern::{CaseMatching, Normalization, Pattern};
use nucleo_matcher::{Matcher, Utf32Str};
pub use waku_protocol::composer::{CommandScope, FileEntry, SlashCommand};
use waku_protocol::model::{ProviderKind, ReportedCommand};

pub const FILTER_CAP: usize = 64;
pub const FILE_INDEX_CAP: usize = 50_000;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TriggerKind {
    Command,
    File,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Trigger {
    pub kind: TriggerKind,
    pub query: String,
    pub range: Range<usize>,
}

pub fn detect_trigger(text: &str, cursor: usize) -> Option<Trigger> {
    let cursor = cursor.min(text.len());
    if !text.is_char_boundary(cursor) {
        return None;
    }
    let line_start = text[..cursor].rfind('\n').map_or(0, |index| index + 1);
    let line_prefix = &text[line_start..cursor];
    if let Some(query) = line_prefix.strip_prefix('/') {
        if !query.chars().any(char::is_whitespace) {
            return Some(Trigger {
                kind: TriggerKind::Command,
                query: query.to_owned(),
                range: line_start..cursor,
            });
        }
        return None;
    }
    let token_start = text[..cursor]
        .rfind(char::is_whitespace)
        .map_or(0, |index| {
            index + text[index..].chars().next().unwrap().len_utf8()
        });
    let token = &text[token_start..cursor];
    Some(Trigger {
        kind: TriggerKind::File,
        query: token.strip_prefix('@')?.to_owned(),
        range: token_start..cursor,
    })
}

pub fn merge_reported_commands(
    discovered: &[SlashCommand],
    reported: &[ReportedCommand],
) -> Vec<SlashCommand> {
    let mut merged = discovered.to_vec();
    for report in reported {
        if let Some(known) = merged
            .iter_mut()
            .find(|command| command.name == report.name)
        {
            if known.description.is_empty() {
                known.description = report.description.clone();
            }
        } else {
            merged.push(SlashCommand {
                name: report.name.clone(),
                description: report.description.clone(),
                scope: CommandScope::Builtin,
                argument_hint: None,
                template: None,
            });
        }
    }
    merged
        .sort_by(|a, b| (a.scope.display_rank(), &a.name).cmp(&(b.scope.display_rank(), &b.name)));
    merged
}

/// Build the slash-prefixed text shown for an autocomplete command.
pub fn command_composer_text(command: &SlashCommand) -> String {
    format!("/{}", command.name)
}

/// Whether the composer submitted Waku's global terminal-session picker.
/// The command is reserved by daemon-side discovery, so it is intentionally
/// provider-neutral and never crosses into a provider transport.
pub fn is_resume_submission(prompt: &str) -> bool {
    prompt.trim() == "/resume"
}

pub fn expand_command_template(template: &str, args: &str) -> String {
    let positional = args.split_whitespace().collect::<Vec<_>>();
    let mut expanded = String::with_capacity(template.len() + args.len());
    let mut consumed_args = false;
    let mut rest = template;
    while let Some(index) = rest.find('$') {
        expanded.push_str(&rest[..index]);
        let after = &rest[index + 1..];
        if let Some(tail) = after.strip_prefix("ARGUMENTS") {
            expanded.push_str(args);
            consumed_args = true;
            rest = tail;
        } else if let Some(tail) = after.strip_prefix('@') {
            expanded.push_str(args);
            consumed_args = true;
            rest = tail;
        } else if let Some(digit) = after
            .chars()
            .next()
            .and_then(|character| character.to_digit(10))
            .filter(|digit| (1..=9).contains(digit))
        {
            if let Some(argument) = positional.get(digit as usize - 1) {
                expanded.push_str(argument);
            }
            consumed_args = true;
            rest = &after[1..];
        } else {
            expanded.push('$');
            rest = after;
        }
    }
    expanded.push_str(rest);
    if !consumed_args && !args.is_empty() {
        expanded.push_str("\n\n");
        expanded.push_str(args);
    }
    expanded
}

/// Resolve composer text into the exact prompt expected by the provider.
///
/// Template commands expand to their body. Skills keep a slash in the
/// composer and transcript, then resolve to each provider's native syntax at
/// the transport boundary.
pub fn resolved_submission(
    _provider: ProviderKind,
    prompt: &str,
    commands: &[SlashCommand],
) -> Option<String> {
    let invocation = prompt.strip_prefix('/')?;
    let (name, args) = invocation
        .split_once(char::is_whitespace)
        .map_or((invocation, ""), |(name, args)| (name, args.trim()));
    let command = commands.iter().find(|command| command.name == name)?;
    let template = command.template.as_deref()?;
    Some(expand_command_template(template, args))
}

pub fn matcher() -> Matcher {
    Matcher::new(nucleo_matcher::Config::DEFAULT.match_paths())
}

#[derive(Clone, Debug)]
pub struct Scored<T> {
    pub item: T,
    pub positions: Vec<u32>,
}

fn filter_scored(
    haystack: &[&str],
    query: &str,
    matcher: &mut Matcher,
    cap: usize,
) -> Vec<(usize, Vec<u32>)> {
    if query.trim().is_empty() {
        return (0..haystack.len().min(cap))
            .map(|index| (index, Vec::new()))
            .collect();
    }
    let pattern = Pattern::parse(query, CaseMatching::Ignore, Normalization::Smart);
    let mut buf = Vec::new();
    let mut scored = Vec::new();
    for (index, text) in haystack.iter().enumerate() {
        if let Some(score) = pattern.score(Utf32Str::new(text, &mut buf), matcher) {
            scored.push((score, index));
        }
    }
    scored.sort_by(|a, b| b.0.cmp(&a.0).then(a.1.cmp(&b.1)));
    scored.truncate(cap);
    scored
        .into_iter()
        .map(|(_, index)| {
            let mut positions = Vec::new();
            pattern.indices(
                Utf32Str::new(haystack[index], &mut buf),
                matcher,
                &mut positions,
            );
            positions.sort_unstable();
            positions.dedup();
            (index, positions)
        })
        .collect()
}

pub fn filter_commands(
    commands: &[SlashCommand],
    query: &str,
    matcher: &mut Matcher,
) -> Vec<Scored<SlashCommand>> {
    let names = commands
        .iter()
        .map(|command| command.name.as_str())
        .collect::<Vec<_>>();
    filter_scored(&names, query, matcher, FILTER_CAP)
        .into_iter()
        .map(|(index, positions)| Scored {
            item: commands[index].clone(),
            positions,
        })
        .collect()
}

pub fn filter_files(
    files: &[FileEntry],
    query: &str,
    matcher: &mut Matcher,
) -> Vec<Scored<FileEntry>> {
    let paths = files
        .iter()
        .map(|file| file.path.as_str())
        .collect::<Vec<_>>();
    filter_scored(&paths, query, matcher, FILTER_CAP)
        .into_iter()
        .map(|(index, positions)| Scored {
            item: files[index].clone(),
            positions,
        })
        .collect()
}

pub fn highlight_byte_ranges(
    text: &str,
    positions: &[u32],
    char_offset: usize,
) -> Vec<Range<usize>> {
    let mut ranges: Vec<Range<usize>> = Vec::new();
    for (char_index, (byte_index, character)) in (char_offset..).zip(text.char_indices()) {
        if positions.binary_search(&(char_index as u32)).is_ok() {
            let byte_end = byte_index + character.len_utf8();
            match ranges.last_mut() {
                Some(last) if last.end == byte_index => last.end = byte_end,
                _ => ranges.push(byte_index..byte_end),
            }
        }
    }
    ranges
}

#[cfg(test)]
mod tests {
    use super::*;

    fn command(name: &str, scope: CommandScope) -> SlashCommand {
        SlashCommand {
            name: name.into(),
            description: String::new(),
            scope,
            argument_hint: None,
            template: None,
        }
    }

    #[test]
    fn merged_command_picker_puts_builtins_first_and_skills_last() {
        let discovered = vec![
            command("deploy", CommandScope::Skill),
            command("format", CommandScope::User),
            command("lint", CommandScope::Project),
            command("review", CommandScope::Builtin),
            command("resume", CommandScope::Waku),
        ];
        let reported = vec![ReportedCommand {
            name: "compact".into(),
            description: "Free up context".into(),
        }];

        let merged = merge_reported_commands(&discovered, &reported);
        assert_eq!(
            merged
                .iter()
                .map(|command| (command.scope, command.name.as_str()))
                .collect::<Vec<_>>(),
            [
                (CommandScope::Builtin, "compact"),
                (CommandScope::Waku, "resume"),
                (CommandScope::Builtin, "review"),
                (CommandScope::Project, "lint"),
                (CommandScope::User, "format"),
                (CommandScope::Skill, "deploy"),
            ]
        );
    }

    #[test]
    fn resume_is_an_exact_provider_neutral_local_command() {
        assert!(is_resume_submission("/resume"));
        assert!(is_resume_submission("  /resume  "));
        assert!(!is_resume_submission("/resume latest"));
        assert!(!is_resume_submission("please /resume"));
    }

}
