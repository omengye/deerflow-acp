package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
	"github.com/omengye/deerflow-acp/go-harness/internal/tools"
)

const personalMemoryInstruction = `
This installation has one persistent user profile shared by all sessions and devices.
Long-term memory IS available. Core personal facts appear in relevant_memory; use
search_memory for additional facts and current keys/revisions. For an explicit
request to remember, correct or forget, call remember_memory or forget_memory
before confirming. Only a successful server receipt establishes a saved change.
Use global for personal profile/preferences, workspace for project facts. Use the
same canonical key when correcting: profile.name, profile.last_reported_location,
preference.language. A location records what the user last reported, NOT real-time
geolocation or permanent residence. Cite its age/source if relevant. Do not save
channel framing, permissions, secrets, assistant claims or tool outputs as personal
facts. When a tool reports a version conflict, search the current fact before retry.
An absent fact means it is not saved; never deny the existence of the memory feature.
`

func personalMemoryTools(store *memory.Store, req harness.RunRequest, cfg Config) ([]tool.BaseTool, error) {
	return tools.MemoryMutationTools(func(ctx context.Context, action string, in tools.MemoryWriteInput) (any, error) {
		bound, ok := budget.ScopeFromContext(ctx)
		if !ok || bound.MemberID != req.RunID || bound.SessionID != req.Session.ID {
			return nil, harness.ErrPermissionDenied
		}
		if source := interaction.FromContext(ctx); source != nil && source.InputSource != nil {
			return nil, harness.ErrPermissionDenied
		}
		text, sequence, err := store.SourceText(ctx, req.RunID, req.InputID)
		if err != nil {
			return nil, err
		}
		if len(strings.TrimSpace(in.Evidence)) < 2 || !strings.Contains(text, in.Evidence) {
			return nil, fmt.Errorf("%w: quote the current user message", harness.ErrInvalidInput)
		}
		kind := harness.MemoryScope(in.Scope)
		if kind != harness.MemoryGlobal && kind != harness.MemoryWorkspace {
			return nil, harness.ErrInvalidInput
		}
		if kind == harness.MemoryWorkspace && in.Category != "project" {
			return nil, harness.ErrInvalidInput
		}
		scope, err := memoryRunScope(cfg, req, kind)
		if err != nil {
			return nil, err
		}
		key := memory.CanonicalKey(in.Key)
		operation := memory.OperationID(req.InputID, action, string(kind), key, in.Content, fmt.Sprint(in.ExpectedRevision))
		return store.Mutate(ctx, scope, memory.Mutation{OperationID: operation, Action: action, Key: key, Content: in.Content, Category: in.Category, ExpectedRevision: in.ExpectedRevision, Source: memory.Source{ID: operation, Kind: "operator", ActorID: "single-user", RunID: req.RunID, InputID: req.InputID, EventSequence: sequence, PolicyVersion: "personal-explicit-v1"}})
	})
}

func personalMemoryPostRun(store *memory.Store, req harness.RunRequest, cfg Config) func(context.Context, model.BaseModel[*schema.Message]) (func(context.Context, string, bool) error, error) {
	return func(ctx context.Context, _ model.BaseModel[*schema.Message]) (func(context.Context, string, bool) error, error) {
		versions := map[string]map[string]int64{}
		for _, kind := range []harness.MemoryScope{harness.MemoryGlobal, harness.MemoryWorkspace} {
			scope, err := memoryRunScope(cfg, req, kind)
			if err != nil {
				return nil, err
			}
			versions[string(kind)], err = store.SlotVersions(ctx, scope)
			if err != nil {
				return nil, err
			}
		}
		return func(ctx context.Context, _ string, _ bool) error {
			if source := interaction.FromContext(ctx); source != nil && source.InputSource != nil {
				return nil
			}
			b, ok := budget.ScopeFromContext(ctx)
			if !ok || b.MemberID != req.RunID || b.AttemptID == "" {
				return nil
			}
			err := store.Enqueue(ctx, memory.Job{RunID: req.RunID, InputID: req.InputID, AttemptID: b.AttemptID, Workspace: req.Session.CWD, ProfileID: cfg.memoryGlobalID, Versions: versions})
			if errors.Is(err, harness.ErrInvalidInput) {
				return nil
			}
			return err
		}, nil
	}
}

func (c *Client) startMemoryWorker(ctx context.Context, cfg Config) error {
	extract, err := einoengine.NewMemoryExtractor(ctx, einoengine.Config{Provider: cfg.Provider, APIKey: cfg.APIKey, BaseURL: cfg.BaseURL, Model: cfg.Model, OutputTokenParameter: cfg.OutputTokenParameter})
	if err != nil {
		return err
	}
	if err = c.service.Memory.RecoverJobs(ctx); err != nil {
		return err
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	c.memoryCancel, c.memoryDone = cancel, make(chan struct{})
	go func() {
		defer close(c.memoryDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
			}
			indexCtx, indexCancel := context.WithTimeout(workerCtx, 15*time.Second)
			_ = c.service.Memory.IndexPending(indexCtx)
			indexCancel()
			j, err := c.service.Memory.ClaimJob(workerCtx)
			if err != nil {
				continue
			}
			callCtx, stop := context.WithTimeout(workerCtx, 60*time.Second)
			payload, _ := json.Marshal(map[string]string{"user": j.Text})
			result := j.Result
			if result == "" {
				result, err = extract(callCtx, memory.ExtractionInstruction, string(payload))
				if err == nil {
					_, err = memory.ParseAttributes(result, j.Text)
				}
				if err == nil {
					err = c.service.Memory.SaveJobResult(callCtx, j.ID, result)
				}
			}
			if err == nil {
				err = c.service.Memory.ApplyJob(callCtx, j, result)
			}
			stop()
			finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = c.service.Memory.FinishJob(finishCtx, j.ID, err != nil)
			finishCancel()
		}
	}()
	return nil
}
