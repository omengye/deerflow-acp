//! Native learning-source view. Render reads only bounded, cached projections.
use super::learning_state::{Pane, Scope};
use super::*;

impl Waku {
    pub(super) fn render_deerflow_learning_overview(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings;
        let browser = &state.skill_browser.learning;
        let unavailable = state.busy() || state.applying() || state.status["running"] != true;
        let mut body = df_group(theme)
            .child(df_label("学习流水线", "后台在线表示执行器可接收工作；是否已有产出请看经验、归并、匹配、生成和发布记录。状态是最近一次查询的快照。", theme))
            .child(df_button("df-learning-overview-refresh", "刷新学习流水线", unavailable, false, theme, cx,
                |this, _, cx| this.deerflow_queue_skill_center(cx)));
        body = body.child(df_label(
            "全目录学习状态",
            browser
                .status_summary
                .clone()
                .unwrap_or_else(|| "正在读取；若当前服务不支持诊断，请更新配套服务后重试。".into()),
            theme,
        ));
        if let Some(queue) = browser.status.as_ref().and_then(|s| s.queue.as_ref()) {
            body = body.child(df_label("全目录队列", queue.summary(), theme));
            if !queue.reasons.is_empty() {
                body = body.child(df_label(
                    "全目录等待与阻塞原因",
                    queue.reasons_summary(),
                    theme,
                ));
            }
        }
        body = body
            .child(df_label(
                "自动发现",
                state
                    .skill_browser
                    .discovery
                    .status_summary()
                    .unwrap_or("尚未读取发现状态")
                    .to_owned(),
                theme,
            ))
            .child(df_label(
                "草稿生成",
                state
                    .skill_browser
                    .generation
                    .status_summary()
                    .unwrap_or_else(|| "尚未读取生成状态".into()),
                theme,
            ));
        let Some(session) = state
            .skills_session_id
            .as_ref()
            .filter(|session| browser.session_id.as_ref() == Some(*session))
        else {
            return body
                .child(df_label(
                    "此聊天诊断不可用",
                    "先打开一个已建立 DeerFlow 原生会话的聊天，再进入进化记录。",
                    theme,
                ))
                .into_any_element();
        };
        let query = browser.diagnostics_request();
        body = body.child(df_label(format!("此聊天：{}", state.skills_session_title),
            "诊断只读取运行与作业元数据。任务未结算时应核对原任务；开始新目标不会解锁或回填历史输入。", theme))
            .child(df_button("df-learning-diagnostics-refresh", "刷新此聊天诊断", unavailable || query.is_none(), false, theme, cx,
                move |this, _, cx| { if let Some(query) = &query { this.deerflow_request("manage", query.clone(), cx); } }));
        let Some(diagnostics) = &browser.diagnostics else {
            return body
                .child(df_label(
                    "尚未读取此聊天诊断",
                    "进入进化记录时会自动查询；不支持此接口的旧服务仍可展开学习详情查询作业。",
                    theme,
                ))
                .into_any_element();
        };
        body = body.child(df_label(
            "此聊天授权与队列",
            format!(
                "贡献 {} · 授权版本 {} · 后台执行器{}\n{}",
                if diagnostics.contribute {
                    "允许"
                } else {
                    "未允许"
                },
                diagnostics.consent_epoch,
                if diagnostics.running {
                    "在线"
                } else {
                    "离线"
                },
                diagnostics.queue.summary()
            ),
            theme,
        ));
        if !diagnostics.queue.reasons.is_empty() {
            body = body.child(df_label(
                "此聊天等待与阻塞原因",
                diagnostics.queue.reasons_summary(),
                theme,
            ));
        }
        if diagnostics.inputs.is_empty() && diagnostics.jobs.is_empty() {
            body = body.child(df_label(
                "此聊天暂无已捕获的学习输入或作业",
                "检查全局学习和此聊天贡献。仅在开启后捕获新的真实输入，旧聊天不会回填。",
                theme,
            ));
        }
        let visible = if browser.diagnostics_expanded {
            super::learning_diagnostics::LIMIT
        } else {
            3
        };
        if diagnostics.inputs.len() > 3
            || diagnostics
                .jobs
                .iter()
                .filter(|job| job.job.state == "failed")
                .count()
                > 3
        {
            body = body.child(df_button(
                "df-learning-diagnostics-details",
                if browser.diagnostics_expanded {
                    "收起诊断明细"
                } else {
                    "展开全部已载入诊断（当前最多显示 3 条输入 / 失败作业）"
                },
                false,
                browser.diagnostics_expanded,
                theme,
                cx,
                |this, _, cx| {
                    this.deerflow_settings
                        .skill_browser
                        .learning
                        .diagnostics_expanded = !this
                        .deerflow_settings
                        .skill_browser
                        .learning
                        .diagnostics_expanded;
                    cx.notify();
                },
            ));
        }
        for (index, input) in diagnostics.inputs.iter().take(visible).enumerate() {
            body = body.child(df_label(
                format!("学习输入 {}", index + 1),
                input.summary(),
                theme,
            ));
            if matches!(
                input.reason.as_str(),
                "tree_unsettled" | "source_active" | "unsafe_outcome"
            ) || input.state == "awaiting_completion"
            {
                let native_session = session.clone();
                let run_id = input.run_id.clone();
                body = body.child(df_button(
                    SharedString::from(format!("df-learning-task-{index}")),
                    "打开原任务核对 / 恢复",
                    unavailable,
                    false,
                    theme,
                    cx,
                    move |this, _, cx| {
                        this.open_deerflow_learning_recovery(
                            &native_session,
                            Some(&run_id),
                            false,
                            cx,
                        )
                    },
                ));
                let native_session = session.clone();
                let run_id = input.run_id.clone();
                body = body.child(df_button(
                    SharedString::from(format!("df-learning-receipt-{index}")),
                    "核对待确认回执",
                    unavailable,
                    false,
                    theme,
                    cx,
                    move |this, _, cx| {
                        this.open_deerflow_learning_recovery(
                            &native_session,
                            Some(&run_id),
                            true,
                            cx,
                        )
                    },
                ));
            }
        }
        for (index, item) in diagnostics
            .jobs
            .iter()
            .filter(|item| item.job.state == "failed")
            .take(visible)
            .enumerate()
        {
            let request = browser.extraction_retry_request(&item.job.id);
            body = body.child(df_label(
                format!("失败学习作业 {}", item.job.id),
                format!(
                    "类型 {} · 错误 {} · 状态版本 {}\n{}",
                    item.job.kind,
                    item.job.error_code,
                    item.job.state_version,
                    if item.retryable {
                        "可重试原提取；保留尝试次数、期限与已发生用量。".to_owned()
                    } else {
                        super::learning_diagnostics::reason(&item.retry_reason)
                    }
                ),
                theme,
            ));
            if item.retryable {
                body = body.child(df_button(SharedString::from(format!("df-learning-extraction-retry-{index}")),
                    "重试原经验提取", unavailable || state.skill_browser.mutations_pending() || request.is_none(), false, theme, cx,
                    move |this, _, cx| {
                        if let Some(request) = &request {
                            this.deerflow_settings.confirm_manage = Some((
                                "重新排队原失败的经验提取，会继续消耗学习与共享维护额度。保留原尝试次数和期限，不重放任务工具，也不补录历史。服务会再次核验来源、授权版本及作业版本。".into(), request.clone()));
                            cx.notify();
                        }
                    }));
            }
        }
        if diagnostics.truncated {
            body = body.child(df_label("诊断结果已截断", "仅显示有界输入和作业，未出现的记录不代表不存在。展开学习详情，在学习作业中分页核对历史。", theme));
        }
        body.into_any_element()
    }

    pub(super) fn render_deerflow_learning(
        &self,
        theme: Theme,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let state = &self.deerflow_settings;
        let browser = &state.skill_browser.learning;
        let supported = state
            .settings_contract
            .allows("/skill_evolution/learning/enabled");
        let unavailable =
            !supported || state.busy() || state.applying() || state.status["running"] != true;
        let mutation_disabled = unavailable || state.skill_browser.mutations_pending();
        let mut body = div().child(df_group(theme).child(df_label(
            "学习来源",
            "系统在后台提取、归并和匹配真实用户多轮经验，并按授权自动生成提案；人工审核只需批准或驳回生成结果，不需手工整理或触发进化。以下内容是模型推断，质量未评估；历史查询不授予创建或发布权限。",
            theme,
        )));
        if !supported {
            return body
                .child(df_label(
                    "当前服务未提供兼容的学习能力",
                    "更新配套 Go 服务并重新加载配置。此页面不会修改草稿或自动开启后台模型工作。",
                    theme,
                ))
                .into_any_element();
        }
        let mut status = df_group(theme).child(df_button(
            "df-learning-status",
            "刷新学习状态",
            unavailable,
            false,
            theme,
            cx,
            |this, _, cx| {
                this.deerflow_request("manage", json!({"operation":"skill.learning.status"}), cx)
            },
        ));
        if let Some(summary) = &browser.status_summary {
            status = status.child(df_label("宿主运行状态", summary.clone(), theme));
        } else {
            status = status.child(df_label(
                "尚未读取学习状态",
                "以运行服务的响应为准，不从配置草稿推断已启用。",
                theme,
            ));
        }
        body = body.child(status);
        let Some(session) = state
            .skills_session_id
            .as_ref()
            .filter(|s| browser.session_id.as_ref() == Some(*s))
        else {
            return body.child(df_group(theme).child(df_label(
                "尚未选定 DeerFlow 原生会话",
                "先打开一个已建立 DeerFlow 会话的聊天，再回到技能中心。其他提供方聊天和本地 transcript UUID 不作为学习会话授权。",
                theme,
            ))).into_any_element();
        };
        let mut controls = df_group(theme)
            .child(df_label(
                format!("学习来源所属聊天：{}", state.skills_session_title),
                format!("原生会话 {session}"),
                theme,
            ))
            .child(df_button(
                "df-learning-control-refresh",
                "刷新此聊天贡献设置",
                unavailable,
                false,
                theme,
                cx,
                |this, _, cx| {
                    this.deerflow_sync_missing_session(cx);
                    if let Some(session) = this.deerflow_settings.skills_session_id.clone() {
                        this.deerflow_request(
                            "manage",
                            json!({"operation":"skill.learning.control.get", "session_id":session}),
                            cx,
                        );
                    }
                },
            ));
        if let Some(control) = &browser.control {
            controls = controls.child(df_label("当前授权", format!(
                "贡献 {} · 工作区共享 {} · 授权版本 {}\n只捕获之后被宿主接受的真实输入，不回填旧聊天。全局学习关闭与撤销此会话贡献是不同操作。",
                if control.contribute { "允许" } else { "不允许" },
                if control.workspace_shared { "允许" } else { "不允许" }, control.consent_epoch), theme));
            if control.consent_epoch == 0 {
                controls = controls.child(df_label(
                    "尚未保存逐会话设置",
                    "贡献当前沿用宿主 learning 默认；工作区共享仍需逐会话明确允许。",
                    theme,
                ));
            }
            let contribute = !control.contribute;
            let shared = contribute && control.workspace_shared;
            let contribution = browser.control_request(contribute, shared);
            let sharing = browser.control_request(control.contribute, !control.workspace_shared);
            let mut actions = div().flex().flex_wrap().gap(px(6.0));
            for (id, label, request, explanation) in [
                (
                    "df-learning-contribute",
                    if contribute {
                        "允许此聊天贡献"
                    } else {
                        "撤销此聊天贡献"
                    },
                    contribution,
                    if contribute {
                        "允许此会话之后的输入用于脱敏经验提取及已授权的后续学习阶段，可能消耗共享维护模型额度。不会回填历史聊天；这不授予技能发布权限。"
                    } else {
                        "停止此会话贡献并撤销工作区共享；删除未发布学习来源及其全部传递派生内容，取消相关未完成作业。维护费用、已消费票和已采用的正式技能不会撤销。"
                    },
                ),
                (
                    "df-learning-sharing",
                    if control.workspace_shared {
                        "撤销工作区共享"
                    } else {
                        "允许同工作区共享"
                    },
                    sharing,
                    if control.workspace_shared {
                        "撤销共享并提高授权版本，清理此会话旧学习来源及传递派生内容。仍允许之后的会话私有贡献；不会把已有共享历史降级为私有历史。已采用技能和费用保留。"
                    } else {
                        "允许此数据目录内、同一规范化工作区中另外也明确允许共享的会话归并经验。授权版本将提高，旧学习来源及派生内容会被清理；只捕获之后输入，不回填旧聊天。这不是多租户用户授权。"
                    },
                ),
            ] {
                actions = actions.child(df_button(
                    id,
                    label,
                    mutation_disabled || request.is_none(),
                    false,
                    theme,
                    cx,
                    move |this, _, cx| {
                        if let Some(request) = &request {
                            this.deerflow_settings.confirm_manage =
                                Some((explanation.to_owned(), request.clone()));
                            cx.notify();
                        }
                    },
                ));
            }
            let episode = browser.episode_request();
            actions = actions.child(df_button("df-learning-episode", "开始一个新目标", mutation_disabled || episode.is_none(), false, theme, cx,
                move |this, _, cx| {
                    if let Some(request) = &episode {
                        this.deerflow_settings.confirm_manage = Some((
                            "仅当用户确实开始独立的新任务时建立新边界；普通追问、纠正、恢复和复制不应新增独立票。只影响之后输入，不回填历史。此操作无幂等回执，结果未知时不要重发。".into(), request.clone()));
                        cx.notify();
                    }
                }));
            controls = controls.child(actions);
            if !browser
                .status
                .as_ref()
                .is_some_and(|s| s.cross_session_enabled)
            {
                controls = controls.child(df_label("共享尚不可用", "需宿主先明确启用 learning、consolidation 和 cross_session。仍可撤销现有共享或贡献，不会在此自动开启配置。", theme));
            }
        } else {
            controls = controls.child(df_label(
                "贡献设置需要刷新",
                "会话或服务变化会撤销界面的旧操作条件。刷新后才能修改贡献、共享或建立任务边界。",
                theme,
            ));
        }
        if let Some(result) = &browser.last_result {
            controls = controls.child(df_label("操作回执", result.clone(), theme));
        }
        body = body.child(controls);

        let mut history = df_group(theme).child(df_label("已保存学习历史", "每次仅显示 20 条，按服务 ID 顺序分页，不代表时间倒序。无记录不等于未执行任务；共享归并与会话私有归并分开查询。", theme));
        let mut panes = div().flex().flex_wrap().gap(px(6.0));
        for pane in [Pane::Experiences, Pane::Patterns, Pane::Matches, Pane::Jobs] {
            panes = panes.child(df_button(
                SharedString::from(format!("df-learning-pane-{pane:?}")),
                pane.label(),
                state.busy(),
                browser.pane == pane,
                theme,
                cx,
                move |this, _, cx| {
                    this.deerflow_settings
                        .skill_browser
                        .learning
                        .choose_pane(pane);
                    this.deerflow_settings.confirm_manage = None;
                    cx.notify();
                },
            ));
        }
        history = history.child(panes);
        if browser.pane.supports_workspace() {
            let mut scopes = div().flex().flex_wrap().gap(px(6.0));
            for (scope, caption) in [
                (Scope::Session, "此聊天私有"),
                (Scope::Workspace, "同工作区已授权共享"),
            ] {
                scopes = scopes.child(df_button(
                    SharedString::from(format!("df-learning-scope-{scope:?}")),
                    caption,
                    state.busy() || scope == Scope::Workspace && !browser.workspace_available(),
                    browser.scope == scope,
                    theme,
                    cx,
                    move |this, _, cx| {
                        if this
                            .deerflow_settings
                            .skill_browser
                            .learning
                            .choose_scope(scope)
                        {
                            this.deerflow_settings.confirm_manage = None;
                            cx.notify();
                        }
                    },
                ));
            }
            history = history.child(scopes).child(df_label("共享读取边界",
                "仅通过认证的本机 MANAGE 查询；服务核对同工作区所有来源会话的共享授权。单会话 owner-bound ACP 并不具有这项权限。", theme));
        }
        let query = browser.page_request();
        history = history.child(df_button(
            "df-learning-history-refresh",
            format!("刷新{}", browser.pane.label()),
            unavailable || query.is_none(),
            false,
            theme,
            cx,
            move |this, _, cx| {
                if let Some(query) = &query {
                    this.deerflow_request("manage", query.clone(), cx);
                }
            },
        ));
        if browser.loaded && browser.rows.is_empty() {
            history = history.child(df_label(
                "此范围暂无记录",
                "可先刷新宿主状态和会话贡献。跨会话共享结果不会出现在此聊天私有查询中。",
                theme,
            ));
        }
        for row in &browser.rows {
            let id = row.id.clone();
            history = history.child(
                df_button(
                    SharedString::from(format!("df-learning-row-{id}")),
                    row.title.clone(),
                    state.busy(),
                    browser.selected.as_ref() == Some(&id),
                    theme,
                    cx,
                    move |this, _, cx| {
                        this.deerflow_settings
                            .skill_browser
                            .learning
                            .select_row(&id);
                        this.deerflow_settings.confirm_manage = None;
                        cx.notify();
                    },
                )
                .w_full()
                .min_w_0()
                .justify_start()
                .overflow_hidden(),
            );
        }
        history = history.child(self.render_skill_page_nav(
            "learning",
            &browser.nav,
            unavailable || !browser.loaded,
            theme,
            cx,
        ));
        if let Some(row) = browser
            .rows
            .iter()
            .find(|row| browser.selected.as_ref() == Some(&row.id))
        {
            history = history
                .child(df_label(row.title.clone(), row.detail.clone(), theme).whitespace_normal());
            if !browser.source_members().is_empty() {
                history = history.child(df_label("查看所引用的脱敏来源",
                    "按经验成员读取真人输入、本地工具回执或实际技能读取版本。仅此成员在读取时核验，不重新认证整条归并或匹配质量；不会调用模型、增加独立票或授予发布权限。每次只缓存一份详情，服务或选择变化后清除。", theme));
                let mut members = div().flex().flex_wrap().gap(px(6.0));
                for (i, member) in browser.source_members().iter().enumerate() {
                    let request = browser.source_request(member);
                    members = members.child(df_button(
                        SharedString::from(format!("df-learning-source-{member}")),
                        format!("读取成员 {} 来源", i + 1),
                        unavailable || request.is_none(),
                        browser.source_selected(member),
                        theme,
                        cx,
                        move |this, _, cx| {
                            if let Some(request) = &request {
                                this.deerflow_request("manage", request.clone(), cx);
                            }
                        },
                    ));
                }
                history = history.child(members);
                if let Some(detail) = browser.source_detail() {
                    history = history.child(
                        df_label("已读取的来源（缓存）", detail.to_owned(), theme)
                            .whitespace_normal(),
                    );
                }
            }
            if let Some(request) = browser.ignore_request() {
                let ignored = request["review"]["ignored"] == true;
                history = history.child(df_button("df-learning-pattern-ignore",
                    if ignored { "忽略此条后续建议" } else { "恢复此条后续建议" }, mutation_disabled,
                    false, theme, cx, move |this, _, cx| {
                        if ignored {
                            this.deerflow_settings.confirm_manage = Some((
                                "忽略此归并结果及之后修订的建议；取消相关匹配和未发布候选，并撤销旧候选来源。保留原经验、费用、消费记录和已采用技能。这不是质量驳回票。".into(), request.clone()));
                            cx.notify();
                        } else {
                            this.deerflow_settings.confirm_manage = None;
                            this.deerflow_request("manage", request.clone(), cx);
                        }
                    }));
            }
            if browser.pane == Pane::Patterns {
                history = history.child(df_label("显式重试新技能创建",
                    "仅适用于此归并结果已失败、已取消或提案已驳回的新技能创建。先在生成页刷新并选择原作业，再核对此处的完整条件。不能换名、换来源或升级旧归并修订；最多 3 次人工重试。复用原独立 episode 证据不新增票，不删除自动创建预留，不退旧消费；每次重新占用提案和维护额度，仍受 cooldown 限制。", theme))
                    .child(df_button("df-learning-choose-retry", "去生成页选择 / 刷新原创建作业", unavailable, false, theme, cx,
                        |this, _, cx| {
                            this.deerflow_settings.skill_center_tab = presentation::SkillCenterTab::Generation;
                            this.deerflow_settings.confirm_manage = None;
                            this.deerflow_request("manage", json!({"operation":"skill.generation.list", "limit":skills::PAGE_SIZE}), cx);
                        }));
                if let Some((previous, caption)) =
                    state.skill_browser.generation.creation_retry_previous()
                {
                    history = history.child(df_label("待核对原作业", caption, theme));
                    let request = browser.creation_retry_prepare_request(&previous);
                    history = history.child(df_button(
                        "df-learning-retry-prepare",
                        "核对当前来源和完整重试条件",
                        unavailable || mutation_disabled || request.is_none(),
                        false,
                        theme,
                        cx,
                        move |this, _, cx| {
                            if let Some(request) = &request {
                                this.deerflow_request("manage", request.clone(), cx);
                            }
                        },
                    ));
                }
                if let Some(caption) = browser.creation_retry_caption() {
                    history = history.child(df_label("已核对人工重试条件", caption, theme));
                    let request = browser.creation_retry_request();
                    let allowed = request
                        .as_ref()
                        .is_some_and(|request| state.skill_browser.can_begin(request));
                    history = history.child(df_button(
                        "df-learning-retry-enqueue",
                        "确认授权一次新技能创建重试",
                        mutation_disabled || !allowed,
                        false,
                        theme,
                        cx,
                        move |this, _, cx| {
                            if let Some(request) = &request {
                                this.deerflow_settings.confirm_manage = None;
                                this.deerflow_request("manage", request.clone(), cx);
                            }
                        },
                    ));
                }
                history = history.child(df_label("人工重新裁决已有技能目标",
                    "先在技能目录选择并刷新一个已启用技能，再回到本归并结果。目录选择不代表 runtime 访问授权；服务将重新核对来源、当前 head / overlay、归并修订与全部学习来源。此操作不调用匹配模型、不确认方法质量、不创建新技能，后续仍使用原 discovery 配额与独立证据；候选必须人工发布。", theme))
                    .child(df_button("df-learning-choose-target", "去技能目录选择 / 刷新目标", unavailable, false, theme, cx,
                        |this, _, cx| {
                            this.deerflow_settings.skill_center_tab = presentation::SkillCenterTab::Catalog;
                            this.deerflow_settings.confirm_manage = None;
                            this.deerflow_request("manage", json!({"operation":"skill.list", "limit":skills::PAGE_SIZE}), cx);
                        }));
                if let Some(caption) = state.skill_browser.learning_target_caption() {
                    history = history.child(df_label("已缓存待裁决目标", caption, theme));
                    let mut actions = div().flex().flex_wrap().gap(px(6.0));
                    for (relation, label) in [
                        ("body_gap", "裁决为正文缺口"),
                        ("trigger_mismatch", "裁决为触发不匹配"),
                        ("already_covered", "裁决为已覆盖，不提案"),
                    ] {
                        let request = state.skill_browser.learning_adjudication_request(relation);
                        actions = actions.child(df_button(SharedString::from(format!("df-learning-adjudicate-{relation}")), label,
                            mutation_disabled || request.is_none(), false, theme, cx, move |this, _, cx| {
                                if let Some(request) = &request {
                                    this.deerflow_settings.confirm_manage = Some((
                                        "按已缓存的精确来源 / head / overlay 重新裁决当前归并目标，取消并清理旧的未发布候选。旧匹配记录保留为历史但不再授予准入。当前来源或目标变更会拒绝，不隐式 rebase、不重置消费和创建预留；允许提案时仍需原独立开关、证据和额度，候选必须人工发布。".into(), request.clone()));
                                    cx.notify();
                                }
                            }));
                    }
                    history = history.child(actions);
                } else {
                    history = history.child(df_label("尚无新鲜目标缓存", "先在技能目录选择一个目标并读取详情；历史匹配中的旧 head 不作为新裁决目标。", theme));
                }
            }
            if let Some(request) = &row.cancel {
                let request = request.clone();
                history = history.child(df_button(
                    "df-learning-job-cancel",
                    "取消此学习作业",
                    mutation_disabled,
                    false,
                    theme,
                    cx,
                    move |this, _, cx| {
                        this.deerflow_settings.confirm_manage = None;
                        this.deerflow_request("manage", request.clone(), cx);
                    },
                ));
                history = history.child(df_label(
                    "取消说明",
                    "停止当前学习作业，晚到输出不再写回；已发生用量、已保存历史和正式技能保留。",
                    theme,
                ));
            }
        }
        body = body.child(history).child(df_group(theme).child(df_label("进入现有审核流程",
            "经验或匹配记录并不证明已生成提案。自动发现记录提供 match / pattern / episode 引用；关联生成作业提供候选、差异与评测。默认人工发布；独立授权的自动补丁由宿主准入，描述修改另需正负触发选择改善，新技能和其他元数据仍需人工审核。", theme))
            .child(div().flex().flex_wrap().gap(px(6.0))
                .child(df_button("df-learning-discovery", "查看自动发现来源", unavailable, false, theme, cx, |this, _, cx| {
                    this.deerflow_settings.skill_center_tab = presentation::SkillCenterTab::Generation;
                    this.deerflow_settings.show_more = true;
                    this.deerflow_settings.confirm_manage = None;
                    this.deerflow_request("manage", json!({"operation":"skill.discovery.list", "limit":skills::PAGE_SIZE}), cx);
                }))
                .child(df_button("df-learning-review", "查看待审核提案与差异", unavailable, false, theme, cx, |this, _, cx| {
                    this.deerflow_settings.skill_center_tab = presentation::SkillCenterTab::Review;
                    this.deerflow_settings.show_more = false;
                    this.deerflow_settings.confirm_manage = None;
                    this.deerflow_request("manage", json!({"operation":"proposal.list", "status":"pending_review", "limit":skills::PAGE_SIZE}), cx);
                }))));
        body.into_any_element()
    }
}
