package deerflow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
	"github.com/omengye/deerflow-acp/go-harness/internal/mcp"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
)

type backgroundResourceAttempt struct {
	controls chan bt.ControlRequest
	done     chan struct{}
	once     sync.Once
	draining atomic.Bool
}

func (a *backgroundResourceAttempt) drain(reason string) {
	a.draining.Store(true)
	select {
	case a.controls <- bt.ControlRequest{Kind: bt.ControlDrain, Reason: reason}:
	default:
	}
}

type executionMCPLease struct {
	ref   harness.MCPResourceRef
	lease *mcp.Lease
}
type executionMCPLeaseKey struct{}
type admittedMCPAttemptKey struct{}
type admittedMCPAttempt struct {
	id      string
	attempt *backgroundResourceAttempt
}
type backgroundResourceRecord struct {
	ref                      harness.MCPResourceRef
	version                  int64
	state, generation, owner string
}

func resourceHash(value any) string {
	data, _ := json.Marshal(value)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
func sameMCPRef(a, b *harness.MCPResourceRef) bool { return resourceHash(a) == resourceHash(b) }
func resourceRef(data json.RawMessage) (*harness.MCPResourceRef, error) {
	var state extensionState
	if json.Unmarshal(data, &state) != nil || state.Version != 1 {
		return nil, harness.ErrInvalidInput
	}
	if state.MCPGeneration != "" {
		return nil, harness.ErrResourcesIncompatible
	}
	ref := state.MCPResources
	if ref != nil && (ref.Version != 1 || ref.SessionID == "" || ref.OriginGeneration == "" || ref.Descriptor.Version != 1 || len(ref.Descriptor.ConfigDigest) != 64 || len(ref.Descriptor.SchemaDigest) != 64 || len(ref.Descriptor.Servers) == 0) {
		return nil, harness.ErrResourcesIncompatible
	}
	return ref, nil
}

func executionMCPResources(ctx context.Context, manager *mcp.Manager, req harness.RunRequest, pinned json.RawMessage) (*harness.MCPResourceRef, *mcp.Lease, error) {
	var ref *harness.MCPResourceRef
	var err error
	if pinned != nil {
		ref, err = resourceRef(pinned)
		if err != nil {
			return nil, nil, err
		}
	}
	if borrowed, ok := ctx.Value(executionMCPLeaseKey{}).(executionMCPLease); ok {
		if !sameMCPRef(ref, &borrowed.ref) {
			return nil, nil, harness.ErrResourcesIncompatible
		}
		if err = borrowed.lease.Check(); err != nil {
			reservation, admitted := ctx.Value(admittedMCPAttemptKey{}).(admittedMCPAttempt)
			if !admitted || reservation.attempt == nil {
				return nil, nil, harness.ErrResourcesWaiting
			}
			reservation.attempt.drain("MCP resource lease ended during construction")
		}
		return ref, borrowed.lease, nil
	}
	if manager == nil {
		if ref != nil {
			return nil, nil, harness.ErrResourcesWaiting
		}
		return nil, nil, nil
	}
	if pinned == nil {
		descriptor, generation, err := manager.Describe(ctx, req.Session.ID)
		if err != nil {
			return nil, nil, err
		}
		if generation == "" {
			return nil, nil, nil
		}
		ref = &harness.MCPResourceRef{Version: 1, SessionID: req.Session.ID, OriginGeneration: generation, Descriptor: descriptor}
		ref.HostRecipeID = manager.HostRecipeID(req.Session.ID)
	}
	if ref == nil {
		generation, err := manager.Generation(ctx, req.Session.ID)
		if err != nil {
			return nil, nil, err
		}
		if generation != "" {
			return nil, nil, harness.ErrResourcesIncompatible
		}
		return nil, nil, nil
	}
	if ref.SessionID != req.Session.ID {
		return nil, nil, harness.ErrPermissionDenied
	}
	generation := ref.OriginGeneration
	if ref.HostRecipeID != "" {
		if manager.HostRecipeID(ref.SessionID) != ref.HostRecipeID {
			return nil, nil, harness.ErrResourcesWaiting
		}
		if manager.HostRecipeStatus(*ref) == "restorable" {
			generation, err = manager.RestoreHost(ctx, *ref, req.Session.CWD)
			if err != nil {
				return nil, nil, err
			}
		}
	}
	if hooks := interaction.FromContext(ctx); hooks != nil && hooks.InputSource != nil && hooks.InputSource.ResourceGeneration != "" {
		generation = hooks.InputSource.ResourceGeneration
	}
	lease, err := manager.Acquire(ctx, ref.SessionID, ref.Descriptor, generation)
	if err != nil {
		return nil, nil, errors.Join(harness.ErrResourcesWaiting, err)
	}
	return ref, lease, nil
}

func (h *backgroundHost) initResourceStore(ctx context.Context) error {
	h.resourceAttempts = map[string]*backgroundResourceAttempt{}
	_, err := h.store.DB().ExecContext(ctx, `CREATE TABLE IF NOT EXISTS harness_background_resources(
task_id TEXT PRIMARY KEY REFERENCES harness_background_specs(task_id) ON DELETE CASCADE,
source_session TEXT NOT NULL,ref BLOB NOT NULL,ref_sha TEXT NOT NULL,
version INTEGER NOT NULL,state TEXT NOT NULL,generation TEXT NOT NULL,owner TEXT NOT NULL);`)
	return err
}

type resourceReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readBackgroundResource(ctx context.Context, db resourceReader, id string) (backgroundResourceRecord, error) {
	var r backgroundResourceRecord
	var data []byte
	var source, sha string
	err := db.QueryRowContext(ctx, `SELECT source_session,ref,ref_sha,version,state,generation,owner FROM harness_background_resources WHERE task_id=?`, id).Scan(&source, &data, &sha, &r.version, &r.state, &r.generation, &r.owner)
	if errors.Is(err, sql.ErrNoRows) {
		return r, harness.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if len(data) > 128<<10 || json.Unmarshal(data, &r.ref) != nil || resourceHash(r.ref) != sha || r.ref.SessionID != source || r.version < 1 || r.generation == "" || r.owner == "" || r.state != "authorized" && r.state != "waiting" {
		return r, harness.ErrResourcesIncompatible
	}
	return r, nil
}
func (h *backgroundHost) checkResourceRecord(ctx context.Context, r backgroundResourceRecord) error {
	if r.state != "authorized" || h.mcp == nil {
		return harness.ErrResourcesWaiting
	}
	if r.ref.HostRecipeID != "" && h.mcp.HostRecipeID(r.ref.SessionID) != r.ref.HostRecipeID {
		return harness.ErrResourcesWaiting
	}
	generation, err := h.mcp.Generation(ctx, r.ref.SessionID)
	if err != nil || generation == "" || generation != r.generation {
		return harness.ErrResourcesWaiting
	}
	return nil
}

func (h *backgroundHost) BeginResourceAttempt(ctx context.Context, id string) (context.Context, func(), func(), error) {
	h.resourceAdmission.RLock()
	var once sync.Once
	release := func() { once.Do(h.resourceAdmission.RUnlock) }
	if err := ctx.Err(); err != nil {
		release()
		return ctx, nil, nil, err
	}
	if err := h.checkResourceRecordForTask(ctx, h.store.DB(), id, false); err != nil {
		release()
		return ctx, nil, nil, err
	}
	var data []byte
	if err := h.store.DB().QueryRowContext(ctx, `SELECT binding FROM harness_background_specs WHERE task_id=?`, id).Scan(&data); err != nil {
		release()
		return ctx, nil, nil, err
	}
	var binding background.Binding
	if json.Unmarshal(data, &binding) != nil || binding.TaskID != id {
		release()
		return ctx, nil, nil, harness.ErrResourcesIncompatible
	}
	_, spec, err := h.specs.Load(ctx, binding)
	if err != nil {
		release()
		return ctx, nil, nil, err
	}
	admitted, attempt, finish, err := h.openResources(ctx, binding, spec)
	if err != nil {
		release()
		return ctx, nil, nil, err
	}
	if attempt != nil {
		admitted = context.WithValue(admitted, admittedMCPAttemptKey{}, admittedMCPAttempt{id: id, attempt: attempt})
	}
	return admitted, release, finish, nil
}

func (h *backgroundHost) checkResourceRecordForTask(ctx context.Context, db resourceReader, id string, active bool) error {
	r, err := readBackgroundResource(ctx, db, id)
	if errors.Is(err, harness.ErrNotFound) {
		var data []byte
		if err = db.QueryRowContext(ctx, `SELECT payload FROM harness_background_specs WHERE task_id=?`, id).Scan(&data); err != nil {
			return err
		}
		var spec hr.BackgroundExecutionSpec
		if json.Unmarshal(data, &spec) != nil {
			return harness.ErrResourcesIncompatible
		}
		ref, err := resourceRef(spec.Extension)
		if err != nil {
			return err
		}
		if ref != nil {
			return harness.ErrResourcesIncompatible
		}
		return nil
	}
	if err != nil {
		return err
	}
	if active {
		// A joined drain owns its original lease until native cancellation has
		// checkpointed. Dispatch is fenced, while native controls decide the
		// active call's safe boundary rather than a generic permission failure.
		h.resourceMu.Lock()
		a := h.resourceAttempts[id]
		if a != nil && a.draining.Load() {
			h.resourceMu.Unlock()
			return nil
		}
		h.resourceMu.Unlock()
	}
	err = h.checkResourceRecord(ctx, r)
	if active && errors.Is(err, harness.ErrResourcesWaiting) {
		h.resourceMu.Lock()
		a := h.resourceAttempts[id]
		if a != nil {
			a.drain("MCP resource lease is no longer available")
			err = nil
		}
		h.resourceMu.Unlock()
	}
	return err
}
func (h *backgroundHost) checkResourceTx(ctx context.Context, tx *sql.Tx, id string) error {
	return h.checkResourceRecordForTask(ctx, tx, id, false)
}
func (h *backgroundHost) CheckResources(ctx context.Context, b background.Binding) error {
	return h.checkResourceRecordForTask(ctx, h.store.DB(), b.TaskID, false)
}
func (h *backgroundHost) validateResources(ctx context.Context, b background.Binding, spec hr.BackgroundExecutionSpec) error {
	ref, err := resourceRef(spec.Extension)
	if err != nil || ref == nil {
		return err
	}
	if ref.SessionID != b.RootSession() {
		return harness.ErrPermissionDenied
	}
	r, err := readBackgroundResource(ctx, h.store.DB(), b.TaskID)
	if err == nil {
		if !sameMCPRef(ref, &r.ref) {
			return harness.ErrResourcesIncompatible
		}
		if reservation, ok := ctx.Value(admittedMCPAttemptKey{}).(admittedMCPAttempt); ok && reservation.id == b.TaskID {
			if scope, active := background.ScopeFromContext(ctx); active && scope.Binding.TaskID == b.TaskID {
				return nil
			}
		}
		return h.checkResourceRecord(ctx, r)
	}
	if !errors.Is(err, harness.ErrNotFound) {
		return err
	}
	if _, proposed := ctx.Value(backgroundSpecKey{}).(hr.BackgroundExecutionSpec); !proposed {
		return harness.ErrResourcesIncompatible
	}
	if b.ParentTaskID != "" || b.ContinuationOf != "" {
		parent := b.ParentTaskID
		if b.ContinuationOf != "" {
			parent = b.ContinuationOf
		}
		r, err = readBackgroundResource(ctx, h.store.DB(), parent)
		if err != nil {
			return err
		}
		if !sameMCPRef(ref, &r.ref) {
			return harness.ErrResourcesIncompatible
		}
		return h.checkResourceRecord(ctx, r)
	}
	if h.mcp == nil {
		return harness.ErrResourcesWaiting
	}
	descriptor, generation, err := h.mcp.Describe(ctx, ref.SessionID)
	if err != nil {
		return err
	}
	if resourceHash(descriptor) != resourceHash(ref.Descriptor) || generation != ref.OriginGeneration && !(ref.HostRecipeID != "" && h.mcp.HostRecipeID(ref.SessionID) == ref.HostRecipeID && h.mcp.HostRecipeStatus(*ref) == "restorable") {
		return harness.ErrResourcesWaiting
	}
	return nil
}
func (h *backgroundHost) bindResourcesTx(ctx context.Context, tx *sql.Tx, b background.Binding, spec hr.BackgroundExecutionSpec) error {
	ref, err := resourceRef(spec.Extension)
	if err != nil || ref == nil {
		return err
	}
	if ref.SessionID != b.RootSession() {
		return harness.ErrPermissionDenied
	}
	generation, owner := ref.OriginGeneration, b.SubmittedBy
	if ref.HostRecipeID != "" && h.mcp != nil && h.mcp.HostRecipeID(ref.SessionID) == ref.HostRecipeID && h.mcp.HostRecipeStatus(*ref) == "restorable" {
		generation, err = h.mcp.Generation(ctx, ref.SessionID)
		if err != nil {
			return err
		}
	}
	if b.ParentTaskID != "" || b.ContinuationOf != "" {
		parent := b.ParentTaskID
		if b.ContinuationOf != "" {
			parent = b.ContinuationOf
		}
		r, err := readBackgroundResource(ctx, tx, parent)
		if err != nil {
			return err
		}
		if !sameMCPRef(ref, &r.ref) {
			return harness.ErrResourcesIncompatible
		}
		if err = h.checkResourceRecord(ctx, r); err != nil {
			return err
		}
		generation, owner = r.generation, r.owner
	}
	r := backgroundResourceRecord{ref: *ref, version: 1, state: "authorized", generation: generation, owner: owner}
	if err = h.checkResourceRecord(ctx, r); err != nil {
		return err
	}
	data, _ := json.Marshal(ref)
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_background_resources(task_id,source_session,ref,ref_sha,version,state,generation,owner) VALUES(?,?,?,?,1,'authorized',?,?)`, b.TaskID, ref.SessionID, data, resourceHash(ref), generation, owner)
	return err
}

// Query is descriptive. It never renews authority just because the client
// successfully recreated an equivalent MCP transport.
func (h *backgroundHost) taskResources(ctx context.Context, actor harness.TaskActor, id string) (*harness.BackgroundResources, error) {
	b, err := h.service.BindingForTask(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	_, spec, err := h.specs.Load(ctx, b)
	if err != nil {
		return nil, err
	}
	ref, err := resourceRef(spec.Extension)
	if errors.Is(err, harness.ErrResourcesIncompatible) {
		return &harness.BackgroundResources{Status: "unreconstructable", Reason: "This older checkpoint has no rebuildable MCP resource descriptor."}, nil
	}
	if err != nil || ref == nil {
		return nil, err
	}
	r, err := readBackgroundResource(ctx, h.store.DB(), id)
	if err != nil {
		return nil, err
	}
	var status string
	if err = h.store.DB().QueryRowContext(ctx, `SELECT status FROM eino_background_tasks WHERE id=?`, id).Scan(&status); err != nil {
		return nil, err
	}
	terminal := status == "completed" || status == "failed" || status == "canceled"
	view := &harness.BackgroundResources{Version: r.version, Status: "waiting_connection", Reason: "Reconnect the original root session with its MCP servers, then authorize this task.", Descriptor: r.ref.Descriptor}
	if terminal {
		view.Reason = "Reconnect the original MCP resources to continue this task's work in a new execution. The finished task is not replayed."
	}
	if h.mcp == nil {
		return view, nil
	}
	if r.ref.HostRecipeID != "" {
		switch h.mcp.HostRecipeStatus(r.ref) {
		case "missing":
			view.Reason = "The saved host MCP recipe is unavailable. Restore its host configuration and restart the harness."
			return view, nil
		case "incompatible":
			view.Status, view.Reason = "incompatible", "The host MCP recipe differs from the saved connection configuration."
			return view, nil
		default:
			view.Reason = "The host MCP recipe is waiting for its transport. Reattach this workspace or restart the harness after restoring the host service."
		}
	}
	descriptor, generation, err := h.mcp.Describe(ctx, r.ref.SessionID)
	if err != nil || generation == "" {
		return view, nil
	}
	if resourceHash(descriptor) != resourceHash(r.ref.Descriptor) {
		view.Status = "incompatible"
		view.Reason = "Current MCP configuration or tool schemas differ from the saved task."
		return view, nil
	}
	hostOwned := r.ref.HostRecipeID != "" && h.mcp.HostRecipeID(r.ref.SessionID) == r.ref.HostRecipeID
	if r.state == "authorized" && r.generation == generation && (hostOwned || h.runtime.Coordinator.Authorize(r.ref.SessionID, r.owner) == nil) {
		view.Status = "ready"
		view.Reason = ""
		return view, nil
	}
	view.Status = "waiting_authorization"
	view.Reason = "The connection has changed. Explicitly authorize these saved resources before resuming."
	if terminal {
		view.Reason = "Authorize the replacement MCP connection for a continuation of this task's work. The finished task is not replayed."
	}
	view.CanAuthorize = status != "running"
	if status == "running" {
		view.Reason = "The prior attempt is still settling. Inspect and reconcile uncertain tool outcomes before continuing."
	}
	return view, nil
}

func (h *backgroundHost) authorizeResources(ctx context.Context, actor harness.TaskActor, id string, taskVersion, resourceVersion int64) error {
	if taskVersion < 1 || resourceVersion < 1 {
		return harness.ErrInvalidInput
	}
	b, err := h.service.BindingForTask(ctx, actor, id)
	if err != nil {
		return err
	}
	r, err := readBackgroundResource(ctx, h.store.DB(), id)
	if err != nil {
		return err
	}
	if r.ref.SessionID != actor.SessionID || b.RootSession() != actor.SessionID || h.mcp == nil {
		return harness.ErrPermissionDenied
	}
	if r.ref.HostRecipeID != "" && h.mcp.HostRecipeID(r.ref.SessionID) != r.ref.HostRecipeID {
		return harness.ErrResourcesWaiting
	}
	descriptor, generation, err := h.mcp.Describe(ctx, r.ref.SessionID)
	if err != nil || generation == "" {
		return harness.ErrResourcesWaiting
	}
	if resourceHash(descriptor) != resourceHash(r.ref.Descriptor) {
		return harness.ErrResourcesIncompatible
	}
	tx, err := h.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = h.runtime.Coordinator.Authorize(actor.SessionID, actor.OwnerID); err != nil {
		return err
	}
	fresh, err := readBackgroundResource(ctx, tx, id)
	if err != nil {
		return err
	}
	var version int64
	var status string
	if err = tx.QueryRowContext(ctx, `SELECT version,status FROM eino_background_tasks WHERE id=?`, id).Scan(&version, &status); err != nil {
		return err
	}
	if version != taskVersion || fresh.version != resourceVersion || !sameMCPRef(&fresh.ref, &r.ref) || status == "running" {
		return harness.ErrExecutionConflict
	}
	current, err := h.mcp.Generation(ctx, r.ref.SessionID)
	if err != nil || current != generation {
		return harness.ErrResourcesWaiting
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_background_resources SET version=version+1,state='authorized',generation=?,owner=? WHERE task_id=? AND version=?`, generation, actor.OwnerID, id, resourceVersion); err != nil {
		return err
	}
	return tx.Commit()
}

func (h *backgroundHost) releaseResourceSession(ctx context.Context, owner, sessionID string) error {
	h.resourceAdmission.Lock()
	var unlockOnce sync.Once
	unlock := func() { unlockOnce.Do(h.resourceAdmission.Unlock) }
	defer unlock()
	// Admission is blocked while each active attempt receives drain authority.
	// Parent transports remain alive until those attempts checkpoint and join.
	rows, err := h.store.DB().QueryContext(ctx, `SELECT task_id FROM harness_background_resources WHERE (source_session=? OR ?='') AND owner=? AND COALESCE(json_extract(ref,'$.hostRecipeId'),'')=''`, sessionID, sessionID, owner)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	var active []*backgroundResourceAttempt
	h.resourceMu.Lock()
	for _, id := range ids {
		if a := h.resourceAttempts[id]; a != nil {
			a.drain("MCP client disconnected")
			active = append(active, a)
		}
	}
	h.resourceMu.Unlock()
	_, err = h.store.DB().ExecContext(ctx, `UPDATE harness_background_resources SET state='waiting',version=version+1 WHERE (source_session=? OR ?='') AND owner=? AND state='authorized' AND COALESCE(json_extract(ref,'$.hostRecipeId'),'')=''`, sessionID, sessionID, owner)
	if err != nil {
		return err
	}
	unlock()
	for _, a := range active {
		select {
		case <-a.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (h *backgroundHost) openResources(ctx context.Context, b background.Binding, spec hr.BackgroundExecutionSpec) (context.Context, *backgroundResourceAttempt, func(), error) {
	ref, err := resourceRef(spec.Extension)
	if err != nil || ref == nil {
		return ctx, nil, func() {}, err
	}
	if reservation, ok := ctx.Value(admittedMCPAttemptKey{}).(admittedMCPAttempt); ok {
		borrowed, valid := ctx.Value(executionMCPLeaseKey{}).(executionMCPLease)
		if !valid || reservation.id != b.TaskID || !sameMCPRef(ref, &borrowed.ref) {
			return ctx, nil, nil, harness.ErrResourcesIncompatible
		}
		// The worker owns reservation.finish through the native final commit.
		// Attempt cleanup closes its borrower, while disconnect keeps waiting
		// until the saved checkpoint/lifecycle transition has also settled.
		return ctx, reservation.attempt, func() { _ = borrowed.lease.Close() }, nil
	}
	a := &backgroundResourceAttempt{controls: make(chan bt.ControlRequest, 1), done: make(chan struct{})}
	h.resourceMu.Lock()
	if h.resourceAttempts[b.TaskID] != nil {
		h.resourceMu.Unlock()
		return ctx, nil, nil, harness.ErrBusy
	}
	h.resourceAttempts[b.TaskID] = a
	h.resourceMu.Unlock()
	closeAttempt := func() {
		a.once.Do(func() {
			h.resourceMu.Lock()
			delete(h.resourceAttempts, b.TaskID)
			close(a.done)
			h.resourceMu.Unlock()
		})
	}
	r, err := readBackgroundResource(ctx, h.store.DB(), b.TaskID)
	if err == nil {
		err = h.checkResourceRecord(ctx, r)
	}
	if err != nil {
		closeAttempt()
		return ctx, nil, func() {}, err
	}
	lease, err := h.mcp.Acquire(ctx, ref.SessionID, ref.Descriptor, r.generation)
	if err != nil {
		closeAttempt()
		return ctx, nil, func() {}, errors.Join(harness.ErrResourcesWaiting, err)
	}
	go func() {
		select {
		case <-lease.Done:
			a.drain("MCP resource lease ended")
		case <-a.done:
		}
	}()
	finish := func() { _ = lease.Close(); closeAttempt() }
	return context.WithValue(ctx, executionMCPLeaseKey{}, executionMCPLease{ref: *ref, lease: lease}), a, finish, nil
}

func (h *backgroundHost) checkContinuationResources(ctx context.Context, tx *sql.Tx, b background.Binding, spec hr.BackgroundExecutionSpec) error {
	ref, err := resourceRef(spec.Extension)
	if err != nil || ref == nil {
		return err
	}
	r, err := readBackgroundResource(ctx, tx, b.TaskID)
	if err != nil {
		return err
	}
	if !sameMCPRef(ref, &r.ref) {
		return harness.ErrResourcesIncompatible
	}
	return h.checkResourceRecord(ctx, r)
}
func (h *backgroundHost) continuationResourceGeneration(ctx context.Context, tx *sql.Tx, b background.Binding, spec hr.BackgroundExecutionSpec) (string, error) {
	if err := h.checkContinuationResources(ctx, tx, b, spec); err != nil {
		return "", err
	}
	r, err := readBackgroundResource(ctx, tx, b.TaskID)
	if errors.Is(err, harness.ErrNotFound) {
		return "", nil
	}
	return r.generation, err
}

func (h *backgroundHost) prepareContinuationResourceLease(ctx context.Context, owner, sessionID, notificationID, runID string) (context.Context, func(), error) {
	noop := func() {}
	b, spec, found, err := h.runtime.ContinuationResourceSource(ctx, owner, sessionID, notificationID, runID)
	if err != nil || !found {
		return ctx, noop, err
	}
	ref, err := resourceRef(spec.Extension)
	if err != nil || ref == nil {
		return ctx, noop, err
	}
	r, err := readBackgroundResource(ctx, h.store.DB(), b.TaskID)
	if err != nil {
		return ctx, nil, err
	}
	if !sameMCPRef(ref, &r.ref) {
		return ctx, nil, harness.ErrResourcesIncompatible
	}
	if err = h.checkResourceRecord(ctx, r); err != nil {
		return ctx, nil, err
	}
	lease, err := h.mcp.Acquire(ctx, ref.SessionID, ref.Descriptor, r.generation)
	if err != nil {
		return ctx, nil, errors.Join(harness.ErrResourcesWaiting, err)
	}
	return context.WithValue(ctx, executionMCPLeaseKey{}, executionMCPLease{ref: *ref, lease: lease}), func() { _ = lease.Close() }, nil
}
