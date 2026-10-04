package wslhelper

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type frameWriter struct {
	mu     sync.Mutex
	id     string
	err    error
	failed chan struct{}
}

func newFrameWriter(id string) *frameWriter { return &frameWriter{id: id, failed: make(chan struct{})} }

func (w *frameWriter) send(frame Frame) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	frame.Version, frame.ID = Version, w.id
	data, err := EncodeFrame(frame)
	if err == nil {
		err = writeTransport(data)
	}
	if err != nil {
		w.err = err
		close(w.failed)
	}
	return err
}

type streamWriter struct {
	kind   string
	output *frameWriter
}

func (w streamWriter) Write(data []byte) (int, error) {
	n := len(data)
	for len(data) > 0 {
		size := min(len(data), ChunkSize)
		if err := w.output.send(Frame{Kind: w.kind, Data: data[:size]}); err != nil {
			return n - len(data), err
		}
		data = data[size:]
	}
	return n, nil
}

func supervise(request Request, stop <-chan string, output *frameWriter) Receipt {
	failed := 127
	result := Receipt{State: "failed", ExitCode: &failed, Confirmed: true}
	// PR_SET_PDEATHSIG is attached to the spawning OS thread on Linux.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	input, err := os.Open(os.DevNull)
	if err != nil {
		return result
	}
	defer input.Close()
	outR, outW, err := os.Pipe()
	if err != nil {
		return result
	}
	defer outR.Close()
	defer outW.Close()
	errR, errW, err := os.Pipe()
	if err != nil {
		return result
	}
	defer errR.Close()
	defer errW.Close()
	select {
	case reason := <-stop:
		result.State = reason
		result.ExitCode = nil
		return result
	default:
	}
	child, err := os.StartProcess(request.Argv[0], request.Argv, &os.ProcAttr{
		Dir: request.CWD, Env: commandEnvironment(request.Environment), Files: []*os.File{input, outW, errW},
		Sys: &syscall.SysProcAttr{Setsid: true, Pdeathsig: syscall.SIGKILL},
	})
	if err != nil {
		_ = output.send(Frame{Kind: "stderr", Data: []byte("WSL command could not start\n")})
		return result
	}
	defer child.Release()
	outW.Close()
	errW.Close()
	var drains sync.WaitGroup
	drains.Add(2)
	go func() { defer drains.Done(); _, _ = io.Copy(streamWriter{"stdout", output}, outR) }()
	go func() { defer drains.Done(); _, _ = io.Copy(streamWriter{"stderr", output}, errR) }()
	drained := make(chan struct{})
	go func() { drains.Wait(); close(drained) }()
	result = Receipt{State: "completed", Confirmed: true}
	deadline := time.NewTimer(time.Duration(request.TimeoutNanos))
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	running := true
	for running {
		_, err := reap(child.Pid, &result.ExitCode)
		if err != nil {
			result.Confirmed = false
			break
		}
		if result.ExitCode != nil {
			break
		}
		select {
		case reason := <-stop:
			result.State = reason
			running = false
		case <-deadline.C:
			result.State = "timed_out"
			running = false
		case <-output.failed:
			result.State = "cancelled"
			running = false
		case <-tick.C:
		}
	}
	// Only our direct children are signalled, using a pidfd acquired before
	// checking PPID. Orphans (including setsid/double-fork descendants) become
	// direct children of this subreaper. Never signal an unverified reused PID.
	if !cleanupChildren(child.Pid, &result.ExitCode) {
		result.Confirmed = false
	}
	select {
	case <-drained:
	case <-time.After(1500 * time.Millisecond):
		result.Confirmed = false
		outR.Close()
		errR.Close()
		select {
		case <-drained:
		case <-time.After(1100 * time.Millisecond):
		}
	}
	if !result.Confirmed || result.ExitCode == nil {
		result.State = "uncertain"
		result.Confirmed = false
	} else if result.State == "completed" && *result.ExitCode != 0 {
		result.State = "failed"
	}
	return result
}

func reap(leader int, exit **int) (bool, error) {
	for range 16384 {
		var status unix.WaitStatus
		pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
		if errors.Is(err, unix.ECHILD) {
			return true, nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false, err
		}
		if pid == 0 {
			return false, nil
		}
		if pid == leader {
			code := status.ExitStatus()
			if status.Signaled() {
				code = -int(status.Signal())
			}
			*exit = &code
		}
	}
	return false, errors.New("WSL child reap limit exceeded")
}

func ownedChildren() (map[int]bool, error) {
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil || len(tasks) > 1024 {
		return nil, errors.New("cannot enumerate WSL threads")
	}
	children := map[int]bool{}
	for _, task := range tasks {
		file, err := os.Open(filepath.Join("/proc/self/task", task.Name(), "children"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(file, 128<<10+1))
		file.Close()
		if err != nil || len(data) > 128<<10 {
			return nil, errors.New("WSL child list exceeds limit")
		}
		for _, value := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(value)
			if err != nil || pid <= 0 {
				return nil, errors.New("invalid WSL child identity")
			}
			children[pid] = true
			if len(children) > 16384 {
				return nil, errors.New("WSL child count exceeds limit")
			}
		}
	}
	return children, nil
}

func killOwned(pid int) error {
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	at := strings.LastIndexByte(string(stat), ')')
	if at < 0 {
		return errors.New("invalid WSL child stat")
	}
	fields := strings.Fields(string(stat[at+1:]))
	if len(fields) < 2 {
		return errors.New("invalid WSL child parent")
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return err
	}
	if parent != os.Getpid() {
		return nil
	}
	err = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

func cleanupChildren(leader int, exit **int) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		empty, err := reap(leader, exit)
		if err != nil {
			return false
		}
		children, err := ownedChildren()
		if err != nil {
			return false
		}
		if empty && len(children) == 0 {
			return true
		}
		for pid := range children {
			if err := killOwned(pid); err != nil {
				return false
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	empty, err := reap(leader, exit)
	return empty && err == nil
}
