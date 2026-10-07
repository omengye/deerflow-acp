package adapter

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestCLIProcessBoundaries(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		write bool
		want  string
	}{
		{"cli-send", true, ""}, {"cli-timeout", true, "delivery_unknown"}, {"cli-timeout", false, "transport"}, {"cli-overflow", true, "delivery_unknown"}, {"cli-transport", true, "transport"}, {"cli-unknown", true, "delivery_unknown"},
	} {
		t.Run(tc.mode+tc.want, func(t *testing.T) {
			t.Setenv("BUZZ_ADAPTER_TEST_HELPER", tc.mode)
			c := testConfig(t)
			c.Buzz.Args = []string{"-test.run=^TestHelperProcess$", "--"}
			c.Buzz.Timeout = 5
			if tc.mode == "cli-timeout" {
				c.Buzz.Timeout = .4
			}
			b := BuzzCLI{Config: c}
			start := time.Now()
			_, e := b.run(context.Background(), nil, "", tc.write, nil, 1000)
			if tc.want == "" {
				if e != nil {
					t.Fatal(e)
				}
			} else if !errorKind(e, tc.want) {
				t.Fatal(e)
			}
			if time.Since(start) > 5*time.Second {
				t.Fatal("subprocess not bounded")
			}
		})
	}
}
func TestCLINoRetryAmbiguousReceipt(t *testing.T) {
	for _, receipt := range []string{"invalid", `{}`, `{"accepted":true}`, `{"accepted":true,"event_id":"invalid"}`} {
		t.Run(receipt, func(t *testing.T) {
			calls := 0
			b := BuzzCLI{Config: testConfig(t), runner: func(context.Context, []string, string, bool, io.Writer, int64) ([]byte, error) {
				calls++
				return []byte(receipt), nil
			}}
			_, e := b.Send(context.Background(), "c", "e", "body")
			if !errorKind(e, "delivery_unknown") || calls != 1 {
				t.Fatal(e, calls)
			}
		})
	}
}
func TestCLISafeRetryAndEditContract(t *testing.T) {
	calls := 0
	b := BuzzCLI{Config: testConfig(t)}
	b.runner = func(_ context.Context, args []string, input string, _ bool, _ io.Writer, _ int64) ([]byte, error) {
		calls++
		if calls < 3 {
			return nil, &CLIError{"transport", "not launched"}
		}
		if args[1] == "edit" {
			if args[len(args)-1] != "body" || input != "" {
				t.Fatal("edit CLI has no stdin contract")
			}
		}
		return []byte(`{"accepted":true,"event_id":"` + strings.Repeat("a", 64) + `"}`), nil
	}
	if e := b.Edit(context.Background(), strings.Repeat("b", 64), "body"); e != nil || calls != 3 {
		t.Fatal(e, calls)
	}
}
func TestCLIMalformedPageDoesNotAdvance(t *testing.T) {
	b := BuzzCLI{Config: testConfig(t), runner: func(context.Context, []string, string, bool, io.Writer, int64) ([]byte, error) {
		return json.Marshal([]any{map[string]any{"content": "bad"}})
	}}
	if _, e := b.Messages(context.Background(), Channel{ID: "c"}, 200, nil, nil); e == nil {
		t.Fatal("invalid page accepted")
	}
}
