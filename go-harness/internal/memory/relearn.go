package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const RelearnPolicy = "memory-relearn-v1"

// No historical text is parsed or reclassified by this migration. Identities
// belong only to new, host-validated revisions; unknown old values fail closed.
const relearnMigration = `
CREATE TABLE memory_value_identities(
 fact_id TEXT NOT NULL, revision INTEGER NOT NULL, policy TEXT NOT NULL,
 slot_key TEXT NOT NULL, value_sha TEXT NOT NULL, content_sha TEXT NOT NULL,
 PRIMARY KEY(fact_id,revision));
`

// An unexported proof binds the validator's exact result to its accepted text.
// Public fields alone cannot manufacture it or mutate it after validation.
type writeProof struct {
	sourceSHA                     [32]byte
	key, category, value, content string
	automatic, explicit           bool
}

func sealWrite(source, clause string, v ValidatedWrite, automatic bool) *writeProof {
	explicit := rememberPrefix.MatchString(clause)
	// Evidence can omit a leading "请记住，". Only accept that form when the
	// remainder of the complete source is exactly the validated declaration.
	full := strings.TrimRight(strings.TrimSpace(source), "。.!！")
	body := rememberPrefix.ReplaceAllString(full, "")
	if body != full && strings.TrimSpace(body) == strings.TrimSpace(clause) {
		explicit = true
	}
	return &writeProof{sha256.Sum256([]byte(source)), v.Key, v.Category, v.Value, v.Content, automatic, explicit}
}

func mutationWriteProof(m InputSnapshot, in Mutation) (*writeProof, error) {
	if in.Validated == nil {
		return nil, nil
	}
	v, p := in.Validated, in.Validated.proof
	if p == nil || p.sourceSHA != sha256.Sum256([]byte(m.Text)) || p.key != in.Key || p.category != in.Category || p.content != in.Content || p.automatic != in.Automatic ||
		p.key != v.Key || p.category != v.Category || p.value != v.Value || p.content != v.Content {
		return nil, harness.ErrInvalidInput
	}
	return p, nil
}

// This is literal, field-specific identity, not general entity resolution.
// Case, compatible Unicode forms, whitespace and punctuation cannot manufacture
// a new name/location/identifier. Language aliases have an explicit finite map.
func structuredValueIdentity(key, value string) (string, bool) {
	folded := cases.Fold().String(norm.NFKC.String(value))
	switch key {
	case "preference.language", "preference.answer_style":
		switch folded {
		case "中文", "chinese":
			folded = "zh"
		case "英文", "英语", "english":
			folded = "en"
		default:
			return "", false
		}
	case "profile.name", "profile.last_reported_location", "profile.hardware", "project.database":
		folded = strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) || unicode.IsPunct(r) {
				return -1
			}
			return r
		}, folded)
		if folded == "" {
			return "", false
		}
		if key == "project.database" {
			switch folded {
			case "postgres", "pg":
				folded = "postgresql"
			case "sqlite3":
				folded = "sqlite"
			}
		}
	default:
		return "", false
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(RelearnPolicy+"\x00"+key+"\x00"+folded))), true
}

func relearnDecisionTx(ctx context.Context, tx *sql.Tx, f Fact, in Mutation, m InputSnapshot, p *writeProof) (status, reason string, err error) {
	if !in.Automatic {
		if in.RequireSnapshot && (p == nil || !p.explicit) {
			return "requires_explicit", "explicit_request_required", nil
		}
		return "", "", nil // A direct operator management call is explicit authority.
	}
	if !in.RequireSnapshot || m.RelearnPolicy != RelearnPolicy || p == nil || !m.AutomaticEligible {
		return "superseded", "relearn_source_unverifiable", nil
	}
	current, ok := structuredValueIdentity(in.Key, p.value)
	if !ok {
		return "requires_explicit", "value_identity_unsupported", nil
	}
	var previous, policy, key, contentSHA, content string
	// The immediately preceding live revision defines the deleted value. Never
	// fall back to an older typed revision after an untyped operator correction.
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(v.value_sha,''),COALESCE(v.policy,''),COALESCE(v.slot_key,''),COALESCE(v.content_sha,''),r.content
 FROM memory_fact_revisions r LEFT JOIN memory_value_identities v ON v.fact_id=r.fact_id AND v.revision=r.revision
 WHERE r.fact_id=? AND r.revision<? AND r.deleted=0 ORDER BY r.revision DESC LIMIT 1`, f.ID, f.Revision).Scan(&previous, &policy, &key, &contentSHA, &content)
	if errors.Is(err, sql.ErrNoRows) {
		return "requires_explicit", "deleted_value_unverifiable", nil
	}
	if err != nil {
		return "", "", err
	}
	if policy != RelearnPolicy || key != in.Key || previous == "" || contentSHA != fmt.Sprintf("%x", sha256.Sum256([]byte(content))) {
		return "requires_explicit", "deleted_value_unverifiable", nil
	}
	if previous == current {
		return "requires_explicit", "same_deleted_value", nil
	}
	return "", "relearned_new_value", nil
}

func recordValueIdentityTx(ctx context.Context, tx *sql.Tx, f Fact, m InputSnapshot, p *writeProof) error {
	if p == nil || m.RelearnPolicy != RelearnPolicy {
		return nil
	}
	identity, ok := structuredValueIdentity(p.key, p.value)
	if !ok {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO memory_value_identities(fact_id,revision,policy,slot_key,value_sha,content_sha) VALUES(?,?,?,?,?,?)`, f.ID, f.Revision, RelearnPolicy, p.key, identity, fmt.Sprintf("%x", sha256.Sum256([]byte(f.Content))))
	return err
}
