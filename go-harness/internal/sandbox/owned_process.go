package sandbox

import (
	"os/exec"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// OwnedProcess prepares a process group (Unix) or kill-on-close Job (Windows)
// for another local adapter. Prepare must run before Cmd.Start, Attach right
// after Start, and Terminate/Close before the owner releases the process.
type OwnedProcess struct{ tree processTree }

func PrepareOwnedProcess(cmd *exec.Cmd) (*OwnedProcess, error) {
	tree, err := newProcessTree(cmd, harness.CommandLimits{})
	if err != nil {
		return nil, err
	}
	return &OwnedProcess{tree: tree}, nil
}
func (p *OwnedProcess) Attach(cmd *exec.Cmd) error { return p.tree.afterStart(cmd) }
func (p *OwnedProcess) Terminate() error           { return p.tree.terminate() }
func (p *OwnedProcess) Close() error               { return p.tree.close() }
