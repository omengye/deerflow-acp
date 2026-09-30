package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/assets"
)

func isToolEvent(kind string) bool {
	switch kind {
	case "tool_start", "tool_execute", "tool_update", "tool_end":
		return true
	}
	return false
}

func (s *Store) appendToolEvent(ctx context.Context, e harness.RunEvent, stores ...*assets.Store) (out harness.RunEvent, returnErr error) {
	var artifacts *assets.Prepared
	if e.Kind == "tool_end" && len(stores) > 0 && stores[0] != nil {
		artifacts = stores[0].TakeArtifacts(e.SessionID, e.RunID, e.ToolCallID)
	}
	committed := false
	if artifacts != nil {
		defer func() { returnErr = errors.Join(returnErr, artifacts.Finish(committed)) }()
	}
	if e.ToolCallID == "" || e.ToolName == "" {
		return e, fmt.Errorf("%w: tool event requires call ID and name", harness.ErrInvalidInput)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return e, err
	}
	defer tx.Rollback()
	attempt, governed := ctx.Value(executionAttemptKey{}).(*executionAttempt)
	if governed {
		if _, err = checkExecutionLease(ctx, tx, attempt.lease); err != nil {
			return e, err
		}
	}
	backgroundAttempt, backgroundGoverned := ctx.Value(backgroundInteractionAttemptKey{}).(*BackgroundInteractionAttempt)
	if backgroundGoverned {
		if err = backgroundAttempt.check(ctx, tx, e.Kind != "tool_execute"); err != nil {
			return e, err
		}
	}
	var receipt harness.ToolReceipt
	if e.Kind == "tool_start" {
		if e.Status != "pending" || !json.Valid(e.Arguments) {
			return e, fmt.Errorf("%w: invalid tool start", harness.ErrInvalidInput)
		}
		var sessionID, status string
		var configVersion int64
		err = tx.QueryRowContext(ctx, `SELECT r.session_id,r.status,COALESCE(c.version,1) FROM harness_runs r LEFT JOIN harness_session_configs c ON c.session_id=r.session_id WHERE r.id=?`, e.RunID).Scan(&sessionID, &status, &configVersion)
		if err != nil {
			return e, err
		}
		if sessionID != e.SessionID || status != "running" {
			return e, harness.ErrReceiptConflict
		}
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_tool_receipts WHERE run_id=? AND tool_call_id=?)`, e.RunID, e.ToolCallID).Scan(&exists); err != nil {
			return e, err
		}
		if exists {
			return e, harness.ErrReceiptConflict
		}
		now := time.Now().UTC()
		receipt = harness.ToolReceipt{SessionID: e.SessionID, RunID: e.RunID, ToolCallID: e.ToolCallID, ToolName: e.ToolName,
			ArgumentsDigest: fmt.Sprintf("%x", sha256.Sum256(e.Arguments)), ArgumentsSummary: summarizeArguments(e.Arguments),
			ConfigVersion: configVersion, Attempt: 1, State: harness.ReceiptPending, Version: 1, CreatedAt: now, UpdatedAt: now}
		data, marshalErr := json.Marshal(receipt)
		if marshalErr != nil {
			return e, marshalErr
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO harness_tool_receipts(run_id,tool_call_id,session_id,state,version,receipt) VALUES(?,?,?,?,?,?)`, receipt.RunID, receipt.ToolCallID, receipt.SessionID, receipt.State, receipt.Version, data)
		if err != nil {
			return e, err
		}
	} else {
		receipt, err = readReceipt(ctx, tx, e.SessionID, e.RunID, e.ToolCallID)
		if err != nil {
			return e, err
		}
		if receipt.ToolName != e.ToolName {
			return e, harness.ErrReceiptConflict
		}
		if backgroundGoverned {
			if err = backgroundAttempt.consumeEventTx(ctx, tx, e, receipt); err != nil {
				return e, err
			}
		}
		if governed {
			grant, hasGrant := attempt.grants[e.ToolCallID]
			consume := hasGrant && (e.Kind == "tool_execute" || e.Kind == "tool_end" && e.Status == "failed" && receipt.State == harness.ReceiptPending && (grant.Decision == harness.RejectOnce || grant.Decision == harness.RejectAlways))
			if consume {
				decision, err := s.ConsumePermissionGrantTx(ctx, tx, attempt.lease, grant, attempt.requests[e.ToolCallID])
				if err != nil {
					return e, err
				}
				if e.Kind == "tool_execute" && decision != harness.AllowOnce && decision != harness.AllowAlways {
					return e, harness.ErrPermissionDenied
				}
			}
		}
		switch e.Kind {
		case "tool_execute":
			if receipt.State != harness.ReceiptPending || e.Status != "in_progress" {
				return e, harness.ErrReceiptConflict
			}
			receipt.State = harness.ReceiptStarted
		case "tool_update":
			if receipt.State != harness.ReceiptStarted || e.Status != "in_progress" {
				return e, harness.ErrReceiptConflict
			}
			receipt.Result, receipt.ResultTruncated = boundedReceiptResult(append(receipt.Result, e.Content...), receipt.ResultTruncated)
		case "tool_end":
			if artifacts != nil && e.Status == "completed" {
				if err = artifacts.ValidateToolContent(e.Content); err != nil {
					return e, err
				}
				if err = artifacts.AttachArtifacts(ctx, tx, e.RunID, e.ToolCallID); err != nil {
					return e, err
				}
				if !artifacts.ToolImages() {
					e.Content = append([]harness.Content(nil), artifacts.Input...)
				}
			}
			switch {
			case e.Status == "completed" && receipt.State == harness.ReceiptStarted:
				receipt.State = harness.ReceiptCompleted
				if len(e.Content) > 0 {
					// Enhanced streams already recorded text and image references in
					// tool_update. Their terminal image list validates the staged
					// assets; it must not erase earlier text evidence.
					if onlyStagedImages(e.Content) && len(receipt.Result) > 0 {
						receipt.Result, receipt.ResultTruncated = boundedReceiptResult(append(receipt.Result, e.Content...), receipt.ResultTruncated)
					} else {
						receipt.Result, receipt.ResultTruncated = boundedReceiptResult(e.Content, false)
					}
				}
			case e.Status == "failed" && receipt.State == harness.ReceiptPending:
				receipt.State = harness.ReceiptNotExecuted
				receipt.Error = eventError(e)
			case e.Status == "failed" && receipt.State == harness.ReceiptStarted:
				receipt.State = failedStartedState(receipt.ToolName)
				// This state is supplied only by the trusted engine after a native
				// tool returns typed evidence of pre-execution rejection or a
				// completed read-only operation. Recovery has no such evidence.
				if e.Receipt != nil && (e.Receipt.State == harness.ReceiptNotExecuted || e.Receipt.State == harness.ReceiptNoEffect) {
					receipt.State = e.Receipt.State
				}
				receipt.Error = eventError(e)
			default:
				return e, harness.ErrReceiptConflict
			}
			if e.Status == "failed" && len(e.Content) > 0 {
				receipt.Result, receipt.ResultTruncated = boundedReceiptResult(append(receipt.Result, e.Content...), receipt.ResultTruncated)
			}
		}
		receipt.Version++
		receipt.UpdatedAt = time.Now().UTC()
		if err = writeReceipt(ctx, tx, receipt); err != nil {
			return e, err
		}
	}
	e.Receipt = &receipt
	e, err = appendEventTx(ctx, tx, e)
	if err != nil {
		return e, err
	}
	if artifacts != nil && e.Status == "completed" {
		var presented []harness.Content
		for _, c := range e.Content {
			if c.Asset != nil && c.Asset.Kind == harness.AssetArtifact {
				presented = append(presented, c)
			}
		}
		if len(presented) > 0 {
			if _, err = appendEventTx(ctx, tx, harness.RunEvent{SessionID: e.SessionID, RunID: e.RunID, Kind: "artifact_presented", ToolCallID: e.ToolCallID, ToolName: e.ToolName, Content: presented}); err != nil {
				return e, err
			}
		}
	}
	err = tx.Commit()
	committed = err == nil && e.Status == "completed"
	return e, err
}

func onlyStagedImages(contents []harness.Content) bool {
	if len(contents) == 0 {
		return false
	}
	for _, c := range contents {
		if c.Type != "image" || c.Asset == nil || c.Asset.Kind != harness.AssetImage {
			return false
		}
	}
	return true
}

type receiptQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readReceipt(ctx context.Context, q receiptQuery, sessionID, runID, callID string) (harness.ToolReceipt, error) {
	var data []byte
	err := q.QueryRowContext(ctx, `SELECT receipt FROM harness_tool_receipts WHERE session_id=? AND run_id=? AND tool_call_id=?`, sessionID, runID, callID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return harness.ToolReceipt{}, harness.ErrNotFound
	}
	if err != nil {
		return harness.ToolReceipt{}, err
	}
	var receipt harness.ToolReceipt
	err = json.Unmarshal(data, &receipt)
	return receipt, err
}

func writeReceipt(ctx context.Context, tx *sql.Tx, r harness.ToolReceipt) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE harness_tool_receipts SET state=?,version=?,receipt=? WHERE run_id=? AND tool_call_id=?`, r.State, r.Version, data, r.RunID, r.ToolCallID)
	return err
}

func appendEventTx(ctx context.Context, tx *sql.Tx, e harness.RunEvent) (harness.RunEvent, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return e, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO harness_events(session_id,run_id,event) VALUES(?,?,?)`, e.SessionID, e.RunID, data)
	if err != nil {
		return e, err
	}
	e.Sequence, err = result.LastInsertId()
	return e, err
}

// No execution is retried during recovery. A start boundary is deliberately
// conservative: a crash immediately after that commit is still uncertain.
func settleOpenReceipts(ctx context.Context, tx *sql.Tx, runID, reason string) error {
	rows, err := tx.QueryContext(ctx, `SELECT receipt FROM harness_tool_receipts WHERE state IN ('pending','started') AND (?='' OR run_id=?) AND (?<>'' OR NOT EXISTS(SELECT 1 FROM harness_runs r WHERE r.id=harness_tool_receipts.run_id AND r.status='waiting_input')) ORDER BY run_id,tool_call_id`, runID, runID, runID)
	if err != nil {
		return err
	}
	var receipts []harness.ToolReceipt
	for rows.Next() {
		var data []byte
		var receipt harness.ToolReceipt
		if err = rows.Scan(&data); err != nil {
			rows.Close()
			return err
		}
		if err = json.Unmarshal(data, &receipt); err != nil {
			rows.Close()
			return err
		}
		receipts = append(receipts, receipt)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	for _, receipt := range receipts {
		if receipt.State == harness.ReceiptPending {
			receipt.State = harness.ReceiptNotExecuted
		} else {
			receipt.State = failedStartedState(receipt.ToolName)
		}
		receipt.Error = reason
		receipt.Version++
		receipt.UpdatedAt = time.Now().UTC()
		if err = writeReceipt(ctx, tx, receipt); err != nil {
			return err
		}
		_, err = appendEventTx(ctx, tx, harness.RunEvent{SessionID: receipt.SessionID, RunID: receipt.RunID, Kind: "tool_end", ToolCallID: receipt.ToolCallID, ToolName: receipt.ToolName, Status: "failed", Content: []harness.Content{{Type: "text", Text: reason}}, Receipt: &receipt})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListToolReceipts(ctx context.Context, sessionID string) ([]harness.ToolReceipt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT receipt FROM harness_tool_receipts WHERE session_id=? ORDER BY run_id,tool_call_id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]harness.ToolReceipt, 0)
	for rows.Next() {
		var data []byte
		var receipt harness.ToolReceipt
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(data, &receipt); err != nil {
			return nil, err
		}
		result = append(result, receipt)
	}
	return result, rows.Err()
}

func (s *Store) requireReconciled(ctx context.Context, sessionID string) error {
	var uncertain bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_tool_receipts WHERE session_id=? AND state='uncertain')`, sessionID).Scan(&uncertain); err != nil {
		return err
	}
	if uncertain {
		return harness.ErrReconciliationRequired
	}
	return nil
}

func (s *Store) validateReceiptPermission(ctx context.Context, p harness.PermissionRequest) error {
	receipt, err := readReceipt(ctx, s.db, p.SessionID, p.RunID, p.ToolCallID)
	if errors.Is(err, harness.ErrNotFound) {
		return harness.ErrReceiptConflict
	}
	if err != nil {
		return err
	}
	if receipt.State != harness.ReceiptPending || receipt.ToolName != p.ToolName || receipt.ConfigVersion != p.ConfigVersion || receipt.ArgumentsDigest != fmt.Sprintf("%x", sha256.Sum256(p.Arguments)) {
		return harness.ErrReceiptConflict
	}
	return nil
}

func (s *Service) ListToolReceipts(ctx context.Context, owner, sessionID string) ([]harness.ToolReceipt, error) {
	ctx, release, err := s.Coordinator.Begin(ctx, sessionID, owner)
	if err != nil {
		return nil, err
	}
	defer release()
	return s.Store.ListToolReceipts(ctx, sessionID)
}

func (s *Service) ReconcileToolReceipt(ctx context.Context, owner, sessionID string, review harness.ToolReconciliation) (harness.ToolReceipt, error) {
	ctx, release, err := s.Coordinator.Begin(ctx, sessionID, owner)
	if err != nil {
		return harness.ToolReceipt{}, err
	}
	defer release()
	if review.RunID == "" || review.ToolCallID == "" || review.ExpectedVersion < 1 || strings.TrimSpace(review.Reviewer) == "" || strings.TrimSpace(review.Note) == "" || len(review.Reviewer) > 256 || len(review.Note) > 16384 || (review.Outcome != harness.ReceiptCompleted && review.Outcome != harness.ReceiptNoEffect) {
		return harness.ToolReceipt{}, fmt.Errorf("%w: review requires identity, version, reviewer, evidence and completed/no_effect outcome", harness.ErrInvalidInput)
	}
	if err := validateReviewResult(review.Result); err != nil {
		return harness.ToolReceipt{}, err
	}
	tx, err := s.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return harness.ToolReceipt{}, err
	}
	defer tx.Rollback()
	receipt, err := readReceipt(ctx, tx, sessionID, review.RunID, review.ToolCallID)
	if err != nil {
		return receipt, err
	}
	if receipt.State != harness.ReceiptUncertain || receipt.Version != review.ExpectedVersion {
		return receipt, harness.ErrReceiptConflict
	}
	result, truncated := boundedReceiptResult(review.Result, false)
	receipt.Review = &harness.ReceiptReview{PreviousState: receipt.State, Outcome: review.Outcome, Reviewer: review.Reviewer, Note: review.Note, ReviewedAt: time.Now().UTC(), Result: result, ResultTruncated: truncated}
	receipt.State = review.Outcome
	receipt.Version++
	receipt.UpdatedAt = receipt.Review.ReviewedAt
	// Keep the original partial result/error intact alongside the human review.
	if err = writeReceipt(ctx, tx, receipt); err != nil {
		return receipt, err
	}
	data, err := json.Marshal(receipt.Review)
	if err != nil {
		return receipt, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_tool_reconciliations(run_id,tool_call_id,version,review) VALUES(?,?,?,?)`, receipt.RunID, receipt.ToolCallID, receipt.Version, data); err != nil {
		return receipt, err
	}
	_, err = appendEventTx(ctx, tx, harness.RunEvent{SessionID: sessionID, RunID: receipt.RunID, Kind: "tool_reconciled", ToolCallID: receipt.ToolCallID, ToolName: receipt.ToolName, Status: string(receipt.State), Receipt: &receipt})
	if err != nil {
		return receipt, err
	}
	return receipt, tx.Commit()
}

// Human evidence is descriptive, never a way to introduce trusted media into
// the durable transcript. Keep SDK validation aligned with the ACP extension.
func validateReviewResult(input []harness.Content) error {
	invalid := func() error {
		return fmt.Errorf("%w: review evidence requires bounded text or ordinary resource links", harness.ErrInvalidInput)
	}
	if len(input) > 128 {
		return invalid()
	}
	remaining := 64 * 1024
	for _, c := range input {
		if c.Asset != nil || c.Data != "" || c.Size != nil || c.Description != "" {
			return invalid()
		}
		remaining -= len(c.Text) + len(c.URI) + len(c.MimeType) + len(c.Name)
		if remaining < 0 {
			return invalid()
		}
		switch c.Type {
		case "text":
			if c.Text == "" || c.URI != "" || c.MimeType != "" || c.Name != "" {
				return invalid()
			}
		case "resource_link":
			u, err := url.Parse(c.URI)
			if err != nil || !u.IsAbs() || u.Scheme == "deerflow-asset" || c.Text != "" || len(c.URI) > 4096 || len(c.MimeType) > 256 || len(c.Name) > 1024 {
				return invalid()
			}
		default:
			return invalid()
		}
	}
	return nil
}

func eventError(e harness.RunEvent) string {
	if e.Receipt != nil && e.Receipt.Error != "" {
		return truncateUTF8(e.Receipt.Error, 4096)
	}
	var message strings.Builder
	for _, content := range e.Content {
		if content.Type == "text" {
			message.WriteString(content.Text)
		}
		if message.Len() >= 4096 {
			break
		}
	}
	return truncateUTF8(message.String(), 4096)
}

// Only these reserved built-in local tools have a verified read-only contract.
// MCP tools always have a namespace; their readOnlyHint is not evidence.
func failedStartedState(toolName string) harness.ReceiptState {
	switch toolName {
	case "read_file", "read_tool_output", "list_directory", "search_files", "read_skill_file", "skill", "view_image", "search_memory":
		return harness.ReceiptNoEffect
	default:
		return harness.ReceiptUncertain
	}
}

func summarizeArguments(data []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return "invalid JSON"
	}
	object, ok := value.(map[string]any)
	if !ok {
		return jsonType(value)
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		quoted, _ := json.Marshal(key)
		parts = append(parts, string(quoted)+":"+jsonType(object[key]))
	}
	return truncateUTF8("{"+strings.Join(parts, ",")+"}", 1024)
}

func jsonType(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

// Receipts keep bounded output evidence and artifact URIs. Full streaming
// content remains in the event log; inline binary data is not duplicated.
func boundedReceiptResult(input []harness.Content, truncated bool) ([]harness.Content, bool) {
	const maxBytes = 64 * 1024
	remaining := maxBytes
	result := make([]harness.Content, 0, min(len(input), 128))
	for _, content := range input {
		if remaining <= 0 || len(result) == 128 {
			truncated = true
			break
		}
		if content.Data != "" {
			content.Data = ""
			truncated = true
		}
		if content.Asset != nil {
			encoded, err := json.Marshal(content)
			if err != nil || len(encoded) > remaining {
				truncated = true
				break
			}
			remaining -= len(encoded)
			result = append(result, content)
			continue
		}
		for _, field := range []*string{&content.Type, &content.MimeType, &content.Name, &content.URI, &content.Text, &content.Description} {
			if len(*field) > remaining {
				*field = truncateUTF8(*field, remaining)
				truncated = true
			}
			remaining -= len(*field)
		}
		result = append(result, content)
	}
	return result, truncated
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
