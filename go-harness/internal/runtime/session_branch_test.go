package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/assets"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

type branchAgent struct{ input []*schema.Message }

func (*branchAgent) Name(context.Context) string        { return "branch-fixture" }
func (*branchAgent) Description(context.Context) string { return "branch fixture" }
func (a *branchAgent) Run(_ context.Context, input *adk.AgentInput, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	a.input = append([]*schema.Message(nil), input.Messages...)
	iter, gen := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		defer gen.Close()
		gen.Send(&adk.AgentEvent{AgentName: "branch-fixture", Output: &adk.AgentOutput{MessageOutput: &adk.MessageVariant{Message: schema.AssistantMessage("answer", nil), Role: schema.Assistant}}})
	}()
	return iter
}
func branchFixture(t *testing.T) (context.Context, *sqlite.Store, *Service, harness.Session) {
	t.Helper()
	ctx := context.Background()
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "branch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { native.Close() })
	store, err := NewStore(ctx, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, nil, "fixture")
	x, err := service.NewSession(ctx, "owner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ctx, native, service, x
}
func branchTurn(t *testing.T, ctx context.Context, native *sqlite.Store, s *Service, x harness.Session, text string) *branchAgent {
	t.Helper()
	req := harness.RunRequest{Session: x, RunID: NewID(), InputID: NewID(), Input: []harness.Content{{Type: "text", Text: text}}}
	if err := s.Store.BeginRun(ctx, req); err != nil {
		t.Fatal(err)
	}
	agent := &branchAgent{}
	iter := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent, SessionID: x.ID, SessionStore: native}).Query(ctx, text)
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			t.Fatal(event.Err)
		}
	}
	if _, err := s.Store.Append(ctx, harness.RunEvent{SessionID: x.ID, RunID: req.RunID, Kind: "text_delta", Text: "answer"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Finish(ctx, req.RunID, "end_turn", nil); err != nil {
		t.Fatal(err)
	}
	return agent
}
func TestConversationForkAndRewindUseNativeHistory(t *testing.T) {
	ctx, native, s, x := branchFixture(t)
	branchTurn(t, ctx, native, s, x, "first")
	branchTurn(t, ctx, native, s, x, "second")
	branchTurn(t, ctx, native, s, x, "third")
	state, err := s.ConversationTurns(ctx, "owner", x.ID)
	if err != nil || len(state.Turns) != 3 {
		t.Fatalf("turns: %+v %v", state, err)
	}
	req := ConversationBranchRequest{SessionID: x.ID, ExpectedHeadEventID: state.HeadEventID, TargetEventID: state.Turns[0].EventID, OperationID: "fork-one"}
	fork, err := s.BranchConversation(ctx, "owner", req, false)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := s.BranchConversation(ctx, "owner", req, false)
	if err != nil || retry.SessionID != fork.SessionID {
		t.Fatalf("idempotence: %+v %v", retry, err)
	}
	forkSession, err := s.Load(ctx, "fork-owner", fork.SessionID, x.CWD, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	next := branchTurn(t, ctx, native, s, forkSession, "fork-next")
	if len(next.input) != 3 || next.input[0].Content != "first" || next.input[2].Content != "fork-next" {
		t.Fatalf("fork native context: %+v", next.input)
	}
	sourceNext := branchTurn(t, ctx, native, s, x, "source-next")
	if len(sourceNext.input) != 7 {
		t.Fatalf("source was truncated: %d", len(sourceNext.input))
	}
	state, err = s.ConversationTurns(ctx, "owner", x.ID)
	if err != nil {
		t.Fatal(err)
	}
	req = ConversationBranchRequest{SessionID: x.ID, ExpectedHeadEventID: state.HeadEventID, TargetEventID: state.Turns[0].EventID, OperationID: "rewind-one"}
	rewound, err := s.BranchConversation(ctx, "owner", req, true)
	if err != nil || len(rewound.Turns) != 1 {
		t.Fatalf("rewind: %+v %v", rewound, err)
	}
	next = branchTurn(t, ctx, native, s, x, "replacement")
	if len(next.input) != 3 || next.input[0].Content != "first" || next.input[2].Content != "replacement" {
		t.Fatalf("rewind native context: %+v", next.input)
	}
	page, err := s.Store.HistoryPage(ctx, x.ID, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range page.Events {
		for _, c := range e.Content {
			if c.Text == "second" || c.Text == "third" || c.Text == "source-next" {
				t.Fatal("inactive branch leaked into replay")
			}
		}
	}
	history, err := s.Store.History(ctx, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != len(page.Events) {
		t.Fatalf("unpaged history includes inactive branches: %d vs %d", len(history), len(page.Events))
	}
	searchReq := harness.RunRequest{Session: x, RunID: NewID(), InputID: NewID(), Input: []harness.Content{{Type: "text", Text: "search after rewind"}}}
	if err = s.Store.BeginRun(ctx, searchReq); err != nil {
		t.Fatal(err)
	}
	search, err := s.Store.SearchTaskHistory(ctx, searchReq, "second", "user", 0)
	if err != nil || len(search.Entries) != 0 {
		t.Fatalf("model search resurrected inactive turn: %+v %v", search, err)
	}
	var hiddenID int64
	if err = native.DB().QueryRow(`SELECT e.sequence FROM harness_events e JOIN harness_conversation_hidden_events h ON h.session_id=e.session_id AND e.sequence BETWEEN h.first_sequence AND h.last_sequence WHERE e.session_id=? AND json_extract(e.event,'$.kind')='user_message' LIMIT 1`, x.ID).Scan(&hiddenID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.ReadTaskHistory(ctx, searchReq, hiddenID, 0); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("direct model history read exposed inactive turn: %v", err)
	}
	if err = s.Store.Finish(ctx, searchReq.RunID, "cancelled", nil); err != nil {
		t.Fatal(err)
	}
	var count int
	s.Store.db.QueryRow(`SELECT COUNT(*) FROM harness_runs WHERE session_id=?`, x.ID).Scan(&count)
	if count != 6 {
		t.Fatalf("rewind removed execution evidence: %d", count)
	}
	req.OperationID = "stale-head"
	if _, err = s.BranchConversation(ctx, "owner", req, false); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatalf("stale head admitted: %v", err)
	}
	state, _ = s.ConversationTurns(ctx, "owner", x.ID)
	_, err = s.BranchConversation(ctx, "owner", ConversationBranchRequest{SessionID: x.ID, ExpectedHeadEventID: state.HeadEventID, OperationID: "clear"}, true)
	if err != nil {
		t.Fatal(err)
	}
	next = branchTurn(t, ctx, native, s, x, "fresh")
	if len(next.input) != 1 || next.input[0].Content != "fresh" {
		t.Fatalf("empty rewind kept context: %+v", next.input)
	}
}

func TestConversationCaptureBackfillsLegacyTurnsBeforeNewTurn(t *testing.T) {
	ctx, native, s, source := branchFixture(t)
	branchTurn(t, ctx, native, s, source, "legacy-first")
	branchTurn(t, ctx, native, s, source, "legacy-second")
	if _, err := native.DB().Exec(`DELETE FROM harness_conversation_turns WHERE session_id=?`, source.ID); err != nil {
		t.Fatal(err)
	}
	branchTurn(t, ctx, native, s, source, "first-after-upgrade")
	state, err := s.ConversationTurns(ctx, "owner", source.ID)
	if err != nil || len(state.Turns) != 3 {
		t.Fatalf("legacy turns lost after new completion: %+v %v", state, err)
	}
	fork, err := s.BranchConversation(ctx, "owner", ConversationBranchRequest{SessionID: source.ID, ExpectedHeadEventID: state.HeadEventID, TargetEventID: state.Turns[0].EventID, OperationID: "legacy-fork"}, false)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load(ctx, "fork-owner", fork.SessionID, source.CWD, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	next := branchTurn(t, ctx, native, s, loaded, "continue")
	if len(next.input) != 3 || next.input[0].Content != "legacy-first" {
		t.Fatalf("legacy turn boundary changed context: %+v", next.input)
	}
}

func TestConversationForkIdempotenceDoesNotRecreateDeletedTarget(t *testing.T) {
	ctx, native, s, source := branchFixture(t)
	branchTurn(t, ctx, native, s, source, "original")
	state, err := s.ConversationTurns(ctx, "owner", source.ID)
	if err != nil {
		t.Fatal(err)
	}
	req := ConversationBranchRequest{SessionID: source.ID, ExpectedHeadEventID: state.HeadEventID, TargetEventID: state.Turns[0].EventID, OperationID: "deleted-fork"}
	fork, err := s.BranchConversation(ctx, "owner", req, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DeleteSession(ctx, fork.SessionID); err != nil {
		t.Fatal(err)
	}
	if result, err := s.BranchConversation(ctx, "owner", req, false); !errors.Is(err, harness.ErrNotFound) || result.SessionID != "" {
		t.Fatalf("deleted fork identity replayed: %+v %v", result, err)
	}
	var sessions, operations int
	if err = native.DB().QueryRow(`SELECT COUNT(*) FROM harness_sessions`).Scan(&sessions); err != nil || sessions != 1 {
		t.Fatalf("fork recreated after deletion: %d %v", sessions, err)
	}
	if err = native.DB().QueryRow(`SELECT COUNT(*) FROM harness_conversation_operations WHERE session_id=? AND operation_id=?`, source.ID, req.OperationID).Scan(&operations); err != nil || operations != 1 {
		t.Fatalf("idempotency tombstone lost: %d %v", operations, err)
	}
}

func TestConversationForkCopiesAssetsWithoutExecutableState(t *testing.T) {
	ctx, native, s, x := branchFixture(t)
	store, err := assets.NewStore(ctx, filepath.Join(t.TempDir(), "assets"), native.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s.Assets = store
	ref, err := store.StoreToolOutput(ctx, x, "immutable historical tool output")
	if err != nil {
		t.Fatal(err)
	}
	marker := fmt.Sprintf("[Tool output: 32 UTF-8 bytes; session snapshot %q. Read with read_tool_output using id=%q and offset=0.]\nread %s", ref.ID, ref.ID, harness.AssetURI(ref))
	branchTurn(t, ctx, native, s, x, marker)
	state, err := s.ConversationTurns(ctx, "owner", x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.db.Exec(`CREATE TRIGGER fail_branch_operation BEFORE INSERT ON harness_conversation_operations BEGIN SELECT RAISE(ABORT,'fixture branch failure'); END`); err != nil {
		t.Fatal(err)
	}
	failedCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_, err = s.BranchConversation(failedCtx, "owner", ConversationBranchRequest{SessionID: x.ID, ExpectedHeadEventID: state.HeadEventID, TargetEventID: state.Turns[0].EventID, OperationID: "asset-fork"}, false)
	cancel()
	if err == nil {
		t.Fatal("partial fork reported success")
	}
	var owners int
	if err = s.Store.db.QueryRow(`SELECT COUNT(*) FROM harness_assets`).Scan(&owners); err != nil || owners != 1 {
		t.Fatalf("failed fork retained orphan owner: %d %v", owners, err)
	}
	if _, err = s.Store.db.Exec(`DROP TRIGGER fail_branch_operation`); err != nil {
		t.Fatal(err)
	}
	fork, err := s.BranchConversation(ctx, "owner", ConversationBranchRequest{SessionID: x.ID, ExpectedHeadEventID: state.HeadEventID, TargetEventID: state.Turns[0].EventID, OperationID: "asset-fork"}, false)
	if err != nil {
		t.Fatal(err)
	}
	var data []byte
	if err = s.Store.db.QueryRow(`SELECT metadata FROM harness_assets WHERE session_id=?`, fork.SessionID).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var copied harness.AssetRef
	if err = json.Unmarshal(data, &copied); err != nil {
		t.Fatal(err)
	}
	if copied.ID == ref.ID || copied.SessionID != fork.SessionID {
		t.Fatalf("shared mutable ownership: %+v", copied)
	}
	if _, err = s.DeleteAttachedSession(ctx, "owner", x.ID); err != nil {
		t.Fatal(err)
	}
	content, err := store.Resolve(ctx, fork.SessionID, copied)
	if err != nil || string(content) != "immutable historical tool output" {
		t.Fatalf("source deletion broke fork: %s %v", content, err)
	}
	loaded, err := s.Load(ctx, "fork-owner", fork.SessionID, x.CWD, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	next := branchTurn(t, ctx, native, s, loaded, "continue")
	if strings.Contains(next.input[0].Content, harness.AssetURI(ref)) || !strings.Contains(next.input[0].Content, harness.AssetURI(copied)) {
		t.Fatalf("native reference not remapped: %s", next.input[0].Content)
	}
	if strings.Contains(next.input[0].Content, ref.ID) || !strings.Contains(next.input[0].Content, "id="+fmt.Sprintf("%q", copied.ID)) {
		t.Fatalf("tool output reader identity not remapped: %s", next.input[0].Content)
	}
	chunk, err := store.ReadToolOutput(ctx, loaded, copied.ID, 0, 4096)
	if err != nil || chunk.Text != "immutable historical tool output" {
		t.Fatalf("fork reader cannot resolve its historical snapshot: %+v %v", chunk, err)
	}
	var grants int
	if err = s.Store.db.QueryRow(`SELECT COUNT(*) FROM harness_approvals WHERE session_id=?`, fork.SessionID).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("fork copied authorization: %d %v", grants, err)
	}
}
