package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/checkpoint"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

func TestFramedCheckpointBindsTaskWaits(t *testing.T) {
	waits := []interaction.TaskWaitBinding{{NativeInterruptID: "native/wait", Wait: interaction.TaskWait{TaskID: "task", CallID: "call", AfterVersion: 1}}}
	metadata, err := json.Marshal(map[string]any{"version": 2, "taskWaitsEnabled": true, "taskWaits": waits})
	if err != nil {
		t.Fatal(err)
	}
	data, stats, err := checkpoint.Pack(metadata, bytes.Repeat([]byte("opaque native state"), 1<<16))
	if err != nil || stats.Codec != "gzip" {
		t.Fatalf("frame: %+v %v", stats, err)
	}
	if err = checkExecutionWaitBytes(data, waits); err != nil {
		t.Fatal(err)
	}
	if err = checkExecutionWaitBytes(data, nil); !errors.Is(err, harness.ErrExecutionUnresumable) {
		t.Fatalf("frame hid dropped wait bindings: %v", err)
	}
	changed := append([]interaction.TaskWaitBinding(nil), waits...)
	changed[0].NativeInterruptID = "different/wait"
	if err = checkExecutionWaitBytes(data, changed); !errors.Is(err, harness.ErrExecutionUnresumable) {
		t.Fatalf("frame authorized a changed native target: %v", err)
	}
	data[len(data)-1] ^= 1
	if err = checkExecutionWaitBytes(data, waits); !errors.Is(err, harness.ErrCheckpointInvalid) || !errors.Is(err, harness.ErrExecutionUnresumable) {
		t.Fatalf("corrupt frame: %v", err)
	}
}

func TestCheckpointStagingRejectsCorruptFramesBeforePublication(t *testing.T) {
	data, _, err := checkpoint.Pack([]byte(`{"version":2}`), []byte("native state"))
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	a := executionAttempt{lease: ExecutionLease{Scope: budget.Scope{MemberID: "run"}}}
	err = a.stage(context.Background(), interaction.StagedExecutionCheckpoint{ID: "harness/turn/v1/run", Data: data})
	if !errors.Is(err, harness.ErrCheckpointInvalid) || a.checkpoint != nil {
		t.Fatalf("invalid frame was staged: checkpoint=%v err=%v", a.checkpoint != nil, err)
	}
}
