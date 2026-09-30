package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

type Backend struct {
	cfg       harness.SandboxConfig
	cwd       string
	identity  os.FileInfo
	workspace *os.File
	guestCWD  string
	mu        sync.Mutex
	tasks     map[string]*task
	active    int
	closed    bool
	closeDone chan struct{}
	closeErr  error
}

type task struct {
	mu             sync.Mutex
	snapshot       harness.CommandSnapshot
	stdout, stderr *capture
	cancel         context.CancelFunc
	done           chan struct{}
}

var _ harness.CommandBackend = (*Backend)(nil)

func (b *Backend) Start(ctx context.Context, request harness.CommandRequest) (harness.CommandSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return harness.CommandSnapshot{}, harness.MarkToolNotExecuted(err)
	}
	if err := b.checkWorkspace(); err != nil {
		return harness.CommandSnapshot{}, harness.MarkToolNotExecuted(err)
	}
	request.Args = append([]string(nil), request.Args...)
	if err := b.validateRequest(&request); err != nil {
		return harness.CommandSnapshot{}, harness.MarkToolNotExecuted(err)
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return harness.CommandSnapshot{}, harness.MarkToolNotExecuted(err)
	}
	id := hex.EncodeToString(random[:])
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return harness.CommandSnapshot{}, harness.MarkToolNotExecuted(fmt.Errorf("command backend is closed"))
	}
	if b.active >= b.cfg.Limits.MaxConcurrent || len(b.tasks) >= b.cfg.Limits.MaxRetained {
		return harness.CommandSnapshot{}, harness.MarkToolNotExecuted(fmt.Errorf("%w: command capacity exhausted; release completed task records", harness.ErrBusy))
	}
	runCtx, cancel := context.WithTimeout(ctx, request.Timeout)
	t := &task{snapshot: harness.CommandSnapshot{ID: id, Provider: b.cfg.Provider, State: harness.CommandStarting, StartedAt: time.Now().UTC(), ProcessLocal: true}, cancel: cancel, done: make(chan struct{}), stdout: newCapture(b.cfg.Limits.OutputBytes), stderr: newCapture(b.cfg.Limits.OutputBytes)}
	b.tasks[id] = t
	b.active++
	initial := t.snapshot
	go b.execute(runCtx, t, request)
	return initial, nil
}

func (b *Backend) checkWorkspace() error {
	cwd, err := session.NormalizeWorkspace(b.cwd)
	if err != nil || !session.SameWorkspace(cwd, b.cwd) {
		return fmt.Errorf("workspace path changed since backend creation")
	}
	current, err := os.Open(cwd)
	if err != nil {
		return fmt.Errorf("workspace directory is inaccessible")
	}
	defer current.Close()
	info, err := current.Stat()
	if err != nil || !os.SameFile(b.identity, info) {
		return fmt.Errorf("workspace directory identity changed since backend creation")
	}
	return nil
}

func (b *Backend) validateRequest(r *harness.CommandRequest) error {
	if r.Timeout < 0 || r.Timeout > b.cfg.Limits.Timeout {
		return fmt.Errorf("requested timeout exceeds command policy")
	}
	if r.Timeout == 0 {
		r.Timeout = b.cfg.Limits.Timeout
	}
	if len(r.Script) > 1<<20 || len(r.Args) > 1024 {
		return fmt.Errorf("command is too large")
	}
	if strings.ContainsRune(r.Script, 0) {
		return fmt.Errorf("script contains NUL")
	}
	for _, arg := range r.Args {
		if len(arg) > 1<<20 || strings.ContainsRune(arg, 0) {
			return fmt.Errorf("invalid command argument")
		}
	}
	if r.Script != "" {
		if !b.cfg.AllowShell || b.cfg.Provider == harness.SandboxLocal {
			return fmt.Errorf("%w: scripts are not enabled", harness.ErrPermissionDenied)
		}
		if r.Executable != "" || len(r.Args) != 0 {
			return fmt.Errorf("choose either script or executable/args")
		}
		return nil
	}
	if b.cfg.Provider == harness.SandboxPowerShell {
		return fmt.Errorf("PowerShell provider requires a nonempty script")
	}
	if r.Executable == "" {
		return fmt.Errorf("executable is required")
	}
	path := r.Executable
	if b.cfg.Provider == harness.SandboxLocal {
		var err error
		path, err = absoluteExecutable(path)
		if err != nil {
			return err
		}
	}
	allowed := false
	for _, entry := range b.cfg.AllowedExecutables {
		if entry == path || b.cfg.Provider == harness.SandboxLocal && session.SameWorkspace(entry, path) {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("%w: executable is not allowlisted", harness.ErrPermissionDenied)
	}
	r.Executable = path
	return nil
}

func (b *Backend) lookup(id string) (*task, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.tasks[id]
	if t == nil {
		return nil, harness.ErrNotFound
	}
	return t, nil
}
func (t *task) view() harness.CommandSnapshot {
	t.mu.Lock()
	s := t.snapshot
	if s.ExitCode != nil {
		n := *s.ExitCode
		s.ExitCode = &n
	}
	t.mu.Unlock()
	s.Stdout = t.stdout.snapshot()
	s.Stderr = t.stderr.snapshot()
	return s
}
func (b *Backend) Poll(ctx context.Context, id string) (harness.CommandSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return harness.CommandSnapshot{}, err
	}
	t, err := b.lookup(id)
	if err != nil {
		return harness.CommandSnapshot{}, err
	}
	return t.view(), nil
}
func (b *Backend) Wait(ctx context.Context, id string) (harness.CommandSnapshot, error) {
	t, err := b.lookup(id)
	if err != nil {
		return harness.CommandSnapshot{}, err
	}
	select {
	case <-t.done:
		return t.view(), nil
	case <-ctx.Done():
		return t.view(), ctx.Err()
	}
}
func (b *Backend) Cancel(ctx context.Context, id string) (harness.CommandSnapshot, error) {
	t, err := b.lookup(id)
	if err != nil {
		return harness.CommandSnapshot{}, err
	}
	t.cancel()
	return b.Wait(ctx, id)
}
func (b *Backend) Release(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.tasks[id]
	if t == nil {
		return harness.ErrNotFound
	}
	select {
	case <-t.done:
	default:
		return harness.ErrBusy
	}
	if !t.view().TerminationConfirmed {
		return harness.ErrCommandUncertain
	}
	delete(b.tasks, id)
	return nil
}
func (b *Backend) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		<-b.closeDone
		return b.closeErr
	}
	b.closed = true
	tasks := make([]*task, 0, len(b.tasks))
	for _, t := range b.tasks {
		tasks = append(tasks, t)
		t.cancel()
	}
	b.mu.Unlock()
	var errs []error
	for _, t := range tasks {
		<-t.done
		if !t.view().TerminationConfirmed {
			errs = append(errs, fmt.Errorf("task %s: %w", t.snapshot.ID, harness.ErrCommandUncertain))
		}
	}
	errs = append(errs, b.workspace.Close())
	b.closeErr = errors.Join(errs...)
	close(b.closeDone)
	return b.closeErr
}

func (b *Backend) execute(ctx context.Context, t *task, request harness.CommandRequest) {
	defer func() { t.cancel(); b.mu.Lock(); b.active--; b.mu.Unlock(); close(t.done) }()
	result := b.run(ctx, t, request)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.snapshot.FinishedAt = time.Now().UTC()
	t.snapshot.ExitCode = result.exitCode
	t.snapshot.TerminationConfirmed = result.confirmed
	t.snapshot.State = result.state
	if result.err != nil {
		t.snapshot.Error = result.err.Error()
	}
}

func stateFor(err error, exit *int, confirmed bool) harness.CommandState {
	if !confirmed {
		return harness.CommandUncertain
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return harness.CommandTimedOut
	}
	if errors.Is(err, context.Canceled) {
		return harness.CommandCancelled
	}
	if err != nil || exit != nil && *exit != 0 {
		return harness.CommandFailed
	}
	return harness.CommandCompleted
}
