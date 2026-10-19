package runtime

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

func legacyExecutionWaitBytesAccepts(data []byte, waits []interaction.TaskWaitBinding) bool {
	var envelope struct {
		TaskWaitsEnabled bool                          `json:"taskWaitsEnabled"`
		TaskWaits        []interaction.TaskWaitBinding `json:"taskWaits"`
	}
	err := json.Unmarshal(data, &envelope)
	return len(waits) == 0 && len(envelope.TaskWaits) == 0 || err == nil && envelope.TaskWaitsEnabled && slices.Equal(envelope.TaskWaits, waits)
}

func TestExecutionWaitBytesJSONCompatibility(t *testing.T) {
	waits := []interaction.TaskWaitBinding{{NativeInterruptID: "native/wait", Wait: interaction.TaskWait{TaskID: "task", CallID: "call", AfterVersion: 1}}}
	bindings, err := json.Marshal(waits)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name, data string
		waits      []interaction.TaskWaitBinding
		accepted   bool
	}{
		{"legacy-opaque", "opaque native checkpoint", nil, true},
		{"permission-only", `{"native":"opaque"}`, nil, true},
		{"empty-waits", `{"taskWaitsEnabled":true,"taskWaits":[]}`, nil, true},
		{"null-waits", `{"taskWaitsEnabled":true,"taskWaits":null}`, nil, true},
		{"bound", `{"taskWaitsEnabled":true,"taskWaits":` + string(bindings) + `}`, waits, true},
		{"case-folded", `{"TASKWAITSENABLED":true,"TASKWAITS":` + string(bindings) + `}`, waits, true},
		{"escaped-key", `{"taskWaitsEnabled":true,"task\u0057aits":` + string(bindings) + `}`, waits, true},
		{"duplicate-enabled", `{"taskWaitsEnabled":false,"taskWaits":` + string(bindings) + `,"taskWaitsEnabled":true}`, waits, true},
		{"nested-decoy", `{"native":{"taskWaits":[{}]},"taskWaitsEnabled":true,"taskWaits":` + string(bindings) + `}`, waits, true},
		{"dropped-waits", `{"taskWaitsEnabled":true,"taskWaits":` + string(bindings) + `}`, nil, false},
		{"missing-waits", `{"taskWaitsEnabled":true}`, waits, false},
		{"disabled", `{"taskWaitsEnabled":false,"taskWaits":` + string(bindings) + `}`, waits, false},
		{"wrong-type", `{"taskWaitsEnabled":"true","taskWaits":` + string(bindings) + `}`, waits, false},
		{"wrong-type-dropped-waits", `{"taskWaitsEnabled":"true","taskWaits":` + string(bindings) + `}`, nil, false},
		{"malformed", `{"taskWaitsEnabled":true,"taskWaits":` + string(bindings), waits, false},
		{"trailing-data", `{"taskWaitsEnabled":true,"taskWaits":` + string(bindings) + `} {}`, waits, false},
	} {
		t.Run(item.name, func(t *testing.T) {
			got := checkExecutionWaitBytes([]byte(item.data), item.waits) == nil
			legacy := legacyExecutionWaitBytesAccepts([]byte(item.data), item.waits)
			if got != item.accepted || got != legacy {
				t.Fatalf("accepted=%v legacy=%v want=%v", got, legacy, item.accepted)
			}
		})
	}
}

func TestExecutionWaitBytesLargeOpaqueCheckpoint(t *testing.T) {
	waits := []interaction.TaskWaitBinding{{NativeInterruptID: "native/wait", Wait: interaction.TaskWait{TaskID: "task"}}}
	data, err := json.Marshal(struct {
		Native           string                        `json:"native"`
		TaskWaitsEnabled bool                          `json:"taskWaitsEnabled"`
		TaskWaits        []interaction.TaskWaitBinding `json:"taskWaits"`
	}{strings.Repeat("A", 32<<20), true, waits})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkExecutionWaitBytes(data, waits); err != nil {
		t.Fatal(err)
	}
	if err := checkExecutionWaitBytes(data, nil); err == nil {
		t.Fatal("large opaque field hid dropped wait bindings")
	}
}

func BenchmarkExecutionWaitBytes(b *testing.B) {
	for _, size := range []int{1024, 32 << 20} {
		data := []byte(`{"native":"` + strings.Repeat("A", size) + `","taskWaitsEnabled":true}`)
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				if err := checkExecutionWaitBytes(data, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
