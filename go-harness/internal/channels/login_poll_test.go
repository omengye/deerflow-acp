package channels

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestLoginWaitsPreserveAttemptAndBindingEnablesFullAccess(t *testing.T) {
	m, i := fixture(t, nil)
	ctx := context.Background()
	i.cfg.Enabled = false
	i.auth = channelproto.Auth{}
	if err := m.persist(ctx, i); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetPermissions(ctx, i.id, i.revision, harness.ApprovalAllowAlways); err == nil {
		t.Fatal("Full Access was granted before account binding")
	}
	polls, starts := 0, 0
	plugin := &fakeTransport{call: func(_ context.Context, op string, in, out any) error {
		if op == "login.start" {
			return assign(out, channelproto.Login{ID: "attempt", State: "pending", QR: "qr", ExpiresIn: 180})
		}
		if op != "login.poll" {
			t.Fatal("unexpected plugin operation", op)
		}
		polls++
		if polls <= 3 {
			return assign(out, channelproto.Login{ID: "attempt", State: "pending"})
		}
		return assign(out, channelproto.Login{ID: "attempt", State: "confirmed", Auth: &channelproto.Auth{
			Account: "bot", Owner: "owner", Secret: json.RawMessage(`{"token":"private"}`),
		}})
	}}
	m.start = func(context.Context, Manifest) (transport, channelproto.Descriptor, error) {
		starts++
		return plugin, i.manifest.Descriptor, nil
	}
	if _, err := m.Login(ctx, i.id, "login.start", "", ""); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 3; n++ {
		value, err := m.Login(ctx, i.id, "login.poll", "attempt", "")
		if err != nil {
			t.Fatal("pending poll ended the login", err)
		}
		if value.(map[string]any)["state"] != "pending" || i.login != "attempt" || i.proc != plugin || starts != 1 {
			t.Fatal("pending poll replaced the login attempt or plugin")
		}
	}
	if _, err := m.Login(ctx, i.id, "login.poll", "attempt", ""); err != nil {
		t.Fatal("login failed after repeated waits", err)
	}
	if i.auth.Owner != "owner" || i.login != "" || fullAccessAuthorized(i.cfg, i.auth) {
		t.Fatal("login did not bind the account independently of Full Access")
	}
	if _, err := m.SetPermissions(ctx, i.id, i.revision, harness.ApprovalAllowAlways); err != nil {
		t.Fatal("bound account cannot authorize Full Access", err)
	}
	if !fullAccessAuthorized(i.cfg, i.auth) {
		t.Fatal("Full Access authorization was not saved")
	}
}
