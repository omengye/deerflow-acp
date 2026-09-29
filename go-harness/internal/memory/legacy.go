package memory

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type LegacyImportReport = harness.LegacyImportReport

type legacyFact struct {
	ID         string  `json:"id"`
	Content    string  `json:"content"`
	Category   string  `json:"category"`
	Confidence float64 `json:"confidence"`
}

func parseLegacyDocument(raw []byte) ([]legacyFact, LegacyImportReport, error) {
	report := LegacyImportReport{RejectionReasons: make(map[string]int)}
	if len(raw) == 0 || len(raw) > 4*1024*1024 || !utf8.Valid(raw) {
		return nil, report, fmt.Errorf("%w: invalid DeerMem document size or encoding", harness.ErrInvalidInput)
	}
	var document struct {
		Version string            `json:"version"`
		Facts   []json.RawMessage `json:"facts"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&document); err != nil {
		return nil, report, fmt.Errorf("%w: decode DeerMem document: %v", harness.ErrInvalidInput, err)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || document.Version != "1.0" || len(document.Facts) > 1000 {
		return nil, report, fmt.Errorf("%w: unsupported DeerMem document", harness.ErrInvalidInput)
	}
	accepted := make([]legacyFact, 0, len(document.Facts))
	seen := make(map[string]bool, len(document.Facts))
	report.Read = len(document.Facts)
	for _, encoded := range document.Facts {
		var fact legacyFact
		if err := json.Unmarshal(encoded, &fact); err != nil {
			report.RejectionReasons["invalid_fact"]++
			continue
		}
		if fact.ID != "" && len(fact.ID) <= 128 && utf8.ValidString(fact.ID) {
			if seen[fact.ID] {
				return nil, report, fmt.Errorf("%w: duplicate DeerMem fact ID", harness.ErrInvalidInput)
			}
			seen[fact.ID] = true
		}
		fact.Content = strings.TrimSpace(fact.Content)
		fact.Category = strings.TrimSpace(fact.Category)
		if fact.Category == "" {
			fact.Category = "context"
		}
		reason := ""
		switch {
		case fact.ID == "" || len(fact.ID) > 128 || !utf8.ValidString(fact.ID):
			reason = "invalid_id"
		case len(fact.Category) > 64 || !utf8.ValidString(fact.Category):
			reason = "invalid_category"
		case math.IsNaN(fact.Confidence) || math.IsInf(fact.Confidence, 0) || fact.Confidence < 0 || fact.Confidence > 1:
			reason = "invalid_confidence"
		case fact.Content == "":
			reason = "empty_content"
		default:
			// Reuse the same conservative content gate as live extraction while
			// retaining the legacy category and confidence in the final fact.
			probe, _ := json.Marshal(map[string]any{"facts": []map[string]any{{"scope": "workspace", "durability": "durable", "authority": "descriptive", "category": "project", "content": fact.Content, "confidence": 0.75}}})
			filtered, rejected, err := FilterExtraction(probe, false)
			if err != nil || len(filtered) != 1 {
				reason = "unsafe_content"
				if len(rejected) > 0 {
					reason = rejected[0]
				}
			}
		}
		if reason != "" {
			report.RejectionReasons[reason]++
			continue
		}
		accepted = append(accepted, fact)
	}
	for _, count := range report.RejectionReasons {
		report.Rejected += count
	}
	return accepted, report, nil
}

// PreviewLegacy parses one old memory.json without opening the Go database.
func PreviewLegacy(raw []byte) (LegacyImportReport, error) {
	_, report, err := parseLegacyDocument(raw)
	return report, err
}

// ImportLegacy atomically imports accepted facts into an explicitly selected
// host scope. Rerunning the same document is idempotent by scope and legacy ID.
func (s *Store) ImportLegacy(ctx context.Context, scope Scope, raw []byte) (LegacyImportReport, error) {
	accepted, report, err := parseLegacyDocument(raw)
	if err != nil {
		return report, err
	}
	if len(accepted) == 0 {
		return report, ctx.Err()
	}
	err = transact(ctx, s.db, func(tx *sql.Tx) error {
		version, err := checkScope(ctx, tx, scope, true)
		if err != nil {
			return err
		}
		for _, item := range accepted {
			identity := sha256.Sum256([]byte(scope.key + "\x00" + item.ID))
			sourceID := "legacy-deermem/v1/" + hex.EncodeToString(identity[:])
			var oldContent, oldCategory, oldScope string
			err := tx.QueryRowContext(ctx, `SELECT r.content,r.category,f.scope_key FROM memory_fact_revisions r JOIN memory_facts f ON f.id=r.fact_id WHERE r.source_id=?`, sourceID).Scan(&oldContent, &oldCategory, &oldScope)
			if err == nil {
				if oldScope != scope.key || oldContent != item.Content || oldCategory != item.Category {
					return errors.Join(ErrVersionConflict, fmt.Errorf("legacy fact changed after import"))
				}
				report.Existing++
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			var duplicate bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memory_facts f JOIN memory_fact_revisions r ON r.fact_id=f.id AND r.revision=f.head_revision WHERE f.scope_key=? AND f.deleted=0 AND r.category=? AND lower(trim(r.content))=lower(trim(?)))`, scope.key, item.Category, item.Content).Scan(&duplicate); err != nil {
				return err
			}
			if duplicate {
				report.Duplicates++
				continue
			}
			version, err = bumpScope(ctx, tx, scope, version)
			if err != nil {
				return err
			}
			now := time.Now().UTC()
			fact := Fact{ID: rand.Text(), ScopeKey: scope.key, Revision: 1, Content: item.Content, Category: item.Category, Confidence: item.Confidence, Source: Source{ID: sourceID, Kind: "operator", ActorID: "legacy-deermem", PolicyVersion: "legacy-deermem-v1"}, CreatedAt: now, UpdatedAt: now}
			if _, err = tx.ExecContext(ctx, `INSERT INTO memory_facts(id,scope_key,head_revision,created_at) VALUES(?,?,1,?)`, fact.ID, scope.key, now.Format(time.RFC3339Nano)); err != nil {
				return err
			}
			if err = insertRevision(ctx, tx, fact, version, false); err != nil {
				return err
			}
			report.Imported++
		}
		return nil
	})
	if err != nil {
		report.Imported, report.Existing, report.Duplicates = 0, 0, 0
	}
	return report, err
}
