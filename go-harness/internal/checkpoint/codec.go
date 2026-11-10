// Package checkpoint frames harness metadata and opaque native state without
// base64, with optional lossless compression. Legacy JSON envelopes remain readable.
package checkpoint

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const (
	// Both budgets include metadata and the frame header. Compression never
	// authorizes an unbounded decoded checkpoint or a larger native allocation.
	MaxRawBytes    = 64 << 20
	MaxStoredBytes = 64 << 20
	headerSize     = 64
	compressAfter  = 64 << 10
	formatVersion  = 1
	codecRaw       = 0
	codecGzip      = 1
)

var magic = [8]byte{'D', 'F', 'C', 'P', '\r', '\n', 0x1a, '\n'}

// Stats contains only lengths and format identifiers, suitable for local audits.
type Stats struct {
	Format        string `json:"format"`
	Codec         string `json:"codec"`
	RawBytes      int    `json:"rawBytes"`
	StoredBytes   int    `json:"storedBytes"`
	NativeBytes   int    `json:"nativeBytes"`
	MetadataBytes int    `json:"metadataBytes"`
}

func invalid(reason string) error { return fmt.Errorf("%w: %s", harness.ErrCheckpointInvalid, reason) }

func sizeLimit(representation string, size uint64, limit uint64) error {
	return &harness.CheckpointSizeError{Representation: representation, Bytes: size, Limit: limit}
}

// Pack leaves small metadata readable without inflating native state inside a
// SQLite transaction. The checksum binds the header, metadata and stored state.
func Pack(metadata, native []byte) ([]byte, Stats, error) {
	stats := Stats{Format: "dfcp-v1", Codec: "raw", NativeBytes: len(native), MetadataBytes: len(metadata)}
	rawSize := uint64(headerSize) + uint64(len(metadata)) + uint64(len(native))
	if rawSize > MaxRawBytes {
		return nil, stats, sizeLimit("raw", rawSize, MaxRawBytes)
	}
	if len(metadata) == 0 || len(native) == 0 {
		return nil, stats, invalid("metadata and native state are required")
	}
	if !json.Valid(metadata) {
		return nil, stats, invalid("metadata is not valid JSON")
	}
	stats.RawBytes = int(rawSize)
	payload, codec := native, byte(codecRaw)
	if len(native) >= compressAfter {
		var buffer bytes.Buffer
		writer, err := gzip.NewWriterLevel(&buffer, gzip.BestSpeed)
		if err != nil {
			return nil, stats, err
		}
		if _, err = writer.Write(native); err != nil {
			return nil, stats, err
		}
		if err = writer.Close(); err != nil {
			return nil, stats, err
		}
		if buffer.Len() < len(native) {
			payload, codec = buffer.Bytes(), codecGzip
			stats.Codec = "gzip"
		}
	}
	storedSize := headerSize + len(metadata) + len(payload)
	if storedSize > MaxStoredBytes {
		return nil, stats, sizeLimit("stored", uint64(storedSize), MaxStoredBytes)
	}
	data := make([]byte, storedSize)
	copy(data, magic[:])
	data[8], data[9] = formatVersion, codec
	binary.BigEndian.PutUint32(data[12:16], uint32(len(metadata)))
	binary.BigEndian.PutUint64(data[16:24], uint64(len(native)))
	binary.BigEndian.PutUint64(data[24:32], uint64(len(payload)))
	copy(data[headerSize:], metadata)
	copy(data[headerSize+len(metadata):], payload)
	sum := checksum(data)
	copy(data[32:headerSize], sum[:])
	stats.StoredBytes = len(data)
	return data, stats, nil
}

func checksum(data []byte) [sha256.Size]byte {
	h := sha256.New()
	_, _ = h.Write(data[8:32])
	_, _ = h.Write(data[headerSize:])
	var result [sha256.Size]byte
	copy(result[:], h.Sum(nil))
	return result
}

// Metadata validates the frame and its bounded lengths without decompressing
// native state. For a legacy envelope it returns the unchanged JSON bytes.
func Metadata(data []byte) ([]byte, Stats, error) {
	stats := Stats{Format: "legacy-json", Codec: "raw", RawBytes: len(data), StoredBytes: len(data), MetadataBytes: len(data)}
	if len(data) > MaxStoredBytes {
		return nil, stats, sizeLimit("stored", uint64(len(data)), MaxStoredBytes)
	}
	if !bytes.HasPrefix(data, magic[:4]) {
		return data, stats, nil
	}
	if len(data) < headerSize || !bytes.Equal(data[:8], magic[:]) {
		return nil, stats, invalid("truncated or invalid frame")
	}
	if data[8] != formatVersion || data[9] > codecGzip || data[10] != 0 || data[11] != 0 {
		return nil, stats, invalid("unsupported frame version or codec")
	}
	metadataSize := uint64(binary.BigEndian.Uint32(data[12:16]))
	nativeSize := binary.BigEndian.Uint64(data[16:24])
	storedSize := binary.BigEndian.Uint64(data[24:32])
	// Check each component before adding: header lengths are untrusted uint64s.
	if nativeSize > MaxRawBytes {
		return nil, stats, sizeLimit("raw", nativeSize, MaxRawBytes)
	}
	if metadataSize > MaxRawBytes || metadataSize+nativeSize+headerSize > MaxRawBytes {
		return nil, stats, sizeLimit("raw", metadataSize+nativeSize+headerSize, MaxRawBytes)
	}
	if metadataSize == 0 || nativeSize == 0 || storedSize == 0 || storedSize > MaxStoredBytes ||
		metadataSize+storedSize+headerSize != uint64(len(data)) {
		return nil, stats, invalid("frame lengths do not match its data")
	}
	if data[9] == codecRaw && nativeSize != storedSize {
		return nil, stats, invalid("raw state length does not match its data")
	}
	sum := checksum(data)
	if !bytes.Equal(sum[:], data[32:headerSize]) {
		return nil, stats, invalid("frame checksum mismatch")
	}
	metadata := data[headerSize : headerSize+int(metadataSize)]
	if !json.Valid(metadata) {
		return nil, stats, invalid("frame metadata is not valid JSON")
	}
	stats = Stats{Format: "dfcp-v1", Codec: "raw", RawBytes: int(metadataSize + nativeSize + headerSize),
		StoredBytes: len(data), NativeBytes: int(nativeSize), MetadataBytes: int(metadataSize)}
	if data[9] == codecGzip {
		stats.Codec = "gzip"
	}
	return metadata, stats, nil
}

// Unpack restores opaque native bytes only after the encoded form is verified.
// Legacy envelopes return nil native bytes; their JSON carries the native field.
func Unpack(data []byte) (metadata, native []byte, stats Stats, err error) {
	metadata, stats, err = Metadata(data)
	if err != nil || stats.Format == "legacy-json" {
		return metadata, nil, stats, err
	}
	payload := data[headerSize+stats.MetadataBytes:]
	if stats.Codec == "raw" {
		return metadata, payload, stats, nil
	}
	source := bytes.NewReader(payload)
	reader, err := gzip.NewReader(source)
	if err != nil {
		return nil, nil, stats, invalid("invalid gzip state")
	}
	reader.Multistream(false)
	native, readErr := io.ReadAll(io.LimitReader(reader, int64(stats.NativeBytes)+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || len(native) != stats.NativeBytes || source.Len() != 0 {
		return nil, nil, stats, invalid("gzip state is truncated, oversized or corrupt")
	}
	return metadata, native, stats, nil
}
