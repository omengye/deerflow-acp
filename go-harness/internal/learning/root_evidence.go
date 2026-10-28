package learning

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const RootEvidencePolicyVersion = "procedural-root-observations-v1"

// RootObservation is a host-validated physical run, not a human input or vote.
// EvidenceSHA pins policy, ancestry and native state without copying private text.
type RootObservation struct {
	Run         string `json:"run"`
	Session     string `json:"session"`
	Input       string `json:"input"`
	InputSHA    string `json:"input_sha"`
	Parent      string `json:"parent"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	Ended       string `json:"ended_at"`
	Frontier    int64  `json:"frontier"`
	EvidenceSHA string `json:"evidence_sha"`
}

type RootEvidenceReader func(context.Context, *sql.Tx, string, string, string) ([]RootObservation, error)

type rootCapture struct {
	Source  string            `json:"source"`
	Members []RootObservation `json:"members"`
}

// ConfigureRootEvidence is startup-only host wiring. Every read must use tx;
// callers cannot supply an observation list through a model or management API.
func (s *Store) ConfigureRootEvidence(reader RootEvidenceReader) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.done != nil || s.rootEvidence != nil || reader == nil {
		return invalid("invalid root evidence reader")
	}
	s.rootEvidence = reader
	return nil
}

func extractionID(snap extractionSnapshot, model string) string {
	identity := []any{PolicyVersion, snap.Session, snap.Episode, snap.Epoch, snap.Sources, snap.View, model, snap.MaxDailyModelCalls, snap.MaxDailyTokens}
	if snap.RootPolicy != "" || len(snap.Roots) != 0 {
		identity = append(identity, snap.RootPolicy, snap.Roots)
	}
	return "lex_" + encodedDigest(identity)
}

func rootObservationCount(snap extractionSnapshot) int {
	n := 0
	for _, root := range snap.Roots {
		n += len(root.Members)
	}
	return n
}

func validRootMembers(members []RootObservation, source string) bool {
	if len(members) > 8 {
		return false
	}
	prior := ""
	for _, member := range members {
		if member.Run == "" || member.Run == source || member.Run <= prior || member.Session == "" || member.Input == "" || member.Parent == "" || len(member.InputSHA) != 64 || len(member.EvidenceSHA) != 64 || member.Frontier < 1 || member.Status != "completed" && member.Status != "failed" {
			return false
		}
		if _, err := time.Parse(time.RFC3339Nano, member.Ended); err != nil {
			return false
		}
		prior = member.Run
	}
	return true
}

func (s *Store) captureRootTx(ctx context.Context, tx *sql.Tx, snap *extractionSnapshot) error {
	if s.rootEvidence == nil {
		return nil
	}
	snap.RootPolicy = RootEvidencePolicyVersion
	for _, src := range snap.Sources {
		members, err := s.rootEvidence(ctx, tx, src.Root, snap.Session, src.Run)
		if err != nil {
			return rootEvidenceFailure(err)
		}
		if len(members) > 8 {
			return sourceBounds("root member bounds")
		}
		if !validRootMembers(members, src.Run) {
			return invalid("invalid bounded root evidence")
		}
		snap.Roots = append(snap.Roots, rootCapture{Source: src.Run, Members: members})
		if rootObservationCount(*snap) > 8 {
			return sourceBounds("root observation bounds")
		}
		for _, member := range members {
			refs, err := runObservationView(ctx, tx, member.Run, 0, member.Frontier, false)
			if err != nil {
				return rootEvidenceFailure(err)
			}
			snap.View.References = append(snap.View.References, refs...)
		}
	}
	return nil
}

func (s *Store) verifyRootCaptureTx(ctx context.Context, tx *sql.Tx, snap extractionSnapshot, view *harness.SkillLearningInput) error {
	if snap.RootPolicy == "" && len(snap.Roots) == 0 {
		// Historical foreground-only captures retain their original scope.
		return nil
	}
	if snap.RootPolicy != RootEvidencePolicyVersion || s.rootEvidence == nil || len(snap.Roots) != len(snap.Sources) || rootObservationCount(snap) > 8 {
		return harness.ErrPermissionDenied
	}
	for i, src := range snap.Sources {
		root := snap.Roots[i]
		if root.Source != src.Run || !validRootMembers(root.Members, src.Run) {
			return harness.ErrPermissionDenied
		}
		members, err := s.rootEvidence(ctx, tx, src.Root, snap.Session, src.Run)
		if err != nil {
			return rootEvidenceFailure(err)
		}
		// Detect new descendants as well as changed or removed ones. Never
		// silently grow or rebase a previously admitted observation snapshot.
		if encodedDigest(members) != encodedDigest(root.Members) {
			return harness.ErrPermissionDenied
		}
		for _, member := range members {
			refs, err := runObservationView(ctx, tx, member.Run, 0, member.Frontier, false)
			if err != nil {
				if errors.Is(err, harness.ErrInvalidInput) {
					return harness.ErrPermissionDenied
				}
				return rootEvidenceFailure(err)
			}
			view.References = append(view.References, refs...)
		}
	}
	return nil
}

func rootEvidenceFailure(err error) error {
	if errors.Is(err, harness.ErrTaskOriginConflict) || errors.Is(err, harness.ErrExecutionConflict) || errors.Is(err, harness.ErrExecutionUnresumable) || errors.Is(err, harness.ErrReceiptConflict) || errors.Is(err, harness.ErrReconciliationRequired) || errors.Is(err, harness.ErrNotFound) || errors.Is(err, harness.ErrTaskTreeStopped) {
		return harness.ErrPermissionDenied
	}
	return err
}

func observationEnded(snap extractionSnapshot, src sourceRef, run string) (time.Time, bool) {
	ended, err := time.Parse(time.RFC3339Nano, src.Ended)
	if src.Status != "completed" || err != nil {
		return time.Time{}, false
	}
	if run == src.Run {
		return ended, true
	}
	for _, root := range snap.Roots {
		if root.Source != src.Run {
			continue
		}
		for _, member := range root.Members {
			if member.Run != run || member.Status != "completed" {
				continue
			}
			childEnded, err := time.Parse(time.RFC3339Nano, member.Ended)
			if err != nil {
				return time.Time{}, false
			}
			if childEnded.After(ended) {
				ended = childEnded
			}
			return ended, true
		}
	}
	return time.Time{}, false
}
