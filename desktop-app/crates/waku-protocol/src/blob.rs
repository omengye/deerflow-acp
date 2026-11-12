//! Durable references for daemon-owned binary task payloads.

/// Scheme for a stored blob reference, e.g. `waku-blob:3f2a...c1.png`.
pub const SCHEME: &str = "waku-blob:";

pub fn is_reference(value: &str) -> bool {
    value.starts_with(SCHEME)
}

/// Find durable image references in JSON strings or Markdown destinations.
pub fn references(text: &str) -> impl Iterator<Item = &str> {
    text.match_indices(SCHEME).filter_map(move |(start, _)| {
        let value = &text[start..];
        let name = &value[SCHEME.len()..];
        let len = name
            .bytes()
            .take_while(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'-' | b'_'))
            .count();
        (len > 0).then_some(&value[..SCHEME.len() + len])
    })
}

#[cfg(test)]
mod tests {
    #[test]
    fn references_stop_at_markdown_and_json_boundaries() {
        let text = r#"{"text":"before ![](waku-blob:123.png) after ![](<waku-blob:456.webp>)","image":"waku-blob:789.jpg"}"#;
        assert_eq!(
            super::references(text).collect::<Vec<_>>(),
            [
                "waku-blob:123.png",
                "waku-blob:456.webp",
                "waku-blob:789.jpg"
            ]
        );
    }
}
