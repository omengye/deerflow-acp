package deerflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type engineFunc func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error)

func (f engineFunc) Run(ctx context.Context, r harness.RunRequest, e harness.EventHandler, p harness.PermissionHandler) (harness.RunResult, error) {
	return f(ctx, r, e, p)
}

func TestDataDirectoryHasOneOwner(t *testing.T) {
	cfg := Config{DataDir: t.TempDir(), Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{}, nil
	})}
	one, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	if two, err := Open(context.Background(), cfg); err == nil {
		two.Close()
		t.Fatal("second runtime acquired same database")
	}
	if err = one.Close(); err != nil {
		t.Fatal(err)
	}
	two, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = two.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHostToolAllowlistCannotBeExpandedBySessionSubagentOption(t *testing.T) {
	engine := engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	})
	c, err := Open(context.Background(), Config{DataDir: t.TempDir(), Engine: engine, ToolPolicy: harness.ToolPolicy{Allowlist: []string{"read_file"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	session, err := c.NewSession(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if session.Subagents {
		t.Fatal("host-denied task enabled native subagents by default")
	}
	options, err := c.ConfigOptions(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, option := range options {
		if option.ID == "subagent" {
			t.Fatal("client can enable host-denied task")
		}
	}
	if _, err := c.SetConfigOption(context.Background(), session.ID, "subagent", "on"); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("client enabled host-denied task: %v", err)
	}
}

func TestCloseWaitsForRunCleanupAndRejectsNewWork(t *testing.T) {
	started, cancelled, finish := make(chan struct{}), make(chan struct{}), make(chan struct{})
	engine := engineFunc(func(ctx context.Context, r harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-finish
		return harness.RunResult{StopReason: "cancelled"}, emit(ctx, harness.RunEvent{Kind: "text_delta", Text: "cleanup recorded"})
	})
	c, err := Open(context.Background(), Config{DataDir: t.TempDir(), Engine: engine})
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.NewSession(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() {
		_, err := c.Run(context.Background(), s.ID, []harness.Content{{Type: "text", Text: "start"}}, nil, nil)
		runDone <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not start")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- c.Close() }()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not cancel")
	}
	select {
	case <-closeDone:
		t.Fatal("close released database before cleanup")
	default:
	}
	if _, err := c.NewSession(context.Background(), t.TempDir()); err == nil {
		t.Fatal("closed client accepted new session")
	}
	close(finish)
	if err := <-runDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSDKSessionConfigurationIsAppliedAndRecovered(t *testing.T) {
	var seen harness.Session
	engine := engineFunc(func(_ context.Context, req harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		seen = req.Session
		return harness.RunResult{StopReason: "end_turn"}, nil
	})
	cfg := Config{DataDir: t.TempDir(), Model: "primary", Models: []harness.ConfigValue{{Value: "secondary"}}, Engine: engine}
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	cwd := t.TempDir()
	s, err := c.NewSession(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"model": "secondary", "approval": "read_only", "subagent": "off"} {
		if _, err := c.SetConfigOption(context.Background(), s.ID, key, value); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Run(context.Background(), s.ID, []harness.Content{{Type: "text", Text: "test"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if seen.Model != "secondary" || seen.ApprovalMode != harness.ApprovalReadOnly || seen.Subagents || seen.ConfigVersion != 4 {
		t.Fatalf("engine session=%+v", seen)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := c.LoadSession(context.Background(), s.ID, cwd, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ConfigVersion != seen.ConfigVersion || loaded.Model != seen.Model || loaded.ApprovalMode != seen.ApprovalMode || loaded.Subagents != seen.Subagents {
		t.Fatalf("reopened=%+v", loaded)
	}
	options, err := c.ConfigOptions(context.Background(), s.ID)
	if err != nil || len(options) != 3 {
		t.Fatalf("options=%+v err=%v", options, err)
	}
}
