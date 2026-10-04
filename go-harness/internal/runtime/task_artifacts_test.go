package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/assets"
)

type taskArtifactFixture struct {
	host    *backgroundHostFixture
	service *Service
	actor   harness.TaskActor
	request harness.TaskArtifactReadRequest
	ref     harness.AssetRef
	data    []byte
}

func newTaskArtifactFixture(t *testing.T) *taskArtifactFixture {
	t.Helper()
	ctx := context.Background()
	host := newBackgroundHostFixture(t)
	if err := host.create(); err != nil {
		t.Fatal(err)
	}
	host.start(t)
	if err := host.begin(host.scope); err != nil {
		t.Fatal(err)
	}
	if _, err := agents.NewController(host.store.db); err != nil {
		t.Fatal(err)
	}
	store, err := assets.NewStore(ctx, t.TempDir(), host.native.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	request, _, err := host.host.Load(ctx, host.binding)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(request.Session.CWD, ".deerflow", "outputs")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte{0, 1, 2, 255, 65, 66, 67, 68, 69, 70, 71}
	if err = os.WriteFile(filepath.Join(dir, "result.bin"), data, 0600); err != nil {
		t.Fatal(err)
	}
	host.receipt(t, "present", "present_files", true)
	staged, err := store.StageArtifacts(ctx, request.Session, request.RunID, "present", []string{"result.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = host.store.Append(ctx, harness.RunEvent{SessionID: request.Session.ID, RunID: request.RunID, ToolCallID: "present", ToolName: "present_files", Kind: "tool_end", Status: "completed"}, store); err != nil {
		t.Fatal(err)
	}
	service := NewService(host.store, nil, host.spec.Parent.Model)
	service.Assets = store
	actor := harness.TaskActor{SessionID: host.binding.RootSession(), OwnerID: "owner"}
	if _, err = service.Coordinator.Attach(actor.SessionID, actor.OwnerID); err != nil {
		t.Fatal(err)
	}
	ref := *staged[0].Asset
	return &taskArtifactFixture{host: host, service: service, actor: actor, request: harness.TaskArtifactReadRequest{RootID: host.binding.RootBudgetID, NodeID: host.binding.RoleID(), ArtifactID: ref.ID, Limit: 4}, ref: ref, data: data}
}

func TestTaskArtifactReadChunksAuthorizationAndWireShape(t *testing.T) {
	f := newTaskArtifactFixture(t)
	ctx := context.Background()
	request := f.request
	var joined []byte
	for {
		out, err := f.service.ReadTaskArtifact(ctx, f.actor, request)
		if err != nil {
			t.Fatal(err)
		}
		if out.Offset != request.Offset || out.NextOffset != request.Offset+int64(len(out.Data)) || out.Artifact.ID != f.ref.ID || out.Artifact.Name != f.ref.Name || out.SHA256 != f.ref.SHA256 || out.Encoding != "base64" {
			t.Fatalf("bad metadata %+v", out)
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err = json.Unmarshal(encoded, &wire); err != nil {
			t.Fatal(err)
		}
		if wire["data"] != base64.StdEncoding.EncodeToString(out.Data) {
			t.Fatalf("binary wire data %s", encoded)
		}
		for _, private := range []string{f.ref.SessionID, f.host.spec.Parent.CWD, ".blob", "workspace", "assetRef"} {
			if strings.Contains(string(encoded), private) {
				t.Fatalf("private metadata %q in %s", private, encoded)
			}
		}
		joined = append(joined, out.Data...)
		request.Offset, request.ExpectedSHA256 = out.NextOffset, out.SHA256
		if out.EOF {
			break
		}
	}
	if !bytes.Equal(joined, f.data) {
		t.Fatalf("joined %x", joined)
	}
	out, err := f.service.ReadTaskArtifact(ctx, f.actor, request)
	if err != nil || !out.EOF || len(out.Data) != 0 {
		t.Fatalf("EOF %+v %v", out, err)
	}
	badOwner := f.actor
	badOwner.OwnerID = "unattached"
	if _, err = f.service.ReadTaskArtifact(ctx, badOwner, f.request); !errors.Is(err, harness.ErrNotAttached) {
		t.Fatalf("owner %v", err)
	}
	private := harness.TaskActor{SessionID: f.host.binding.ChildSessionID, OwnerID: "private"}
	if _, err = f.service.Coordinator.Attach(private.SessionID, private.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.ReadTaskArtifact(ctx, private, f.request); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("private child %v", err)
	}
	for _, field := range []string{"root", "node", "artifact", "root-node"} {
		bad := f.request
		switch field {
		case "root":
			bad.RootID = "missing"
		case "node":
			bad.NodeID = "missing"
		case "artifact":
			bad.ArtifactID = "missing"
		case "root-node":
			bad.NodeID = "root/" + bad.RootID
		}
		if out, err = f.service.ReadTaskArtifact(ctx, f.actor, bad); err == nil || len(out.Data) != 0 {
			t.Fatalf("invalid attribution %s: %+v %v", field, out, err)
		}
	}
	bad := f.request
	bad.ExpectedSHA256 = strings.Repeat("0", 64)
	if _, err = f.service.ReadTaskArtifact(ctx, f.actor, bad); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatalf("explicit hash at offset zero %v", err)
	}
	_, release, err := f.service.Coordinator.Begin(ctx, f.actor.SessionID, f.actor.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.ReadTaskArtifact(ctx, f.actor, f.request); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("active root read %v", err)
	}
	release()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if out, err = f.service.ReadTaskArtifact(cancelled, f.actor, f.request); !errors.Is(err, context.Canceled) || len(out.Data) != 0 {
		t.Fatalf("cancelled read %+v %v", out, err)
	}
}

func TestTaskArtifactReadInputValidation(t *testing.T) {
	valid := harness.TaskArtifactReadRequest{RootID: "root", NodeID: "node", ArtifactID: "asset"}
	if limit, err := validateTaskArtifactRead(valid); err != nil || limit != harness.DefaultTaskArtifactReadLimit {
		t.Fatalf("default limit %d: %v", limit, err)
	}
	for _, field := range []string{"root", "node", "artifact", "path", "negative-offset", "overflow", "negative-limit", "large-limit", "missing-hash", "bad-hash", "short-hash"} {
		t.Run(field, func(t *testing.T) {
			r := valid
			switch field {
			case "root":
				r.RootID = ""
			case "node":
				r.NodeID = ""
			case "artifact":
				r.ArtifactID = ""
			case "path":
				r.ArtifactID = "C:\\private"
			case "negative-offset":
				r.Offset = -1
			case "overflow":
				r.Offset = math.MaxInt64
				r.ExpectedSHA256 = strings.Repeat("0", 64)
			case "negative-limit":
				r.Limit = -1
			case "large-limit":
				r.Limit = harness.MaxTaskArtifactReadLimit + 1
			case "missing-hash":
				r.Offset = 1
			case "bad-hash":
				r.ExpectedSHA256 = strings.Repeat("z", 64)
			case "short-hash":
				r.ExpectedSHA256 = "00"
			}
			if _, err := validateTaskArtifactRead(r); !errors.Is(err, harness.ErrInvalidInput) {
				t.Fatalf("invalid request accepted: %v", err)
			}
		})
	}
}

func TestTaskArtifactReadRequiresCommittedArtifactKind(t *testing.T) {
	f := newTaskArtifactFixture(t)
	ctx := context.Background()
	if _, err := f.host.store.db.Exec(`DELETE FROM harness_artifacts WHERE asset_id=?`, f.ref.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ReadTaskArtifact(ctx, f.actor, f.request); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("unregistered asset %v", err)
	}
	if _, err := f.host.store.db.Exec(`INSERT INTO harness_artifacts(session_id,run_id,tool_call_id,asset_id) VALUES(?,?,?,?)`, f.ref.SessionID, f.host.binding.TaskID, "present", f.ref.ID); err != nil {
		t.Fatal(err)
	}
	ref := f.ref
	ref.Kind = harness.AssetFile
	data, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.host.store.db.Exec(`UPDATE harness_assets SET metadata=? WHERE id=?`, data, ref.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.ReadTaskArtifact(ctx, f.actor, f.request); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("input kind retrieved %v", err)
	}
}

func TestTaskArtifactReadRetainsHistoricalLogicalNodeAttribution(t *testing.T) {
	f := newTaskArtifactFixture(t)
	ctx := context.Background()
	// Install only the newer native task's projection here. Continuation admission
	// has dedicated tests; this verifies that readers do not substitute the newest
	// task's private session when resolving an earlier immutable artifact.
	newer := f.host.binding
	newer.TaskID, newer.ChildSessionID = "continued-task", "continued-task/session"
	newer.OriginToolCallID = "continued-call"
	newer.InvocationID, newer.ContinuationOf = f.request.NodeID, f.host.binding.TaskID
	if _, err := f.host.native.Tasks().Create(ctx, &bt.CreateTaskRequest{Spec: bt.Spec{ID: newer.TaskID, ExecutorKey: "fixture", SessionID: newer.ParentSessionID}, LeaseExpiryPolicy: bt.LeaseExpiryFail}); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(newer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.host.store.db.Exec(`INSERT INTO harness_background_bindings(task_id,parent_session_id,child_session_id,origin_run_id,origin_tool_call_id,intent_hash,payload) VALUES(?,?,?,?,?,?,?)`, newer.TaskID, newer.ParentSessionID, newer.ChildSessionID, newer.OriginRunID, newer.OriginToolCallID, "", encoded); err != nil {
		t.Fatal(err)
	}
	node, err := f.service.TaskTreeNode(ctx, f.actor, f.request.RootID, f.request.NodeID)
	if err != nil || node.TaskID != newer.TaskID {
		t.Fatalf("latest projection %+v: %v", node, err)
	}
	out, err := f.service.ReadTaskArtifact(ctx, f.actor, f.request)
	if err != nil || !bytes.Equal(out.Data, f.data[:f.request.Limit]) {
		t.Fatalf("historical artifact %+v: %v", out, err)
	}
}
