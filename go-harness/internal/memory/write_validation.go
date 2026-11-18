package memory

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"golang.org/x/text/unicode/norm"
)

const WritePolicy = "memory-write-v2"
const ExplicitPolicyV2 = "personal-explicit-v2"
const ExtractionPolicyV2 = "personal-extraction-v2"

type WriteRequest struct {
	Action, Key, FactID, Category, Value, Content, Evidence string
	EvidenceStart, EvidenceEnd                              *int
}

type ValidatedWrite struct {
	Key, Category, Value, Content string
	proof                         *writeProof
}

type ValidationError struct{ Reason string }

func (e *ValidationError) Error() string { return "memory write rejected: " + e.Reason }
func (e *ValidationError) Unwrap() error { return harness.ErrInvalidInput }
func rejectWrite(reason string) error    { return &ValidationError{reason} }

// Evidence positions count Unicode code points in the original accepted text,
// before trimming or template normalization. A quote must be a complete clause.
func evidenceClause(source string, in WriteRequest) (string, error) {
	if !utf8.ValidString(source) || len(source) > 8192 || in.EvidenceStart == nil || in.EvidenceEnd == nil {
		return "", rejectWrite("evidence_position")
	}
	runes := []rune(source)
	start, end := *in.EvidenceStart, *in.EvidenceEnd
	if start < 0 || end <= start || end > len(runes) || string(runes[start:end]) != in.Evidence {
		return "", rejectWrite("evidence_position")
	}
	boundary := func(r rune) bool { return strings.ContainsRune("\r\n。.!！,，;；", r) }
	left, right := start-1, end
	// A model may quote only the factual body. Recover an explicit command
	// only when it is the entire immediately adjacent clause prefix in the
	// accepted source. A remember request elsewhere cannot authorize this fact.
	prefixStart := start
	for prefixStart > 0 && !strings.ContainsRune("\r\n。.!！;；", runes[prefixStart-1]) {
		prefixStart--
	}
	prefix := strings.TrimSpace(string(runes[prefixStart:start]))
	explicitPrefix := prefix != "" && rememberPrefix.ReplaceAllString(prefix, "") == ""
	if explicitPrefix {
		left = prefixStart - 1
	}
	for left >= 0 && unicode.IsSpace(runes[left]) {
		left--
	}
	for right < len(runes) && unicode.IsSpace(runes[right]) {
		right++
	}
	if left >= 0 && !boundary(runes[left]) || right < len(runes) && !boundary(runes[right]) && !boundary(runes[end-1]) {
		return "", rejectWrite("incomplete_declaration")
	}
	if unsafeDeclaration(source) {
		return "", rejectWrite("quoted_ambiguous_instruction_or_secret")
	}
	clause := strings.TrimSpace(in.Evidence)
	if clause == "" || strings.ContainsAny(clause, "\r\n") {
		return "", rejectWrite("incomplete_declaration")
	}
	if explicitPrefix {
		clause = prefix + " " + clause
	}
	return strings.TrimRight(clause, "。.!！"), nil
}

func unsafeDeclaration(text string) bool {
	lower := strings.ToLower(text)
	if strings.ContainsAny(text, "\"“”<>") || strings.ContainsRune(text, '`') || strings.ContainsRune(text, utf8.RuneError) || credentialLike.MatchString(text) {
		return true
	}
	runes := []rune(text)
	for i, r := range runes {
		if r == '\'' || r == '‘' || r == '’' {
			// Contractions and possessives are not quoted speech.
			if i == 0 || i+1 == len(runes) || !unicode.IsLetter(runes[i-1]) || !unicode.IsLetter(runes[i+1]) {
				return true
			}
		}
	}
	for _, marker := range []string{
		"他说", "她说", "网页", "文档说", "示例", "引用", "假设", "如果", "并非", "不是", "不在", "不喜欢",
		"忽略", "安全检查", "授权", "批准", "权限", "允许", "无需", "跳过", "密码", "密钥", "令牌", "凭据",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	// Keep long-range attribution/conditional checks across the WHOLE message.
	// A separate trailing diagnostic request has no scope over a prior fact.
	// Only this finite sentence form is excluded, never arbitrary suffix text.
	context := trailingBuildCheck.ReplaceAllString(lower, "")
	return unsafeEnglishContext.MatchString(context) || unsafeEnglishAuthority.MatchString(lower)
}

var trailingBuildCheck = regexp.MustCompile(`(?i)[.!]\s*(?:please )?check if the build (?:passes|passed|succeeds|succeeded|fails|failed)[.!]?\s*$`)
var unsafeEnglishContext = regexp.MustCompile(`(?i)\b(?:says|said|examples?|quotes?|quoted|suppose|supposing|if|unless|not|don't|ignore)\b`)
var unsafeEnglishAuthority = regexp.MustCompile(`(?i)(?:\b(?:permissions?|approv\w*|authoriz\w*|without confirmation|passwords?|credentials?|api key|secrets?)\b|token\s*=)`)

var rememberPrefix = regexp.MustCompile(`(?i)^(?:(?:请)?(?:记住|记下|更正)(?:一下)?[，,:： ]*|(?:please )?(?:remember|correct)(?: that)?[,: ]+)`)
var nameStatement = regexp.MustCompile(`(?i)^(?:我叫|我的(?:名字|姓名)是|my name is\s+)([\p{L} .-]{1,40})$`)
var cityStatement = regexp.MustCompile(`(?i)^(?:我(?:目前|现在)?(?:住在|在)|我的(?:所在地|城市)是|i (?:live in|am in)\s+)([\p{L}\p{N} .-]{1,48})$`)
var bareLocationStatement = regexp.MustCompile(`(?i)^(?:我(?:目前|现在)?在|i am in\s+)`)

// Bare 在/in is ambiguous with activities. Support a documented finite set;
// other place names require the explicitly typed 住在/城市是/live in grammar.
var bareLocations = strings.Fields("北京 上海 杭州 深圳 广州 南京 成都 武汉 西安 天津 重庆 苏州 厦门 香港 澳门 台北 新加坡 东京 伦敦 巴黎 柏林 悉尼 纽约 beijing shanghai hangzhou shenzhen guangzhou london paris berlin singapore tokyo sydney")
var languageStatement = regexp.MustCompile(`(?i)^(?:(?:以后)?(?:请)?用(中文|英文|英语)回答|我偏好(?:用|使用)?(中文|英文|英语)(?:回答|回复)|(?:please )?answer in (Chinese|English)|i prefer (Chinese|English) answers)$`)
var styleStatement = regexp.MustCompile(`(?i)^(?:(?:from now on, )?(?:please )?answer concisely in (Chinese|English)|continue to answer concisely in (Chinese|English))$`)
var hardwareStatement = regexp.MustCompile(`(?i)^(?:我的硬件是\s*|my hardware is\s+)([A-Za-z0-9._-]{1,80})$`)
var databaseStatement = regexp.MustCompile(`(?i)^(?:项目数据库(?:采用|使用)\s*|the project database is\s+)([A-Za-z0-9._-]{1,80})$`)
var pathStatement = regexp.MustCompile(`(?i)^(?:项目路径是\s*|the project path is\s+)([A-Za-z0-9_./\\:-]{1,240})$`)

// Generic keys are still constrained to known descriptive sentence forms;
// arbitrary text following "I prefer" is not a supported fact language.
var genericStatement = regexp.MustCompile(`(?i)^(?:我(?:喜欢|偏好)(?:茶|咖啡|牛奶|水|红色|蓝色|绿色|黑色|白色)|i (?:like|prefer) (?:tea|coffee|milk|water|red|blue|green|black|white)|我的编辑器是 ?(?:VS Code|Vim|Emacs)|my editor is (?:VS Code|Vim|Emacs)|项目(?:采用|使用) ?(?:Go|Python|Java|TypeScript|Rust|SQLite|PostgreSQL)|the project uses (?:Go|Python|Java|TypeScript|Rust|SQLite|PostgreSQL))$`)

func captured(pattern *regexp.Regexp, text string) string {
	match := pattern.FindStringSubmatch(text)
	if len(match) == 0 {
		return ""
	}
	for _, part := range match[1:] {
		if part != "" {
			return strings.TrimSpace(part)
		}
	}
	return ""
}

func canonicalDeclaration(key, clause string) (value, content string, err error) {
	body := rememberPrefix.ReplaceAllString(clause, "")
	explicit := body != clause
	switch key {
	case "profile.name":
		value = captured(nameStatement, body)
		content = "用户姓名是" + value + "。"
	case "profile.last_reported_location":
		value = captured(cityStatement, body)
		if bareLocationStatement.MatchString(body) {
			known := false
			for _, place := range bareLocations {
				known = known || strings.EqualFold(norm.NFKC.String(value), place)
			}
			if !known {
				value = ""
			}
		}
		content = "用户最近告知所在地是" + value + "。"
	case "preference.language":
		value = captured(languageStatement, body)
		language := value
		switch strings.ToLower(value) {
		case "chinese":
			language = "中文"
		case "english", "英文", "英语":
			language = "英语"
		}
		content = "用户偏好使用" + language + "回答。"
	case "preference.answer_style":
		value = captured(styleStatement, body)
		language := strings.ToUpper(value[:min(1, len(value))]) + value[min(1, len(value)):]
		content = "User prefers concise " + language + " answers"
	case "profile.hardware":
		value = captured(hardwareStatement, body)
		content = "The user's hardware is " + value + "."
	case "project.database":
		value = captured(databaseStatement, body)
		content = "项目数据库采用 " + value + "。"
	case "project.path":
		if explicit {
			value = captured(pathStatement, body)
		}
		content = "项目路径是 " + value + "。"
	default:
		if explicit && len([]rune(body)) <= 160 && genericStatement.MatchString(body) {
			category := "profile"
			lower := strings.ToLower(body)
			if strings.HasPrefix(body, "我喜欢") || strings.HasPrefix(body, "我偏好") || strings.HasPrefix(lower, "i like ") || strings.HasPrefix(lower, "i prefer ") {
				category = "preference"
			}
			if strings.HasPrefix(body, "项目") || strings.HasPrefix(lower, "the project ") {
				category = "project"
			}
			if strings.HasPrefix(key, category+".") {
				value, content = body, body+"。"
			}
		}
	}
	if value == "" {
		return "", "", rejectWrite("unsupported_declaration")
	}
	return value, content, nil
}

func ValidateWrite(source string, in WriteRequest, automatic bool) (ValidatedWrite, error) {
	key := CanonicalKey(in.Key)
	if in.Action != "remember" || in.FactID != "" || !slotPattern.MatchString(key) ||
		(in.Category != "profile" && in.Category != "preference" && in.Category != "project") ||
		!strings.HasPrefix(key, in.Category+".") {
		return ValidatedWrite{}, rejectWrite("key_category")
	}
	clause, err := evidenceClause(source, in)
	if err != nil {
		return ValidatedWrite{}, err
	}
	if automatic {
		lower := strings.ToLower(source)
		for _, marker := range []string{"忘记", "忘掉", "删除", "forget ", "delete "} {
			if strings.Contains(lower, marker) {
				return ValidatedWrite{}, rejectWrite("forget_request")
			}
		}
	}
	value, content, err := canonicalDeclaration(key, clause)
	if err != nil {
		return ValidatedWrite{}, err
	}
	if in.Value != value || in.Content != "" && in.Content != content {
		return ValidatedWrite{}, rejectWrite("unsupported_value_or_content")
	}
	if automatic && (strings.ContainsAny(value, "/\\<>") || strings.Contains(value, "://") || len(UngroundedActionableTokens(value, "")) > 0) {
		return ValidatedWrite{}, rejectWrite("automatic_path_or_markup")
	}
	v := ValidatedWrite{Key: key, Category: in.Category, Value: value, Content: content}
	v.proof = sealWrite(source, clause, v, automatic)
	return v, nil
}

// Forget binds the user's complete request to a concrete host-resolved target.
// An ID from search is never itself proof that the user requested its removal.
func ValidateForget(source string, in WriteRequest, target Fact, targetKey string) error {
	if in.Action != "forget" || (in.Key == "") == (in.FactID == "") || in.Content != "" || in.Value != "" {
		return rejectWrite("forget_target")
	}
	clause, err := evidenceClause(source, in)
	if err != nil {
		return err
	}
	requested := captured(forgetStatement, clause)
	if requested == "" {
		return rejectWrite("forget_request")
	}
	if in.FactID != "" && requested == in.FactID {
		return nil
	}
	key := CanonicalKey(targetKey)
	if key == "" {
		for _, entry := range []struct{ prefix, key string }{
			{"用户最近告知所在地是", "profile.last_reported_location"}, {"用户姓名是", "profile.name"},
			{"The user's hardware is ", "profile.hardware"}, {"用户偏好使用", "preference.language"},
		} {
			if strings.HasPrefix(target.Content, entry.prefix) {
				key = entry.key
				break
			}
		}
	}
	aliases := map[string][]string{
		"profile.last_reported_location": {"所在地", "城市", "位置", "location", "city"},
		"profile.name":                   {"姓名", "名字", "name"}, "profile.hardware": {"硬件", "hardware"},
		"preference.language":     {"语言偏好", "语言", "language", "language preference"},
		"preference.answer_style": {"回答风格", "answer style"},
		"project.path":            {"项目路径", "project path"}, "project.database": {"项目数据库", "project database"},
	}
	// Strip possessives only for attribute aliases, not for a complete fact
	// (我喜欢茶 / I like tea). Full facts must match the target exactly.
	attribute := forgetAttributePrefix.ReplaceAllString(requested, "")
	for _, alias := range append(aliases[key], key) {
		if alias != "" && strings.EqualFold(attribute, alias) {
			return nil
		}
	}
	if target.Content != "" && requested == strings.TrimRight(target.Content, "。.!！") {
		return nil
	}
	return rejectWrite("forget_target")
}

var forgetStatement = regexp.MustCompile(`(?i)^(?:(?:请)?(?:忘记|忘掉|删除)\s*|(?:please )?(?:forget|delete)\s+(?:that\s+)?)(.+)$`)
var forgetAttributePrefix = regexp.MustCompile(`(?i)^(?:(?:我)?(?:之前保存的|保存的|的)|my\s+|the\s+)`)

// Legacy callers get the same deterministic gate; actionable-token checks alone
// are not evidence that arbitrary text follows from a user's statement.
func validateLegacyContent(content, evidence, source string) error {
	startByte := strings.Index(source, evidence)
	if startByte < 0 {
		return rejectWrite("evidence_position")
	}
	start, end := len([]rune(source[:startByte])), len([]rune(source[:startByte+len(evidence)]))
	in := WriteRequest{Action: "remember", Content: content, Evidence: evidence, EvidenceStart: &start, EvidenceEnd: &end}
	for _, key := range []string{"profile.name", "profile.last_reported_location", "preference.language", "preference.answer_style", "profile.hardware", "project.database", "project.path", "preference.other"} {
		in.Key, in.Category = key, strings.SplitN(key, ".", 2)[0]
		clause, err := evidenceClause(source, in)
		if err != nil {
			return err
		}
		value, _, err := canonicalDeclaration(key, clause)
		if err != nil {
			continue
		}
		in.Value = value
		if _, err = ValidateWrite(source, in, false); err == nil {
			return nil
		}
	}
	return fmt.Errorf("%w: external content cannot establish a supported memory value", harness.ErrInvalidInput)
}
