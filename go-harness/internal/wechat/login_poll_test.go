package wechat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
)

func loginTestPlugin(t *testing.T, poll transportFunc) *Plugin {
	t.Helper()
	p := NewPlugin()
	qr := 0
	p.client = fakeClient(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			qr++
			return response(fmt.Sprintf(`{"qrcode":"qr-%d","qrcode_img_content":"https://example.test/qr/%d"}`, qr, qr)), nil
		case "/ilink/bot/get_qrcode_status":
			return poll(r)
		default:
			t.Fatalf("unexpected endpoint: %s", r.URL.Path)
			return nil, nil
		}
	})
	return p
}

func startPluginLogin(t *testing.T, p *Plugin) channelproto.Login {
	t.Helper()
	value, err := p.Handle(context.Background(), "login.start", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	login := value.(channelproto.Login)
	if login.ID == "" || login.State != "pending" || login.QR == "" || login.ExpiresIn != 180 {
		t.Fatal("invalid login challenge", login)
	}
	return login
}

func pollPluginLogin(t *testing.T, p *Plugin, ctx context.Context, id, verify string) (channelproto.Login, error) {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"id": id, "verify_code": verify})
	if err != nil {
		t.Fatal(err)
	}
	value, err := p.Handle(ctx, "login.poll", raw)
	if err != nil {
		return channelproto.Login{}, err
	}
	return value.(channelproto.Login), nil
}

func assertPluginLoginCleared(t *testing.T, p *Plugin) {
	t.Helper()
	if p.id != "" || p.origin != "" || p.verify != "" || p.loginState != "" || p.qr != (QR{}) || !p.expires.IsZero() {
		t.Fatal("terminal login retained attempt data")
	}
}

func TestPluginLoginPollTimeoutsRetainAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		polls := 0
		p := loginTestPlugin(t, func(r *http.Request) (*http.Response, error) {
			polls++
			if r.URL.Query().Get("qrcode") != "qr-1" {
				t.Fatal("poll replaced the original QR")
			}
			if polls <= 3 {
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			return response(`{"status":"confirmed","bot_token":"private","ilink_bot_id":"bot","ilink_user_id":"owner"}`), nil
		})
		login := startPluginLogin(t, p)
		started, expires := time.Now(), p.expires
		for i := 0; i < 3; i++ {
			value, err := pollPluginLogin(t, p, context.Background(), login.ID, "")
			if err != nil || value.ID != login.ID || value.State != "pending" || value.Auth != nil {
				t.Fatal("normal long poll ended the login", value.State, err)
			}
			if p.qr.Content != login.QR || !p.expires.Equal(expires) {
				t.Fatal("poll replaced or extended the QR")
			}
		}
		if time.Since(started) != 21*time.Second {
			t.Fatal("poll no longer respects the 7 second management budget")
		}
		value, err := pollPluginLogin(t, p, context.Background(), login.ID, "")
		if err != nil || value.ID != login.ID || value.State != "confirmed" || value.Auth == nil || value.Auth.Owner != "owner" {
			t.Fatal("login could not complete after repeated waits", value.State, err)
		}
		assertPluginLoginCleared(t, p)
	})
}

func TestPluginLoginVerificationSurvivesTimeoutsAndRedirects(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		polls := 0
		p := loginTestPlugin(t, func(r *http.Request) (*http.Response, error) {
			polls++
			wantVerify := "123456"
			if polls == 1 {
				wantVerify = ""
			} else if polls == 7 {
				wantVerify = "654321"
			}
			if r.URL.Query().Get("verify_code") != wantVerify {
				t.Fatal("verification code lost or reused after rejection")
			}
			if polls >= 4 && r.URL.Host != "other.weixin.qq.com" {
				t.Fatal("provider redirect was not retained")
			}
			switch polls {
			case 1, 6:
				return response(`{"status":"need_verifycode"}`), nil
			case 2, 4:
				<-r.Context().Done()
				return nil, r.Context().Err()
			case 3:
				return response(`{"status":"scaned_but_redirect","redirect_host":"other.weixin.qq.com"}`), nil
			case 5:
				return response(`{"status":"wait"}`), nil
			default:
				return response(`{"status":"confirmed","bot_token":"private","ilink_bot_id":"bot","ilink_user_id":"owner"}`), nil
			}
		})
		login := startPluginLogin(t, p)
		for i, want := range []string{"verification_required", "pending", "scanned", "scanned", "scanned", "verification_required", "confirmed"} {
			verify := ""
			if i == 1 {
				verify = "123456"
			} else if i == 6 {
				verify = "654321"
			}
			value, err := pollPluginLogin(t, p, context.Background(), login.ID, verify)
			if err != nil || value.ID != login.ID || value.State != want {
				t.Fatalf("step %d: got %s, want %s; %v", i, value.State, want, err)
			}
			if want == "verification_required" && p.verify != "" {
				t.Fatal("rejected verification code retained")
			}
		}
		assertPluginLoginCleared(t, p)
	})
}

func TestPluginLoginPollDoesNotHideCancellationOrFailures(t *testing.T) {
	for _, mode := range []string{"caller_cancel", "caller_deadline", "network", "transport_deadline", "provider", "invalid_credentials"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if mode == "caller_deadline" {
					var deadlineCancel context.CancelFunc
					ctx, deadlineCancel = context.WithTimeout(ctx, time.Second)
					defer deadlineCancel()
				}
				p := loginTestPlugin(t, func(r *http.Request) (*http.Response, error) {
					switch mode {
					case "caller_cancel", "caller_deadline":
						if mode == "caller_cancel" {
							cancel()
						}
						<-r.Context().Done()
						return nil, r.Context().Err()
					case "network":
						return nil, errors.New("connection failed")
					case "transport_deadline":
						return nil, context.DeadlineExceeded
					case "provider":
						return response(`{"ret":-1}`), nil
					default:
						return response(`{"status":"confirmed","bot_token":"private","ilink_bot_id":"bot","baseurl":"https://evil.test"}`), nil
					}
				})
				login := startPluginLogin(t, p)
				_, err := pollPluginLogin(t, p, ctx, login.ID, "")
				if err == nil {
					t.Fatal("actual failure was converted into a successful wait")
				}
				if mode == "caller_cancel" && !errors.Is(err, context.Canceled) {
					t.Fatal("caller cancellation lost", err)
				}
				if mode == "caller_deadline" && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("caller deadline lost", err)
				}
			})
		})
	}
}

func TestPluginLoginExpiresAtOriginalDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := loginTestPlugin(t, func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		})
		login := startPluginLogin(t, p)
		started := time.Now()
		for i := 0; i < 26; i++ {
			value, err := pollPluginLogin(t, p, context.Background(), login.ID, "123456")
			want := "pending"
			if i == 25 {
				want = "expired"
			}
			if err != nil || value.ID != login.ID || value.State != want || value.Auth != nil {
				t.Fatal("incorrect expiry state", i, value.State, err)
			}
		}
		if time.Since(started) != 180*time.Second {
			t.Fatal("polling changed the original QR lifetime", time.Since(started))
		}
		assertPluginLoginCleared(t, p)
		if _, err := pollPluginLogin(t, p, context.Background(), login.ID, ""); !errors.Is(err, channelproto.ErrAuthExpired) {
			t.Fatal("expired login remained usable", err)
		}
	})
}

func TestPluginLoginTerminalProviderStatesClearAttempt(t *testing.T) {
	for _, state := range []string{"expired", "expired_code", "verify_code_blocked"} {
		t.Run(state, func(t *testing.T) {
			p := loginTestPlugin(t, func(*http.Request) (*http.Response, error) {
				if state == "expired_code" {
					return response(`{"ret":-14}`), nil
				}
				return response(fmt.Sprintf(`{"status":%q}`, state)), nil
			})
			login := startPluginLogin(t, p)
			value, err := pollPluginLogin(t, p, context.Background(), login.ID, "123456")
			want := "expired"
			if state == "verify_code_blocked" {
				want = "failed"
			}
			if err != nil || value.State != want || value.ID != login.ID {
				t.Fatal("incorrect terminal state", value.State, err)
			}
			assertPluginLoginCleared(t, p)
		})
	}
}

func TestPluginLoginCancelAndRefreshFenceOldAttempt(t *testing.T) {
	for _, operation := range []string{"login.cancel", "logout", "login.start"} {
		t.Run(operation, func(t *testing.T) {
			p := loginTestPlugin(t, func(*http.Request) (*http.Response, error) {
				return response(`{"status":"wait"}`), nil
			})
			old := startPluginLogin(t, p)
			if _, err := pollPluginLogin(t, p, context.Background(), old.ID, "123456"); err != nil {
				t.Fatal(err)
			}
			if operation != "login.start" {
				if _, err := p.Handle(context.Background(), operation, json.RawMessage(`{}`)); err != nil {
					t.Fatal(err)
				}
				assertPluginLoginCleared(t, p)
			}
			next := startPluginLogin(t, p)
			if next.ID == old.ID || next.QR == old.QR || p.verify != "" {
				t.Fatal("new login reused the previous attempt")
			}
			if _, err := pollPluginLogin(t, p, context.Background(), old.ID, ""); !errors.Is(err, channelproto.ErrAuthExpired) {
				t.Fatal("old QR remained usable", err)
			}
			if value, err := pollPluginLogin(t, p, context.Background(), next.ID, ""); err != nil || value.State != "pending" {
				t.Fatal("stale poll damaged the new login", err)
			}
		})
	}
}
