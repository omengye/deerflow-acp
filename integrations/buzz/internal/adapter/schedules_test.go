package adapter

import (
	"context"
	"strings"
	"testing"
)

func TestScheduleAdmissionIsPinnedAndSenderSessionsAreIsolated(t *testing.T) {
	s := testState(t)
	one := testMessage(t, "a", 1)
	two := one
	two.ID = strings.Repeat("b", 64)
	two.Author = strings.Repeat("c", 64)
	for _, scope := range []string{"channel", "thread"} {
		one.Tags = [][]string{{"e", strings.Repeat("d", 64), "", "root"}, {"e", strings.Repeat("e", 64), "", "reply"}}
		two.Tags = one.Tags
		if one.conversation(scope) == two.conversation(scope) {
			t.Fatal("senders share trusted session", scope)
		}
	}
	if err := s.EnqueueAndAdvance(context.Background(), one.Channel, 1, []Message{one}, "thread"); err != nil {
		t.Fatal(err)
	}
	one.ScheduleAllowed = true
	if err := s.EnqueueAndAdvance(context.Background(), one.Channel, 1, []Message{one}, "thread"); err != nil {
		t.Fatal(err)
	}
	pending, err := s.Pending()
	if err != nil || len(pending) != 1 || pending[0].ScheduleAllowed {
		t.Fatal("queued message acquired a later grant", pending, err)
	}
}
