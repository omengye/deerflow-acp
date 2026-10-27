package memory

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"
)

// Opt-in release acceptance uses the pinned on-premise model, never an external
// embedding API. Only synthetic project facts enter this temporary database.
func TestPersonalMemoryChineseEmbeddingAcceptance(t *testing.T) {
	endpoint := os.Getenv("MEMORY_EMBEDDING_TEST_URL")
	if endpoint == "" {
		t.Skip("set MEMORY_EMBEDDING_TEST_URL for pinned-model release acceptance")
	}
	corpus := [][6]string{
		{"项目使用 PostgreSQL 保存业务数据。", "业务数据落在哪种数据库？", "持久化采用哪个关系库？", "服务端数据存储选型是什么？", "项目数据库用的是什么？", "业务表存在什么系统？"},
		{"Redis 用于缓存热点查询结果，过期时间五分钟。", "热点缓存多久失效？", "查询缓存存在什么服务？", "怎么缓解高频读取的压力？", "缓存的有效期是多久？", "读缓存采用的方案是什么？"},
		{"生产环境部署在 Gentoo Linux 的 Docker 容器中。", "线上服务跑在哪里？", "生产使用哪个 Linux 发行版？", "应用采用什么容器化部署方式？", "正式环境是什么操作系统？", "线上运行环境是什么？"},
		{"外部 HTTPS 请求通过 Caddy 反向代理进入服务。", "公网流量如何转发？", "TLS 入口使用什么软件？", "谁负责外部的安全访问入口？", "HTTPS 请求经过哪个代理？", "站点的反代是什么？"},
		{"用户身份认证采用短期访问令牌和刷新令牌。", "登录状态怎么续期？", "身份验证用什么机制？", "访问令牌过期以后怎么办？", "账号授权是如何维持的？", "用户认证方案是什么？"},
		{"移动端断线重连使用指数退避并加入随机抖动。", "网络掉线后怎么安排重试？", "怎样避免客户端同时重新连接？", "手机连接重试的等待时间怎么算？", "重连有退避机制吗？", "弱网重连如何降低服务器压力？"},
		{"WebSocket 事件保存递增序号，重连后按游标回放。", "掉线时遗漏的消息怎么补回来？", "实时事件断点续传依靠什么？", "重连如何追赶消息进度？", "如何保证事件按顺序恢复？", "消息回放游标怎样工作？"},
		{"支付接口用幂等请求编号防止重复扣款。", "支付超时重试会多扣钱吗？", "如何处理重复的付款请求？", "扣款操作怎样防止执行两次？", "交易的去重依据是什么？", "支付幂等怎么保证？"},
		{"审计日志保留九十天，只允许管理员读取。", "操作记录保存多长时间？", "谁能查看审计记录？", "审计日志什么时候清理？", "安全审计数据的访问权限是什么？", "日志留存策略是什么？"},
		{"数据库每天凌晨两点执行备份，保留三十份。", "备份任务几点执行？", "数据库有多少天的备份？", "自动备份保留几份？", "数据副本多久生成一次？", "数据库备份计划是什么？"},
		{"文件上传限制二十兆字节，只接受 PDF 和图片。", "附件最大能传多大？", "上传支持哪些文件格式？", "大文件上传有什么限制？", "可以提交什么类型的文档？", "附件大小上限是多少？"},
		{"搜索索引由 Elasticsearch 提供，每分钟刷新一次。", "全文搜索依赖什么引擎？", "搜索结果多久更新？", "索引刷新频率是多少？", "检索服务采用什么组件？", "新内容什么时候能搜到？"},
		{"任务队列用 RabbitMQ，失败任务最多重试三次。", "异步任务通过什么消息队列调度？", "任务失败以后重跑几次？", "后台作业的队列选型是什么？", "消息消费失败的上限是什么？", "RabbitMQ 的失败重试策略是什么？"},
		{"接口超时设置为十五秒，超过后返回明确错误。", "请求等多久会超时？", "API 最大等待时间是多少？", "慢请求到达截止时间如何处理？", "服务接口超时阈值是多少？", "接口等待十五秒后会怎样？"},
		{"指标监控采用 Prometheus，仪表盘用 Grafana。", "系统监控数据用什么采集？", "性能图表在哪里展示？", "运行指标如何可视化？", "监控平台由哪些组件组成？", "指标看板用了什么软件？"},
		{"错误告警发送到值班人员的企业微信。", "线上故障会通知谁？", "异常告警发到什么渠道？", "值班人员如何收到错误通知？", "生产报错会通过什么方式提醒？", "故障通知的接收人是谁？"},
		{"项目后端用 Go 编写，前端使用 TypeScript。", "服务端的开发语言是什么？", "页面代码采用什么语言？", "项目的前后端语言分别是什么？", "接口服务是用 Go 还是 Java？", "客户端网页用 TypeScript 吗？"},
		{"CI 在每次合并请求时运行单元测试与静态检查。", "提交代码后什么时候跑测试？", "合并前自动验证哪些内容？", "代码质量流水线的触发时机是什么？", "持续集成包含什么检查？", "每个合并请求都做单元验证吗？"},
		{"服务健康检查路径为 /health，每十秒探测一次。", "负载均衡怎么判断服务可用？", "健康探测访问哪个接口？", "存活检查多长时间做一次？", "如何确认容器里的服务健康？", "健康检查的周期是什么？"},
		{"记忆删除写入墓碑版本，后台旧任务不能恢复被删内容。", "已经忘记的资料会被重新写回来吗？", "删除后如何防止旧提取任务复活记录？", "遗忘操作怎样持久化？", "后台任务能否覆盖已删除的记忆？", "记忆删除为什么需要墓碑？"},
	}
	s, _ := personalStore(t)
	ctx := context.Background()
	scope, err := NewScope(WorkspaceScope, t.TempDir(), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	embedder, err := NewHTTPEmbedder(endpoint, "BAAI/bge-small-zh-v1.5@46fbe35")
	if err != nil {
		t.Fatal(err)
	}
	s.SetEmbedder(embedder)
	ids := make([]string, len(corpus))
	for i, c := range corpus {
		m := change(fmt.Sprintf("eval-%d", i), fmt.Sprintf("project.fact_%d", i), c[0], 0)
		m.Category = "project"
		receipt, err := s.Mutate(ctx, scope, m)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = receipt.FactID
	}
	for range 2 {
		if err = s.IndexPending(ctx); err != nil {
			t.Fatal(err)
		}
	}
	hits, total := 0, 0
	var times []int64
	for i, c := range corpus {
		for _, q := range c[1:] {
			start := time.Now()
			facts, _, err := s.HybridSearch(ctx, scope, q, 5)
			times = append(times, time.Since(start).Milliseconds())
			if err != nil {
				t.Fatal(err)
			}
			total++
			found := false
			for _, f := range facts {
				if f.ID == ids[i] {
					hits++
					found = true
					break
				}
			}
			if !found {
				t.Logf("miss synthetic case=%d query=%q", i, q)
			}
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	t.Logf("synthetic_chinese_queries=%d recall_at_5=%.3f p95_ms=%d", total, float64(hits)/float64(total), times[len(times)*95/100])
	if float64(hits)/float64(total) < 0.95 {
		t.Fatalf("recall acceptance failed: %d/%d", hits, total)
	}
	other, _ := NewScope(WorkspaceScope, t.TempDir(), "", "", "")
	facts, _, err := s.HybridSearch(ctx, other, "项目使用什么数据库", 5)
	if err != nil || len(facts) != 0 {
		t.Fatal("workspace isolation failed", err)
	}
}
