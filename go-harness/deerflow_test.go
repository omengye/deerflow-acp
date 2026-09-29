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
