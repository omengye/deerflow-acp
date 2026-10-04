package evolution

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func pointer[T any](value T) *T { return &value }

func TestProposalMutationActionsKeepBaseAndReplay(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	request := Request{SourceID: "project", Name: "research", RequestKey: "support", Action: "write_file", Base: f.head(t), Path: "reference.md", Content: pointer("Reference text")}
	base := request.Base
	support := f.submit(t, request)
	if f.head(t) != base || len(support.ChangedFiles) != 1 || support.ChangedFiles[0] != "reference.md" {
		t.Fatalf("support changed base: %+v", support)
	}
	if _, err := f.service.Approve(ctx, support.ID, review(support)); err != nil {
		t.Fatal(err)
	}
	request = Request{SourceID: "project", Name: "research", RequestKey: "patch", Action: "patch", Base: f.head(t), Find: pointer("Original instructions"), Replace: pointer("Improved instructions"), ExpectedCount: pointer(1)}
	patch := f.submit(t, request)
	if len(patch.ChangedFiles) != 1 || patch.ChangedFiles[0] != "SKILL.md" || !strings.Contains(patch.Diff, "+Improved instructions") {
		t.Fatalf("patch: %+v", patch)
	}
	if _, err := f.service.Approve(ctx, patch.ID, review(patch)); err != nil {
		t.Fatal(err)
	}
	if again, err := f.service.Submit(ctx, request); err != nil || again.ID != patch.ID || f.calls.Load() != 2 {
		t.Fatalf("retry reconstructed from current base: %+v %v", again, err)
	}
	request = Request{SourceID: "project", Name: "research", RequestKey: "edit", Action: "edit", Base: f.head(t), Content: pointer(string(skillFiles("Replace main only")["SKILL.md"]))}
	edited := f.submit(t, request)
	if len(edited.ChangedFiles) != 1 {
		t.Fatal("main edit removed support files")
	}
	if _, err := f.service.Approve(ctx, edited.ID, review(edited)); err != nil {
		t.Fatal(err)
	}
	request = Request{SourceID: "project", Name: "research", RequestKey: "remove", Action: "remove_file", Base: f.head(t), Path: "reference.md"}
	removed := f.submit(t, request)
	if len(removed.ChangedFiles) != 1 || removed.ChangedFiles[0] != "reference.md" {
		t.Fatalf("remove: %+v", removed)
	}
	if _, err := f.service.Approve(ctx, removed.ID, review(removed)); err != nil {
		t.Fatal(err)
	}
	history, err := f.service.History(ctx, "project", "research", 0, 100)
	if err != nil || len(history) != 5 || history[2].Action != "patch" {
		t.Fatalf("mutation history: %+v %v", history, err)
	}
}

func TestProposalMutationRejectsAmbiguousOrInvalidInputs(t *testing.T) {
	f := newFixture(t, nil)
	base := f.head(t)
	for _, request := range []Request{
		{Action: "patch", Find: pointer("Original"), Replace: pointer("Improved"), ExpectedCount: pointer(2)},
		{Action: "patch", Find: pointer(""), Replace: pointer("Improved")},
		{Action: "patch", Find: pointer("Absent"), Replace: pointer("Improved")},
		{Action: "patch", Find: pointer("Original"), Replace: pointer("Improved"), ExpectedCount: pointer(0)},
		{Action: "edit", Files: skillFiles("Full candidate"), Content: pointer("Ambiguous")},
		{Action: "write_file", Path: "SKILL.md", Content: pointer("Cannot use support-file API")},
		{Action: "write_file", Path: "../../outside", Content: pointer("outside")},
		{Action: "write_file", Path: "folder/../inside", Content: pointer("noncanonical")},
		{Action: "write_file", Path: "run.py", Content: pointer("print('python')")},
		{Action: "remove_file", Path: "SKILL.md"},
		{Action: "remove_file", Path: "missing.md"},
		{Action: "delete", Content: pointer("unexpected")},
	} {
		request.SourceID, request.Name, request.RequestKey, request.Base = "project", "research", "invalid", base
		if _, err := f.service.Submit(context.Background(), request); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("%+v: %v", request, err)
		}
		if f.head(t) != base {
			t.Fatal("invalid mutation changed publication")
		}
	}
	if f.calls.Load() != 0 {
		t.Fatal("invalid mutation consumed a model check")
	}
}
