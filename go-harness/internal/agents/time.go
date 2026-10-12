package agents

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

var ErrTimeLimit = errors.New("subagent cumulative time limit reached")
var ErrAttemptBusy = errors.New("subagent already has an active time lease")
var ErrAttemptConflict = errors.New("subagent time lease no longer belongs to this attempt")

const timeLease = 6 * time.Second

// Attempt reserves time before work begins. The invocation's elapsed total
// includes this reservation. Clean teardown settles actual elapsed time;
// after a crash the unconfirmed tail stays charged (at most one lease window).
// Expired owners cannot renew or refund a reservation claimed by a successor.
type Attempt struct {
	controller     *Controller
	id, invocation string
	started        time.Time
	remaining      time.Duration
	unlimited      bool
	mu             sync.Mutex
	activities     map[string]*activityClock
	closed         bool
}

// ID is the immutable host-assigned time-lease attempt identity. It does not
// grant authority; users must still fence the attempt with Check or CheckTx.
func (a *Attempt) ID() string {
	if a == nil {
		return ""
	}
	return a.id
}

type activityClock struct {
	count   int
	started time.Time
	elapsed time.Duration
}

type timeRecord struct {
	id, invocation, state string
	started               int64
	reserved, observed    time.Duration
	timings               map[string]time.Duration
}

// Timing is an accounting view, not a billable model usage measurement.
// Activity durations are unions per kind and may overlap (e.g. parallel model
// and tool I/O). Paused durable approval time is outside execution attempts.
type Timing struct {
	Elapsed     time.Duration            `json:"elapsed_ns"`
	Unconfirmed time.Duration            `json:"unconfirmed_ns"`
	Activities  map[string]time.Duration `json:"activities_ns"`
}

func (c *Controller) BeginAttempt(ctx context.Context, invocation string, limit time.Duration) (_ *Attempt, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	defer func() { err = wrapTimePersistence("begin role attempt", err) }()
	if invocation == "" || limit < 0 {
		return nil, ErrInvocationConflict
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	a := &Attempt{controller: c, id: rand.Text(), invocation: invocation, started: now, unlimited: limit == 0, activities: map[string]*activityClock{}}
	var elapsed time.Duration
	if c.db == nil {
		found := false
		for _, root := range c.roots {
			if root.invocations[invocation] != "" {
				found = true
				break
			}
		}
		if !found {
			return nil, ErrInvocationConflict
		}
		for _, old := range c.attempts {
			if old.invocation != invocation || old.state != "active" {
				continue
			}
			if now.UnixNano() < old.started+int64(old.reserved) {
				return nil, ErrAttemptBusy
			}
			old.state = "interrupted"
		}
		elapsed = c.elapsed[invocation]
		if limit > 0 && elapsed >= limit {
			return nil, ErrTimeLimit
		}
		a.remaining = max(0, limit-elapsed)
		held := timeLease
		if !a.unlimited {
			held = min(held, a.remaining)
		}
		c.elapsed[invocation] += held
		c.attempts[a.id] = &timeRecord{id: a.id, invocation: invocation, state: "active", started: now.UnixNano(), reserved: held, timings: map[string]time.Duration{}}
		return a, nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var root string
	if err = tx.QueryRowContext(ctx, `SELECT root_id FROM harness_agent_invocations WHERE id=?`, invocation).Scan(&root); err != nil {
		return nil, err
	}
	if err = CheckStopped(ctx, tx, root, invocation); err != nil {
		return nil, err
	}
	var oldStart, oldHeld int64
	err = tx.QueryRowContext(ctx, `SELECT started_ns,reserved_ns FROM harness_agent_attempts WHERE invocation_id=? AND state='active'`, invocation).Scan(&oldStart, &oldHeld)
	if err == nil {
		if now.UnixNano() < oldStart+oldHeld {
			return nil, ErrAttemptBusy
		}
		if _, err = tx.ExecContext(ctx, `UPDATE harness_agent_attempts SET state='interrupted' WHERE invocation_id=? AND state='active'`, invocation); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT elapsed_ns FROM harness_agent_invocations WHERE id=?`, invocation).Scan(&elapsed); err != nil {
		return nil, err
	}
	if limit > 0 && elapsed >= limit {
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrTimeLimit
	}
	a.remaining = max(0, limit-elapsed)
	held := timeLease
	if !a.unlimited {
		held = min(held, a.remaining)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_agent_attempts(id,invocation_id,state,started_ns,reserved_ns,observed_ns,timings_json) VALUES(?,?,'active',?,?,0,'{}')`, a.id, invocation, now.UnixNano(), int64(held)); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_agent_invocations SET elapsed_ns=elapsed_ns+? WHERE id=?`, int64(held), invocation); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Attempt) Remaining() time.Duration {
	if a.unlimited {
		return 0
	}
	return max(0, a.remaining-max(0, a.controller.now().Sub(a.started)))
}
func (a *Attempt) HeartbeatInterval() time.Duration { return timeLease / 3 }

// Activity counts the union of parallel work of one kind, rather than summing
// all concurrent tool durations and presenting them as wall time.
func (a *Attempt) Activity(kind string) func() {
	if a == nil {
		return func() {}
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return func() {}
	}
	clock := a.activities[kind]
	if clock == nil {
		clock = &activityClock{}
		a.activities[kind] = clock
	}
	if clock.count == 0 {
		clock.started = a.controller.now()
	}
	clock.count++
	a.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.closed {
				return
			}
			clock.count--
			if clock.count == 0 {
				clock.elapsed += max(0, a.controller.now().Sub(clock.started))
			}
		})
	}
}

func (a *Attempt) sample(now time.Time, finish bool) map[string]time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make(map[string]time.Duration, len(a.activities))
	for kind, clock := range a.activities {
		result[kind] = clock.elapsed
		if clock.count > 0 {
			result[kind] += max(0, now.Sub(clock.started))
		}
	}
	if finish {
		a.closed = true
	}
	return result
}

func (a *Attempt) Heartbeat(ctx context.Context) error { return a.update(ctx, false) }
func (a *Attempt) Finish(ctx context.Context) error    { return a.update(ctx, true) }

// CheckTx fences delegation accepted inside the caller's storage transaction.
// Do not acquire the controller mutex here: admission can hold it while waiting
// for this database connection. The durable attempt is authoritative.
func (a *Attempt) CheckTx(ctx context.Context, tx *sql.Tx) (err error) {
	if a == nil || tx == nil || a.controller.db == nil {
		return ErrAttemptConflict
	}
	defer func() { err = wrapTimePersistence("check role attempt", err) }()
	var root string
	if err = tx.QueryRowContext(ctx, `SELECT root_id FROM harness_agent_invocations WHERE id=?`, a.invocation).Scan(&root); err != nil {
		return err
	}
	if err = CheckStopped(ctx, tx, root, a.invocation); err != nil {
		return err
	}
	var record timeRecord
	if err = tx.QueryRowContext(ctx, `SELECT invocation_id,state,started_ns,reserved_ns FROM harness_agent_attempts WHERE id=?`, a.id).Scan(&record.invocation, &record.state, &record.started, &record.reserved); err != nil {
		return err
	}
	if record.invocation != a.invocation || record.state != "active" || a.controller.now().UnixNano() >= record.started+int64(record.reserved) {
		return ErrAttemptConflict
	}
	return nil
}

// Check fences new I/O after a time lease has expired or been superseded.
func (a *Attempt) Check(ctx context.Context) (err error) {
	if a == nil {
		return nil
	}
	defer func() { err = wrapTimePersistence("check role attempt", err) }()
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = a.CheckStop(ctx); err != nil {
		return err
	}
	c := a.controller
	c.mu.Lock()
	defer c.mu.Unlock()
	var record timeRecord
	if c.db == nil {
		current := c.attempts[a.id]
		if current == nil {
			return ErrAttemptConflict
		}
		record = *current
	} else if err = c.db.QueryRowContext(ctx, `SELECT invocation_id,state,started_ns,reserved_ns FROM harness_agent_attempts WHERE id=?`, a.id).Scan(&record.invocation, &record.state, &record.started, &record.reserved); err != nil {
		return err
	}
	if record.invocation != a.invocation || record.state != "active" || c.now().UnixNano() >= record.started+int64(record.reserved) {
		return ErrAttemptConflict
	}
	return nil
}

func (a *Attempt) update(ctx context.Context, finish bool) (err error) {
	defer func() { err = wrapTimePersistence("update role attempt", err) }()
	if err = ctx.Err(); err != nil {
		return err
	}
	c := a.controller
	now := c.now()
	actual := max(0, now.Sub(a.started))
	timings := a.sample(now, finish)
	c.mu.Lock()
	defer c.mu.Unlock()
	apply := func(record *timeRecord) error {
		if record.state == "ended" && finish {
			return nil
		}
		if record.state != "active" || record.invocation != a.invocation {
			return ErrAttemptConflict
		}
		if !finish && actual > record.reserved {
			return ErrAttemptConflict
		}
		record.observed = actual
		record.timings = timings
		if finish {
			record.reserved = actual
			record.state = "ended"
		} else {
			held := actual + timeLease
			if !a.unlimited {
				held = min(a.remaining, held)
			}
			record.reserved = max(record.reserved, held)
		}
		return nil
	}
	if c.db == nil {
		record := c.attempts[a.id]
		if record == nil {
			return ErrAttemptConflict
		}
		before := record.reserved
		if err := apply(record); err != nil {
			return err
		}
		c.elapsed[a.invocation] += record.reserved - before
		return nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var record timeRecord
	if err = tx.QueryRowContext(ctx, `SELECT invocation_id,state,reserved_ns FROM harness_agent_attempts WHERE id=?`, a.id).Scan(&record.invocation, &record.state, &record.reserved); err != nil {
		return err
	}
	before := record.reserved
	if record.state == "ended" && finish {
		return nil
	}
	if err = apply(&record); err != nil {
		return err
	}
	raw, err := json.Marshal(record.timings)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_agent_attempts SET state=?,reserved_ns=?,observed_ns=?,timings_json=? WHERE id=?`, record.state, int64(record.reserved), int64(record.observed), raw, a.id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_agent_invocations SET elapsed_ns=elapsed_ns+? WHERE id=?`, int64(record.reserved-before), a.invocation); err != nil {
		return err
	}
	return tx.Commit()
}

type timePersistenceError struct {
	operation string
	cause     error
}

func (e *timePersistenceError) Error() string            { return e.operation + ": " + e.cause.Error() }
func (e *timePersistenceError) Unwrap() error            { return e.cause }
func (e *timePersistenceError) PersistenceFailure() bool { return true }

func wrapTimePersistence(operation string, err error) error {
	if err == nil || errors.Is(err, ErrStopped) || errors.Is(err, ErrTimeLimit) || errors.Is(err, ErrAttemptBusy) || errors.Is(err, ErrAttemptConflict) || errors.Is(err, ErrInvocationConflict) {
		return err
	}
	return &timePersistenceError{operation: operation, cause: err}
}

func (c *Controller) Timing(ctx context.Context, invocation string) (Timing, error) {
	result := Timing{Activities: map[string]time.Duration{}}
	add := func(record timeRecord) {
		if record.state != "ended" {
			result.Unconfirmed += max(0, record.reserved-record.observed)
		}
		for kind, value := range record.timings {
			result.Activities[kind] += value
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil {
		result.Elapsed = c.elapsed[invocation]
		for _, record := range c.attempts {
			if record.invocation == invocation {
				add(*record)
			}
		}
		return result, nil
	}
	if err := c.db.QueryRowContext(ctx, `SELECT elapsed_ns FROM harness_agent_invocations WHERE id=?`, invocation).Scan(&result.Elapsed); err != nil {
		return result, err
	}
	rows, err := c.db.QueryContext(ctx, `SELECT state,reserved_ns,observed_ns,timings_json FROM harness_agent_attempts WHERE invocation_id=?`, invocation)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var record timeRecord
		var raw []byte
		if err = rows.Scan(&record.state, &record.reserved, &record.observed, &raw); err != nil {
			return result, err
		}
		if err = json.Unmarshal(raw, &record.timings); err != nil {
			return result, fmt.Errorf("read subagent timings: %w", err)
		}
		add(record)
	}
	return result, rows.Err()
}
