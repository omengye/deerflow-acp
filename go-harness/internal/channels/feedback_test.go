package channels

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
)

func feedbackFixture(t *testing.T, call func(context.Context, string, any, any) error) (context.Context, *feedback) {
	t.Helper()
	m, i := fixture(t, nil)
	i.manifest.Descriptor.IndependentFeedback = true
	descriptor := i.manifest.Descriptor
	m.start = func(context.Context, Manifest) (transport, channelproto.Descriptor, error) {
		return &fakeTransport{call: call}, descriptor, nil
	}
	ctx, f := m.withFeedback(context.Background(), i, i.auth, i.cfg, message("a", "alice"))
	f.callTimeout, f.drainTimeout, f.typingInterval = 100*time.Millisecond, 200*time.Millisecond, 10*time.Millisecond
	t.Cleanup(f.close)
	return ctx, f
}

func progressEvent(phase string) channelproto.ToolProgress {
	return channelproto.ToolProgress{RunID: "run", ToolCallID: "PRIVATE-CALL-ID", ToolName: "读取文件", Phase: phase, Status: "completed"}
}

func TestFeedbackStreamsLifecycleAndKeepsTypingAlive(t *testing.T) {
	var mu sync.Mutex
	var progress []channelproto.Progress
	var typingStates []bool
	var configs int
	ends, heartbeat, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	ctx, f := feedbackFixture(t, func(_ context.Context, method string, in, out any) error {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "typing.prepare":
			configs++
			return assign(out, channelproto.TypingResult{Ticket: "ticket"})
		case "typing":
			v := in.(channelproto.Typing)
			if v.Ticket != "ticket" {
				t.Error("typing ticket changed")
			}
			typingStates = append(typingStates, v.Active)
			if len(typingStates) == 2 && v.Active {
				close(heartbeat)
			}
			if !v.Active {
				close(stopped)
			}
		case "progress":
			v := in.(channelproto.Progress)
			progress = append(progress, v)
			if v.Tool.Phase == "end" {
				close(ends)
			}
		default:
			t.Errorf("unexpected feedback method %s", method)
		}
		return assign(out, map[string]bool{"sent": true})
	})
	StartFeedback(ctx)
	PublishToolProgress(ctx, progressEvent("start"))
	PublishToolProgress(ctx, progressEvent("start")) // tool_execute after tool_start
	PublishToolProgress(ctx, progressEvent("end"))
	PublishToolProgress(ctx, progressEvent("end"))
	waitChannel(t, ends, "tool result before execution finishes")
	waitChannel(t, heartbeat, "typing keepalive during execution")
	if !FinishFeedback(ctx) {
		t.Fatal("acknowledged native progress unnecessarily fell back")
	}
	f.close()
	waitChannel(t, stopped, "typing cancel")
	PublishToolProgress(ctx, progressEvent("start"))
	mu.Lock()
	defer mu.Unlock()
	if len(progress) != 2 || configs != 1 || typingStates[len(typingStates)-1] {
		t.Fatal(progress, configs, typingStates)
	}
	if progress[0].Tool.Phase != "start" || progress[1].Tool.Phase != "end" || progress[0].Tool.ToolCallID != progress[1].Tool.ToolCallID || progress[0].Key == progress[1].Key {
		t.Fatal("tool identity, order or send keys lost", progress)
	}
	for _, v := range progress {
		if v.Tool.RunID != "run" || strings.Contains(v.Tool.ToolCallID, "PRIVATE") || v.Message.Text != "" || v.Message.Context != "reply-token" {
			t.Fatal("feedback leaked prompt/opaque ID or lost context", v)
		}
	}
}

func TestFeedbackTypingFailureDoesNotDisableNativeProgress(t *testing.T) {
	for _, failedMethod := range []string{"typing.prepare", "typing"} {
		t.Run(failedMethod, func(t *testing.T) {
			ctx, f := feedbackFixture(t, func(_ context.Context, method string, in, out any) error {
				if method == failedMethod {
					return errors.New("typing unavailable")
				}
				if method == "typing.prepare" {
					return assign(out, channelproto.TypingResult{Ticket: "ticket"})
				}
				return assign(out, map[string]bool{"sent": true})
			})
			StartFeedback(ctx)
			PublishToolProgress(ctx, progressEvent("start"))
			PublishToolProgress(ctx, progressEvent("end"))
			if !FinishFeedback(ctx) {
				t.Fatal("typing failure disabled independent native progress")
			}
			f.close()
		})
	}
}

func TestFeedbackFailurePreservesExecutionFinalAnswerAndAdmission(t *testing.T) {
	for _, failure := range []string{"network", "unacknowledged", "timeout", "auth_expired", "startup", "old_plugin"} {
		t.Run(failure, func(t *testing.T) {
			var runs, feedbackCalls atomic.Int32
			m, i := fixture(t, func(ctx context.Context, _, _, _, _ string, _ func(string) error) (automation.Outcome, error) {
				StartFeedback(ctx)
				PublishToolProgress(ctx, progressEvent("start"))
				runs.Add(1) // tool execution must not depend on delivery
				PublishToolProgress(ctx, progressEvent("end"))
				if FinishFeedback(ctx) {
					t.Error("unconfirmed progress suppressed the text fallback")
				}
				return automation.Outcome{Status: "completed", Text: "工具调用摘要\n读取文件：已完成\n\nanswer"}, nil
			})
			i.manifest.Descriptor.IndependentFeedback = failure != "old_plugin"
			i.cfg.Options = map[string]string{"typing": "false"}
			descriptor := i.manifest.Descriptor
			m.start = func(context.Context, Manifest) (transport, channelproto.Descriptor, error) {
				if failure == "old_plugin" {
					t.Error("old plugin received unsupported feedback")
				}
				if failure == "startup" {
					return nil, channelproto.Descriptor{}, errors.New("cannot start feedback process")
				}
				return &fakeTransport{call: func(ctx context.Context, method string, _, out any) error {
					feedbackCalls.Add(1)
					if method != "progress" {
						t.Error("feedback used answer/poll method", method)
					}
					switch failure {
					case "timeout":
						<-ctx.Done()
						return ctx.Err()
					case "auth_expired":
						return channelproto.ErrAuthExpired
					case "unacknowledged":
						return assign(out, map[string]bool{"sent": false})
					default:
						return errors.New("uncertain send")
					}
				}}, descriptor, nil
			}
			sent := make(chan struct{}, 2)
			i.proc = &fakeTransport{call: func(_ context.Context, method string, input, out any) error {
				if method == "poll" {
					return assign(out, channelproto.Batch{})
				}
				if method != "send" || !strings.Contains(input.(channelproto.Send).Text, "工具调用摘要") || !strings.Contains(input.(channelproto.Send).Text, "answer") {
					t.Error("final answer or fallback lost", method, input)
				}
				sent <- struct{}{}
				return assign(out, map[string]bool{"sent": true})
			}}
			for _, id := range []string{"first", "next"} {
				if err := m.accept(context.Background(), i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{message(id, "alice")}}); err != nil {
					t.Fatal(err)
				}
			}
			m.startWorker(i)
			waitChannel(t, sent, "final reply despite failed progress")
			waitChannel(t, sent, "next request not fenced by failed progress")
			waitMessageState(t, m, "next", "sent")
			m.stopWorker(i)
			if runs.Load() != 2 || feedbackCalls.Load() > 2 || i.auth.Account != "bot" || len(i.auth.Secret) == 0 {
				t.Fatal("optional failure retried progress, cancelled execution or revoked auth", runs.Load(), feedbackCalls.Load())
			}
			var unknown int
			if err := m.db.QueryRow(`SELECT COUNT(*) FROM harness_channel_messages WHERE status='unknown'`).Scan(&unknown); err != nil || unknown != 0 {
				t.Fatal("optional delivery fenced conversation", unknown, err)
			}
		})
	}
}

func TestFinalAnswerDoesNotWaitForTypingCleanup(t *testing.T) {
	cleanup, release, sent := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	var cleanupExited atomic.Bool
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	m, i := fixture(t, func(ctx context.Context, _, _, _, _ string, _ func(string) error) (automation.Outcome, error) {
		StartFeedback(ctx)
		PublishToolProgress(ctx, progressEvent("start"))
		PublishToolProgress(ctx, progressEvent("end"))
		if !FinishFeedback(ctx) {
			t.Error("unexpected fallback")
		}
		return automation.Outcome{Status: "completed", Text: "answer"}, nil
	})
	i.manifest.Descriptor.IndependentFeedback = true
	descriptor := i.manifest.Descriptor
	m.start = func(context.Context, Manifest) (transport, channelproto.Descriptor, error) {
		return &fakeTransport{call: func(ctx context.Context, method string, in, out any) error {
			if method == "typing.prepare" {
				return assign(out, channelproto.TypingResult{Ticket: "ticket"})
			}
			if method == "typing" && !in.(channelproto.Typing).Active {
				defer cleanupExited.Store(true)
				close(cleanup)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return assign(out, map[string]bool{"sent": true})
		}}, descriptor, nil
	}
	i.proc = &fakeTransport{call: func(_ context.Context, method string, in, out any) error {
		if method == "poll" {
			return assign(out, channelproto.Batch{})
		}
		if in.(channelproto.Send).Text != "answer" {
			t.Error("final answer changed")
		}
		if cleanupExited.Load() {
			t.Error("final answer waited for optional typing cleanup")
		}
		close(sent)
		return assign(out, map[string]bool{"sent": true})
	}}
	if err := m.accept(context.Background(), i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{message("a", "alice")}}); err != nil {
		t.Fatal(err)
	}
	m.startWorker(i)
	waitChannel(t, cleanup, "typing cleanup entered")
	waitChannel(t, sent, "answer while typing cleanup is blocked")
	unblock()
	m.stopWorker(i)
}

func TestFeedbackQueueOverflowNeverBlocksProducer(t *testing.T) {
	entered := make(chan struct{})
	ctx, f := feedbackFixture(t, func(ctx context.Context, method string, _, _ any) error {
		if method != "progress" {
			return errors.New("unexpected method")
		}
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	})
	f.typing = false
	StartFeedback(ctx)
	PublishToolProgress(ctx, progressEvent("start"))
	waitChannel(t, entered, "slow progress started")
	published := make(chan struct{})
	go func() {
		defer close(published)
		for n := range feedbackQueueLimit + 10 {
			v := progressEvent("start")
			v.ToolCallID = fmt.Sprint(n)
			PublishToolProgress(ctx, v)
		}
	}()
	waitChannel(t, published, "bounded nonblocking producer")
	if !f.failedProgress() || len(f.queue) > feedbackQueueLimit || FinishFeedback(ctx) {
		t.Fatal("overflow failed to enable fallback")
	}
}

func TestFeedbackStopDropsQueuedProgressAndCancelsTyping(t *testing.T) {
	entered, stopped := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	ctx, f := feedbackFixture(t, func(ctx context.Context, method string, in, out any) error {
		switch method {
		case "typing.prepare":
			return assign(out, channelproto.TypingResult{Ticket: "ticket"})
		case "typing":
			if !in.(channelproto.Typing).Active {
				close(stopped)
			}
		case "progress":
			calls.Add(1)
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		}
		return assign(out, map[string]bool{"sent": true})
	})
	StartFeedback(ctx)
	PublishToolProgress(ctx, progressEvent("start"))
	waitChannel(t, entered, "inflight progress")
	PublishToolProgress(ctx, progressEvent("end"))
	f.cancel()
	f.close()
	waitChannel(t, stopped, "typing cancelled after stop with a fresh process")
	if calls.Load() != 1 || FinishFeedback(ctx) {
		t.Fatal("cancelled progress was replayed or acknowledged")
	}
}

func TestFeedbackModesAndLazyStart(t *testing.T) {
	for _, mode := range []string{"native", "summary"} {
		ctx, f := feedbackFixture(t, func(_ context.Context, method string, _, out any) error {
			t.Errorf("command without model execution sent feedback: %s", method)
			return assign(out, map[string]bool{"sent": true})
		})
		f.native = mode == "native"
		if FinishFeedback(ctx) {
			t.Fatal("unstarted feedback reported delivery")
		}
		StartFeedback(ctx)
		f.close()
	}
	for _, options := range []map[string]string{{"progress_mode": "invalid"}, {"typing": "sometimes"}} {
		if validateFeedbackOptions(options) == nil {
			t.Fatal("invalid feedback settings accepted")
		}
	}
	ctx, f := feedbackFixture(t, func(_ context.Context, method string, _, out any) error {
		if method == "progress" {
			t.Error("summary mode sent native progress")
		}
		return assign(out, channelproto.TypingResult{Ticket: "ticket", Sent: true})
	})
	f.native = false
	StartFeedback(ctx)
	PublishToolProgress(ctx, progressEvent("start"))
	if FinishFeedback(ctx) {
		t.Fatal("summary mode suppressed text fallback")
	}
}

func TestFeedbackClosesIncompleteToolWithoutClaimingSuccess(t *testing.T) {
	var result channelproto.ToolProgress
	ctx, f := feedbackFixture(t, func(_ context.Context, method string, in, out any) error {
		if method == "progress" && in.(channelproto.Progress).Tool.Phase == "end" {
			result = in.(channelproto.Progress).Tool
		}
		return assign(out, map[string]bool{"sent": true})
	})
	f.typing = false
	StartFeedback(ctx)
	PublishToolProgress(ctx, progressEvent("start"))
	if !FinishFeedback(ctx) || result.Status != "unknown" || result.ToolName != "读取文件" {
		t.Fatal("incomplete tool left active or falsely completed", result)
	}
}

func TestFeedbackIdentityChangeFencesRemainingSends(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var current atomic.Bool
	current.Store(true)
	var calls atomic.Int32
	ctx, f := feedbackFixture(t, func(ctx context.Context, method string, _, out any) error {
		if method != "progress" {
			t.Error(method)
		}
		calls.Add(1)
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return assign(out, map[string]bool{"sent": true})
	})
	f.typing, f.current = false, current.Load
	StartFeedback(ctx)
	PublishToolProgress(ctx, progressEvent("start"))
	waitChannel(t, entered, "progress in flight")
	PublishToolProgress(ctx, progressEvent("end"))
	current.Store(false)
	close(release)
	if FinishFeedback(ctx) || calls.Load() != 1 {
		t.Fatal("feedback crossed identity/workspace/revision fence", calls.Load())
	}
}
