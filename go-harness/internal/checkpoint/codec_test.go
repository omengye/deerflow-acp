package checkpoint

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestFrameRoundTrip(t *testing.T) {
	metadata := []byte(`{"version":2,"taskWaitsEnabled":true,"taskWaits":[]}`)
	random := make([]byte, 128<<10)
	_, _ = rand.NewChaCha8([32]byte{7}).Read(random)
	for _, item := range []struct {
		name   string
		native []byte
		codec  string
	}{
		{"small", []byte("native checkpoint"), "raw"},
		{"compressible", bytes.Repeat([]byte("native checkpoint message and tool state\n"), 1<<14), "gzip"},
		{"incompressible", random, "raw"},
	} {
		t.Run(item.name, func(t *testing.T) {
			packed, stats, err := Pack(metadata, item.native)
			if err != nil {
				t.Fatal(err)
			}
			gotMetadata, gotNative, decoded, err := Unpack(packed)
			if err != nil || !bytes.Equal(gotMetadata, metadata) || !bytes.Equal(gotNative, item.native) || stats != decoded || stats.Codec != item.codec {
				t.Fatalf("round trip: stats=%+v decoded=%+v err=%v", stats, decoded, err)
			}
			if stats.StoredBytes > stats.RawBytes || stats.NativeBytes != len(item.native) {
				t.Fatalf("size accounting: %+v", stats)
			}
			if stats.Codec == "gzip" && stats.StoredBytes >= stats.RawBytes/4 {
				t.Fatalf("compressible state was not reduced: %+v", stats)
			}
		})
	}
}

func TestLegacyCheckpointBytesRemainUnchanged(t *testing.T) {
	for _, original := range [][]byte{[]byte(`{"version":1,"native":"Ynl0ZXM="}`), []byte(`{"version":2,"native":"Ynl0ZXM="}`), []byte("legacy opaque native fixture")} {
		metadata, native, stats, err := Unpack(original)
		if err != nil || !bytes.Equal(metadata, original) || native != nil || stats.Format != "legacy-json" || stats.StoredBytes != len(original) {
			t.Fatalf("legacy bytes changed: %+v %v", stats, err)
		}
	}
}

func repairChecksum(data []byte) {
	sum := checksum(data)
	copy(data[32:headerSize], sum[:])
}

func TestFrameRejectsDamageAndUnboundedLengths(t *testing.T) {
	good, _, err := Pack([]byte(`{"version":2}`), bytes.Repeat([]byte("state\n"), 1<<15))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name   string
		mutate func([]byte) []byte
		want   error
	}{
		{"short-header", func(b []byte) []byte { return b[:12] }, harness.ErrCheckpointInvalid},
		{"magic", func(b []byte) []byte { b[5] ^= 1; return b }, harness.ErrCheckpointInvalid},
		{"version", func(b []byte) []byte { b[8]++; return b }, harness.ErrCheckpointInvalid},
		{"codec", func(b []byte) []byte { b[9] = 99; return b }, harness.ErrCheckpointInvalid},
		{"reserved-flags", func(b []byte) []byte { b[10] = 1; return b }, harness.ErrCheckpointInvalid},
		{"metadata", func(b []byte) []byte { b[headerSize] ^= 1; return b }, harness.ErrCheckpointInvalid},
		{"invalid-json-metadata", func(b []byte) []byte {
			b[headerSize] = '['
			repairChecksum(b)
			return b
		}, harness.ErrCheckpointInvalid},
		{"payload", func(b []byte) []byte { b[len(b)-1] ^= 1; return b }, harness.ErrCheckpointInvalid},
		{"stored-length", func(b []byte) []byte { binary.BigEndian.PutUint64(b[24:32], math.MaxUint64); return b }, harness.ErrCheckpointInvalid},
		{"raw-limit", func(b []byte) []byte { binary.BigEndian.PutUint64(b[16:24], MaxRawBytes+1); return b }, harness.ErrCheckpointTooLarge},
		{"raw-overflow", func(b []byte) []byte { binary.BigEndian.PutUint64(b[16:24], math.MaxUint64); return b }, harness.ErrCheckpointTooLarge},
		{"metadata-limit", func(b []byte) []byte { binary.BigEndian.PutUint32(b[12:16], math.MaxUint32); return b }, harness.ErrCheckpointTooLarge},
		{"empty-state", func(b []byte) []byte { binary.BigEndian.PutUint64(b[16:24], 0); return b }, harness.ErrCheckpointInvalid},
		{"decompression-bomb", func(b []byte) []byte {
			binary.BigEndian.PutUint64(b[16:24], 1)
			repairChecksum(b)
			return b
		}, harness.ErrCheckpointInvalid},
		{"gzip-crc", func(b []byte) []byte {
			b[len(b)-8] ^= 1
			repairChecksum(b)
			return b
		}, harness.ErrCheckpointInvalid},
		{"trailing-bytes", func(b []byte) []byte {
			b = append(b, 1, 2, 3)
			binary.BigEndian.PutUint64(b[24:32], binary.BigEndian.Uint64(b[24:32])+3)
			repairChecksum(b)
			return b
		}, harness.ErrCheckpointInvalid},
		{"gzip-truncated", func(b []byte) []byte {
			b = b[:len(b)-4]
			binary.BigEndian.PutUint64(b[24:32], binary.BigEndian.Uint64(b[24:32])-4)
			repairChecksum(b)
			return b
		}, harness.ErrCheckpointInvalid},
	} {
		t.Run(item.name, func(t *testing.T) {
			data := item.mutate(bytes.Clone(good))
			if _, _, _, err := Unpack(data); !errors.Is(err, item.want) {
				t.Fatalf("malformed checkpoint: got=%v want=%v", err, item.want)
			}
		})
	}
}

func TestCompressionDoesNotBypassRawBudget(t *testing.T) {
	native := make([]byte, MaxRawBytes)
	_, _, err := Pack([]byte(`{"version":2}`), native)
	var size *harness.CheckpointSizeError
	if !errors.As(err, &size) || size.Representation != "raw" || size.Bytes <= MaxRawBytes || size.Limit != MaxRawBytes {
		t.Fatalf("compressible oversized state bypassed the raw budget: %v", err)
	}
	if _, _, err := Metadata(append(native, 1)); !errors.As(err, &size) || size.Representation != "stored" {
		t.Fatalf("oversized stored checkpoint: %v", err)
	}
}
