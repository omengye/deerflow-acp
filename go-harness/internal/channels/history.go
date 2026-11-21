package channels

import (
	"context"
	"net/url"
	"strings"
)

type sessionSummary struct {
	SessionID string `json:"session_id"`
	Account   string `json:"account"`
	Workspace string `json:"workspace"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	CreatedMS int64  `json:"created_ms"`
	UpdatedMS int64  `json:"updated_ms"`
	Messages  int    `json:"message_count"`
}

// Restrict title fallback to user prompts. Slash commands (including approval
// codes) and automatically enqueued outbound messages cannot name a session.
const channelSessionsSQL = `WITH groups AS (
 SELECT instance,account,session_id,count(*) AS messages,min(created_ms) AS first_ms,max(created_ms) AS last_ms
 FROM harness_channel_messages WHERE instance=? AND session_id<>'' GROUP BY instance,account,session_id
), activity AS (
 SELECT g.*, COALESCE(s.cwd,(SELECT workspace FROM harness_channel_messages m WHERE m.instance=g.instance AND m.account=g.account AND m.session_id=g.session_id LIMIT 1)) AS workspace,
 COALESCE(s.title,'') AS title,
 max(g.last_ms,COALESCE(CAST(round((julianday(r.updated_at)-2440587.5)*86400000) AS INTEGER),0)) AS updated_ms,
 CASE WHEN s.id IS NULL THEN 'history_only'
 WHEN coalesce(e.status,r.status,'')<>'waiting_input' THEN coalesce(e.status,r.status,'idle')
 WHEN e.blocked_reason<>'' OR coalesce(json_valid(CAST(e.manifest AS TEXT)),0)=0 OR w.attempts>=3 THEN 'blocked'
 WHEN stop.root_id IS NOT NULL THEN 'stopped'
 WHEN json_array_length(CAST(e.manifest AS TEXT),'$.Interrupts')>0 THEN 'waiting_approval'
 WHEN json_array_length(CAST(e.manifest AS TEXT),'$.TaskWaits')>0 THEN 'waiting_tasks'
 ELSE 'waiting_input' END AS task_status
 FROM groups g LEFT JOIN harness_sessions s ON s.id=g.session_id
 LEFT JOIN harness_runs r ON r.id=(SELECT id FROM harness_runs WHERE session_id=g.session_id ORDER BY created_at DESC,id DESC LIMIT 1)
 LEFT JOIN harness_executions e ON e.run_id=r.id
 LEFT JOIN harness_execution_wakeups w ON w.run_id=r.id AND w.checkpoint_sha=CASE WHEN json_valid(CAST(e.manifest AS TEXT)) THEN json_extract(CAST(e.manifest AS TEXT),'$.CheckpointSHA') END
 LEFT JOIN harness_automatic_stops stop ON stop.root_id=e.root_budget_id
 ORDER BY updated_ms DESC,g.session_id,g.account LIMIT 100
)
SELECT a.session_id,a.account,a.workspace,a.title,a.task_status,a.first_ms,a.updated_ms,a.messages,
 COALESCE((SELECT substr(prompt,1,1024) FROM (
   SELECT text AS prompt, trim(replace(replace(replace(text,char(9),' '),char(10),' '),char(13),' ')) AS normalized, created_ms,rowid AS position
   FROM harness_channel_messages m WHERE m.instance=a.instance AND m.account=a.account AND m.session_id=a.session_id)
 WHERE normalized<>'' AND substr(normalized,1,instr(normalized||' ',' ')-1) NOT IN ('/new','/status','/help','/permission','/resume','/approve','/deny','/stop','/pair')
 ORDER BY created_ms,position LIMIT 1),'')
FROM activity a ORDER BY updated_ms DESC,session_id,account`

func messagePreview(text string) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}
	if receiptExcludedCommand(text) {
		return words[0]
	}
	if u, err := url.Parse(words[0]); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" {
		if len(words) == 1 {
			return u.Hostname()
		}
		words = words[1:]
	}
	runes := []rune(strings.Join(words, " "))
	if len(runes) > 64 {
		return string(runes[:64]) + "…"
	}
	return string(runes)
}

func (m *Manager) Sessions(ctx context.Context, id string) ([]sessionSummary, error) {
	rows, err := m.db.QueryContext(ctx, channelSessionsSQL, id)
	if err != nil {
		return nil, err
	}
	items := []sessionSummary{}
	for rows.Next() {
		var s sessionSummary
		var prompt string
		if err := rows.Scan(&s.SessionID, &s.Account, &s.Workspace, &s.Title, &s.Status, &s.CreatedMS, &s.UpdatedMS, &s.Messages, &prompt); err != nil {
			rows.Close()
			return nil, err
		}
		if strings.TrimSpace(s.Title) == "" {
			s.Title = messagePreview(prompt)
		}
		if s.Title == "" {
			s.Title = "渠道会话 " + s.SessionID[:min(8, len(s.SessionID))]
		}
		items = append(items, s)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Keep messages without a session (including unknown sends) reachable
	// without consuming one of the 100 session slots.
	rows, err = m.db.QueryContext(ctx, `SELECT account,count(*),min(created_ms),max(created_ms) FROM harness_channel_messages WHERE instance=? AND session_id='' GROUP BY account ORDER BY max(created_ms) DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		s := sessionSummary{Title: "未关联会话的消息"}
		if err := rows.Scan(&s.Account, &s.Messages, &s.CreatedMS, &s.UpdatedMS); err != nil {
			return nil, err
		}
		items = append(items, s)
	}
	return items, rows.Err()
}

type sessionMessage struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	CreatedMS int64  `json:"created_ms"`
	Preview   string `json:"preview"`
}

type messagePage struct {
	Items      []sessionMessage `json:"items"`
	NextBefore int64            `json:"next_before"`
}

func (m *Manager) SessionMessages(ctx context.Context, id, account, session string, before int64) (messagePage, error) {
	const limit = 50
	page := messagePage{Items: []sessionMessage{}}
	rows, err := m.db.QueryContext(ctx, `SELECT rowid,id,status,created_ms,substr(text,1,1024) FROM harness_channel_messages
WHERE instance=? AND account=? AND session_id=? AND (?=0 OR rowid<?) ORDER BY rowid DESC LIMIT ?`, id, account, session, before, before, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	var cursor int64
	for rows.Next() {
		if len(page.Items) == limit {
			page.NextBefore = cursor
			break
		}
		var message sessionMessage
		var prompt string
		if err := rows.Scan(&cursor, &message.ID, &message.Status, &message.CreatedMS, &prompt); err != nil {
			return page, err
		}
		message.Preview = messagePreview(prompt)
		page.Items = append(page.Items, message)
	}
	return page, rows.Err()
}
