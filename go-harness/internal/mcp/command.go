package mcp

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Own both pipes so Close also unblocks reads when a descendant inherits stdout.
// This is bounded direct-child cleanup, not a process-tree sandbox.
type commandTransport struct {
	command   string
	args, env []string
	cwd       string
	grace     time.Duration
}

func (t *commandTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(t.command, t.args...)
	cmd.Dir, cmd.Env, cmd.Stderr = t.cwd, t.env, io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}
	conn, err := (&sdk.IOTransport{Reader: stdout, Writer: stdin}).Connect(ctx)
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	c := &commandConnection{Connection: conn, terminate: cmd.Process.Kill, grace: t.grace, exited: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(c.exited) }()
	return c, nil
}

type commandConnection struct {
	sdk.Connection
	terminate func() error
	grace     time.Duration
	exited    chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func (c *commandConnection) Close() error {
	c.closeOnce.Do(func() {
		// Closing the pipes terminates protocol I/O even if the direct child or
		// one of its descendants keeps an inherited stdout handle alive.
		_ = c.Connection.Close()
		timer := time.NewTimer(c.grace)
		defer timer.Stop()
		select {
		case <-c.exited:
			return
		case <-timer.C:
		}
		if err := c.terminate(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			c.closeErr = errors.New("MCP process termination failed")
		}
		// Even a failed Kill cannot relinquish ownership of a live child. Release
		// may stop waiting, but global Close must retain the runtime lock until
		// Wait confirms process exit (possibly after external intervention).
		<-c.exited
	})
	return c.closeErr
}
