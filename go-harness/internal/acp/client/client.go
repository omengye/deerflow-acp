// Package client runs explicitly configured ACP agents over local stdio.
// The subprocess is a trusted host choice; protocol requests from it are not.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	"github.com/omengye/deerflow-acp/go-harness/internal/sandbox"
)

// Config is selected by the host, never by model supplied arguments.
type Config struct {
	Command string
	Args    []string
	Env     map[string]string
	Timeout time.Duration
}

type PermissionRequest struct {
	SessionID string          `json:"sessionId"`
	ToolCall  json.RawMessage `json:"toolCall"`
	Options   []struct {
		OptionID string `json:"optionId"`
		Kind     string `json:"kind"`
	} `json:"options"`
}

type Callbacks struct {
	// Update receives the complete ACP update. The caller must validate resource
	// links before importing them into a local artifact store.
	Update func(context.Context, json.RawMessage) error
	// Permission returns an offered option ID. Nil means deny every request.
	Permission func(context.Context, PermissionRequest) (string, error)
}

type Result struct {
	SessionID  string
	Text       string
	StopReason string
}

// Run starts one allowed executable. A previous remote session is loaded when
// remoteSessionID is supplied; otherwise a new session is created. The caller
// persists that ID only after a successful session/new response.
func Run(ctx context.Context, cfg Config, workspace, remoteSessionID, prompt string, callbacks Callbacks) (result Result, returnErr error) {
	if err := validate(cfg, workspace, remoteSessionID, prompt); err != nil {
		return Result{}, err
	}
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = workspace
	cmd.Env = childEnvironment(cfg.Env)
	tree, err := sandbox.PrepareOwnedProcess(cmd)
	if err != nil {
		return Result{}, err
	}
	defer tree.Close()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Result{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, err
	}
	if err = cmd.Start(); err != nil {
		return Result{}, err
	}
	if err = tree.Attach(cmd); err != nil {
		_ = tree.Terminate()
		_ = cmd.Wait()
		return Result{}, fmt.Errorf("attach external process tree: %w", err)
	}
	stderrDone := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, stderr); close(stderrDone) }()
	var mu sync.Mutex
	var text strings.Builder
	sessionID := remoteSessionID
	promptActive := false
	fatal := make(chan error, 1)
	var peer *protocol.Peer
	report := func(err error) error {
		select {
		case fatal <- err:
		default:
		}
		_ = peer.Close()
		return err
	}
	peer = protocol.NewPeer(stdout, stdin, func(callCtx context.Context, method string, params json.RawMessage) (any, error) {
		switch method {
		case "session/update":
			var event struct {
				SessionID string          `json:"sessionId"`
				Update    json.RawMessage `json:"update"`
			}
			if err := json.Unmarshal(params, &event); err != nil {
				return nil, report(err)
			}
			mu.Lock()
			valid := sessionID == "" || event.SessionID == sessionID
			active := promptActive
			mu.Unlock()
			if !valid || len(event.Update) == 0 {
				return nil, report(errors.New("external agent sent an update for another session"))
			}
			if !active {
				return nil, nil
			}
			var chunk struct {
				SessionUpdate string `json:"sessionUpdate"`
				Content       struct {
					Type string `json:"type"`
					Text string `json:"text"`
					URI  string `json:"uri"`
				} `json:"content"`
			}
			if err := json.Unmarshal(event.Update, &chunk); err != nil {
				return nil, report(err)
			}
			if chunk.Content.Type == "resource_link" {
				if err := safeResourceLink(workspace, chunk.Content.URI); err != nil {
					return nil, report(err)
				}
			}
			if chunk.SessionUpdate == "agent_message_chunk" && chunk.Content.Type == "text" {
				mu.Lock()
				if text.Len()+len(chunk.Content.Text) > 4<<20 {
					mu.Unlock()
					return nil, report(errors.New("external agent text exceeds 4 MiB"))
				}
				text.WriteString(chunk.Content.Text)
				mu.Unlock()
			}
			if callbacks.Update != nil {
				if err := callbacks.Update(callCtx, event.Update); err != nil {
					return nil, report(err)
				}
			}
			return nil, nil
		case "session/request_permission":
			var request PermissionRequest
			if err := json.Unmarshal(params, &request); err != nil {
				return nil, report(err)
			}
			mu.Lock()
			valid := sessionID != "" && request.SessionID == sessionID
			mu.Unlock()
			if !valid || len(request.ToolCall) == 0 || len(request.ToolCall) > 64<<10 || len(request.Options) > 16 {
				return nil, report(errors.New("external permission request has invalid session or tool call"))
			}
			if callbacks.Permission == nil {
				return map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}, nil
			}
			selected, err := callbacks.Permission(callCtx, request)
			if err != nil {
				return nil, report(err)
			}
			for _, option := range request.Options {
				if selected != "" && selected == option.OptionID {
					return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": selected}}, nil
				}
			}
			return map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}, nil
		default:
			return nil, &protocol.Error{Code: protocol.MethodNotFound, Message: "Method not found"}
		}
	}, protocol.Options{})
	serveDone := make(chan error, 1)
	go func() { serveDone <- peer.Serve(context.Background()) }()
	defer func() {
		if ctx.Err() != nil && sessionID != "" {
			cancelCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			_ = peer.Notify(cancelCtx, "session/cancel", map[string]string{"sessionId": sessionID})
			cancel()
		}
		_ = peer.Close()
		terminationErr := tree.Terminate()
		_ = cmd.Wait()
		<-serveDone
		<-stderrDone
		select {
		case failure := <-fatal:
			returnErr = errors.Join(returnErr, failure)
		default:
		}
		if ctx.Err() != nil {
			returnErr = errors.Join(returnErr, ctx.Err())
		}
		returnErr = errors.Join(returnErr, terminationErr)
	}()
	initRequest := map[string]any{"protocolVersion": 1, "clientInfo": map[string]string{"name": "deerflow-go", "version": "0.1.0-dev"}, "clientCapabilities": map[string]any{}}
	var initialized struct {
		ProtocolVersion   int `json:"protocolVersion"`
		AgentCapabilities struct {
			LoadSession bool `json:"loadSession"`
		} `json:"agentCapabilities"`
	}
	if err := peer.Call(ctx, "initialize", initRequest, &initialized); err != nil {
		return Result{}, fmt.Errorf("initialize external agent: %w", err)
	}
	if initialized.ProtocolVersion != 1 {
		return Result{}, fmt.Errorf("external agent protocol version %d is unsupported", initialized.ProtocolVersion)
	}
	if remoteSessionID == "" {
		var created struct {
			SessionID string `json:"sessionId"`
		}
		if err := peer.Call(ctx, "session/new", map[string]any{"cwd": workspace, "mcpServers": []any{}}, &created); err != nil {
			return Result{}, fmt.Errorf("new external session: %w", err)
		}
		if created.SessionID == "" || len(created.SessionID) > 512 {
			return Result{}, errors.New("external agent returned an invalid session ID")
		}
		mu.Lock()
		sessionID = created.SessionID
		mu.Unlock()
		result.SessionID = created.SessionID
	} else {
		if !initialized.AgentCapabilities.LoadSession {
			return Result{}, errors.New("external agent does not support session/load")
		}
		if err := peer.Call(ctx, "session/load", map[string]any{"sessionId": sessionID, "cwd": workspace, "mcpServers": []any{}}, nil); err != nil {
			return Result{}, fmt.Errorf("load external session: %w", err)
		}
		if err := peer.DrainNotifications(ctx); err != nil {
			return Result{}, fmt.Errorf("drain external history: %w", err)
		}
		result.SessionID = sessionID
	}
	mu.Lock()
	promptActive = true
	mu.Unlock()
	var prompted struct {
		StopReason string `json:"stopReason"`
	}
	if err := peer.Call(ctx, "session/prompt", map[string]any{"sessionId": sessionID, "prompt": []map[string]string{{"type": "text", "text": prompt}}}, &prompted); err != nil {
		return result, fmt.Errorf("prompt external agent: %w", err)
	}
	if err := peer.DrainNotifications(ctx); err != nil {
		return result, fmt.Errorf("drain external updates: %w", err)
	}
	mu.Lock()
	result = Result{SessionID: sessionID, Text: text.String(), StopReason: prompted.StopReason}
	mu.Unlock()
	if result.StopReason == "" {
		return result, errors.New("external agent omitted prompt stop reason")
	}
	if result.StopReason != "end_turn" {
		return result, fmt.Errorf("external agent stopped with %q", result.StopReason)
	}
	return result, nil
}

func validate(cfg Config, workspace, remoteSessionID, prompt string) error {
	if !filepath.IsAbs(cfg.Command) || !filepath.IsAbs(workspace) || prompt == "" || len(prompt) > 128<<10 || cfg.Timeout < 0 || len(remoteSessionID) > 512 {
		return errors.New("external ACP command and workspace must be absolute; prompt, session ID and timeout must be valid")
	}
	info, err := os.Stat(cfg.Command)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("external ACP command is not a regular file")
	}
	info, err = os.Stat(workspace)
	if err != nil || !info.IsDir() {
		return errors.New("external ACP workspace is not a directory")
	}
	return nil
}

func childEnvironment(extra map[string]string) []string {
	allowed := []string{"PATH", "SystemRoot", "WINDIR", "TEMP", "TMP", "HOME", "USERPROFILE", "LANG", "LC_ALL"}
	values := make(map[string]string, len(allowed)+len(extra))
	for _, name := range allowed {
		if value, ok := os.LookupEnv(name); ok {
			values[name] = value
		}
	}
	for name, value := range extra {
		values[name] = value
	}
	result := make([]string, 0, len(values))
	for name, value := range values {
		result = append(result, name+"="+value)
	}
	return result
}

func safeResourceLink(workspace, raw string) error {
	if len(raw) == 0 || len(raw) > 4096 {
		return errors.New("external resource URI has invalid length")
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return errors.New("external resource URI is invalid")
	}
	switch u.Scheme {
	case "https", "http":
		if u.Host == "" {
			return errors.New("external resource URI has no host")
		}
		return nil
	case "file":
		if u.Host != "" || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("external file resource URI is invalid")
		}
		path := u.Path
		if runtime.GOOS == "windows" && len(path) >= 3 && path[0] == '/' && path[2] == ':' {
			path = path[1:]
		}
		path = filepath.FromSlash(path)
		if !filepath.IsAbs(path) {
			return errors.New("external file resource is not absolute")
		}
		root, err := filepath.EvalSymlinks(workspace)
		if err != nil {
			return err
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return errors.New("external file resource escapes its workspace")
		}
		return nil
	default:
		return errors.New("external resource URI scheme is unsupported")
	}
}
