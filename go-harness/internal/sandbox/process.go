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
	exitCode  *int
	confirmed bool
	state     harness.CommandState
	err       error
}
type processSpec struct {
	argv, env   []string
	cwd         string
	limits      harness.CommandLimits
	remoteInput bool
	started     func(int)
}
type processTree interface {
	afterStart(*exec.Cmd) error
	terminate() error
	close() error
}

// The command's process Wait and pipe drains have separate ownership. This
// permits killing descendants that inherited output handles after the leader
// exits, without deadlocking in exec.Cmd's implicit pipe-drain wait.
func runProcess(ctx context.Context, spec processSpec, stdout, stderr *capture) execution {
	finish := func(err error, code *int, confirmed bool) execution {
		return execution{exitCode: code, confirmed: confirmed, state: stateFor(err, code, confirmed), err: err}
	}
	if err := ctx.Err(); err != nil {
		return finish(err, nil, true)
	}
	if len(spec.argv) == 0 {
		return finish(fmt.Errorf("empty command"), nil, true)
	}
	cmd := exec.Command(spec.argv[0], spec.argv[1:]...)
	cmd.Dir = spec.cwd
	cmd.Env = spec.env
	outR, outW, err := os.Pipe()
	if err != nil {
		return finish(err, nil, true)
	}
	defer outR.Close()
	defer outW.Close()
	errR, errW, err := os.Pipe()
	if err != nil {
		return finish(err, nil, true)
	}
	defer errR.Close()
	defer errW.Close()
	cmd.Stdout, cmd.Stderr = outW, errW
	var input io.WriteCloser
	if spec.remoteInput {
		input, err = cmd.StdinPipe()
		if err != nil {
			return finish(err, nil, true)
		}
		defer input.Close()
	}
	tree, err := newProcessTree(cmd, spec.limits)
	if err != nil {
		return finish(err, nil, true)
	}
	defer tree.close()
	if err = cmd.Start(); err != nil {
		return finish(err, nil, true)
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
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	var waitErr, reason error
	select {
	case waitErr = <-wait:
	case <-ctx.Done():
		reason = ctx.Err()
		if input != nil {
			message := "cancelled\n"
			if errors.Is(reason, context.DeadlineExceeded) {
				message = "timed_out\n"
			}
			_, _ = io.WriteString(input, message)
			_ = input.Close()
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
		_ = input.Close()
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
