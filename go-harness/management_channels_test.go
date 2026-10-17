package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/internal/channels"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestChannelManagementLogoutAndCancelReturnAcknowledgements(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(t.TempDir() + "/channels.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := channels.New(ctx, db.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	if _, err = db.DB().Exec(`INSERT INTO harness_channels(id,manifest,config,revision) VALUES('fixture','{}','{}',1)`); err != nil {
		t.Fatal(err)
	}
	m, err = channels.New(ctx, db.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := &Client{channels: m}
	for _, op := range []string{"channel.login.cancel", "channel.logout"} {
		value, err := c.managementChannel(ctx, localhost.ManagementRequest{Operation: op, Fields: map[string]json.RawMessage{"id": json.RawMessage(`"fixture"`)}})
		if err != nil {
			t.Fatal(op, err)
		}
		data, ok := value.(map[string]bool)
		if !ok || len(data) != 1 {
			t.Fatal(op, value)
		}
	}
}

func TestChannelErrorsExposeOnlyHostAuthoredDiagnostics(t *testing.T) {
	var public *localhost.ManagementError
	if err := channelManagementError(&channels.UserError{Message: "choose an existing workspace directory"}); !errors.As(err, &public) {
		t.Fatal("safe actionable diagnostic was hidden")
	}
	if err := channelManagementError(errors.New("provider token=private")); errors.As(err, &public) {
		t.Fatal("provider error escaped redaction")
	}
}
