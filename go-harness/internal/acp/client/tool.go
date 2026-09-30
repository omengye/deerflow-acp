package client

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

var agentName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type callbackKey struct{}

// WithCallbacks scopes external updates and reverse permission decisions to
// the active Eino tool call. No callback or decision is retained by the tool.
func WithCallbacks(ctx context.Context, callbacks Callbacks) context.Context {
	return context.WithValue(ctx, callbackKey{}, callbacks)
}

func CallbacksFromContext(ctx context.Context) (Callbacks, bool) {
	callbacks, ok := ctx.Value(callbackKey{}).(Callbacks)
	return callbacks, ok
}

type invokeInput struct {
	Agent  string `json:"agent" jsonschema:"description=Name of a configured external ACP agent"`
	Prompt string `json:"prompt" jsonschema:"description=Task to delegate to the selected agent"`
}

type invokeTool struct {
	info   *schema.ToolInfo
	root   string
	parent harness.Session
	agents map[string]harness.ACPAgentConfig
}

var invocationLocks sync.Map // state path -> *sync.Mutex, serialized across runs

// Tool exposes only host configured agent names. A nonempty map is an explicit
// opt-in; the caller omits this tool entirely in read-only/plan sessions.
func Tool(root string, parent harness.Session, agents map[string]harness.ACPAgentConfig) (tool.InvokableTool, error) {
	if !filepath.IsAbs(root) || parent.ID == "" || len(agents) == 0 || len(agents) > 16 {
		return nil, errors.New("external ACP tool requires an absolute state root, parent session and 1..16 agents")
	}
	copyAgents := make(map[string]harness.ACPAgentConfig, len(agents))
	for name, config := range agents {
		if !agentName.MatchString(name) || config.Timeout < 0 || config.TimeoutSeconds < 0 || config.TimeoutSeconds > 86400 || len(config.Description) > 512 {
			return nil, fmt.Errorf("invalid external ACP agent %q", name)
		}
		if !filepath.IsAbs(config.Command) {
			return nil, fmt.Errorf("external ACP agent %q command must be absolute", name)
		}
		info, err := os.Stat(config.Command)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("external ACP agent %q command is unavailable", name)
		}
		for key, value := range config.Env {
			if !environmentName.MatchString(key) || strings.ContainsRune(value, 0) {
				return nil, fmt.Errorf("external ACP agent %q has invalid environment", name)
			}
		}
		config.Args = append([]string(nil), config.Args...)
		config.Env = cloneEnv(config.Env)
		copyAgents[name] = config
	}
	names := make([]string, 0, len(copyAgents))
	for name := range copyAgents {
		names = append(names, name)
	}
	sort.Strings(names)
	description := "Delegate a task to an explicitly configured external ACP agent. Available agents: "
	for i, name := range names {
		if i > 0 {
			description += "; "
		}
		description += name
		if note := strings.TrimSpace(copyAgents[name].Description); note != "" {
			description += " (" + strings.ReplaceAll(note, "\n", " ") + ")"
		}
	}
	description += ". The agent has its own workspace and may request additional permission."
	info, err := utils.GoStruct2ToolInfo[invokeInput]("invoke_acp_agent", description)
	if err != nil {
		return nil, err
	}
	return &invokeTool{info: info, root: root, parent: parent, agents: copyAgents}, nil
}

func cloneEnv(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func (t *invokeTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }

type sessionState struct {
	SessionID string         `json:"session_id"`
	Policy    string         `json:"policy"`
	Pending   *pendingPrompt `json:"pending,omitempty"`
}

type pendingPrompt struct {
	ID           string `json:"id"`
	PromptSHA    string `json:"prompt_sha"`
	RunID        string `json:"run_id,omitempty"`
	CallID       string `json:"call_id,omitempty"`
	ArgumentsSHA string `json:"arguments_sha,omitempty"`
}

func (t *invokeTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	var input invokeInput
	decoder := json.NewDecoder(strings.NewReader(args))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return "", err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return "", errors.New("invoke_acp_agent expects one JSON object")
	}
	config, ok := t.agents[input.Agent]
	if !ok {
		return "", errors.New("external ACP agent is not configured")
	}
	if strings.TrimSpace(input.Prompt) == "" || len(input.Prompt) > 128<<10 {
		return "", errors.New("external ACP prompt must be 1..131072 bytes")
	}
	identity := sha256.Sum256([]byte(t.parent.ID))
	segment := fmt.Sprintf("%x", identity[:])
	workspace := filepath.Join(t.root, "acp-workspaces", segment, input.Agent)
	stateFile := filepath.Join(t.root, "acp-agent-sessions", segment, input.Agent+".json")
	lockAny, _ := invocationLocks.LoadOrStore(stateFile, &sync.Mutex{})
	lock := lockAny.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	if err := os.MkdirAll(workspace, 0700); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(stateFile), 0700); err != nil {
		return "", err
	}
	policyBytes, err := json.Marshal(struct {
		Config   harness.ACPAgentConfig
		Duration time.Duration
	}{config, config.Timeout})
	if err != nil {
		return "", err
	}
	policy := fmt.Sprintf("%x", sha256.Sum256(policyBytes))
	var state sessionState
	if raw, readErr := readSessionState(stateFile); readErr == nil {
		if err := json.Unmarshal(raw, &state); err != nil {
			return "", fmt.Errorf("external ACP session state is corrupt: %w", err)
		}
		if state.Policy != policy || state.SessionID == "" || len(state.SessionID) > 512 {
			return "", errors.New("external ACP agent configuration changed; existing session requires reconciliation")
		}
		if state.Pending != nil {
			return "", fmt.Errorf("%w: external ACP prompt %s has no committed terminal receipt; reconcile the remote session before sending another prompt", harness.ErrCommandUncertain, state.Pending.ID)
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return "", readErr
	}
	timeout := config.Timeout
	if timeout == 0 {
		timeout = time.Duration(config.TimeoutSeconds) * time.Second
		if timeout == 0 {
			timeout = 10 * time.Minute
		}
	}
	callbacks, _ := CallbacksFromContext(ctx)
	promptID := rand.Text()
	promptDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(input.Prompt)))
	previousReady := callbacks.SessionReady
	callbacks.SessionReady = func(callCtx context.Context, sessionID string) error {
		if previousReady != nil {
			if err := previousReady(callCtx, sessionID); err != nil {
				return err
			}
		}
		data, err := json.Marshal(sessionState{SessionID: sessionID, Policy: policy})
		if err != nil {
			return err
		}
		if err := writeSessionState(stateFile, data); err != nil {
			return err
		}
		state.SessionID = sessionID
		state.Policy = policy
		return nil
	}
	previousPromptReady := callbacks.PromptReady
	callbacks.PromptReady = func(callCtx context.Context, sessionID string) error {
		if previousPromptReady != nil {
			if err := previousPromptReady(callCtx, sessionID); err != nil {
				return err
			}
		}
		state.SessionID = sessionID
		state.Pending = &pendingPrompt{ID: promptID, PromptSHA: promptDigest, RunID: callbacks.ParentRunID, CallID: callbacks.ParentToolCallID, ArgumentsSHA: callbacks.ParentArgumentsSHA}
		data, err := json.Marshal(state)
		if err != nil {
			return err
		}
		return writeSessionState(stateFile, data)
	}
	result, runErr := Run(ctx, Config{Command: config.Command, Args: config.Args, Env: config.Env, Timeout: timeout}, workspace, state.SessionID, input.Prompt, callbacks)
	if runErr != nil {
		if result.PromptDispatched {
			return "", errors.Join(runErr, harness.ErrCommandUncertain)
		}
		return "", runErr
	}
	complete := func(context.Context) error {
		lock.Lock()
		defer lock.Unlock()
		raw, err := readSessionState(stateFile)
		if err != nil {
			return err
		}
		var current sessionState
		if err := json.Unmarshal(raw, &current); err != nil {
			return err
		}
		if current.SessionID != result.SessionID || current.Policy != policy || current.Pending == nil || current.Pending.ID != promptID || current.Pending.PromptSHA != promptDigest {
			return fmt.Errorf("%w: external ACP prompt completion state changed", harness.ErrCommandUncertain)
		}
		current.Pending = nil
		data, err := json.Marshal(current)
		if err != nil {
			return err
		}
		return writeSessionState(stateFile, data)
	}
	if callbacks.RegisterCompletion != nil {
		callbacks.RegisterCompletion(complete)
	} else {
		// Direct callers have no harness receipt; their own successful return
		// is the only available completion boundary.
		state.Pending = nil
		data, err := json.Marshal(state)
		if err != nil {
			return "", err
		}
		if err := writeSessionState(stateFile, data); err != nil {
			return "", errors.Join(err, harness.ErrCommandUncertain)
		}
	}
	return result.Text, nil
}

func writeSessionState(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".external-session-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func readSessionState(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return nil, err
	}
	if len(raw) > 4096 {
		return nil, errors.New("external ACP session state exceeds 4 KiB")
	}
	return raw, nil
}
