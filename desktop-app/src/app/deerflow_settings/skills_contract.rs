//! Shared wire identities and single-page navigation; no desktop or I/O dependencies.
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub(super) struct Target {
    pub(super) source_id: String,
    pub(super) source_identity: String,
    pub(super) name: String,
    pub(super) scope: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub(super) workspace: String,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub(super) struct Head {
    pub(super) version: String,
    pub(super) state_version: u64,
    pub(super) exists: bool,
    pub(super) enabled: bool,
    pub(super) managed: bool,
}

pub(super) fn digest(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
}

// Navigation commits only after a valid response. Keep one page and a bounded
// cursor stack instead of accumulating history on the render path.
#[derive(Default)]
pub(super) struct PageNav {
    query: Value,
    cursor: String,
    previous: Vec<String>,
    next: String,
    cursor_field: String,
}

impl PageNav {
    pub(super) fn page_number(&self) -> usize {
        self.previous.len() + 1
    }

    pub(super) fn current_request(&self) -> Option<Value> {
        if !self.query.is_object() {
            return None;
        }
        let mut request = self.query.clone();
        if !self.cursor.is_empty() {
            request[&self.cursor_field] = json!(self.cursor);
        }
        Some(request)
    }

    pub(super) fn accept(
        &mut self,
        request: &Value,
        next: &str,
        field: &str,
    ) -> anyhow::Result<()> {
        anyhow::ensure!(next.len() <= 2048, "分页游标过长");
        let cursor = request[field].as_str().unwrap_or("");
        let mut query = request.clone();
        query
            .as_object_mut()
            .ok_or_else(|| anyhow::anyhow!("分页请求无效"))?
            .remove(field);
        if cursor.is_empty() {
            self.previous.clear();
        } else {
            anyhow::ensure!(
                query == self.query && field == self.cursor_field,
                "分页条件已变化，请刷新"
            );
            if cursor == self.next && cursor != self.cursor {
                anyhow::ensure!(
                    self.previous.len() < 1000,
                    "分页数量达到上限，请缩小筛选范围"
                );
                self.previous.push(self.cursor.clone());
            } else if self.previous.last().is_some_and(|value| value == cursor) {
                self.previous.pop();
            } else {
                anyhow::ensure!(cursor == self.cursor, "分页游标已变化，请刷新");
            }
        }
        self.query = query;
        self.cursor = cursor.to_owned();
        self.next = next.to_owned();
        self.cursor_field = field.to_owned();
        Ok(())
    }

    pub(super) fn request(&self, next: bool) -> Option<Value> {
        let cursor = if next {
            &self.next
        } else {
            self.previous.last()?
        };
        if next && cursor.is_empty() {
            return None;
        }
        let mut request = self.query.clone();
        if !cursor.is_empty() {
            request[&self.cursor_field] = json!(cursor);
        }
        Some(request)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn page_navigation_keeps_query_and_commits_only_valid_cursors() {
        let mut nav = PageNav::default();
        let first = json!({"operation":"skill.learning.patterns", "session_id":"a", "limit":20});
        assert!(nav.request(true).is_none());
        nav.accept(&first, "next", "cursor").unwrap();
        let next = nav.request(true).unwrap();
        nav.accept(&next, "third", "cursor").unwrap();
        assert_eq!(nav.request(false).unwrap(), first);
        let mut wrong = nav.request(true).unwrap();
        wrong["session_id"] = json!("b");
        assert!(nav.accept(&wrong, "", "cursor").is_err());
        assert_eq!(nav.request(true).unwrap()["cursor"], "third");
        assert!(nav.accept(&next, &"x".repeat(2049), "cursor").is_err());
        nav.accept(&first, "next", "cursor").unwrap();
        assert!(nav.request(false).is_none());
    }

    #[test]
    fn refreshing_current_page_preserves_filters_and_navigation() {
        let mut nav = PageNav::default();
        assert!(nav.current_request().is_none());
        let first = json!({"operation":"proposal.list", "status":"pending_review", "needs_attention":true, "source_id":"local", "name":"test", "limit":20});
        nav.accept(&first, "next", "cursor").unwrap();
        assert_eq!(nav.current_request(), Some(first.clone()));
        let second = nav.request(true).unwrap();
        nav.accept(&second, "third", "cursor").unwrap();
        assert_eq!(nav.current_request(), Some(second.clone()));
        nav.accept(&nav.current_request().unwrap(), "new-next", "cursor")
            .unwrap();
        assert_eq!(nav.page_number(), 2);
        assert_eq!(nav.request(false), Some(first));
        assert_eq!(nav.request(true).unwrap()["cursor"], "new-next");
        assert_eq!(nav.current_request(), Some(second));
    }
}
