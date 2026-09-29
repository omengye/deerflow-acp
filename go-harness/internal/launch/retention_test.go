package launch

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

type retentionEngine struct{}

func (retentionEngine) Run(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
	return harness.RunResult{StopReason: "end_turn"}, nil
}

func TestStartRetentionRunsImmediateSweepAndJoins(t *testing.T) {
	ctx := context.Background()
	policy := harness.RetentionPolicy{Enabled: true, ClosedDays: 0, InactiveDays: 30, CheckInterval: time.Hour}
	c, err := deerflow.Open(ctx, deerflow.Config{DataDir: t.TempDir(), Retention: policy, Engine: retentionEngine{}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	x, err := c.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = c.CloseSession(ctx, x.ID); err != nil {
		t.Fatal(err)
	}
	stop := StartRetention(ctx, c, policy, func(err error) { t.Errorf("retention sweep: %v", err) })
	defer stop()
	request := localhost.ManagementRequest{Operation: "session.list", Fields: map[string]json.RawMessage{"operation": json.RawMessage(`"session.list"`)}}
	deadline := time.After(5 * time.Second)
	for {
		value, listErr := c.ManageLocal(ctx, request)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(value.(map[string]any)["sessions"].([]map[string]any)) == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("startup retention sweep did not delete closed session")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
