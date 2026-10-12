//! Read-only, cited source inspection. No model work or publication authority.
use super::{SkillRef, identity, named_digest, text, version};
use serde::Deserialize;
use serde_json::Value;
use std::collections::HashSet;

pub(super) struct SourceSelection {
    pub(super) kind: &'static str,
    pub(super) id: String,
    pub(super) revision: u64,
    pub(super) members: Vec<String>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Reference {
    id: String,
    run_id: String,
    #[serde(default)]
    input_id: String,
    #[serde(default)]
    sequence: u64,
    #[serde(default)]
    tool_call_id: String,
    #[serde(default)]
    version: u64,
    kind: String,
    #[serde(default)]
    state: String,
    #[serde(default)]
    text: String,
    #[serde(default)]
    tool_name: String,
    #[serde(default)]
    failure_code: String,
    skill: Option<SkillRef>,
    #[serde(default)]
    path: String,
    #[serde(default)]
    sha256: String,
    #[serde(default)]
    truncated: bool,
    #[serde(default)]
    role_id: String,
}

impl Reference {
    fn detail(&self) -> anyhow::Result<String> {
        anyhow::ensure!(
            text(&self.id, 2048, true)
                && identity(&self.run_id)
                && self.sequence < i64::MAX as u64
                && self.version < i64::MAX as u64
                && text(&self.text, 8192, false)
                && text(&self.input_id, 256, false)
                && text(&self.tool_call_id, 256, false)
                && text(&self.tool_name, 256, false)
                && text(&self.failure_code, 128, false)
                && text(&self.path, 32768, false)
                && text(&self.role_id, 256, false),
            "来源引用超出身份或文本边界"
        );
        let label = match self.kind.as_str() {
            "user" => {
                anyhow::ensure!(
                    identity(&self.input_id)
                        && self.id == format!("input/{}", self.input_id)
                        && version(self.sequence)
                        && self.tool_call_id.is_empty()
                        && self.state.is_empty()
                        && self.skill.is_none(),
                    "真人输入引用身份不一致"
                );
                "宿主接受的真人输入（脱敏）".to_owned()
            }
            "assistant" => {
                anyhow::ensure!(
                    version(self.sequence)
                        && self.id == format!("event/{}", self.sequence)
                        && self.input_id.is_empty()
                        && self.tool_call_id.is_empty()
                        && self.state.is_empty()
                        && self.skill.is_none(),
                    "主代理回答引用身份不一致"
                );
                "主代理回答（脱敏，不含隐藏推理 / 子代理正文）".to_owned()
            }
            "tool_observation" => {
                anyhow::ensure!(
                    identity(&self.tool_call_id)
                        && identity(&self.tool_name)
                        && self.id == format!("receipt/{}/{}", self.run_id, self.tool_call_id)
                        && version(self.version)
                        && matches!(
                            self.state.as_str(),
                            "completed" | "failed" | "not_executed" | "no_effect"
                        )
                        && !matches!(
                            self.tool_name.as_str(),
                            "skill_manage" | "task" | "background_agent"
                        )
                        && self.input_id.is_empty()
                        && self.skill.is_none(),
                    "工具回执引用身份或终态无效"
                );
                let local = matches!(
                    self.tool_name.as_str(),
                    "ls" | "read_file"
                        | "list_directory"
                        | "search_files"
                        | "write_file"
                        | "edit_file"
                        | "str_replace"
                        | "glob"
                        | "grep"
                        | "delete_path"
                        | "move_path"
                        | "bash"
                );
                anyhow::ensure!(
                    local || self.text.starts_with("Argument shape: "),
                    "外部来源不能包含结果正文"
                );
                format!(
                    "{} · {} · 回执版本 {} · {}\n错误代码 {}",
                    self.tool_name,
                    self.state,
                    self.version,
                    if local {
                        "本地观察（脱敏）"
                    } else {
                        "外部提示，仅参数形状；无结果正文"
                    },
                    self.failure_code
                )
            }
            "skill_read" => {
                let skill = self
                    .skill
                    .as_ref()
                    .ok_or_else(|| anyhow::anyhow!("缺少真实技能读取版本"))?;
                anyhow::ensure!(skill.validate(), "真实技能读取版本身份无效");
                anyhow::ensure!(
                    self.id
                        .strip_prefix("skill_read/")
                        .is_some_and(|id| { id.parse::<u64>().is_ok_and(version) })
                        && self.text.is_empty()
                        && self.state.is_empty()
                        && self.tool_call_id.is_empty()
                        && self.input_id.is_empty()
                        && identity(&self.path)
                        && super::digest(&self.sha256),
                    "技能读取引用或文件摘要不一致"
                );
                format!(
                    "真实技能读取（不证明因果贡献）\n{}\n文件 {} · sha256 {}\n角色 {}",
                    skill.detail(),
                    self.path,
                    self.sha256,
                    self.role_id
                )
            }
            _ => anyhow::bail!("不支持的来源引用类型"),
        };
        Ok(format!(
            "{label}\n引用 {} · run {}\n{}\n{}",
            self.id,
            self.run_id,
            if self.truncated {
                "已截断；不能当作完整成功证据"
            } else {
                "学习捕获边界内的引用，不等于完整原始会话"
            },
            self.text
        ))
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub(super) struct SourceDetail {
    session_id: String,
    source: Value,
    source_session_id: String,
    episode_id: String,
    consent_epoch: u64,
    extraction_id: String,
    input_sha256: String,
    result_sha256: String,
    policy_version: String,
    external_context_policy: String,
    source_state: String,
    quality: String,
    references: Vec<Reference>,
}

impl SourceDetail {
    pub(super) fn detail(&self, request: &Value) -> anyhow::Result<String> {
        anyhow::ensure!(
            request["session_id"] == self.session_id
                && request["source"] == self.source
                && identity(&self.source_session_id)
                && (self.source["scope"] == "workspace"
                    || self.source_session_id == self.session_id)
                && identity(&self.episode_id)
                && self.episode_id.starts_with("lep_")
                && version(self.consent_epoch)
                && named_digest(&self.extraction_id, "lex_")
                && super::digest(&self.input_sha256)
                && super::digest(&self.result_sha256)
                && self.policy_version == "procedural-experience-v1"
                && self.external_context_policy == "hints_only"
                && self.source_state == "checked_at_read"
                && self.quality == "unassessed"
                && (1..=32).contains(&self.references.len())
                && self
                    .references
                    .iter()
                    .map(|r| &r.id)
                    .collect::<HashSet<_>>()
                    .len()
                    == self.references.len(),
            "来源检查响应与原请求、授权或内容边界不一致"
        );
        let refs = self
            .references
            .iter()
            .map(Reference::detail)
            .collect::<anyhow::Result<Vec<_>>>()?
            .join("\n\n");
        Ok(format!(
            "来源会话 {} · 任务边界 {} · 授权版本 {}\n经验 {}\n提取 {}\n输入摘要 {}\n结果摘要 {}\n服务在此次读取时重建此成员的捕获视图并核验授权；缓存不保证持续有效。未重新裁决整条归并、技能匹配或方法质量，不增加独立票。\n只显示该经验引用的脱敏来源；外部 / MCP / 未知工具不返回结果正文。\n\n{refs}",
            self.source_session_id,
            self.episode_id,
            self.consent_epoch,
            self.source["experience_id"].as_str().unwrap_or(""),
            self.extraction_id,
            self.input_sha256,
            self.result_sha256
        ))
    }
}
