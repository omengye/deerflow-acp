package protocol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

const DefaultMaxFrameBytes = 64 << 20

// Handler processes a request or notification. Requests run concurrently;
// notifications run sequentially in their own bounded queue. Notification
// handlers must return promptly; session/cancel should signal, not await, a run.
// Every handler must honor context cancellation and finish its cleanup before
// returning. Serve waits for these handlers before returning.
type Handler func(context.Context, string, json.RawMessage) (any, error)

// Admission executes synchronously in wire order before a request is dispatched.
// It can reserve a session and return a derived context carrying that reservation.
// It must not perform network calls or block waiting for other requests. A nonnil
// release function runs exactly once after the handler finishes and before its
// response is written (or immediately if admission fails). A client that has
// received the response can therefore start the next operation on the session.
// Returning a nil context retains the original request context.
type Admission func(context.Context, string, json.RawMessage) (context.Context, func(), error)

type Options struct {
	MaxFrameBytes         int
	MaxConcurrentRequests int
	MaxPendingCalls       int
	WriteQueueCapacity    int
	MaxQueuedEventBytes   int64
	NotificationQueueSize int
	Admit                 Admission
}

func (o Options) defaults() Options {
	if o.MaxFrameBytes <= 0 {
		o.MaxFrameBytes = DefaultMaxFrameBytes
	}
	if o.MaxConcurrentRequests <= 0 {
		o.MaxConcurrentRequests = 64
	}
	if o.MaxPendingCalls <= 0 {
		o.MaxPendingCalls = 64
	}
	if o.WriteQueueCapacity <= 0 {
		o.WriteQueueCapacity = 64
	}
	if o.MaxQueuedEventBytes <= 0 {
		o.MaxQueuedEventBytes = 64 << 20
	}
	if o.NotificationQueueSize <= 0 {
		o.NotificationQueueSize = 64
	}
	return o
}

type writeJob struct {
	ctx        context.Context
	frame      []byte
	done       chan error
	eventBytes int64
}

type notificationKey struct{}

// IsNotification identifies calls for which no JSON-RPC response is expected.
// Business adapters can use it to reject request-only methods received as a
// notification without accidentally starting a long-running prompt.
func IsNotification(ctx context.Context) bool {
	yes, _ := ctx.Value(notificationKey{}).(bool)
	return yes
}

// Peer owns both streams. Their Close methods must unblock concurrent Read and
// Write calls (os.File, net.Conn, and io.Pipe provide this). Do not wrap a blocking
// stream in a no-op closer. Close or parent cancellation interrupts both loops.
// Stdout must be exclusively owned by this peer; diagnostics belong on stderr.
type Peer struct {
	in            io.ReadCloser
	out           io.WriteCloser
	handler       Handler
	opts          Options
	ctx           context.Context
	cancel        context.CancelCauseFunc
	closeOnce     sync.Once
	serving       atomic.Bool
	nextID        atomic.Uint64
	queuedEvents  atomic.Int64
	writes        chan writeJob
	notifications chan envelope
	requestSlots  chan struct{}
	loops         sync.WaitGroup
	handlers      sync.WaitGroup
	mu            sync.Mutex
	pending       map[string]chan envelope
	inflight      map[string]context.CancelFunc
}

func NewPeer(in io.ReadCloser, out io.WriteCloser, handler Handler, options Options) *Peer {
	options = options.defaults()
	ctx, cancel := context.WithCancelCause(context.Background())
	return &Peer{
		in: in, out: out, handler: handler, opts: options, ctx: ctx, cancel: cancel,
		writes:        make(chan writeJob, options.WriteQueueCapacity),
		notifications: make(chan envelope, options.NotificationQueueSize),
		requestSlots:  make(chan struct{}, options.MaxConcurrentRequests),
		pending:       make(map[string]chan envelope), inflight: make(map[string]context.CancelFunc),
	}
}

// Serve runs until EOF, cancellation, Close, or an I/O/protocol capacity failure.
// Clean EOF and explicit Close return nil. It can be called only once. It waits
// for canceled handlers, including their persistence/cleanup, before returning.
func (p *Peer) Serve(ctx context.Context) error {
	if !p.serving.CompareAndSwap(false, true) {
		return ErrAlreadyServing
	}
	stop := context.AfterFunc(ctx, func() { p.shutdown(context.Cause(ctx)) })
	defer stop()
	p.loops.Add(3)
	go func() { defer p.loops.Done(); p.readLoop() }()
	go func() { defer p.loops.Done(); p.writeLoop() }()
	go func() { defer p.loops.Done(); p.notificationLoop() }()
	p.loops.Wait()
	p.handlers.Wait()
	cause := context.Cause(p.ctx)
	if errors.Is(cause, io.EOF) || errors.Is(cause, ErrClosed) {
		return nil
	}
	return cause
}

// Done closes as soon as disconnect is detected. Serve additionally waits for
// cleanup, so Done must not be used as evidence that a session is idle.
func (p *Peer) Done() <-chan struct{} { return p.ctx.Done() }

func (p *Peer) Close() error {
	p.shutdown(ErrClosed)
	return nil
}

func (p *Peer) shutdown(cause error) {
	p.closeOnce.Do(func() {
		p.cancel(cause)
		_ = p.in.Close()
		_ = p.out.Close()
	})
}

// Notify waits until the complete frame has been written. On context cancellation
// an in-progress write may still reach the client; only connection shutdown can
// interrupt a generic io.Writer mid-write. No notification is silently dropped.
func (p *Peer) Notify(ctx context.Context, method string, params any) error {
	if method == "" {
		return &Error{Code: InvalidRequest, Message: "Method must be non-empty"}
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if params == nil {
		raw = nil
	}
	return p.send(ctx, envelope{JSONRPC: "2.0", Method: method, Params: raw})
}

// NotifyAsync accepts a bounded event frame without waiting for the client to
// read it. Once admitted, the connection owns delivery, including after the
// prompt context ends. The single writer preserves queue order with replies;
// a full queue terminates the connection so unsent events can be replayed from
// the durable session log instead of being silently dropped.
func (p *Peer) NotifyAsync(ctx context.Context, method string, params any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.ctx.Err() != nil {
		return ErrClosed
	}
	if method == "" {
		return &Error{Code: InvalidRequest, Message: "Method must be non-empty"}
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if params == nil {
		raw = nil
	}
	frame, err := marshalFrame(envelope{JSONRPC: "2.0", Method: method, Params: raw}, p.opts.MaxFrameBytes)
	if err != nil {
		return err
	}
	size := int64(len(frame))
	for {
		current := p.queuedEvents.Load()
		if size > p.opts.MaxQueuedEventBytes-current {
			p.shutdown(ErrBackpressure)
			return ErrBackpressure
		}
		if p.queuedEvents.CompareAndSwap(current, current+size) {
			break
		}
	}
	job := writeJob{ctx: p.ctx, frame: frame, eventBytes: size}
	select {
	case <-ctx.Done():
		p.queuedEvents.Add(-size)
		return ctx.Err()
	case <-p.ctx.Done():
		p.queuedEvents.Add(-size)
		return ErrClosed
	case p.writes <- job:
		if p.ctx.Err() != nil {
			return ErrClosed
		}
		return nil
	default:
		p.queuedEvents.Add(-size)
		p.shutdown(ErrBackpressure)
		return ErrBackpressure
	}
}

// Call sends an agent-to-client request and routes its response independently of
// active prompts. IDs are unique within this Peer and must not be persisted or
// reused on another connection. Canceling Call removes its pending response slot;
// ACP session cancellation remains the business adapter's explicit operation.
func (p *Peer) Call(ctx context.Context, method string, params any, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if method == "" {
		return &Error{Code: InvalidRequest, Message: "Method must be non-empty"}
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if params == nil {
		raw = nil
	}
	id, _ := json.Marshal("go-acp-" + strconv.FormatUint(p.nextID.Add(1), 10))
	key, _ := idKey(id)
	reply := make(chan envelope, 1)
	p.mu.Lock()
	if len(p.pending) >= p.opts.MaxPendingCalls {
		p.mu.Unlock()
		return ErrPendingLimit
	}
	p.pending[key] = reply
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.pending, key); p.mu.Unlock() }()
	if err = p.send(ctx, envelope{JSONRPC: "2.0", ID: id, Method: method, Params: raw}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return ErrClosed
	case msg := <-reply:
		if err := ctx.Err(); err != nil {
			return err
		}
		if msg.Error != nil {
			return msg.Error
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(msg.Result, result)
	}
}

func (p *Peer) send(ctx context.Context, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.ctx.Err() != nil {
		return ErrClosed
	}
	frame, err := marshalFrame(value, p.opts.MaxFrameBytes)
	if err != nil {
		return err
	}
	job := writeJob{ctx: ctx, frame: frame, done: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return ErrClosed
	case p.writes <- job:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return ErrClosed
	case err := <-job.done:
		return err
	}
}

// Reader-generated error replies cannot wait for a slow writer, because doing
// so would deadlock the permission-response and cancellation path.
func (p *Peer) reject(id json.RawMessage, rpcError *Error) {
	if id == nil {
		id = json.RawMessage("null")
	}
	frame, err := marshalFrame(envelope{JSONRPC: "2.0", ID: id, Error: rpcError}, p.opts.MaxFrameBytes)
	if err != nil {
		p.shutdown(err)
		return
	}
	select {
	case <-p.ctx.Done():
	case p.writes <- writeJob{ctx: p.ctx, frame: frame}:
	default:
		p.shutdown(ErrBackpressure)
	}
}

func (p *Peer) writeLoop() {
	for {
		select {
		case <-p.ctx.Done():
			return
		case job := <-p.writes:
			if err := job.ctx.Err(); err != nil {
				if job.eventBytes != 0 {
					p.queuedEvents.Add(-job.eventBytes)
				}
				if job.done != nil {
					job.done <- err
				}
				continue
			}
			frame := job.frame
			var err error
			for len(frame) > 0 {
				var n int
				n, err = p.out.Write(frame)
				if n < 0 || n > len(frame) {
					err = io.ErrShortWrite
					break
				}
				frame = frame[n:]
				if err != nil {
					break
				}
				if n == 0 {
					err = io.ErrShortWrite
					break
				}
			}
			if job.done != nil {
				job.done <- err
			}
			if job.eventBytes != 0 {
				p.queuedEvents.Add(-job.eventBytes)
			}
			if err != nil {
				p.shutdown(err)
				return
			}
		}
	}
}

func (p *Peer) readLoop() {
	reader := bufio.NewReaderSize(p.in, min(p.opts.MaxFrameBytes, 64<<10))
	for {
		frame, err := readFrame(reader, p.opts.MaxFrameBytes)
		if err != nil {
			p.shutdown(err)
			return
		}
		if len(bytes.TrimSpace(frame)) == 0 {
			continue
		}
		msg, parseErr := parseEnvelope(frame)
		if parseErr != nil {
			if msg.Method == "" || msg.ID != nil {
				p.reject(msg.ID, parseErr)
			}
			continue
		}
		if msg.Method == "" {
			key, _ := idKey(msg.ID)
			p.mu.Lock()
			reply := p.pending[key]
			p.mu.Unlock()
			if reply != nil {
				select {
				case reply <- msg:
				default: // Duplicate or late responses never block the reader.
				}
			}
			continue
		}
		if msg.ID == nil {
			select {
			case p.notifications <- msg:
			case <-p.ctx.Done():
				return
			default:
				p.shutdown(ErrBackpressure)
				return
			}
			continue
		}
		p.dispatchRequest(msg)
	}
}

func (p *Peer) dispatchRequest(msg envelope) {
	key, _ := idKey(msg.ID)
	p.mu.Lock()
	_, duplicate := p.inflight[key]
	p.mu.Unlock()
	if duplicate {
		p.reject(msg.ID, &Error{Code: InvalidRequest, Message: "Duplicate active request ID"})
		return
	}
	select {
	case p.requestSlots <- struct{}{}:
	default:
		p.reject(msg.ID, &Error{Code: ServerBusy, Message: "Too many active requests"})
		return
	}
	ctx, cancel := context.WithCancel(p.ctx)
	var release func()
	if p.opts.Admit != nil {
		admittedCtx, releaseFn, err := p.opts.Admit(ctx, msg.Method, msg.Params)
		release = releaseFn
		if err != nil {
			if release != nil {
				release()
			}
			cancel()
			<-p.requestSlots
			p.reject(msg.ID, asRPCError(err))
			return
		}
		if admittedCtx != nil {
			ctx = admittedCtx
		}
	}
	p.mu.Lock()
	p.inflight[key] = cancel
	p.mu.Unlock()
	p.handlers.Add(1)
	go func() {
		defer p.handlers.Done()
		defer func() {
			if release != nil {
				release()
			}
			cancel()
			p.mu.Lock()
			delete(p.inflight, key)
			p.mu.Unlock()
			<-p.requestSlots
		}()
		result, err := p.invoke(ctx, msg.Method, msg.Params)
		response := envelope{JSONRPC: "2.0", ID: msg.ID}
		if err != nil {
			response.Error = asRPCError(err)
		} else {
			response.Result, err = json.Marshal(result)
			if err != nil {
				response.Error = asRPCError(err)
			}
		}
		if release != nil {
			release()
			release = nil
		}
		// A canceled prompt can still return its ACP stopReason. Only connection
		// cancellation should suppress the final response.
		if err := p.send(p.ctx, response); err != nil && p.ctx.Err() == nil {
			p.shutdown(err)
		}
	}()
}

func (p *Peer) notificationLoop() {
	ctx := context.WithValue(p.ctx, notificationKey{}, true)
	for {
		select {
		case <-p.ctx.Done():
			return
		case msg := <-p.notifications:
			_, _ = p.invoke(ctx, msg.Method, msg.Params)
		}
	}
}

func (p *Peer) invoke(ctx context.Context, method string, params json.RawMessage) (result any, err error) {
	defer func() {
		if recover() != nil {
			err = fmt.Errorf("ACP handler panicked")
			result = nil
		}
	}()
	if p.handler == nil {
		return nil, &Error{Code: MethodNotFound, Message: "Method not found"}
	}
	return p.handler(ctx, method, params)
}
