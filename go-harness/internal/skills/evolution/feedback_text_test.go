package evolution

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestFeedbackRedactsCompleteAuthenticationHeadersBeforePersistence(t *testing.T) {
	f := newCollectorFixture(t)
	run := f.run(t, "", 1, "completed", false)
	f.drain(t, 128)
	evidence := feedbackEvidence(t, f, run)
	cases := []struct {
		input   string
		private []string
	}{
		{"Authorization: Basic Zm9vOmJhcg==", []string{"Basic", "Zm9vOmJhcg=="}},
		{"Cookie: session=first-private; sid=second-private", []string{"first-private", "second-private"}},
		{"Proxy-Authorization: Digest username=private-user, response=private-digest", []string{"private-user", "private-digest"}},
		{"Set-Cookie: sid=private-cookie;\r\n Secure; HttpOnly\r\nClarify the verification step.", []string{"private-cookie", "HttpOnly"}},
	}
	for i, test := range cases {
		request := feedbackRequest(evidence, "header-"+string(rune('a'+i)))
		request.ExpectedFeedbackVersion = int64(i)
		request.Summary = test.input
		saved := mustSubmitFeedback(t, f.service, request)
		if !saved.Feedback.SummaryRedacted {
			t.Fatal("credential header was not identified")
		}
		var payload []byte
		if err := f.db.DB().QueryRow(`SELECT payload FROM df_skill_feedback WHERE signal_id=? AND revision=?`, request.SignalID, saved.Feedback.Revision).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		for _, secret := range test.private {
			if strings.Contains(string(payload), secret) || strings.Contains(saved.Feedback.Summary, secret) {
				t.Fatal("recognized authentication header retained a value")
			}
		}
		current, err := f.service.Feedback(context.Background(), request.SignalID)
		if err != nil || current.Feedback == nil || current.Feedback.Summary != saved.Feedback.Summary {
			t.Fatal("redacted feedback cannot be read back", err)
		}
		encoded, _ := json.Marshal(current)
		for _, secret := range test.private {
			if strings.Contains(string(encoded), secret) {
				t.Fatal("feedback query exposed a credential value")
			}
		}
	}
}
