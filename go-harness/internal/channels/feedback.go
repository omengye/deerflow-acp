package channels

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
)

const feedbackQueueLimit = 64
const feedbackToolLimit = 256

type feedbackKey struct{}

type feedbackTool struct {
	phases uint8
	tool   channelproto.ToolProgress
}

// Feedback is deliberately ephemeral. Restarting must not replay stale progress,
// and an ambiguous optional send must not fence the durable final answer.
type feedback struct {
	ctx      context.Context
	cancel   context.CancelFunc
	start    func(context.Context, Manifest) (transport, channelproto.Descriptor, error)
	manifest Manifest
	auth     channelproto.Auth
	message  channelproto.Message
	runID    string
	native   bool
	typing   bool
	current  func() bool

	callTimeout, drainTimeout, typingInterval time.Duration
	mu                                        sync.Mutex
	started, finished, failed                 bool
	seen                                      map[string]feedbackTool
	queue                                     chan channelproto.Progress
	flush, drained, done                      chan struct{}
	finishOnce                                sync.Once
	complete                                  bool
}

func validateFeedbackOptions(options map[string]string) error {
	switch options["progress_mode"] {
	case "", "native", "summary":
	default:
		return userError("progress_mode must be native or summary")
	}
	switch options["typing"] {
	case "", "true", "false":
	default:
		return userError("typing must be true or false")
	}
	return nil
}

func (m *Manager) withFeedback(ctx context.Context, i *instance, auth channelproto.Auth, cfg Config, msg channelproto.Message) (context.Context, *feedback) {
	i.mu.Lock()
	manifest, revision := i.manifest, i.revision
	i.mu.Unlock()
	if !manifest.Descriptor.IndependentFeedback || strings.TrimSpace(msg.Context) == "" {
		return ctx, nil
	}
	native := cfg.Options["progress_mode"] != "summary"
	typing := cfg.Options["typing"] != "false"
	if validateFeedbackOptions(cfg.Options) != nil || (!native && !typing) {
		return ctx, nil
	}
	work, cancel := context.WithCancel(ctx)
	f := &feedback{
		ctx: work, cancel: cancel, start: m.start, manifest: manifest, auth: auth,
		// Never forward the original prompt or quoted content as feedback.
		message: channelproto.Message{ID: msg.ID, Conversation: msg.Conversation, Sender: msg.Sender, Context: msg.Context},
		runID:   "df-run-" + rand.Text(), native: native, typing: typing,
		callTimeout: 2 * time.Second, drainTimeout: 200 * time.Millisecond, typingInterval: 5 * time.Second,
		seen: map[string]feedbackTool{}, queue: make(chan channelproto.Progress, feedbackQueueLimit),
		flush: make(chan struct{}), drained: make(chan struct{}), done: make(chan struct{}),
		current: func() bool {
			i.mu.Lock()
			defer i.mu.Unlock()
			return i.cfg.Enabled && i.revision == revision && i.auth.Account == auth.Account &&
				i.auth.Owner == auth.Owner && i.cfg.Workspace == cfg.Workspace && allowed(i.cfg, i.auth, msg.Sender)
		},
	}
	return context.WithValue(ctx, feedbackKey{}, f), f
}

// StartFeedback is called only when a model execution actually starts, including
// checkpoint resumes. Status/help/new and already-waiting sessions stay silent.
func StartFeedback(ctx context.Context) {
	if f, _ := ctx.Value(feedbackKey{}).(*feedback); f != nil {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.started && !f.finished && f.ctx.Err() == nil {
			f.started = true
			go f.run()
		}
	}
}

// PublishToolProgress never performs I/O or returns a delivery error to the
// engine. The caller must supply a host-authored (not model-authored) tool label.
func PublishToolProgress(ctx context.Context, tool channelproto.ToolProgress) {
	f, _ := ctx.Value(feedbackKey{}).(*feedback)
	if f == nil || !f.native {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.started || f.finished || f.failed || f.ctx.Err() != nil {
		return
	}
	if tool.ToolCallID == "" || len(tool.ToolName) > 256 || strings.TrimSpace(tool.ToolName) == "" {
		f.failed = true
		return
	}
	if tool.RunID == "" {
		tool.RunID = f.runID
	}
	// Model-issued tool IDs are opaque and may contain arbitrary text. Hash them
	// with this dispatch's identity; a resume is a new presentation attempt.
	id := fmt.Sprintf("df-tool-%x", sha256.Sum256([]byte(f.runID+"\x00"+tool.RunID+"\x00"+tool.ToolCallID)))
	phase := uint8(1)
	if tool.Phase == "end" {
		phase = 2
		switch tool.Status {
		case "completed", "failed", "blocked", "unknown":
		default:
			tool.Status = "unknown"
		}
	} else if tool.Phase != "start" {
		f.failed = true
		return
	}
	seen := f.seen[id].phases
	if seen&phase != 0 || seen&2 != 0 {
		return
	}
	if seen == 0 && len(f.seen) >= feedbackToolLimit {
		f.failed = true
		return
	}
	tool.ToolCallID = id
	if f.enqueue(tool) {
		f.seen[id] = feedbackTool{phases: seen | phase, tool: tool}
	}
}

// Caller holds mu. Enqueue only sanitized, bounded metadata.
func (f *feedback) enqueue(tool channelproto.ToolProgress) bool {
	progress := channelproto.Progress{Auth: f.auth, Message: f.message, Key: "df-progress-" + rand.Text(), Tool: tool}
	select {
	case f.queue <- progress:
		return true
	default:
		// Bounded memory and no backpressure on tools. The final summary covers
		// the missing/unconfirmed feedback instead of silently losing it.
		f.failed = true
		return false
	}
}

// FinishFeedback reports whether all native progress was acknowledged. A short
// grace period lets fast sends settle; slow/failed sends trigger the text summary.
// Typing cleanup continues independently of the final answer's send transport.
func FinishFeedback(ctx context.Context) bool {
	f, _ := ctx.Value(feedbackKey{}).(*feedback)
	return f != nil && f.finish()
}

func (f *feedback) finish() bool {
	if f == nil {
		return false
	}
	f.finishOnce.Do(func() {
		f.mu.Lock()
		f.finished = true
		started := f.started
		// An interrupted engine may return without an end event. Close every
		// remaining native item honestly; do not leave it appearing to run.
		if started && !f.failed && f.ctx.Err() == nil {
			for _, seen := range f.seen {
				if seen.phases&2 == 0 {
					tool := seen.tool
					tool.Phase, tool.Status = "end", "unknown"
					if !f.enqueue(tool) {
						break
					}
				}
			}
		}
		f.mu.Unlock()
		if !started {
			f.cancel()
			close(f.drained)
			close(f.done)
			return
		}
		close(f.flush)
		timer := time.NewTimer(f.drainTimeout)
		defer timer.Stop()
		select {
		case <-f.drained:
		case <-f.ctx.Done():
			f.fail()
		case <-timer.C:
			f.fail()
		}
		f.mu.Lock()
		f.complete = f.native && !f.failed && f.ctx.Err() == nil
		f.mu.Unlock()
		f.cancel()
	})
	return f.complete
}

func (f *feedback) close() {
	if f != nil {
		f.finish()
		<-f.done
	}
}

func (f *feedback) fail()                { f.mu.Lock(); f.failed = true; f.mu.Unlock() }
func (f *feedback) failedProgress() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.failed }

func (f *feedback) run() {
	var p transport
	var ticket string
	var typingAttempted bool
	// Every transport is owned by this goroutine. No feedback error touches
	// i.proc, workerError, credentials, the inbox status, or the run context.
	call := func(ctx context.Context, method string, input, out any) bool {
		if ctx.Err() != nil || !f.current() {
			return false
		}
		bounded, cancel := context.WithTimeout(ctx, f.callTimeout)
		defer cancel()
		if p == nil {
			var descriptor channelproto.Descriptor
			var err error
			p, descriptor, err = f.start(bounded, f.manifest)
			if err != nil || !descriptor.IndependentFeedback {
				if p != nil {
					p.Close()
					p = nil
				}
				return false
			}
		}
		if err := p.Call(bounded, method, input, out); err != nil {
			p.Close()
			p = nil
			return false
		}
		return true
	}
	defer func() {
		// Let the final answer proceed before optional typing cleanup. The
		// manager still joins us before another run or a channel reconfiguration.
		close(f.drained)
		if typingAttempted && ticket != "" {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(f.ctx), f.callTimeout)
			var result channelproto.TypingResult
			call(cleanup, "typing", channelproto.Typing{Auth: f.auth, Message: f.message, Ticket: ticket, Active: false}, &result)
			cancel()
		}
		if p != nil {
			p.Close()
		}
		close(f.done)
	}()
	typing := f.typing
	if typing {
		var result channelproto.TypingResult
		typing = call(f.ctx, "typing.prepare", channelproto.Typing{Auth: f.auth, Message: f.message}, &result) && result.Ticket != "" && len(result.Ticket) <= 8192
		if typing {
			ticket = result.Ticket
		}
	}
	sendTyping := func() {
		if !typing {
			return
		}
		typingAttempted = true
		var result channelproto.TypingResult
		typing = call(f.ctx, "typing", channelproto.Typing{Auth: f.auth, Message: f.message, Ticket: ticket, Active: true}, &result) && result.Sent
	}
	sendTyping()
	ticker := time.NewTicker(f.typingInterval)
	defer ticker.Stop()
	deliver := func(progress channelproto.Progress) {
		if f.failedProgress() {
			return
		}
		var result struct {
			Sent bool `json:"sent"`
		}
		if !call(f.ctx, "progress", progress, &result) || !result.Sent {
			f.fail()
		}
	}
	for {
		if f.ctx.Err() != nil || !f.current() {
			f.fail()
			return
		}
		select {
		case <-f.ctx.Done():
			f.fail()
			return
		case <-f.flush:
			for {
				select {
				case progress := <-f.queue:
					deliver(progress)
				default:
					return
				}
			}
		case progress := <-f.queue:
			deliver(progress)
		case <-ticker.C:
			sendTyping()
		}
	}
}
