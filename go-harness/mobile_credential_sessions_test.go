package deerflow

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
)

func sealPhoneFixture(form secrets.EnrollmentForm, username, secret string) (*secrets.SealedCredential, error) {
	raw, err := x509.ParsePKIXPublicKey(form.PublicKey)
	if err != nil {
		return nil, err
	}
	server, err := raw.(*ecdsa.PublicKey).ECDH()
	if err != nil {
		return nil, err
	}
	client, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := client.ECDH(server)
	if err != nil {
		return nil, err
	}
	label := "DeerFlow service credential v1"
	if form.Version == 2 {
		label = "DeerFlow service credential v2"
	}
	key, err := hkdf.Key(sha256.New, shared, nil, label, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 12)
	rand.Read(nonce)
	plain := binary.BigEndian.AppendUint32(nil, uint32(len(username)))
	plain = append(append(plain, username...), secret...)
	der, err := x509.MarshalPKIXPublicKey(client.PublicKey())
	if err != nil {
		return nil, err
	}
	aad := []byte(label + "\x00" + form.Consumer + "\x00" + form.Service + "\x00" + form.Account)
	if form.Version == 2 {
		aad = append(aad, ("\x00" + strings.Join(form.RequiredFields, ","))...)
	}
	return &secrets.SealedCredential{ClientKey: der, Nonce: nonce, Ciphertext: gcm.Seal(nil, nonce, plain, aad)}, nil
}

func TestMobileCredentialAcrossIndependentClients(t *testing.T) {
	t.Run("legacy-pair", func(t *testing.T) { mobileCredentialAcrossClients(t, 1) })
	t.Run("remote-token", func(t *testing.T) { mobileCredentialAcrossClients(t, 2) })
}

func TestSecureCredentialFormBindsFieldSelection(t *testing.T) {
	client, err := Open(t.Context(), Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture-only"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	scope, _ := secrets.NewCredentialScope("tool:web_fetch", "https://offline.example", "default")
	v := client.secrets
	form, err := v.NewEnrollment(t.Context(), scope, secrets.UsernameField, 2)
	if err != nil || form == nil || !slices.Equal(form.Form.RequiredFields, []string{"username"}) {
		t.Fatal("missing username-only form", err)
	}
	wrong := form.Form
	wrong.RequiredFields = []string{"key"}
	for _, attempt := range []struct {
		public    secrets.EnrollmentForm
		user, key string
		ok        bool
	}{
		{wrong, "fixture-user", "", false},
		{form.Form, "fixture-user", "unrequested", false},
		{form.Form, "u", "", true},
	} {
		sealed, err := sealPhoneFixture(attempt.public, attempt.user, attempt.key)
		if err != nil {
			t.Fatal(err)
		}
		if err = form.Save(t.Context(), v, sealed); (err == nil) != attempt.ok {
			t.Fatal("unexpected field selection acceptance", err)
		}
	}
	remaining, err := v.NewEnrollment(t.Context(), scope, secrets.BothFields, 2)
	if err != nil || remaining == nil || !slices.Equal(remaining.Form.RequiredFields, []string{"key"}) {
		t.Fatal("key not requested for the existing username", err)
	}
	response, err := sealPhoneFixture(remaining.Form, "", "k")
	if err != nil {
		t.Fatal(err)
	}
	if err = remaining.Save(t.Context(), v, response); err != nil {
		t.Fatal(err)
	}
	if err = remaining.Save(t.Context(), v, response); err == nil {
		t.Fatal("consumed enrollment overwrote the saved key")
	}
}

func mobileCredentialAcrossClients(t *testing.T, version int) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	user, key := randomTestSecret(t, "mobile-user-"), randomTestSecret(t, "mobile-secret-")
	binding := map[string]string{"service": "https://mobile.example", "key_env": "FIXTURE_TOKEN"}
	if version == 1 {
		binding["username_env"] = "FIXTURE_OTHER"
	}
	provider, seen := secretProvider(t, []secretToolStep{{"execute", map[string]any{"executable": exe, "args": []string{"-test.run=^TestSecretCommandFixtureChild$"}, "credential": binding}}})
	root, workspace := t.TempDir(), testutil.CanonicalTempDir(t)
	cfg := Config{DataDir: root, Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", DisableSubagents: true, PermissionMode: harness.PermissionModeOff, Sandbox: harness.SandboxConfig{Enabled: true, Provider: harness.SandboxLocal, AllowedExecutables: []string{exe}, Environment: map[string]string{"DEERFLOW_SECRET_COMMAND_FIXTURE": "1"}, Limits: harness.CommandLimits{Timeout: 15 * time.Second}}}
	if version == 2 {
		cfg.PermissionMode = harness.PermissionModeDangerous
	}
	scope, _ := secrets.NewCredentialScope("tool:execute", "https://mobile.example", "default")
	u, k := scope.Names()
	var old *secrets.SealedCredential
	for _, mode := range []string{"disconnect", "reject", "stale", "plain-approval", "enroll", "replace-phone"} {
		t.Run(mode, func(t *testing.T) {
			client, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			sin, cout := io.Pipe()
			cin, sout := io.Pipe()
			var approvals atomic.Int32
			var peer *protocol.Peer
			handler := func(_ context.Context, method string, raw json.RawMessage) (any, error) {
				if bytes.Contains(raw, []byte(key)) || bytes.Contains(raw, []byte(user)) {
					return nil, errors.New("plaintext in ACP callback")
				}
				if method != "session/request_permission" {
					return nil, nil
				}
				approvals.Add(1)
				var req struct {
					Meta struct {
						Deerflow struct {
							Form               *secrets.EnrollmentForm `json:"credentialForm"`
							CredentialApproval bool                    `json:"credentialApproval"`
						} `json:"deerflow"`
					} `json:"_meta"`
				}
				if err := json.Unmarshal(raw, &req); err != nil {
					return nil, err
				}
				form := req.Meta.Deerflow.Form
				if mode == "replace-phone" {
					t.Error("using saved credentials requested another decision from the replacement phone")
					return nil, errors.New("unexpected permission request")
				}
				if !req.Meta.Deerflow.CredentialApproval || form == nil || form.CredentialScope != scope || form.Version != version || version == 2 && !slices.Equal(form.RequiredFields, []string{"key"}) {
					t.Error("unexpected public enrollment form")
					return nil, errors.New("wrong form")
				}
				if mode == "disconnect" {
					_ = peer.Close()
					return nil, context.Canceled
				}
				answer := map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": "allow_once"}}
				login := user
				if version == 2 {
					login = ""
				}
				sealed, err := sealPhoneFixture(*form, login, key)
				if err != nil {
					return nil, err
				}
				switch mode {
				case "reject":
					old = sealed
					answer["outcome"] = map[string]string{"outcome": "selected", "optionId": "reject_once"}
				case "stale":
					answer["_meta"] = map[string]any{"sealedCredential": old}
				case "enroll":
					answer["_meta"] = map[string]any{"sealedCredential": sealed}
				}
				return answer, nil
			}
			peer = protocol.NewPeer(cin, cout, handler, protocol.Options{})
			wire := &continuationACPConnection{peer: peer, serverDone: make(chan error, 1), clientDone: make(chan error, 1)}
			go func() { wire.serverDone <- client.ServeACP(ctx, sin, sout) }()
			go func() { wire.clientDone <- wire.peer.Serve(ctx) }()
			defer wire.close(t)
			capability := map[string]any{"_meta": map[string]any{"deerflow": map[string]any{"mobileCredentials": version}}}
			if err = wire.peer.Call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": capability}, nil); err != nil {
				t.Fatal(err)
			}
			var sess struct {
				ID string `json:"sessionId"`
			}
			if err = wire.peer.Call(ctx, "session/new", map[string]any{"cwd": workspace, "mcpServers": []any{}}, &sess); err != nil {
				t.Fatal(err)
			}
			approval := harness.ApprovalAllowAlways
			if version == 2 && mode == "enroll" {
				// The command decision and missing key share one interaction.
				approval = harness.ApprovalAsk
			}
			if err = wire.peer.Call(ctx, "session/set_config_option", map[string]string{"sessionId": sess.ID, "configId": "approval", "value": approval}, nil); err != nil {
				t.Fatal(err)
			}
			err = wire.peer.Call(ctx, "session/prompt", map[string]any{"sessionId": sess.ID, "prompt": []harness.Content{{Type: "text", Text: "Use the configured service."}}}, nil)
			if mode != "stale" && mode != "disconnect" && mode != "plain-approval" && err != nil {
				t.Fatal(err)
			}
			wire.close(t)
			missing, err := client.secrets.Missing(ctx, []string{u, k})
			if err != nil {
				t.Fatal(err)
			}
			saved := mode == "enroll" || mode == "replace-phone"
			wantPrompts := int32(1)
			if mode == "replace-phone" {
				wantPrompts = 0
			}
			if approvals.Load() != wantPrompts || slices.Contains(missing, k) == saved || slices.Contains(missing, u) == (saved && version == 1) {
				t.Fatalf("approvals=%d missing=%d", approvals.Load(), len(missing))
			}
		})
	}
	bodies, requests := seen()
	for _, body := range bodies {
		if bytes.Contains(body, []byte(key)) || bytes.Contains(body, []byte(user)) {
			t.Fatal("credential reached model")
		}
	}
	success := 0
	for _, r := range requests {
		for _, v := range r.toolResults() {
			if bytes.Contains([]byte(v), []byte("token=[secret:DF_CREDENTIAL_")) {
				success++
			}
		}
	}
	if success != 2 {
		t.Fatalf("successful masked tool results=%d, want 2", success)
	}
	assertNoSecretBytes(t, root, user, key)
}
