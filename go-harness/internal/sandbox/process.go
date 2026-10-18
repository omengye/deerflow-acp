package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const cleanupTimeout = 6 * time.Second

type execution struct {
	exitCode    *int
	confirmed   bool
	notExecuted bool
	state       harness.CommandState
	err         error
}
type processSpec struct {
	argv, env    []string
	argv0        string
	cwd          string
	limits       harness.CommandLimits
	remoteInput  bool
	initialInput []byte
	started      func(int)
}
type processTree interface {
	afterStart(*exec.Cmd) error
	terminate() error
	close() error
}

// The command's process Wait and pipe drains have separate ownership. This
// permits killing descendants that inherited output handles after the leader
// exits, without deadlocking in exec.Cmd's implicit pipe-drain wait.
func runProcess(ctx context.Context, spec processSpec, stdout, stderr io.Writer) execution {
	finish := func(err error, code *int, confirmed bool) execution {
		return execution{exitCode: code, confirmed: confirmed, state: stateFor(err, code, confirmed), err: err}
	}
	beforeStart := func(err error) execution {
		r := finish(err, nil, true)
		r.notExecuted = true
		return r
	}
	if err := ctx.Err(); err != nil {
		return beforeStart(err)
	}
	if len(spec.argv) == 0 {
		return beforeStart(fmt.Errorf("empty command"))
	}
	cmd := exec.Command(spec.argv[0], spec.argv[1:]...)
	if spec.argv0 != "" {
		cmd.Args[0] = spec.argv0
	}
	cmd.Dir = spec.cwd
	cmd.Env = spec.env
	outR, outW, err := os.Pipe()
	if err != nil {
		return beforeStart(err)
	}
	defer outR.Close()
	defer outW.Close()
	errR, errW, err := os.Pipe()
	if err != nil {
		return beforeStart(err)
	}
	defer errR.Close()
	defer errW.Close()
	cmd.Stdout, cmd.Stderr = outW, errW
	var input io.WriteCloser
	if spec.remoteInput {
		input, err = cmd.StdinPipe()
		if err != nil {
			return beforeStart(err)
		}
		defer input.Close()
	}
	tree, err := newProcessTree(cmd, spec.limits)
	if err != nil {
		return beforeStart(err)
	}
	defer tree.close()
	if err = cmd.Start(); err != nil {
		return beforeStart(err)
	}
	_ = outW.Close()
	_ = errW.Close()
	if err = tree.afterStart(cmd); err != nil {
		_ = cmd.Process.Kill()
		_ = tree.terminate()
		_ = cmd.Wait()
		return finish(fmt.Errorf("process ownership setup failed: %w", err), nil, true)
	}
	if spec.started != nil {
		spec.started(cmd.Process.Pid)
	}
	var drains sync.WaitGroup
	drains.Add(2)
	go func() { defer drains.Done(); _, _ = io.Copy(stdout, outR) }()
	go func() { defer drains.Done(); _, _ = io.Copy(stderr, errR) }()
	inputDone := make(chan struct{})
	inputStopped := make(chan struct{})
	if input != nil {
		// One writer owns both the initial request and cancellation. A stalled
		// guest cannot block the process wait or interleave control with JSON.
		go func() {
			defer close(inputStopped)
			if _, err := input.Write(spec.initialInput); err != nil {
				return
			}
			select {
			case <-ctx.Done():
				message := "cancelled\n"
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					message = "timed_out\n"
				}
				_, _ = io.WriteString(input, message)
				_ = input.Close()
			case <-inputDone:
			}
		}()
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	var waitErr, reason error
	select {
	case waitErr = <-wait:
	case <-ctx.Done():
		reason = ctx.Err()
		if input != nil {
			select {
			case waitErr = <-wait:
				wait = nil
			case <-time.After(cleanupTimeout):
			}
		}
		if wait != nil {
			_ = tree.terminate()
			_ = cmd.Process.Kill()
			waitErr = <-wait
		}
	}
	// Always clean remaining children, including a successfully detached child
	// whose shell has already exited. The Job/process group is command-owned.
	cleanupErr := tree.terminate()
	if input != nil {
		close(inputDone)
		_ = input.Close()
		<-inputStopped
	}
	drained := make(chan struct{})
	go func() { drains.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(cleanupTimeout):
		_ = outR.Close()
		_ = errR.Close()
		<-drained
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("output pipes did not drain after termination"))
	}
	var code *int
	if cmd.ProcessState != nil {
		n := cmd.ProcessState.ExitCode()
		code = &n
	}
	if reason == nil && waitErr != nil {
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) {
			reason = waitErr
		}
	}
	if cleanupErr != nil {
		return finish(errors.Join(reason, cleanupErr), code, false)
	}
	return finish(reason, code, true)
}
