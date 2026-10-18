package evolution

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

var markdownLink = regexp.MustCompile(`\]\(([^\s)]+)(?:\s+"[^"]*")?\)`)

func validateFiles(files map[string][]byte) error {
	if len(files) == 0 || len(files) > 20 {
		return invalid("candidate requires 1..20 files")
	}
	if _, _, err := skills.ParseRuntimeManifest(files); err != nil {
		return err
	}
	total := 0
	for name, data := range files {
		total += len(data)
		if len(data) > 128<<10 || total > 512<<10 {
			return invalid("candidate exceeds configured delivery size bounds")
		}
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			return invalid("candidate files require reviewable UTF-8 content")
		}
		switch strings.ToLower(path.Ext(name)) {
		case ".pyc", ".pyo", ".pyd", ".whl", ".pyz":
			return invalid("compiled Python artifacts are not reviewable source files")
		}
		for _, part := range strings.Split(strings.ToLower(name), "/") {
			if part == ".venv" || part == "site-packages" || part == "__pycache__" {
				return invalid("runtime environments and caches must remain outside the skill package")
			}
		}
		if !strings.EqualFold(path.Ext(name), ".md") {
			continue
		}
		for _, match := range markdownLink.FindAllSubmatch(data, -1) {
			u, err := url.Parse(string(match[1]))
			if err != nil {
				return invalid("candidate contains an invalid reference")
			}
			if u.IsAbs() || u.Host != "" {
				if u.Scheme != "" && u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "mailto" {
					return invalid("unsupported reference scheme")
				}
				continue
			}
			if u.Path == "" {
				continue
			}
			relative := path.Clean(path.Join(path.Dir(name), u.Path))
			if strings.HasPrefix(u.Path, "/") || relative == ".." || strings.HasPrefix(relative, "../") || strings.ContainsAny(relative, "\\:\x00") {
				return invalid("reference escapes the candidate package")
			}
			if _, exists := files[relative]; !exists {
				return invalid("candidate has a missing local reference")
			}
		}
	}
	return nil
}

func containsScripts(files map[string][]byte) bool {
	for name, data := range files {
		if bytes.HasPrefix(bytes.TrimSpace(data), []byte("#!")) {
			return true
		}
		switch strings.ToLower(path.Ext(name)) {
		case ".py", ".pyw", ".sh", ".bash", ".zsh", ".ps1", ".cmd", ".bat", ".js", ".mjs", ".cjs", ".ts", ".go", ".rs", ".rb", ".pl", ".exe", ".dll", ".so":
			return true
		}
	}
	return false
}

// Whole-file unified hunks keep this initial reviewer deterministic and bounded.
// Both paths and package contents have already passed structural validation.
func packageDiff(before, after map[string][]byte) ([]string, string, error) {
	paths := make(map[string]bool)
	for name := range before {
		paths[name] = true
	}
	for name := range after {
		paths[name] = true
	}
	var names []string
	for name := range paths {
		names = append(names, name)
	}
	sort.Strings(names)
	var changed []string
	var out strings.Builder
	for _, name := range names {
		a, aExists := before[name]
		b, bExists := after[name]
		if aExists == bExists && bytes.Equal(a, b) {
			continue
		}
		changed = append(changed, name)
		left, right := "a/"+name, "b/"+name
		if !aExists {
			left = "/dev/null"
		}
		if !bExists {
			right = "/dev/null"
		}
		if !utf8.Valid(a) || !utf8.Valid(b) || bytes.IndexByte(a, 0) >= 0 || bytes.IndexByte(b, 0) >= 0 {
			fmt.Fprintf(&out, "Binary files %s (sha256 %s) and %s (sha256 %s) differ\n", left, hash(a), right, hash(b))
			continue
		}
		lines := func(data []byte) []string {
			if len(data) == 0 {
				return nil
			}
			return strings.Split(strings.TrimSuffix(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n"), "\n")
		}
		old, updated := lines(a), lines(b)
		aStart, bStart := 1, 1
		if len(old) == 0 {
			aStart = 0
		}
		if len(updated) == 0 {
			bStart = 0
		}
		fmt.Fprintf(&out, "--- %s\n+++ %s\n@@ -%d,%d +%d,%d @@\n", left, right, aStart, len(old), bStart, len(updated))
		for _, line := range old {
			out.WriteString("-" + line + "\n")
		}
		if len(a) > 0 && a[len(a)-1] != '\n' {
			out.WriteString("\\ No newline at end of file\n")
		}
		for _, line := range updated {
			out.WriteString("+" + line + "\n")
		}
		if len(b) > 0 && b[len(b)-1] != '\n' {
			out.WriteString("\\ No newline at end of file\n")
		}
		if out.Len() > 2<<20 {
			return nil, "", invalid("review diff exceeds 2 MiB")
		}
	}
	return changed, out.String(), nil
}

func (s *Service) Submit(ctx context.Context, request Request) (Proposal, error) {
	return s.submit(ctx, request, true)
}

func (s *Service) submit(ctx context.Context, request Request, validate bool) (Proposal, error) {
	if request.RequestKey == "" || !bounded(request.RequestKey, 256) || !bounded(request.Reason, 4000) || !bounded(request.Author, 128) || !bounded(request.Trigger.Summary, 4000) || !bounded(request.Trigger.ThreadID, 256) || !bounded(request.Trigger.RunID, 256) || !bounded(request.Trigger.Type, 128) {
		return Proposal{}, invalid("candidate request fields exceed bounds")
	}
	if request.Origin == "" {
		request.Origin = "manual_agent"
	}
	if request.Author == "" {
		request.Author = "agent"
	}
	if request.Origin != "manual_agent" && request.Origin != "automatic" {
		return Proposal{}, invalid("unknown proposal origin")
	}
	request, err := s.materialize(ctx, request)
	if err != nil {
		return Proposal{}, err
	}
	var candidate *skills.Candidate
	if request.Action != "delete" {
		if err = validateFiles(request.Files); err != nil {
			return Proposal{}, err
		}
		candidate, err = s.registry.PrepareCandidate(request.Name, request.Files)
		if err != nil {
			return Proposal{}, err
		}
	} else if len(request.Files) != 0 {
		return Proposal{}, invalid("delete must not include candidate files")
	}
	var candidateVersion string
	if candidate != nil {
		candidateVersion = candidate.Version()
	}
	requestSHA := requestHash(request, candidateVersion)
	var p Proposal
	existing := false
	queued := false
	var preflightErr error
	err = s.registry.WithPublication(ctx, request.SourceID, request.Name, func(tx *skills.PublicationTx) error {
		if candidate != nil {
			if err := s.guardSubmittedCredentials(ctx, tx, candidate); err != nil {
				return err
			}
		}
		var priorID, sha string
		err := tx.SQL().QueryRowContext(ctx, `SELECT id,request_sha FROM df_skill_proposals WHERE source_key=? AND name=? AND request_key=?`, tx.Target().SourceIdentity, request.Name, request.RequestKey).Scan(&priorID, &sha)
		if err == nil {
			if sha != requestSHA {
				return ErrConflict
			}
			existing = true
			p, err = readProposal(ctx, tx.SQL(), priorID)
			if err != nil {
				return err
			}
			return s.attachGenerationProposal(ctx, tx, p)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		head, err := tx.Current()
		if err != nil {
			return err
		}
		if head != request.Base {
			return ErrConflict
		}
		if (request.Action == "create") == head.Exists {
			return invalid("candidate action does not match the installed base")
		}
		if request.Action == "create" {
			if err = tx.GuardCreate(); err != nil {
				return err
			}
			_, preflightErr = tx.CheckCreateTarget(candidateVersion)
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		var baseFiles map[string][]byte
		if head.Exists {
			base, err := tx.ReadVersion(head.Version)
			if err != nil {
				return err
			}
			baseFiles = base.Files()
		}
		var files map[string][]byte
		if candidate != nil {
			files = candidate.Files()
		}
		changed, diff, err := packageDiff(baseFiles, files)
		if err != nil {
			return err
		}
		if len(changed) == 0 {
			return invalid("candidate has no changes")
		}
		var pending int
		if err = tx.SQL().QueryRowContext(ctx, `SELECT count(*) FROM df_skill_proposals WHERE status IN ('validating','pending_review')`).Scan(&pending); err != nil {
			return err
		}
		if pending >= 100 {
			return invalid("pending proposal limit reached")
		}
		now := time.Now().UTC()
		p = Proposal{ID: newID(), Status: "validating", StateVersion: 1, Action: request.Action, SkillName: request.Name, Target: tx.Target(), Base: head, Reason: request.Reason, Trigger: request.Trigger, Origin: request.Origin, Author: request.Author, ChangedFiles: changed, Diff: diff, CreatedAt: now, UpdatedAt: now}
		p.ReviewRetryOf, _ = ctx.Value(reviewRetryKey{}).(string)
		p.OverlayBase, err = tx.OverlayBase()
		if err != nil {
			return err
		}
		if candidate != nil {
			p.CandidateVersion, err = tx.Stage(candidate)
			if err != nil {
				return err
			}
		}
		if preflightErr != nil {
			p.Status, p.Error, p.PublicationError = "stale", "The target directory could not accept this candidate; review its current complete package.", "publication_conflict"
			var conflict *skills.PublicationConflict
			if errors.As(preflightErr, &conflict) {
				p.PublicationError = conflict.Code
			}
			p.Evaluation = Report{BaseVersion: p.Base.Version, CandidateVersion: p.CandidateVersion, Security: "unavailable", Behavior: "not_run"}
			p.EvaluationDigest = encodedHash(p.Evaluation)
		}
		data, err := json.Marshal(p)
		if err != nil {
			return err
		}
		// Reserve room for the bounded report and review fields so every saved
		// proposal remains fetchable over the desktop's management channel.
		if len(data) > (3<<20)-(128<<10) {
			return invalid("encoded review exceeds management bounds")
		}
		_, err = tx.SQL().ExecContext(ctx, `INSERT INTO df_skill_proposals VALUES(?,?,?,?,?,?,?,?)`, p.ID, p.Target.SourceIdentity, p.SkillName, request.RequestKey, requestSHA, p.StateVersion, p.Status, data)
		if err != nil {
			return err
		}
		if err = s.attachGenerationProposal(ctx, tx, p); err != nil {
			return err
		}
		if preflightErr != nil {
			return nil
		}
		queued, err = s.queueBehaviorManual(ctx, tx, &p)
		return err
	})
	if err == nil && preflightErr != nil {
		return p, preflightErr
	}
	if err != nil || existing || !validate || queued {
		return p, err
	}
	validated, err := s.Validate(ctx, p.ID, p.StateVersion)
	if err != nil {
		return p, err
	}
	return validated, nil
}

// Validate can resume a persisted validating proposal after a crash. Duplicate
// submissions merely return it, so retry cannot start unbounded model checks.
func (s *Service) Validate(ctx context.Context, id string, expected int64) (Proposal, error) {
	var input ValidationInput
	err := s.withProposal(ctx, id, func(tx *skills.PublicationTx, p *Proposal) error {
		if err := s.guardGenerationValidation(ctx, tx, *p); err != nil {
			return err
		}
		if p.StateVersion != expected || p.Status != "validating" {
			return ErrConflict
		}
		input.Proposal = *p
		overlay, err := tx.OverlayBase()
		if err != nil {
			return err
		}
		if !skills.SameOverlayBase(overlay, p.OverlayBase) {
			return ErrConflict
		}
		if p.Base.Exists {
			c, err := tx.ReadVersion(p.Base.Version)
			if err != nil {
				return err
			}
			input.BaseFiles = c.Files()
		}
		if p.CandidateVersion != "" {
			c, err := tx.ReadVersion(p.CandidateVersion)
			if err != nil {
				return err
			}
			input.CandidateFiles = c.Files()
		}
		return nil
	})
	if err != nil {
		return Proposal{}, err
	}
	report := Report{BaseVersion: input.Proposal.Base.Version, CandidateVersion: input.Proposal.CandidateVersion, Security: "unavailable", Behavior: "not_run"}
	var reviewErr error
	cached, managed, err := s.behaviorContent(ctx, input)
	if err != nil {
		return Proposal{}, err
	}
	if cached != nil {
		report = *cached
	} else if s.validator != nil {
		report, reviewErr = s.validator.Validate(ctx, input)
	}
	if managed {
		// A content reviewer cannot mint native behavior evidence. Legacy SDK
		// validators keep their prior contract only outside native evaluation.
		report.Behavior = "not_run"
		report.BehaviorReport = nil
		report.BehaviorDigest = ""
		report.IsolatedScripts = false
		if reviewErr == nil && cached == nil {
			if err = s.saveBehaviorContent(ctx, input, report); err != nil {
				return Proposal{}, err
			}
		}
		if reviewErr == nil && validReportFor(report, input) {
			report, reviewErr = s.runBehavior(ctx, input, report)
		}
	} else {
		// Preserve the legacy SDK validator contract, but reserve native
		// provenance fields for the maintenance worker's persisted evidence.
		report.BehaviorReport = nil
		report.BehaviorDigest = ""
	}
	valid := reviewErr == nil && validReportFor(report, input)
	generationID, _ := ctx.Value(generationContextKey{}).(string)
	if generationID != "" && (s.generation == nil || report.ValidatorVersion != s.generation.reviewIdentity) {
		valid = false
	}
	var failure *harness.SkillValidationFailure
	if !valid {
		failure = validationFailure(report, input, reviewErr, s.validator != nil || cached != nil)
	}
	if !reportBoundTo(report, input) {
		report = Report{BaseVersion: input.Proposal.Base.Version, CandidateVersion: input.Proposal.CandidateVersion, Security: "unavailable", Behavior: "not_run"}
	}
	var result Proposal
	err = s.withProposal(ctx, id, func(tx *skills.PublicationTx, p *Proposal) error {
		if err := s.guardGenerationValidation(ctx, tx, *p); err != nil {
			return err
		}
		if reviewErr == nil && managed && (valid || report.BehaviorReport != nil) {
			if err := s.guardBehaviorReport(ctx, tx.SQL(), *p, report); err != nil {
				return err
			}
		}
		if p.StateVersion != expected || p.Status != "validating" {
			return ErrConflict
		}
		head, err := tx.Current()
		if err != nil {
			return err
		}
		p.StateVersion++
		p.UpdatedAt = time.Now().UTC()
		p.Evaluation = report
		p.ScriptReviewRequired = requiresScriptAcknowledgment(report, input)
		p.EvaluationDigest = encodedHash(report)
		p.ValidationFailure = nil
		if head != p.Base {
			p.Status = "stale"
			p.Error = "The installed skill changed during validation."
		} else if generationID != "" && reviewErr != nil {
			// Preserve the candidate while the durable job decides whether its
			// remaining attempts and independent budget permit another review.
			p.Status = "validating"
			p.ValidationFailure = failure
			p.Error = "Candidate review could not complete in this maintenance attempt."
		} else if !valid {
			p.Status = "failed"
			p.ValidationFailure = failure
			p.Error = failure.Error()
		} else {
			p.Status = "pending_review"
			p.Error = ""
		}
		if err = updateProposal(ctx, tx.SQL(), *p, expected); err != nil {
			return err
		}
		result = *p
		return nil
	})
	if err == nil && generationID != "" && reviewErr != nil {
		return result, reviewErr
	}
	return result, err
}

func validReportFor(report Report, input ValidationInput) bool {
	for _, check := range report.ModelChecks {
		if !check.Passed {
			return false
		}
	}
	return reportBoundTo(report, input) && report.Security == "passed" && (report.Behavior == "passed" || report.Behavior == "not_run")
}

func requiresScriptAcknowledgment(report Report, input ValidationInput) bool {
	return containsScripts(input.CandidateFiles) && !(report.IsolatedScripts && report.Behavior == "passed")
}

func reportBoundTo(report Report, input ValidationInput) bool {
	if report.ValidatorVersion == "" || !bounded(report.ValidatorVersion, 128) || report.BaseVersion != input.Proposal.Base.Version || report.CandidateVersion != input.Proposal.CandidateVersion {
		return false
	}
	if report.Security != "passed" && report.Security != "failed" && report.Security != "unavailable" {
		return false
	}
	if report.Behavior != "passed" && report.Behavior != "failed" && report.Behavior != "not_run" && report.Behavior != "unavailable" {
		return false
	}
	if report.BehaviorReport == nil {
		if report.BehaviorDigest != "" {
			return false
		}
	} else {
		native := report.BehaviorReport
		if !hexDigest(report.BehaviorDigest) || report.BehaviorDigest != encodedHash(*native) || native.RuleVersion != behaviorRuleVersion && native.RuleVersion != behaviorTriggerRuleVersion || native.BaseVersion != report.BaseVersion || native.CandidateVersion != report.CandidateVersion || native.State != report.Behavior || native.State != "passed" && native.State != "failed" || report.IsolatedScripts {
			return false
		}
	}
	if len(report.ModelChecks) > 8 {
		return false
	}
	for _, check := range report.ModelChecks {
		if !bounded(check.Stage, 128) || !bounded(check.Provider, 128) || !bounded(check.Model, 256) || !bounded(check.Reason, 2000) || !bounded(check.InputHash, 64) || check.InputTokens < 0 || check.OutputTokens < 0 {
			return false
		}
	}
	if len(report.Evidence) > 64 {
		return false
	}
	for _, evidence := range report.Evidence {
		if !bounded(evidence, 4000) {
			return false
		}
	}
	data, err := json.Marshal(report)
	return err == nil && len(data) <= 64<<10
}
