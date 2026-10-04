package agents

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

var ErrTaskLimit = errors.New("subagent task limit reached")
var ErrQueueFull = errors.New("subagent execution queue is full")
var ErrInvocationConflict = errors.New("subagent invocation policy changed")

// Controller is owned by one daemon and shared by foreground and background
// assemblies. Its durable registrations survive restart; active work is joined
// before the daemon releases ownership. A waiting ancestor holds no I/O slot.
type Controller struct {
	db       *sql.DB
	mu       sync.Mutex
	roots    map[string]*admissionRoot
	elapsed  map[string]time.Duration
	attempts map[string]*timeRecord
	now      func() time.Time
}

type admissionRoot struct {
	limit, maxTasks int
	invocations     map[string]string
	active          map[string]int
	waiters         int
	changed         chan struct{}
}

type Invocation struct {
	RootID   string `json:"rootId"`
	ID       string `json:"id"`
	ParentID string `json:"parentId"`
	Depth    int    `json:"depth"`
	Spec     Spec   `json:"spec"`
}

func (c *Controller) Durable() bool { return c != nil && c.db != nil }

func NewController(db *sql.DB) (*Controller, error) {
	c := &Controller{db: db, roots: map[string]*admissionRoot{}, elapsed: map[string]time.Duration{}, attempts: map[string]*timeRecord{}, now: time.Now}
	if db != nil {
		_, err := db.Exec(`CREATE TABLE IF NOT EXISTS harness_agent_roots (
 root_id TEXT PRIMARY KEY, max_concurrent INTEGER NOT NULL, max_tasks INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS harness_agent_invocations (
 id TEXT PRIMARY KEY, root_id TEXT NOT NULL REFERENCES harness_agent_roots(root_id),
 parent_id TEXT NOT NULL, depth INTEGER NOT NULL, spec_json BLOB NOT NULL,
 elapsed_ns INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS harness_agent_invocations_root ON harness_agent_invocations(root_id);
CREATE TABLE IF NOT EXISTS harness_agent_attempts (
 id TEXT PRIMARY KEY, invocation_id TEXT NOT NULL REFERENCES harness_agent_invocations(id),
 state TEXT NOT NULL, started_ns INTEGER NOT NULL, reserved_ns INTEGER NOT NULL,
 observed_ns INTEGER NOT NULL, timings_json BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS harness_agent_attempts_invocation ON harness_agent_attempts(invocation_id);
CREATE UNIQUE INDEX IF NOT EXISTS harness_agent_attempts_active ON harness_agent_attempts(invocation_id) WHERE state='active';`)
		if err != nil {
			return nil, fmt.Errorf("migrate agent admission: %w", err)
		}
		if err := migrateStates(db); err != nil {
			return nil, fmt.Errorf("migrate agent states: %w", err)
		}
	}
	return c, nil
}

// Register is idempotent for the host-derived ancestor/call identity preserved
// in an Eino checkpoint. A resume cannot consume another task or change its recipe.
func (c *Controller) Register(ctx context.Context, invocation Invocation, maxConcurrent, maxTasks int) error {
	if invocation.RootID == "" || invocation.ID == "" || invocation.Depth < 1 || invocation.Depth > 3 || maxConcurrent < 1 || maxTasks < 1 {
		return ErrInvocationConflict
	}
	identity, err := json.Marshal(invocation)
	if err != nil {
		return err
	}
	// Durable storage keeps the full recipe. The process cache needs only its
	// identity, not another copy of every instruction and policy per task.
	digest := fmt.Sprintf("%x", sha256.Sum256(identity))
	c.mu.Lock()
	defer c.mu.Unlock()
	root := c.roots[invocation.RootID]
	if root != nil {
		if root.limit != maxConcurrent || root.maxTasks != maxTasks {
			return ErrInvocationConflict
		}
		if previous, ok := root.invocations[invocation.ID]; ok {
			if previous != digest {
				return ErrInvocationConflict
			}
			if c.db == nil {
				return nil
			}
		}
	}
	if c.db != nil {
		tx, err := c.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = c.RegisterTx(ctx, tx, invocation, maxConcurrent, maxTasks); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		// Durable registrations are loaded lazily for active I/O. Keeping every
		// historical root here would grow the daemon cache without a bound.
		return nil
	} else if root != nil && len(root.invocations) >= maxTasks {
		return ErrTaskLimit
	}
	if root == nil {
		root = &admissionRoot{limit: maxConcurrent, maxTasks: maxTasks, invocations: map[string]string{}, active: map[string]int{}, changed: make(chan struct{})}
		c.roots[invocation.RootID] = root
	}
	root.invocations[invocation.ID] = digest
	return nil
}

// RegisterTx lets background task creation reserve its task count in the same
// SQLite transaction. No pool access or model-visible input is used here.
func (c *Controller) RegisterTx(ctx context.Context, tx *sql.Tx, invocation Invocation, maxConcurrent, maxTasks int) error {
	if tx == nil || c.db == nil || invocation.RootID == "" || invocation.ID == "" || invocation.Depth < 1 || invocation.Depth > 3 || maxConcurrent < 1 || maxTasks < 1 {
		return ErrInvocationConflict
	}
	if err := CheckStopped(ctx, tx, invocation.RootID, invocation.ParentID); err != nil {
		return err
	}
	if err := CheckStopped(ctx, tx, invocation.RootID, invocation.ID); err != nil {
		return err
	}
	raw, err := json.Marshal(invocation.Spec)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_agent_roots(root_id,max_concurrent,max_tasks) VALUES(?,?,?) ON CONFLICT DO NOTHING`, invocation.RootID, maxConcurrent, maxTasks); err != nil {
		return err
	}
	var concurrent, tasks int
	if err = tx.QueryRowContext(ctx, `SELECT max_concurrent,max_tasks FROM harness_agent_roots WHERE root_id=?`, invocation.RootID).Scan(&concurrent, &tasks); err != nil {
		return err
	}
	if concurrent != maxConcurrent || tasks != maxTasks {
		return ErrInvocationConflict
	}
	var savedRoot, savedParent string
	var savedDepth int
	var saved []byte
	err = tx.QueryRowContext(ctx, `SELECT root_id,parent_id,depth,spec_json FROM harness_agent_invocations WHERE id=?`, invocation.ID).Scan(&savedRoot, &savedParent, &savedDepth, &saved)
	if err == nil {
		if savedRoot != invocation.RootID || savedParent != invocation.ParentID || savedDepth != invocation.Depth || string(saved) != string(raw) {
			return ErrInvocationConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM harness_agent_invocations WHERE root_id=?`, invocation.RootID).Scan(&count); err != nil {
		return err
	}
	if count >= maxTasks {
		return ErrTaskLimit
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_agent_invocations(id,root_id,parent_id,depth,spec_json) VALUES(?,?,?,?,?)`, invocation.ID, invocation.RootID, invocation.ParentID, invocation.Depth, raw)
	return err
}

// Acquire counts active agents, including parallel I/O belonging to the same
// invocation. Release runs only after the actual stream or process has joined.
// No permit is acquired for the orchestration tool that waits on descendants.
func (c *Controller) Acquire(ctx context.Context, rootID, invocationID string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	root := c.roots[rootID]
	if c.db != nil && (root == nil || root.invocations[invocationID] == "") {
		var err error
		root, err = c.loadInvocationLocked(ctx, rootID, invocationID)
		if err != nil {
			c.mu.Unlock()
			return nil, err
		}
	}
	if root == nil || root.invocations[invocationID] == "" {
		c.mu.Unlock()
		return nil, ErrInvocationConflict
	}
	if root.waiters >= 128 {
		c.mu.Unlock()
		return nil, ErrQueueFull
	}
	root.waiters++
	defer func() { c.mu.Lock(); root.waiters--; c.evictIdleLocked(rootID, root); c.mu.Unlock() }()
	for {
		if root.active[invocationID] > 0 || len(root.active) < root.limit {
			root.active[invocationID]++
			c.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					c.mu.Lock()
					defer c.mu.Unlock()
					root.active[invocationID]--
					if root.active[invocationID] == 0 {
						delete(root.active, invocationID)
						close(root.changed)
						root.changed = make(chan struct{})
					}
					c.evictIdleLocked(rootID, root)
				})
			}, nil
		}
		changed := root.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
		c.mu.Lock()
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			return nil, err
		}
	}
}

func (c *Controller) evictIdleLocked(id string, root *admissionRoot) {
	if c.db != nil && root.waiters == 0 && len(root.active) == 0 {
		delete(c.roots, id)
	}
}

func (c *Controller) loadInvocationLocked(ctx context.Context, rootID, id string) (*admissionRoot, error) {
	var concurrent, tasks int
	var invocation Invocation
	var raw []byte
	err := c.db.QueryRowContext(ctx, `SELECT r.max_concurrent,r.max_tasks,i.root_id,i.id,i.parent_id,i.depth,i.spec_json FROM harness_agent_invocations i JOIN harness_agent_roots r ON r.root_id=i.root_id WHERE i.id=? AND i.root_id=?`, id, rootID).Scan(&concurrent, &tasks, &invocation.RootID, &invocation.ID, &invocation.ParentID, &invocation.Depth, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvocationConflict
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &invocation.Spec); err != nil {
		return nil, err
	}
	identity, err := json.Marshal(invocation)
	if err != nil {
		return nil, err
	}
	root := c.roots[rootID]
	if root == nil {
		root = &admissionRoot{limit: concurrent, maxTasks: tasks, invocations: map[string]string{}, active: map[string]int{}, changed: make(chan struct{})}
		c.roots[rootID] = root
	} else if root.limit != concurrent || root.maxTasks != tasks {
		return nil, ErrInvocationConflict
	}
	root.invocations[id] = fmt.Sprintf("%x", sha256.Sum256(identity))
	return root, nil
}

// Elapsed and Charge retain cumulative execution-attempt time across approval
// suspensions. Time while waiting for user approval is outside an attempt.
func (c *Controller) Elapsed(ctx context.Context, id string) (time.Duration, error) {
	if c.db == nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.elapsed[id], nil
	}
	var elapsed int64
	err := c.db.QueryRowContext(ctx, `SELECT elapsed_ns FROM harness_agent_invocations WHERE id=?`, id).Scan(&elapsed)
	return time.Duration(elapsed), err
}

func (c *Controller) Charge(ctx context.Context, id string, elapsed time.Duration) error {
	if elapsed < 0 {
		return ErrInvocationConflict
	}
	if c.db == nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.elapsed[id] += elapsed
		return nil
	}
	_, err := c.db.ExecContext(ctx, `UPDATE harness_agent_invocations SET elapsed_ns=elapsed_ns+? WHERE id=?`, int64(elapsed), id)
	return err
}
