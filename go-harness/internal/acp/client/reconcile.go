package client

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func promptStatePath(root, parentSessionID, agent string) (string, error) {
	if !filepath.IsAbs(root) || parentSessionID == "" || !agentName.MatchString(agent) {
		return "", fmt.Errorf("%w: invalid external ACP state identity", harness.ErrInvalidInput)
	}
	identity := sha256.Sum256([]byte(parentSessionID))
	return filepath.Join(root, "acp-agent-sessions", fmt.Sprintf("%x", identity[:]), agent+".json"), nil
}

// PendingPrompt returns the exact unresolved remote prompt identity. It does
// not query the remote agent or infer whether its effects occurred.
func PendingPrompt(root, parentSessionID, agent string) (harness.ExternalPromptState, error) {
	path, err := promptStatePath(root, parentSessionID, agent)
	if err != nil {
		return harness.ExternalPromptState{}, err
	}
	lockAny, _ := invocationLocks.LoadOrStore(path, &sync.Mutex{})
	lock := lockAny.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	raw, err := readSessionState(path)
	if err != nil {
		return harness.ExternalPromptState{}, err
	}
	var state sessionState
	if err := json.Unmarshal(raw, &state); err != nil {
		return harness.ExternalPromptState{}, err
	}
	if state.SessionID == "" || state.Policy == "" {
		return harness.ExternalPromptState{}, errors.New("external ACP session state is corrupt")
	}
	if state.Pending == nil {
		return harness.ExternalPromptState{}, os.ErrNotExist
	}
	return harness.ExternalPromptState{Agent: agent, SessionID: state.SessionID, PromptID: state.Pending.ID, PromptSHA: state.Pending.PromptSHA, ArgumentsSHA: state.Pending.ArgumentsSHA, RunID: state.Pending.RunID, ToolCallID: state.Pending.CallID}, nil
}

// AcknowledgePrompt clears a pending marker only for the reviewed identity.
// The caller must first verify the matching local terminal tool receipt.
func AcknowledgePrompt(root, parentSessionID, agent string, expected harness.ExternalPromptState) error {
	path, err := promptStatePath(root, parentSessionID, agent)
	if err != nil {
		return err
	}
	lockAny, _ := invocationLocks.LoadOrStore(path, &sync.Mutex{})
	lock := lockAny.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	raw, err := readSessionState(path)
	if err != nil {
		return err
	}
	var state sessionState
	if err := json.Unmarshal(raw, &state); err != nil {
		return err
	}
	if expected.Agent != agent || expected.PromptID == "" || state.SessionID != expected.SessionID || state.Policy == "" || state.Pending == nil || state.Pending.ID != expected.PromptID || state.Pending.PromptSHA != expected.PromptSHA || state.Pending.ArgumentsSHA != expected.ArgumentsSHA || state.Pending.RunID != expected.RunID || state.Pending.CallID != expected.ToolCallID {
		return harness.ErrCommandUncertain
	}
	state.Pending = nil
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return writeSessionState(path, data)
}
