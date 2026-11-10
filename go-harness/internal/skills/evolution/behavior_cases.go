package evolution

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
)

// The immutable suite manifest is small. Case payloads are read one at a time;
// their ordered hash chain must match the snapshot before a report can pass.
func persistBehaviorCases(ctx context.Context, tx *sql.Tx, in *behaviorSnapshot) error {
	if in.Suite == nil || len(in.Suite.Cases) == 0 {
		return nil
	}
	digest, err := skillbehavior.Digest(*in.Suite)
	if err != nil {
		return err
	}
	if digest != in.SuiteDigest {
		return invalid("suite changed before persistence")
	}
	in.CaseCount = len(in.Suite.Cases)
	in.CaseRoot = ""
	for ordinal, c := range in.Suite.Cases {
		raw, _ := json.Marshal(c)
		sha := hash(raw)
		in.CaseRoot = encodedHash([]string{in.CaseRoot, sha})
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO df_skill_behavior_cases(suite_digest,ordinal,case_id,payload,payload_sha) VALUES(?,?,?,?,?)`, digest, ordinal, c.ID, raw, sha); err != nil {
			return err
		}
	}
	header := *in.Suite
	header.Cases = nil
	in.Suite = &header
	return nil
}

func walkBehaviorCases(ctx context.Context, db rowReader, in behaviorSnapshot, visit func(harness.SkillBehaviorCase) error) error {
	if in.Suite == nil {
		return nil
	}
	if in.CaseCount == 0 {
		for _, c := range in.Suite.Cases {
			if err := visit(c); err != nil {
				return err
			}
		}
		return nil
	}
	chain := ""
	for n := 0; n < in.CaseCount; n++ {
		var raw []byte
		var sha, id string
		var c harness.SkillBehaviorCase
		if err := db.QueryRowContext(ctx, `SELECT case_id,payload,payload_sha FROM df_skill_behavior_cases WHERE suite_digest=? AND ordinal=?`, in.SuiteDigest, n).Scan(&id, &raw, &sha); err != nil {
			return err
		}
		if len(raw) > skillbehavior.MaxSuiteBytes || hash(raw) != sha || json.Unmarshal(raw, &c) != nil || c.ID != id {
			return invalid("behavior case integrity failure")
		}
		if err := skillbehavior.ValidateCase(c); err != nil {
			return err
		}
		chain = encodedHash([]string{chain, sha})
		if err := visit(c); err != nil {
			return err
		}
	}
	if chain != in.CaseRoot {
		return invalid("behavior suite manifest integrity failure")
	}
	return nil
}

func findBehaviorCase(ctx context.Context, db rowReader, in behaviorSnapshot, id string) (*harness.SkillBehaviorCase, error) {
	if in.CaseCount == 0 {
		if in.Suite != nil {
			for _, c := range in.Suite.Cases {
				if c.ID == id {
					return &c, nil
				}
			}
		}
		return nil, harness.ErrNotFound
	}
	var raw []byte
	var sha string
	var c harness.SkillBehaviorCase
	if err := db.QueryRowContext(ctx, `SELECT payload,payload_sha FROM df_skill_behavior_cases WHERE suite_digest=? AND case_id=?`, in.SuiteDigest, id).Scan(&raw, &sha); err != nil {
		return nil, err
	}
	if len(raw) > skillbehavior.MaxSuiteBytes || hash(raw) != sha || json.Unmarshal(raw, &c) != nil || c.ID != id {
		return nil, invalid("behavior case integrity failure")
	}
	return &c, skillbehavior.ValidateCase(c)
}
