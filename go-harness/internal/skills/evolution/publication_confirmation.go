package evolution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

const confirmationRule = "native-user-publication-v1"

var ErrUserConfirmation = errors.New("explicit current user confirmation to publish this candidate is required")

// Confirmation is deliberately read from native input, never from a tool's
// reason, an agent paraphrase, a skill body, history or an attachment.
// Ambiguous questions, conditions and quoted examples do not grant authority.
var publishEnglish = regexp.MustCompile(`\b(confirm|approve|authorize|go ahead|please|directly|now|immediately)\b.{0,100}\b(publish|release)\b|\b(publish|release)\b.{0,100}\b(now|directly|immediately|this skill|the skill|this candidate|the candidate)\b`)
var nonDirectiveEnglish = regexp.MustCompile(`\b(don't|do not|not|never|should not|must not|cannot|can't|if|unless|after|once|whether|example|quote|says?|said|support|implement|feature|logic|how|why|would|could|should|later|tomorrow|wait)\b`)
var publicationWords = regexp.MustCompile(`[a-z0-9][a-z0-9_-]*`)
var genericChinesePublication = regexp.MustCompile(`(?:确认|同意|允许|授权|直接|立即|现在|马上|请|可以)?发布(?:(?:这个|该|当前|最新|此|本)?(?:技能|候选|版本))?(?:吧|了|即可|就行)?(?:[\s，。！,.!]|$)`)
var quotedPublicationText = regexp.MustCompile("(?s)```.*?```|~~~.*?~~~|`[^`]*`|\"[^\"]*\"|“[^”]*”|‘[^’]*’|「[^」]*」|『[^』]*』")

func publicationDirective(input []harness.Content) string {
	var parts []string
	for _, c := range input {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	raw := strings.TrimSpace(strings.Join(parts, "\n"))
	if raw == "" || len(raw) > 32<<10 || !bounded(raw, 32<<10) {
		return ""
	}
	// An unclosed code/quote block is not a user instruction either.
	clean := quotedPublicationText.ReplaceAllString(raw, " quoted_publication_context ")
	if strings.Contains(clean, "quoted_publication_context") {
		return ""
	}
	if strings.ContainsAny(clean, "`\"“”‘’「」『』?？") || strings.Contains(clean, "~~~") {
		return ""
	}
	var lines []string
	for _, line := range strings.Split(clean, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, ">") {
			lines = append(lines, line)
		}
	}
	clean = strings.ToLower(strings.Join(lines, "\n"))
	for _, denied := range []string{"不要", "请勿", "暂不", "暂时不", "先别", "别发布", "不发布", "不能", "不可", "不允许", "不确认", "没确认", "不同意", "不应", "不直接", "不希望", "不想", "不打算", "不是", "并非", "并不", "禁止", "无需发布", "不用发布", "不用直接", "无需直接", "取消发布", "如果", "假如", "只有", "之后", "通过后", "完成后", "是否", "能否", "可以吗", "如何", "怎么", "为什么", "例如", "比如", "示例", "原话", "用户说", "用户确认", "支持", "功能", "逻辑", "解释", "翻译", "稍后", "明天", "先验证", "先测试"} {
		if strings.Contains(clean, denied) {
			return ""
		}
	}
	if nonDirectiveEnglish.MatchString(clean) {
		return ""
	}
	for _, positive := range []string{"确认发布", "同意发布", "允许发布", "授权发布", "直接发布", "立即发布", "现在发布", "马上发布", "请发布", "可以发布", "发布吧", "发布即可", "发布这个技能", "发布该技能", "发布这个候选", "发布当前候选", "发布最新候选"} {
		if strings.Contains(clean, positive) {
			return clean
		}
	}
	if publishEnglish.MatchString(clean) {
		return clean
	}
	return ""
}

type ConfirmedPublicationRequest struct {
	Run                  harness.RunRequest
	Author               string
	CandidateVersion     string
	ExpectedStateVersion int64
}

func (s *Service) userConfirmationTx(ctx context.Context, tx *sql.Tx, p Proposal, req ConfirmedPublicationRequest) (*harness.SkillUserConfirmation, error) {
	var sid, inputID, workspace, root, status string
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT r.session_id,r.input_id,s.cwd,m.root_id,r.status,i.content FROM harness_runs r
 JOIN harness_sessions s ON s.id=r.session_id JOIN harness_inputs i ON i.id=r.input_id AND i.run_id=r.id
 JOIN budget_members m ON m.id=r.id AND m.session_id=r.session_id WHERE r.id=?`, req.Run.RunID).Scan(&sid, &inputID, &workspace, &root, &status, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserConfirmation
	}
	if err != nil {
		return nil, err
	}
	if status != "running" || root != req.Run.RunID || sid != req.Run.Session.ID || inputID != req.Run.InputID ||
		!session.SameWorkspace(workspace, req.Run.Session.CWD) || len(raw) > 2<<20 ||
		p.Author != req.Author || p.Trigger.Type != "explicit_agent_proposal" || p.Trigger.ThreadID != sid ||
		p.Target.Scope == harness.SkillScopeWorkspace && !session.SameWorkspace(workspace, p.Target.Workspace) {
		return nil, harness.ErrPermissionDenied
	}
	historical, err := skillmaintenance.HistoricalRootTx(ctx, tx, root)
	if err != nil {
		return nil, err
	}
	if historical {
		return nil, harness.ErrPermissionDenied
	}
	var input []harness.Content
	if json.Unmarshal(raw, &input) != nil {
		return nil, ErrUserConfirmation
	}
	directive := publicationDirective(input)
	if directive == "" {
		return nil, ErrUserConfirmation
	}
	if !strings.Contains(directive, strings.ToLower(p.ID)) && !strings.Contains(directive, strings.ToLower(p.SkillName)) && !genericChinesePublication.MatchString(directive) && !publishEnglish.MatchString(directive) {
		return nil, ErrUserConfirmation
	}
	// Unknown identifiers must not be silently resolved to an unrelated sole
	// candidate. Skill names and proposal IDs are canonical ASCII identifiers.
	allowedWords := " i we you my our your the a an this that current latest skill skills candidate version publish publication release confirm confirmed approve approved authorize authorized allow please now directly immediately go ahead and yes ok okay skip bypass evidence gate gates validation verification tests testing test without waiting for no need to is are insufficient missing accept risk understand have read reviewed changes change it already do can "
	for _, word := range publicationWords.FindAllString(directive, -1) {
		if word != strings.ToLower(p.ID) && word != strings.ToLower(p.SkillName) && !strings.Contains(allowedWords, " "+word+" ") {
			return nil, ErrUserConfirmation
		}
	}
	// Bind the native submission too: a copied candidate cannot acquire a
	// different session/workspace's authorization at publication time.
	if _, err = submissionOriginTx(ctx, tx, p, workspace); err != nil {
		return nil, err
	}
	// Generic confirmation refers to the newest candidate for one skill. An
	// explicit proposal ID may select an older revision; a bare confirmation
	// must not let the model choose V1 when the user was shown V2.
	if !strings.Contains(directive, strings.ToLower(p.ID)) {
		rows, e := tx.QueryContext(ctx, `SELECT id,name FROM df_skill_proposals WHERE
 json_extract(payload,'$.author')=? AND json_extract(payload,'$.trigger.thread_id')=?
 AND json_extract(payload,'$.trigger.type')='explicit_agent_proposal' ORDER BY rowid DESC`, req.Author, sid)
		if e != nil {
			return nil, e
		}
		defer rows.Close()
		latest := map[string]string{}
		for rows.Next() {
			var id, name string
			if e = rows.Scan(&id, &name); e != nil {
				return nil, e
			}
			if _, exists := latest[name]; !exists {
				latest[name] = id
			}
		}
		if e = rows.Err(); e != nil {
			return nil, e
		}
		if latest[p.SkillName] != p.ID || len(latest) > 1 && !strings.Contains(directive, strings.ToLower(p.SkillName)) {
			return nil, ErrUserConfirmation
		}
		for name, id := range latest {
			if name != p.SkillName && (strings.Contains(directive, strings.ToLower(name)) || strings.Contains(directive, strings.ToLower(id))) {
				return nil, ErrUserConfirmation
			}
		}
	}
	return &harness.SkillUserConfirmation{SessionID: sid, RunID: req.Run.RunID, InputID: inputID, InputSHA: hash(raw), Workspace: workspace, RuleVersion: confirmationRule}, nil
}
