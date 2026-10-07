package adapter

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type toolState struct {
	Title  string
	Status string
}
type Progress struct {
	queue   chan map[string]any
	finish  chan string
	done    chan struct{}
	cancel  context.CancelFunc
	buzz    *BuzzCLI
	message Pending
}

func NewProgress(ctx context.Context, buzz *BuzzCLI, message Pending) *Progress {
	if !buzz.Config.Observability.Progress {
		return nil
	}
	workerCtx, cancel := context.WithCancel(ctx)
	p := &Progress{queue: make(chan map[string]any, 128), finish: make(chan string, 1), done: make(chan struct{}), cancel: cancel, buzz: buzz, message: message}
	go p.worker(workerCtx)
	return p
}
func (p *Progress) Update(update map[string]any) {
	if p == nil {
		return
	}
	kind, _ := update["sessionUpdate"].(string)
	if kind != "tool_call" && kind != "tool_call_update" {
		return
	}
	copy := map[string]any{}
	for _, key := range []string{"toolCallId", "title", "status"} {
		copy[key] = update[key]
	}
	select {
	case p.queue <- copy:
	default:
	}
}
func (p *Progress) Finish(outcome string) {
	if p == nil {
		return
	}
	p.finish <- outcome
	timer := time.NewTimer(4 * time.Second)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		p.cancel()
		<-p.done
	}
	p.cancel()
}
func (p *Progress) worker(ctx context.Context) {
	defer close(p.done)
	tools := map[string]toolState{}
	order := []string{}
	event := ""
	disabled := false
	dirty := false
	apply := func(update map[string]any) {
		id, _ := update["toolCallId"].(string)
		if id == "" {
			return
		}
		old, exists := tools[id]
		if !exists {
			if len(tools) >= 500 {
				return
			}
			order = append(order, id)
		}
		if title, ok := update["title"].(string); ok && title != "" {
			old.Title = truncate(title, 160)
		}
		if old.Title == "" {
			old.Title = id
		}
		if status, ok := update["status"].(string); ok {
			old.Status = status
		}
		tools[id] = old
		dirty = true
	}
	render := func(heading string) string {
		lines := []string{heading}
		icons := map[string]string{"pending": "⏳", "in_progress": "🔄", "completed": "✅", "failed": "❌", "cancelled": "⏹️"}
		for _, id := range order[:min(len(order), p.buzz.Config.Observability.MaxTools)] {
			t := tools[id]
			icon := icons[t.Status]
			if icon == "" {
				icon = "•"
			}
			lines = append(lines, icon+" "+t.Title)
		}
		if len(order) > p.buzz.Config.Observability.MaxTools {
			lines = append(lines, fmt.Sprintf("…另有 %d 个工具调用", len(order)-p.buzz.Config.Observability.MaxTools))
		}
		return strings.Join(lines, "\n")
	}
	publish := func(heading string) {
		if disabled || !dirty || len(tools) == 0 {
			return
		}
		deadline, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		var err error
		if event == "" {
			event, err = p.buzz.Send(deadline, p.message.Channel, p.message.ID, render(heading))
		} else {
			err = p.buzz.Edit(deadline, event, render(heading))
		}
		if err != nil {
			disabled = true
		}
		dirty = false
	}
	ticker := time.NewTicker(max(seconds(p.buzz.Config.Observability.Interval), 50*time.Millisecond))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case update := <-p.queue:
			apply(update)
		case <-ticker.C:
			publish("🔧 DeerFlow 工具调用")
		case outcome := <-p.finish:
			for {
				select {
				case update := <-p.queue:
					apply(update)
				default:
					dirty = len(tools) > 0
					if outcome == "ok" {
						publish("✅ DeerFlow 工具调用完成")
					} else {
						publish("❌ DeerFlow 工具调用中断")
					}
					return
				}
			}
		}
	}
}
func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}
