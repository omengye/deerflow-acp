package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
)

func migrateSteering(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS harness_steering (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,session_id TEXT NOT NULL REFERENCES harness_sessions(id),
 run_id TEXT NOT NULL,request_id TEXT NOT NULL,expected_version INTEGER NOT NULL,text TEXT NOT NULL,
 status TEXT NOT NULL,reason TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,updated_at TEXT NOT NULL,
 UNIQUE(session_id,request_id));
 CREATE INDEX IF NOT EXISTS harness_steering_run ON harness_steering(session_id,run_id,sequence);`)
	return err
}

func (s *Service) SteeringEnabled() bool {
	e, ok := s.Engine.(interface{ SteeringSupported() bool })
	return ok && e.SteeringSupported() && s.DurableExecutionsEnabled()
}

func scanSteering(row scanner) (harness.SteeringRecord, int64, error) {
	var item harness.SteeringRecord
	var version int64
	var created, updated string
	err := row.Scan(&item.Sequence, &item.RequestID, &item.SessionID, &item.RunID, &version, &item.Text, &item.Status, &item.Reason, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return item, version, harness.ErrNotFound
	}
	if err != nil {
		return item, version, err
	}
	item.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err == nil {
		item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	}
	return item, version, err
}

const steeringColumns = `sequence,request_id,session_id,run_id,expected_version,text,status,reason,created_at,updated_at`

func readSteering(ctx context.Context, q executionQuery, sessionID, requestID string) (harness.SteeringRecord, int64, error) {
	return scanSteering(q.QueryRowContext(ctx, `SELECT `+steeringColumns+` FROM harness_steering WHERE session_id=? AND request_id=?`, sessionID, requestID))
}

func (s *Service) SubmitSteering(ctx context.Context, owner, sessionID string, req harness.SteeringRequest) (harness.SteeringRecord, error) {
	var item harness.SteeringRecord
	if !s.SteeringEnabled() {
		return item, harness.ErrInvalidInput
	}
	if err := s.Coordinator.Authorize(sessionID, owner); err != nil {
		return item, err
	}
	req.Text = secrets.Mask(req.Text)
	if req.RunID == "" || len(req.RunID) > 256 || req.RequestID == "" || len(req.RequestID) > 128 || req.ExpectedVersion < 1 || len(req.Text) > 8192 || !utf8.ValidString(req.Text) || strings.TrimSpace(req.Text) == "" || strings.ContainsRune(req.Text, 0) {
		return item, harness.ErrInvalidInput
	}
	err := withExecutionTransaction(ctx, s.Store, func(tx *sql.Tx) error {
		if err := s.Coordinator.Authorize(sessionID, owner); err != nil {
			return err
		}
		previous, version, err := readSteering(ctx, tx, sessionID, req.RequestID)
		if err == nil {
			if previous.RunID != req.RunID || version != req.ExpectedVersion || previous.Text != req.Text {
				return harness.ErrExecutionConflict
			}
			item = previous
			return nil
		}
		if !errors.Is(err, harness.ErrNotFound) {
			return err
		}
		var count, bytes int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(length(CAST(text AS BLOB))),0) FROM harness_steering WHERE session_id=? AND run_id=?`, sessionID, req.RunID).Scan(&count, &bytes); err != nil {
			return err
		}
		if count >= 64 || bytes+len(req.Text) > 65536 {
			return harness.ErrInvalidInput
		}
		status, reason := "accepted", ""
		row, err := readExecution(ctx, tx, sessionID, req.RunID)
		if errors.Is(err, harness.ErrNotFound) {
			status, reason = "rejected", "run_not_found"
		} else if err != nil {
			return err
		} else {
			switch {
			case row.SourceKind != "":
				status, reason = "rejected", "not_foreground_root"
			case row.State.Version != req.ExpectedVersion:
				status, reason = "rejected", "execution_changed"
			case row.State.Status != harness.ExecutionRunning && row.State.Status != harness.ExecutionResuming && row.State.Status != harness.ExecutionWaitingInput:
				status, reason = "rejected", "execution_ended"
			}
		}
		now := timestamp()
		if _, err = tx.ExecContext(ctx, `INSERT INTO harness_steering(session_id,run_id,request_id,expected_version,text,status,reason,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, sessionID, req.RunID, req.RequestID, req.ExpectedVersion, req.Text, status, reason, now, now); err != nil {
			return err
		}
		item, _, err = readSteering(ctx, tx, sessionID, req.RequestID)
		return err
	})
	return item, err
}

func steeringRows(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, sessionID, runID string, after int64, activeOnly bool) ([]harness.SteeringRecord, error) {
	query := `SELECT ` + steeringColumns + ` FROM harness_steering WHERE session_id=? AND (?='' OR run_id=?) AND sequence>?`
	if activeOnly {
		query += ` AND status IN ('accepted','applied')`
	}
	order := ` ORDER BY sequence LIMIT 64`
	if runID == "" {
		order = ` ORDER BY sequence DESC LIMIT 64`
	}
	rows, err := q.QueryContext(ctx, query+order, sessionID, runID, runID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []harness.SteeringRecord{}
	for rows.Next() {
		item, _, err := scanSteering(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if runID == "" {
		slices.Reverse(items)
	}
	return items, rows.Err()
}

func (s *Service) ListSteering(ctx context.Context, owner, sessionID, runID string) (harness.SteeringList, error) {
	result := harness.SteeringList{SessionID: sessionID, RunID: runID, Items: []harness.SteeringRecord{}}
	if err := s.Coordinator.Authorize(sessionID, owner); err != nil {
		return result, err
	}
	if len(runID) > 256 {
		return result, harness.ErrInvalidInput
	}
	var err error
	result.Items, err = steeringRows(ctx, s.Store.db, sessionID, runID, 0, false)
	return result, err
}

func rejectPendingSteeringTx(ctx context.Context, tx *sql.Tx, runID, reason string) error {
	_, err := tx.ExecContext(ctx, `UPDATE harness_steering SET status='rejected',reason=?,updated_at=? WHERE run_id=? AND status='accepted'`, reason, timestamp(), runID)
	return err
}

func (s *Service) steeringContext(ctx context.Context, lease ExecutionLease, emit harness.EventHandler) context.Context {
	return interaction.WithSteering(ctx, interaction.SteeringFeed{
		Read: func(ctx context.Context, after int64) ([]harness.SteeringRecord, error) {
			var items []harness.SteeringRecord
			err := withExecutionTransaction(ctx, s.Store, func(tx *sql.Tx) error {
				row, err := checkExecutionLease(ctx, tx, lease)
				if err != nil {
					return err
				}
				if row.SourceKind != "" {
					return nil
				}
				items, err = steeringRows(ctx, tx, lease.Scope.SessionID, lease.Scope.MemberID, after, true)
				return err
			})
			return items, err
		},
		Applied: func(ctx context.Context, sequences []int64) error {
			var events []harness.RunEvent
			err := withExecutionTransaction(ctx, s.Store, func(tx *sql.Tx) error {
				row, err := checkExecutionLease(ctx, tx, lease)
				if err != nil {
					return err
				}
				if row.SourceKind != "" {
					return harness.ErrPermissionDenied
				}
				for _, sequence := range sequences {
					result, err := tx.ExecContext(ctx, `UPDATE harness_steering SET status='applied',updated_at=? WHERE session_id=? AND run_id=? AND sequence=? AND status='accepted'`, timestamp(), lease.Scope.SessionID, lease.Scope.MemberID, sequence)
					if err != nil {
						return err
					}
					changed, err := result.RowsAffected()
					if err != nil {
						return err
					}
					if changed == 0 {
						continue
					}
					item, _, err := scanSteering(tx.QueryRowContext(ctx, `SELECT `+steeringColumns+` FROM harness_steering WHERE sequence=?`, sequence))
					if err != nil {
						return err
					}
					event := harness.RunEvent{SessionID: item.SessionID, RunID: item.RunID, Kind: "steering_updated", Steering: &item}
					data, err := json.Marshal(event)
					if err != nil {
						return err
					}
					if _, err = tx.ExecContext(ctx, `INSERT INTO harness_events(session_id,run_id,event) VALUES(?,?,?)`, item.SessionID, item.RunID, data); err != nil {
						return err
					}
					events = append(events, event)
				}
				return nil
			})
			if err == nil && emit != nil {
				for _, event := range events {
					_ = emit(ctx, event)
				}
			}
			return err
		},
	})
}

// Status notifications may repeat after a resume; records are authoritative
// and merge by sequence/status. A disconnected observer recovers through list.
func (s *Service) publishSteeringStates(sessionID, runID string, emit harness.EventHandler) {
	if emit == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	items, err := steeringRows(ctx, s.Store.db, sessionID, runID, 0, false)
	if err != nil {
		return
	}
	for i := range items {
		_ = emit(ctx, harness.RunEvent{SessionID: sessionID, RunID: runID, Kind: "steering_updated", Steering: &items[i]})
	}
}
