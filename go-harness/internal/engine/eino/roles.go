package eino

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/prebuilt/deep"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
)

func (e *Engine) prepareRoleAgents(ctx context.Context, req harness.RunRequest, parentChat model.ToolCallingChatModel, available []tool.BaseTool, extensions RunExtensions, budget *runBudget, sink *eventSink, lifecycle *runIO, permissions harness.PermissionHandler, parentMedia *mediaProjection) ([]adk.Agent, error) {
	parentModel := req.Session.Model
	if parentModel == "" {
		parentModel = e.config.Model
	}
	parent := agents.Parent{Model: parentModel, Tools: e.config.ToolPolicy, Skills: extensions.SkillNames, MaxTurns: e.config.MaxIterations}
	return e.roleAgents(e.roles.Names(), parent, parentChat, 1, &roleResources{req, available, extensions, budget, sink, lifecycle, permissions, parentMedia})
}

type roleResources struct {
	request     harness.RunRequest
	tools       []tool.BaseTool
	extensions  RunExtensions
	budget      *runBudget
	sink        *eventSink
	lifecycle   *runIO
	permissions harness.PermissionHandler
	media       *mediaProjection
}

// Only instantiate a selected role. Building every possible nested graph at
// startup would expand exponentially with the configured role fanout.
func (e *Engine) roleAgents(names []string, parent agents.Parent, parentChat model.ToolCallingChatModel, depth int, resources *roleResources) ([]adk.Agent, error) {
	var children []adk.Agent
	for _, name := range names {
		spec, err := e.roles.Resolve(name, parent)
		if err != nil {
			return nil, err
		}
		children = append(children, e.newRole(spec, parent, parentChat, depth, resources, ""))
	}
	return children, nil
}

func (e *Engine) delegationLimits() (int, int) {
	concurrent, tasks := 2, 20
	if e.config.Subagents != nil {
		if e.config.Subagents.MaxConcurrent != nil {
			concurrent = *e.config.Subagents.MaxConcurrent
		}
		if e.config.Subagents.MaxTasksPerRoot != nil {
			tasks = *e.config.Subagents.MaxTasksPerRoot
		}
	}
	return concurrent, tasks
}

func (e *Engine) newRole(spec agents.Spec, parent agents.Parent, parentChat model.ToolCallingChatModel, depth int, resources *roleResources, id string) *timedRole {
	root := resources.request.RootBudgetID
	if root == "" {
		root = resources.request.RunID
	}
	concurrent, tasks := e.delegationLimits()
	role := &timedRole{spec: spec, agentName: spec.Name, lifecycle: resources.lifecycle, controller: e.config.Delegations, rootID: root, invocationID: id, depth: depth, maxConcurrent: concurrent, maxTasks: tasks, timeout: time.Duration(spec.TimeoutSeconds * float64(time.Second))}
	role.prepare = func(ctx context.Context, lifecycle *runIO) (adk.ResumableAgent, error) {
		private := *resources
		private.lifecycle = lifecycle
		return e.buildRoleAgent(ctx, spec, parent, parentChat, depth, &private, role.agentName)
	}
	return role
}

func (e *Engine) HasRoles() bool { return e.roles != nil && !e.config.DisableSubAgent }

// ResolveRole is used before durable background creation; both execution paths
// pin exactly the same recipe and intersect it with the parent's visible skills.
func (e *Engine) ResolveRole(name, selectedModel string, skills []string) (agents.Spec, error) {
	if !e.HasRoles() {
		return agents.Spec{}, harness.ErrBackgroundUnavailable
	}
	if name == "" {
		name = "general-purpose"
	}
	if selectedModel == "" {
		selectedModel = e.config.Model
	}
	return e.roles.Resolve(name, agents.Parent{Model: selectedModel, Tools: e.config.ToolPolicy, Skills: skills, MaxTurns: e.config.MaxIterations})
}

func (e *Engine) BindRoleTx(ctx context.Context, tx *sql.Tx, root, id string, spec agents.Spec) error {
	return e.BindPlacedRoleTx(ctx, tx, root, id, spec, RolePlacement{Depth: 1})
}

type RolePlacement struct {
	ParentID string `json:"parentId,omitempty"`
	Depth    int    `json:"depth"`
}

type rolePlacementKey struct{}
type resumedRoleIdentityKey struct{}

func (e *Engine) MaxRoleDepth() int {
	if e.config.Subagents != nil && e.config.Subagents.MaxDepth != nil {
		return *e.config.Subagents.MaxDepth
	}
	return 1
}

// ResolveDescendant applies the saved parent's role policy to the selected
// child. Role names in tool arguments never supply permissions or depth.
func (e *Engine) ResolveDescendant(name string, parent agents.Spec, depth int) (agents.Spec, error) {
	if name == "" {
		name = "general-purpose"
	}
	if !e.HasRoles() || depth < 1 || depth >= e.MaxRoleDepth() || !slices.Contains(parent.DelegateTo, name) {
		return agents.Spec{}, harness.ErrPermissionDenied
	}
	return e.roles.Resolve(name, agents.Parent{Model: parent.Model, Tools: parent.Tools, Skills: parent.Skills, MaxTurns: parent.MaxTurns, TimeoutSeconds: parent.TimeoutSeconds})
}

func (e *Engine) BindPlacedRoleTx(ctx context.Context, tx *sql.Tx, root, id string, spec agents.Spec, placement RolePlacement) error {
	if !e.HasRoles() || e.config.Delegations == nil {
		return harness.ErrBackgroundUnavailable
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	if placement.Depth < 1 || placement.Depth > e.MaxRoleDepth() || (placement.Depth == 1) != (placement.ParentID == "") {
		return harness.ErrPermissionDenied
	}
	concurrent, tasks := e.delegationLimits()
	return e.config.Delegations.RegisterTx(ctx, tx, agents.Invocation{RootID: root, ID: "background/" + id, ParentID: placement.ParentID, Depth: placement.Depth, Spec: spec}, concurrent, tasks)
}

func (e *Engine) PreparePlacedRoleAttempt(ctx context.Context, req harness.RunRequest, name string, pinned json.RawMessage, spec agents.Spec, placement RolePlacement, events harness.EventHandler, permissions harness.PermissionHandler, cancel func()) (*PreparedAttempt, error) {
	if placement.Depth < 1 || placement.Depth > e.MaxRoleDepth() || (placement.Depth == 1) != (placement.ParentID == "") {
		return nil, harness.ErrPermissionDenied
	}
	return e.PrepareRoleAttempt(context.WithValue(ctx, rolePlacementKey{}, placement), req, name, pinned, spec, events, permissions, cancel)
}

// The native host validates this existing invocation against its durable
// continuation source before assembly. A new native task is not a new role.
func (e *Engine) PrepareRestoredRoleAttempt(ctx context.Context, req harness.RunRequest, name string, pinned json.RawMessage, spec agents.Spec, placement RolePlacement, roleID string, events harness.EventHandler, permissions harness.PermissionHandler, cancel func()) (*PreparedAttempt, error) {
	if roleID == "" {
		return nil, harness.ErrPermissionDenied
	}
	return e.PreparePlacedRoleAttempt(context.WithValue(ctx, resumedRoleIdentityKey{}, roleID), req, name, pinned, spec, placement, events, permissions, cancel)
}

func (e *Engine) PrepareRoleAttempt(ctx context.Context, req harness.RunRequest, name string, pinned json.RawMessage, spec agents.Spec, events harness.EventHandler, permissions harness.PermissionHandler, cancel func()) (*PreparedAttempt, error) {
	if req.Session.ID == "" || req.RunID == "" || name == "" {
		return nil, harness.ErrInvalidInput
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if !e.HasRoles() {
		return nil, harness.ErrBackgroundUnavailable
	}
	b, err := e.newBudget(ctx, req)
	if err != nil {
		return nil, err
	}
	sink := &eventSink{request: req, callback: events, cancel: cancel, active: map[string]string{}, finished: map[string]bool{}}
	return e.prepareAgent(ctx, req, name, pinned, b, sink, newRunIO(ctx), permissions, &spec)
}

func (e *Engine) PrepareLegacyAttempt(ctx context.Context, req harness.RunRequest, name string, pinned json.RawMessage, events harness.EventHandler, permissions harness.PermissionHandler, cancel func()) (*PreparedAttempt, error) {
	legacy := *e
	legacy.config.Subagents = nil
	legacy.roles = nil
	return legacy.PrepareAttempt(ctx, req, name, pinned, events, permissions, cancel)
}

func roleContract(host, role string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(host+"/"+role)))
}

func (e *Engine) buildRoleAgent(ctx context.Context, spec agents.Spec, parent agents.Parent, parentChat model.ToolCallingChatModel, depth int, resources *roleResources, name string) (adk.ResumableAgent, error) {
	catalog, err := e.RoleCatalog()
	if err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, roleCatalogKey{}, pinnedRoleCatalog{catalog})
	req, available, extensions := resources.request, resources.tools, resources.extensions
	budget, sink, lifecycle, permissions, parentMedia := resources.budget, resources.sink, resources.lifecycle, resources.permissions, resources.media
	chat, err := e.roleModel(ctx, spec.Model, parent.Model, parentChat)
	if err != nil {
		return nil, fmt.Errorf("subagent %s: %w", name, err)
	}
	tuning, err := e.roleTuning(spec)
	if err != nil {
		return nil, fmt.Errorf("subagent %s: %w", name, err)
	}
	selected := make([]tool.BaseTool, 0, len(available))
	protected := map[string]bool{}
	ownedBackground := map[string]bool{}
	addTool := func(t tool.BaseTool) error {
		info, err := t.Info(ctx)
		if err != nil {
			return err
		}
		if !spec.Tools.Allows(info.Name) {
			return nil
		}
		// Background capabilities currently belong to the attached parent.
		// A role never borrows that TaskActor to create or manage siblings.
		switch info.Name {
		case "background_agent", "task_status", "task_wait", "task_cancel":
			if !ownedBackground[info.Name] {
				return nil
			}
		}
		if info.Name == "view_image" && !e.config.Media.SupportsVision(spec.Model) {
			return nil
		}
		selected = append(selected, t)
		protected[info.Name] = e.config.PermissionMode.RequiresPermission(info.Name) || req.Session.Mode == "plan" || req.Session.ApprovalMode == harness.ApprovalReadOnly
		return nil
	}
	for _, t := range available {
		info, err := t.Info(ctx)
		if err != nil {
			return nil, err
		}
		if info.Name == "read_skill_file" {
			continue
		}
		if info.Name == "view_image" && extensions.ModelToolFactory != nil {
			continue
		}
		if err := addTool(t); err != nil {
			return nil, err
		}
	}
	if extensions.RoleToolFactory != nil {
		roleTools, err := extensions.RoleToolFactory(ctx)
		if err != nil {
			return nil, err
		}
		for _, t := range roleTools {
			info, err := t.Info(ctx)
			if err != nil {
				return nil, err
			}
			if info == nil || info.Name == "" || ownedBackground[info.Name] {
				return nil, harness.ErrInvalidInput
			}
			ownedBackground[info.Name] = true
			if err = addTool(t); err != nil {
				return nil, err
			}
		}
	}
	if extensions.ModelToolFactory != nil {
		modelTools, err := extensions.ModelToolFactory(ctx, spec.Model)
		if err != nil {
			return nil, fmt.Errorf("subagent %s model tools: %w", name, err)
		}
		for _, t := range modelTools {
			if err := addTool(t); err != nil {
				return nil, err
			}
		}
	}
	handlers := append([]adk.ChatModelAgentMiddleware(nil), e.config.Handlers...)
	if extensions.SkillFactory != nil && spec.Tools.Allows("skill") {
		skillHandlers, skillTools, err := extensions.SkillFactory(ctx, spec.Skills)
		if err != nil {
			return nil, fmt.Errorf("subagent %s skills: %w", name, err)
		}
		handlers = append(handlers, skillHandlers...)
		for _, t := range skillTools {
			if err := addTool(t); err != nil {
				return nil, err
			}
		}
	} else if extensions.SkillFactory == nil && spec.Skills == nil {
		// Custom SDK extension factories without skills retain their own
		// middleware. Explicit skill selection requires a snapshot factory.
		handlers = append(handlers, extensions.Handlers...)
	} else if extensions.SkillFactory == nil && len(spec.Skills) > 0 {
		return nil, fmt.Errorf("subagent %s: skill selection requires a pinned skill factory", name)
	}
	for _, native := range []string{"skill", "write_todos", "task"} {
		if spec.Tools.Allows(native) && e.config.PermissionMode != "" {
			protected[native] = e.config.PermissionMode.RequiresPermission(native)
		}
	}
	media := *parentMedia
	media.model = spec.Model
	maxDepth := 1
	if e.config.Subagents.MaxDepth != nil {
		maxDepth = *e.config.Subagents.MaxDepth
	}
	var children []adk.Agent
	if depth < maxDepth && spec.Tools.Allows("task") {
		children, err = e.roleAgents(spec.DelegateTo, agents.Parent{Model: spec.Model, Tools: spec.Tools, Skills: spec.Skills, MaxTurns: spec.MaxTurns, TimeoutSeconds: spec.TimeoutSeconds}, chat, depth+1, resources)
		if err != nil {
			return nil, err
		}
	}
	if e.config.Compaction.Enabled {
		private := media
		private.importer, private.publish = nil, nil
		metered := &trackedModel{inner: chat, io: lifecycle, budget: budget, sink: sink, media: &private, settings: spec.ModelSettings, tuning: tuning, role: true}
		compaction, err := newCompactionMiddleware(ctx, e.config.Compaction, metered)
		if err != nil {
			return nil, err
		}
		handlers = append(handlers, compaction)
	}
	if e.config.Delegations.Durable() {
		handlers = append(handlers, &roleContextMiddleware{request: req})
	}
	handlers = append(handlers,
		&modelLifecycle{io: lifecycle, budget: budget, sink: sink, media: &media, settings: spec.ModelSettings, tuning: tuning, role: true},
		&toolMiddleware{sink: sink, permissions: permissions, protected: protected, policy: roleToolPolicy(spec, len(children) > 0, ownedBackground), permissionMode: e.config.PermissionMode, readOnly: req.Session.Mode == "plan" || req.Session.ApprovalMode == harness.ApprovalReadOnly, durableTaskWait: ownedBackground["task_wait"], io: lifecycle, budget: budget, images: e.config.ToolImageImporter, outputs: e.config.ToolOutputStore})
	child, err := deep.New(ctx, &deep.Config{
		Name: name, Description: spec.Description, Instruction: e.config.Instruction + "\n\n" + spec.Instruction + extensions.InstructionAppend,
		ChatModel: chat, MaxIteration: spec.MaxTurns,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: selected}},
		Handlers:    handlers, SubAgents: children, WithoutGeneralSubAgent: true, WithoutWriteTodos: !spec.Tools.Allows("write_todos"),
	})
	if err != nil {
		return nil, fmt.Errorf("create subagent %s: %w", name, err)
	}
	return child, nil
}

func roleToolPolicy(spec agents.Spec, canDelegate bool, owned ...map[string]bool) *harness.ToolPolicy {
	policy := spec.Tools
	policy.Denylist = append([]string(nil), policy.Denylist...)
	for _, name := range []string{"background_agent", "task_status", "task_wait", "task_cancel"} {
		if len(owned) == 0 || !owned[0][name] {
			policy.Denylist = append(policy.Denylist, name)
		}
	}
	if !canDelegate {
		policy.Denylist = append(policy.Denylist, "task")
	}
	return &policy
}

func (e *Engine) roleModel(ctx context.Context, reference, parentReference string, parent model.ToolCallingChatModel) (model.ToolCallingChatModel, error) {
	// Inheritance must retain the selected parent's provider or injected SDK
	// model, even when its session ID differs from the deployment default.
	if reference == parentReference && parent != nil {
		return parent, nil
	}
	if reference == "" || reference == e.config.Model {
		return e.model, nil
	}
	selected := e.config
	if route, ok := e.config.ModelRoutes[reference]; ok {
		selected.Provider, selected.BaseURL, selected.APIKey = route.Provider, route.BaseURL, route.APIKey
		return newModel(ctx, selected, route.Model)
	}
	// The SDK also permits an explicit catalog on one shared provider. Only
	// catalog entries qualify; arbitrary role strings never reach a provider.
	if e.config.ModelRoutes == nil {
		for _, choice := range e.config.Models {
			if choice.Value == reference {
				return newModel(ctx, selected, reference)
			}
		}
	}
	return nil, fmt.Errorf("%w: role model %q is not configured", harness.ErrInvalidInput, reference)
}

// timedRole retains the native resumable Agent identity and checkpoint state.
// The iterator is fully drained before its context is released. Actual model
// and tool I/O still joins the parent run's tracked lifecycle.
type timedRole struct {
	agentName                      string
	spec                           agents.Spec
	prepare                        func(context.Context, *runIO) (adk.ResumableAgent, error)
	lifecycle                      *runIO
	timeout                        time.Duration
	controller                     *agents.Controller
	rootID                         string
	invocationID                   string
	parentID                       string
	depth, maxConcurrent, maxTasks int
}

type roleWorkKey struct{}
type roleInvocationKey struct{}
type roleWork struct {
	controller *agents.Controller
	rootID, id string
	attempt    *agents.Attempt
	invocation agents.Invocation
}

// CurrentRole returns a detached host-owned recipe for an active invocation.
// It does not confer task authority; the host also checks its connection or
// worker lease and restricts operations to this invocation's descendants.
func CurrentRole(ctx context.Context) (agents.Invocation, bool, error) {
	return CurrentRoleTx(ctx, nil)
}

func CurrentRoleTx(ctx context.Context, tx *sql.Tx) (agents.Invocation, bool, error) {
	work, ok := ctx.Value(roleWorkKey{}).(roleWork)
	if !ok {
		return agents.Invocation{}, false, nil
	}
	var checkErr error
	if tx == nil {
		checkErr = work.attempt.Check(ctx)
	} else {
		checkErr = work.attempt.CheckTx(ctx, tx)
	}
	if checkErr != nil {
		return agents.Invocation{}, false, checkErr
	}
	raw, err := json.Marshal(work.invocation)
	if err != nil {
		return agents.Invocation{}, false, err
	}
	var result agents.Invocation
	err = json.Unmarshal(raw, &result)
	return result, true, err
}

func beginRoleActivity(ctx context.Context, kind string) func() {
	work, _ := ctx.Value(roleWorkKey{}).(roleWork)
	return work.attempt.Activity(kind)
}

// Model and effect I/O own slots; a task orchestration call never holds a slot
// while waiting for its descendants. Parallel calls by one role share a slot
// through the controller's reference count until all real I/O has joined.
func beginRoleWork(ctx context.Context) (func(), error) {
	work, ok := ctx.Value(roleWorkKey{}).(roleWork)
	if !ok {
		return func() {}, nil
	}
	queue, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	endQueue := beginRoleActivity(ctx, "queue")
	defer endQueue()
	release, err := work.controller.Acquire(queue, work.rootID, work.id)
	if errors.Is(err, agents.ErrQueueFull) {
		return nil, harness.MarkToolNotExecuted(err)
	}
	if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return nil, harness.MarkToolNotExecuted(errors.New("subagent execution queue timed out"))
	}
	if err == nil {
		if err = work.attempt.Check(ctx); err != nil {
			release()
			return nil, err
		}
	}
	return release, err
}

func (r *timedRole) Name(context.Context) string        { return r.agentName }
func (r *timedRole) Description(context.Context) string { return r.spec.Description }

func (r *timedRole) forward(ctx context.Context, run func(context.Context, adk.ResumableAgent) *adk.AsyncIterator[*adk.AgentEvent]) *adk.AsyncIterator[*adk.AgentEvent] {
	iterator, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	tracked, finish, err := r.lifecycle.begin(ctx)
	go func() {
		defer generator.Close()
		if err != nil {
			generator.Send(&adk.AgentEvent{AgentName: r.spec.Name, Err: err})
			return
		}
		defer finish()
		id, ok := tracked.Value(roleInvocationKey{}).(string)
		if r.invocationID != "" {
			id, ok = r.invocationID, true
		}
		if !ok || id == "" {
			generator.Send(&adk.AgentEvent{AgentName: r.spec.Name, Err: agents.ErrInvocationConflict})
			return
		}
		parent, _ := tracked.Value(roleWorkKey{}).(roleWork)
		parentID := parent.id
		if r.invocationID != "" {
			parentID = r.parentID
		}
		invocation := agents.Invocation{RootID: r.rootID, ID: id, ParentID: parentID, Depth: r.depth, Spec: r.spec}
		if err := r.controller.Register(tracked, invocation, r.maxConcurrent, r.maxTasks); err != nil {
			if errors.Is(err, agents.ErrStopped) {
				err = r.stoppedRoleResult(tracked, id)
			}
			if errors.Is(err, agents.ErrTaskLimit) {
				err = harness.MarkToolNotExecuted(err)
			}
			generator.Send(&adk.AgentEvent{AgentName: r.spec.Name, Err: err})
			return
		}
		deadline := &roleDeadline{name: r.spec.Name}
		attempt, err := r.controller.BeginAttempt(tracked, id, r.timeout)
		if err != nil {
			if errors.Is(err, agents.ErrStopped) {
				err = r.stoppedRoleResult(tracked, id)
			}
			if errors.Is(err, agents.ErrTimeLimit) {
				err = &delegationFailure{cause: deadline}
			}
			generator.Send(&adk.AgentEvent{AgentName: r.spec.Name, Err: err})
			return
		}
		if err := attempt.RecordState(tracked, "running", nil); err != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(tracked), 5*time.Second)
			err = errors.Join(err, attempt.Finish(cleanup))
			cancel()
			generator.Send(&adk.AgentEvent{AgentName: r.spec.Name, Err: err})
			return
		}
		tracked = context.WithValue(tracked, roleWorkKey{}, roleWork{controller: r.controller, rootID: r.rootID, id: id, attempt: attempt, invocation: invocation})
		observation := &roleTreeObservation{}
		tracked = context.WithValue(tracked, roleTreeObservationKey{}, observation)
		bounded, cancel := context.WithTimeoutCause(tracked, attempt.Remaining(), deadline)
		defer cancel()
		work, stopWork := context.WithCancelCause(bounded)
		defer stopWork(nil)
		stopLease, leaseDone := make(chan struct{}), make(chan error, 1)
		go func() {
			ticker := time.NewTicker(attempt.HeartbeatInterval())
			defer ticker.Stop()
			stopTicker := time.NewTicker(250 * time.Millisecond)
			defer stopTicker.Stop()
			for {
				select {
				case <-work.Done():
					leaseDone <- nil
					return
				case <-stopLease:
					leaseDone <- nil
					return
				case <-stopTicker.C:
					if err := attempt.CheckStop(work); err != nil && work.Err() == nil {
						stopWork(err)
						leaseDone <- err
						return
					}
				case <-ticker.C:
					if err := attempt.Heartbeat(work); err != nil {
						if work.Err() != nil {
							leaseDone <- nil
							return
						}
						stopWork(err)
						leaseDone <- err
						return
					}
				}
			}
		}()
		private := newRunIO(work)
		private.role = true
		agent, terminal := r.prepare(work, private)
		var failedEvent *adk.AgentEvent
		interrupted := false
		if terminal == nil {
			inner := run(work, agent)
			for {
				event, ok := inner.Next()
				if !ok {
					break
				}
				if event != nil && event.Err != nil {
					terminal = errors.Join(terminal, event.Err)
					failedEvent = event
					continue
				}
				if event != nil && event.Action != nil && event.Action.Interrupted != nil {
					interrupted = true
				}
				generator.Send(event)
			}
		}
		// A native iterator may finish before provider/process cleanup. Do not
		// let the parent consume a failure result until every child effect has
		// joined and every late storage/receipt error has been observed.
		private.closeAndWait()
		close(stopLease)
		leaseErr := <-leaseDone
		cleanup, stopCleanup := context.WithTimeout(context.WithoutCancel(tracked), 5*time.Second)
		settleErr := attempt.Finish(cleanup)
		terminal = errors.Join(terminal, private.failure(), leaseErr, settleErr)
		// An admission fence can observe the durable stop before the polling
		// monitor cancels work. Recheck after joining so both orderings produce
		// the same safe parent-facing outcome.
		stopErr := attempt.CheckStop(cleanup)
		stopped := errors.Is(stopErr, agents.ErrStopped)
		if stopErr != nil && !stopped {
			terminal = errors.Join(terminal, stopErr)
		}
		state := "completed"
		var resultText *string
		if terminal != nil {
			state = "failed"
		} else if interrupted {
			state = "waiting_input"
		} else {
			resultText = observation.final()
		}
		if (stopped || tracked.Err() != nil) && recoverableRoleTimeout(withoutRoleStop(terminal)) {
			state = "cancelled"
			resultText = nil
		}
		terminal = errors.Join(terminal, attempt.RecordState(cleanup, state, resultText))
		stopCleanup()
		if tracked.Err() == nil && stopped && recoverableRoleTimeout(withoutRoleStop(terminal)) {
			terminal = &delegationFailure{cause: agents.ErrStopped}
		}
		if tracked.Err() == nil && context.Cause(bounded) == deadline && recoverableRoleTimeout(terminal) {
			terminal = &delegationFailure{cause: deadline}
		}
		if terminal == nil {
			return
		}
		if tracked.Err() == nil && bounded.Err() == nil && recoverableRoleCause(terminal) {
			terminal = &delegationFailure{cause: terminal}
		}
		if failedEvent == nil {
			failedEvent = &adk.AgentEvent{AgentName: r.spec.Name}
		}
		result := *failedEvent
		result.Err = terminal
		generator.Send(&result)
	}()
	return iterator
}

func (r *timedRole) Run(ctx context.Context, input *adk.AgentInput, opts ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	return r.forward(ctx, func(ctx context.Context, agent adk.ResumableAgent) *adk.AsyncIterator[*adk.AgentEvent] {
		return agent.Run(ctx, input, opts...)
	})
}

func (r *timedRole) Resume(ctx context.Context, info *adk.ResumeInfo, opts ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	return r.forward(ctx, func(ctx context.Context, agent adk.ResumableAgent) *adk.AsyncIterator[*adk.AgentEvent] {
		return agent.Resume(ctx, info, opts...)
	})
}

func roleModelOptions(settings harness.AgentModelSettings, opts []model.Option) []model.Option {
	result := append([]model.Option(nil), opts...)
	if settings.Temperature != nil {
		result = append(result, model.WithTemperature(float32(*settings.Temperature)))
	}
	if settings.MaxTokens != nil {
		common := model.GetCommonOptions(nil, result...)
		if common == nil || common.MaxTokens == nil || *common.MaxTokens <= 0 || *common.MaxTokens > *settings.MaxTokens {
			result = append(result, model.WithMaxTokens(*settings.MaxTokens))
		}
	}
	return result
}

var _ adk.ResumableAgent = (*timedRole)(nil)
