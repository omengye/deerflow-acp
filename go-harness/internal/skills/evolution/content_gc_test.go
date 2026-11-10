package evolution

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func TestCandidateGCPreservesSubsetCheckpointAndSharedContent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, validatorFunc(func(_ context.Context, in ValidationInput) (Report, error) { return validReport(in), nil }))
	request := f.request(t, "gc-first", "First discarded candidate")
	request.Files["references/shared.md"] = []byte("Shared immutable reference")
	first := f.submit(t, request)
	request = f.request(t, "gc-survivor", "Second retained candidate")
	request.Files["references/shared.md"] = []byte("Shared immutable reference")
	survivor := f.submit(t, request)
	ref := harness.SkillRef{SourceID: first.Target.SourceID, SourceIdentity: first.Target.SourceIdentity, Name: first.Target.Name, Version: first.CandidateVersion, Hash: first.CandidateVersion}
	snapshot, err := f.registry.Restore(ctx, harness.SkillSelection{Workspace: first.Target.Workspace}, []harness.SkillRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	child, err := snapshot.Subset(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.Reject(ctx, first.ID, review(first)); err != nil {
		t.Fatal(err)
	}
	if err = snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if dry, err := f.registry.MaintainCandidates(ctx, 32, false); err != nil || dry.Candidates != 0 {
		t.Fatal("subset lost its reference", dry, err)
	}
	if _, err = child.Get(ctx, first.SkillName); err != nil {
		t.Fatal(err)
	}
	if err = f.registry.PinVersions(ctx, "checkpoint/fixture", "checkpoint", []harness.SkillRef{ref}); err != nil {
		t.Fatal(err)
	}
	if err = child.Close(); err != nil {
		t.Fatal(err)
	}
	if real, err := f.registry.MaintainCandidates(ctx, 32, true); err != nil || real.Reclaimed != 0 {
		t.Fatal("checkpoint content deleted", real, err)
	}
	if err = f.registry.ReleaseVersions(ctx, "checkpoint/fixture", "checkpoint"); err != nil {
		t.Fatal(err)
	}
	dry, err := f.registry.MaintainCandidates(ctx, 32, false)
	if err != nil || dry.Candidates != 1 {
		t.Fatal(dry, err)
	}
	real, err := f.registry.MaintainCandidates(ctx, 32, true)
	if err != nil || real.Reclaimed != 1 {
		t.Fatal(real, err)
	}
	if _, err = f.registry.Restore(ctx, harness.SkillSelection{Workspace: first.Target.Workspace}, []harness.SkillRef{ref}); !errors.Is(err, skills.ErrContentExpired) {
		t.Fatal("expired version still restorable", err)
	}
	// The live installed version and shared objects remain readable.
	current, err := f.registry.Snapshot(ctx, harness.SkillSelection{Workspace: first.Target.Workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if _, err = current.Get(ctx, first.SkillName); err != nil {
		t.Fatal("GC damaged live baseline", err)
	}
	if err = f.registry.WithPublication(ctx, survivor.Target.SourceID, survivor.SkillName, func(pub *skills.PublicationTx) error {
		pkg, e := pub.ReadVersion(survivor.CandidateVersion)
		if e == nil && string(pkg.Files()["references/shared.md"]) != "Shared immutable reference" {
			return fmt.Errorf("shared content changed")
		}
		return e
	}); err != nil {
		t.Fatal("GC deleted a shared object", err)
	}
}

func TestCandidateGCPinAcquisitionIsAtomicWithDeletion(t *testing.T) {
	for iteration := 0; iteration < 12; iteration++ {
		t.Run(fmt.Sprint(iteration), func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t, nil)
			p := f.submit(t, f.request(t, "racing-gc", "Candidate awaiting deletion"))
			if _, err := f.service.Reject(ctx, p.ID, review(p)); err != nil {
				t.Fatal(err)
			}
			ref := harness.SkillRef{SourceID: p.Target.SourceID, SourceIdentity: p.Target.SourceIdentity, Name: p.SkillName, Version: p.CandidateVersion, Hash: p.CandidateVersion}
			start := make(chan struct{})
			var pinErr, gcErr error
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				pinErr = f.registry.PinVersions(ctx, "checkpoint/race", "checkpoint", []harness.SkillRef{ref})
			}()
			go func() { defer wg.Done(); <-start; _, gcErr = f.registry.MaintainCandidates(ctx, 32, true) }()
			close(start)
			wg.Wait()
			if gcErr != nil {
				t.Fatal(gcErr)
			}
			if pinErr != nil && !errors.Is(pinErr, skills.ErrContentExpired) {
				t.Fatal(pinErr)
			}
			if pinErr == nil {
				snap, err := f.registry.Restore(ctx, harness.SkillSelection{Workspace: p.Target.Workspace}, []harness.SkillRef{ref})
				if err != nil {
					t.Fatal("successfully pinned content was deleted", err)
				}
				defer snap.Close()
				if _, err = snap.Get(ctx, p.SkillName); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
