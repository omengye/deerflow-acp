package eino

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/checkpoint"
)

func TestCheckpointFormatsResumeParallelToolsWithoutReplay(t *testing.T) {
	for _, format := range []string{"legacy-json", "framed"} {
		t.Run(format, func(t *testing.T) {
			f := newParallelPermissionFixture(t, "checkpoint-format-"+format)
			saved, err := decodeCheckpointEnvelope(f.staged.Data)
			if err != nil {
				t.Fatal(err)
			}
			var data []byte
			if format == "legacy-json" {
				data, err = json.Marshal(saved)
			} else {
				native := saved.Native
				saved.Native = nil
				var metadata []byte
				metadata, err = json.Marshal(saved)
				if err == nil {
					data, _, err = checkpoint.Pack(metadata, native)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = f.store.Set(context.Background(), f.staged.ID, data); err != nil {
				t.Fatal(err)
			}
			f.approveAll(t)
			f.assertCompleted(t, f.staged.ID)
		})
	}
}

func TestCheckpointEnvelopeLosslessCompressionAndLegacyVersions(t *testing.T) {
	for _, version := range []int{1, 2} {
		saved := checkpointEnvelope{Version: version, Native: bytes.Repeat([]byte("opaque native state"), 1<<14)}
		if version == 1 {
			saved.Budget = &budgetSnapshot{}
		} else {
			// Reuse a real durable checkpoint identity rather than inventing it.
			f := newParallelPermissionFixture(t, "checkpoint-version-two")
			real, err := decodeCheckpointEnvelope(f.staged.Data)
			if err != nil {
				t.Fatal(err)
			}
			saved.Ledger = real.Ledger
		}
		legacy, err := json.Marshal(saved)
		if err != nil {
			t.Fatal(err)
		}
		native := saved.Native
		saved.Native = nil
		metadata, err := json.Marshal(saved)
		if err != nil {
			t.Fatal(err)
		}
		packed, stats, err := checkpoint.Pack(metadata, native)
		if err != nil || stats.Codec != "gzip" {
			t.Fatalf("compression=%+v err=%v", stats, err)
		}
		for _, data := range [][]byte{legacy, packed} {
			got, err := decodeCheckpointEnvelope(data)
			if err != nil || got.Version != version || !bytes.Equal(got.Native, native) || (got.Budget == nil) != (version == 2) {
				t.Fatalf("version=%d decoded=%+v err=%v", version, got, err)
			}
		}
	}
}

func TestCheckpointCorruptionIsNotClientInputFailure(t *testing.T) {
	data, _, err := checkpoint.Pack([]byte(`{"version":1,"budget":{}}`), []byte("native state"))
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if _, err = decodeCheckpointEnvelope(data); !errors.Is(err, harness.ErrCheckpointInvalid) || errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("checkpoint corruption was misclassified: %v", err)
	}
}

func TestRuntimeCheckpointAuditRetainsSizeAndCodec(t *testing.T) {
	s, session, native := runtimeWithDurableInteraction(t, toolScript(), &receiptEffectTool{run: func(context.Context) (string, error) { return "done", nil }}, false)
	result, err := s.Run(context.Background(), "owner", session.ID, []harness.Content{{Type: "text", Text: "inspect"}}, nil, approveReceiptTool)
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
		t.Fatalf("durable run=%+v err=%v", result, err)
	}
	var format, codec string
	var rawBytes, storedBytes, nativeBytes int
	err = native.DB().QueryRow(`SELECT json_extract(data,'$.checkpoint.format'),json_extract(data,'$.checkpoint.codec'),json_extract(data,'$.checkpoint.rawBytes'),json_extract(data,'$.checkpoint.storedBytes'),json_extract(data,'$.checkpoint.nativeBytes') FROM harness_execution_audits WHERE run_id=? AND kind='waiting_input' LIMIT 1`, result.Execution.RunID).Scan(&format, &codec, &rawBytes, &storedBytes, &nativeBytes)
	if err != nil || format != "dfcp-v1" || (codec != "raw" && codec != "gzip") || rawBytes < storedBytes || storedBytes < 64 || nativeBytes < 1 {
		t.Fatalf("checkpoint audit format=%s codec=%s raw=%d stored=%d native=%d err=%v", format, codec, rawBytes, storedBytes, nativeBytes, err)
	}
}
