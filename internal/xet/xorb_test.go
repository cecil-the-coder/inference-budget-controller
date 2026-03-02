/*
Copyright 2024 eh-ops.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package xet

import (
	"testing"
)

func TestParseChunkHeader(t *testing.T) {
	tests := []struct {
		name           string
		data           []byte
		wantErr        bool
		expectedHeader *ChunkHeader
	}{
		{
			name:    "valid header - no compression",
			data:    []byte{0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x00},
			wantErr: false,
			expectedHeader: &ChunkHeader{
				Version:          0x01,
				CompressedSize:   256, // 0x000100
				CompressionType:  CompressionNone,
				UncompressedSize: 256, // 0x000100
			},
		},
		{
			name:    "valid header - LZ4 compression",
			data:    []byte{0x01, 0x00, 0x80, 0x00, 0x01, 0x00, 0x00, 0x01},
			wantErr: false,
			expectedHeader: &ChunkHeader{
				Version:          0x01,
				CompressedSize:   32768, // 0x008000
				CompressionType:  CompressionLZ4,
				UncompressedSize: 65536, // 0x010000
			},
		},
		{
			name:    "valid header - ByteGrouping4LZ4 compression",
			data:    []byte{0x01, 0xFF, 0xFF, 0x00, 0x02, 0x00, 0x00, 0x01},
			wantErr: false,
			expectedHeader: &ChunkHeader{
				Version:          0x01,
				CompressedSize:   65535, // 0x00FFFF
				CompressionType:  CompressionByteGrouping4LZ4,
				UncompressedSize: 65536, // 0x010000
			},
		},
		{
			name:    "valid header - maximum sizes",
			data:    []byte{0x01, 0xFF, 0xFF, 0xFF, 0x00, 0xFF, 0xFF, 0xFF},
			wantErr: false,
			expectedHeader: &ChunkHeader{
				Version:          0x01,
				CompressedSize:   16777215, // 0xFFFFFF (max 3-byte value)
				CompressionType:  CompressionNone,
				UncompressedSize: 16777215, // 0xFFFFFF (max 3-byte value)
			},
		},
		{
			name:    "valid header - zero sizes",
			data:    []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
			wantErr: false,
			expectedHeader: &ChunkHeader{
				Version:          0x00,
				CompressedSize:   0,
				CompressionType:  CompressionNone,
				UncompressedSize: 0,
			},
		},
		{
			name:    "data too short - 7 bytes",
			data:    []byte{0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01},
			wantErr: true,
		},
		{
			name:    "data too short - 1 byte",
			data:    []byte{0x01},
			wantErr: true,
		},
		{
			name:    "data too short - empty",
			data:    []byte{},
			wantErr: true,
		},
		{
			name:    "data too short - nil",
			data:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header, err := ParseChunkHeader(tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseChunkHeader() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if header.Version != tt.expectedHeader.Version {
				t.Errorf("Version = %d, want %d", header.Version, tt.expectedHeader.Version)
			}
			if header.CompressedSize != tt.expectedHeader.CompressedSize {
				t.Errorf("CompressedSize = %d, want %d", header.CompressedSize, tt.expectedHeader.CompressedSize)
			}
			if header.CompressionType != tt.expectedHeader.CompressionType {
				t.Errorf("CompressionType = %d, want %d", header.CompressionType, tt.expectedHeader.CompressionType)
			}
			if header.UncompressedSize != tt.expectedHeader.UncompressedSize {
				t.Errorf("UncompressedSize = %d, want %d", header.UncompressedSize, tt.expectedHeader.UncompressedSize)
			}
		})
	}
}

func TestChunkHeaderSerialize(t *testing.T) {
	tests := []struct {
		name   string
		header *ChunkHeader
	}{
		{
			name: "no compression",
			header: &ChunkHeader{
				Version:          0x01,
				CompressedSize:   256,
				CompressionType:  CompressionNone,
				UncompressedSize: 256,
			},
		},
		{
			name: "LZ4 compression",
			header: &ChunkHeader{
				Version:          0x01,
				CompressedSize:   32768,
				CompressionType:  CompressionLZ4,
				UncompressedSize: 65536,
			},
		},
		{
			name: "ByteGrouping4LZ4 compression",
			header: &ChunkHeader{
				Version:          0x01,
				CompressedSize:   65535,
				CompressionType:  CompressionByteGrouping4LZ4,
				UncompressedSize: 65536,
			},
		},
		{
			name: "maximum values",
			header: &ChunkHeader{
				Version:          0xFF,
				CompressedSize:   16777215,
				CompressionType:  0xFF,
				UncompressedSize: 16777215,
			},
		},
		{
			name: "zero values",
			header: &ChunkHeader{
				Version:          0x00,
				CompressedSize:   0,
				CompressionType:  0,
				UncompressedSize: 0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := tt.header.Serialize()
			if len(data) != ChunkHeaderSize {
				t.Errorf("Serialize() returned %d bytes, want %d", len(data), ChunkHeaderSize)
			}

			// Parse it back and verify roundtrip
			parsed, err := ParseChunkHeader(data)
			if err != nil {
				t.Fatalf("Failed to parse serialized header: %v", err)
			}

			if parsed.Version != tt.header.Version {
				t.Errorf("Version roundtrip mismatch: got %d, want %d", parsed.Version, tt.header.Version)
			}
			if parsed.CompressedSize != tt.header.CompressedSize {
				t.Errorf("CompressedSize roundtrip mismatch: got %d, want %d", parsed.CompressedSize, tt.header.CompressedSize)
			}
			if parsed.CompressionType != tt.header.CompressionType {
				t.Errorf("CompressionType roundtrip mismatch: got %d, want %d", parsed.CompressionType, tt.header.CompressionType)
			}
			if parsed.UncompressedSize != tt.header.UncompressedSize {
				t.Errorf("UncompressedSize roundtrip mismatch: got %d, want %d", parsed.UncompressedSize, tt.header.UncompressedSize)
			}
		})
	}
}

func TestChunkHeaderIsCompressed(t *testing.T) {
	tests := []struct {
		name       string
		header     *ChunkHeader
		compressed bool
	}{
		{
			name: "no compression",
			header: &ChunkHeader{
				CompressionType: CompressionNone,
			},
			compressed: false,
		},
		{
			name: "LZ4 compression",
			header: &ChunkHeader{
				CompressionType: CompressionLZ4,
			},
			compressed: true,
		},
		{
			name: "ByteGrouping4LZ4 compression",
			header: &ChunkHeader{
				CompressionType: CompressionByteGrouping4LZ4,
			},
			compressed: true,
		},
		{
			name: "unknown compression type",
			header: &ChunkHeader{
				CompressionType: 99,
			},
			compressed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.header.IsCompressed(); got != tt.compressed {
				t.Errorf("IsCompressed() = %v, want %v", got, tt.compressed)
			}
		})
	}
}

func TestChunkHeaderCompressionName(t *testing.T) {
	tests := []struct {
		name     string
		header   *ChunkHeader
		expected string
	}{
		{
			name: "none",
			header: &ChunkHeader{
				CompressionType: CompressionNone,
			},
			expected: "none",
		},
		{
			name: "lz4",
			header: &ChunkHeader{
				CompressionType: CompressionLZ4,
			},
			expected: "lz4",
		},
		{
			name: "bytegrouping4+lz4",
			header: &ChunkHeader{
				CompressionType: CompressionByteGrouping4LZ4,
			},
			expected: "bytegrouping4+lz4",
		},
		{
			name: "unknown",
			header: &ChunkHeader{
				CompressionType: 99,
			},
			expected: "unknown(99)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.header.CompressionName(); got != tt.expected {
				t.Errorf("CompressionName() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestXorbIterator(t *testing.T) {
	// Helper function to create a chunk
	createChunk := func(version byte, compType byte, compressedSize, uncompressedSize uint32, data []byte) []byte {
		header := &ChunkHeader{
			Version:          version,
			CompressionType:  compType,
			CompressedSize:   compressedSize,
			UncompressedSize: uncompressedSize,
		}
		result := header.Serialize()
		result = append(result, data...)
		return result
	}

	t.Run("empty data", func(t *testing.T) {
		iter := NewXorbIterator([]byte{})
		if iter.HasNext() {
			t.Error("HasNext() should return false for empty data")
		}
		header, data, err := iter.Next()
		if err != nil {
			t.Errorf("Next() returned unexpected error: %v", err)
		}
		if header != nil || data != nil {
			t.Error("Next() should return nil for empty data")
		}
	})

	t.Run("single chunk", func(t *testing.T) {
		chunkData := []byte{0x01, 0x02, 0x03, 0x04}
		xorbData := createChunk(0x01, CompressionNone, 4, 4, chunkData)

		iter := NewXorbIterator(xorbData)
		if !iter.HasNext() {
			t.Error("HasNext() should return true")
		}

		header, data, err := iter.Next()
		if err != nil {
			t.Fatalf("Next() returned error: %v", err)
		}
		if header == nil {
			t.Fatal("header should not be nil")
		}
		if header.Version != 0x01 {
			t.Errorf("Version = %d, want 1", header.Version)
		}
		if header.CompressionType != CompressionNone {
			t.Errorf("CompressionType = %d, want 0", header.CompressionType)
		}
		if !equalBytes(data, chunkData) {
			t.Errorf("data = %v, want %v", data, chunkData)
		}

		if iter.HasNext() {
			t.Error("HasNext() should return false after reading single chunk")
		}
	})

	t.Run("multiple chunks", func(t *testing.T) {
		chunk1Data := []byte{0x01, 0x02, 0x03, 0x04}
		chunk2Data := []byte{0x05, 0x06, 0x07, 0x08, 0x09}
		chunk3Data := []byte{0x0A}

		xorbData := createChunk(0x01, CompressionNone, 4, 4, chunk1Data)
		xorbData = append(xorbData, createChunk(0x01, CompressionLZ4, 5, 100, chunk2Data)...)
		xorbData = append(xorbData, createChunk(0x01, CompressionByteGrouping4LZ4, 1, 50, chunk3Data)...)

		iter := NewXorbIterator(xorbData)

		// First chunk
		if !iter.HasNext() {
			t.Fatal("HasNext() should return true for first chunk")
		}
		header1, data1, err := iter.Next()
		if err != nil {
			t.Fatalf("Next() chunk 1 error: %v", err)
		}
		if header1.CompressionType != CompressionNone {
			t.Errorf("chunk 1 CompressionType = %d, want 0", header1.CompressionType)
		}
		if !equalBytes(data1, chunk1Data) {
			t.Errorf("chunk 1 data = %v, want %v", data1, chunk1Data)
		}

		// Second chunk
		if !iter.HasNext() {
			t.Fatal("HasNext() should return true for second chunk")
		}
		header2, data2, err := iter.Next()
		if err != nil {
			t.Fatalf("Next() chunk 2 error: %v", err)
		}
		if header2.CompressionType != CompressionLZ4 {
			t.Errorf("chunk 2 CompressionType = %d, want 1", header2.CompressionType)
		}
		if !equalBytes(data2, chunk2Data) {
			t.Errorf("chunk 2 data = %v, want %v", data2, chunk2Data)
		}

		// Third chunk
		if !iter.HasNext() {
			t.Fatal("HasNext() should return true for third chunk")
		}
		header3, data3, err := iter.Next()
		if err != nil {
			t.Fatalf("Next() chunk 3 error: %v", err)
		}
		if header3.CompressionType != CompressionByteGrouping4LZ4 {
			t.Errorf("chunk 3 CompressionType = %d, want 2", header3.CompressionType)
		}
		if !equalBytes(data3, chunk3Data) {
			t.Errorf("chunk 3 data = %v, want %v", data3, chunk3Data)
		}

		// No more chunks
		if iter.HasNext() {
			t.Error("HasNext() should return false after all chunks read")
		}
	})

	t.Run("chunk extends beyond data boundary", func(t *testing.T) {
		// Create a header that claims more data than exists
		header := &ChunkHeader{
			Version:          0x01,
			CompressionType:  CompressionNone,
			CompressedSize:   1000, // Claims 1000 bytes
			UncompressedSize: 1000,
		}
		xorbData := header.Serialize()
		xorbData = append(xorbData, []byte{0x01, 0x02}...) // Only 2 bytes of data

		iter := NewXorbIterator(xorbData)
		_, _, err := iter.Next()
		if err == nil {
			t.Error("Next() should return error when chunk extends beyond boundary")
		}
	})

	t.Run("offset tracking", func(t *testing.T) {
		chunk1Data := []byte{0x01, 0x02, 0x03, 0x04}
		chunk2Data := []byte{0x05, 0x06, 0x07, 0x08}

		xorbData := createChunk(0x01, CompressionNone, 4, 4, chunk1Data)
		xorbData = append(xorbData, createChunk(0x01, CompressionNone, 4, 4, chunk2Data)...)

		iter := NewXorbIterator(xorbData)

		if iter.Offset() != 0 {
			t.Errorf("Initial offset = %d, want 0", iter.Offset())
		}

		_, _, _ = iter.Next()
		expectedOffset := ChunkHeaderSize + len(chunk1Data)
		if iter.Offset() != expectedOffset {
			t.Errorf("After first chunk, offset = %d, want %d", iter.Offset(), expectedOffset)
		}

		_, _, _ = iter.Next()
		expectedOffset = 2 * (ChunkHeaderSize + len(chunk2Data))
		if iter.Offset() != expectedOffset {
			t.Errorf("After second chunk, offset = %d, want %d", iter.Offset(), expectedOffset)
		}
	})
}

func TestReadWriteUint32LE(t *testing.T) {
	tests := []struct {
		name  string
		value uint32
	}{
		{"zero", 0},
		{"small", 1},
		{"max uint8", 255},
		{"max uint16", 65535},
		{"large", 123456789},
		{"max uint32", 4294967295},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := make([]byte, 8)
			WriteUint32LE(data, 2, tt.value)
			got := ReadUint32LE(data, 2)
			if got != tt.value {
				t.Errorf("ReadUint32LE() = %d, want %d", got, tt.value)
			}
		})
	}
}

func TestReadWriteUint64LE(t *testing.T) {
	tests := []struct {
		name  string
		value uint64
	}{
		{"zero", 0},
		{"small", 1},
		{"max uint32", 4294967295},
		{"large", 12345678901234567890},
		{"max uint64", 18446744073709551615},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := make([]byte, 16)
			WriteUint64LE(data, 4, tt.value)
			got := ReadUint64LE(data, 4)
			if got != tt.value {
				t.Errorf("ReadUint64LE() = %d, want %d", got, tt.value)
			}
		})
	}
}

func TestLittleEndianByteOrder(t *testing.T) {
	// Verify that the byte ordering is correct for little-endian
	// Value 0x12345678 should be stored as [0x78, 0x56, 0x34, 0x12]

	t.Run("uint32 little-endian", func(t *testing.T) {
		value := uint32(0x12345678)
		data := make([]byte, 4)
		WriteUint32LE(data, 0, value)

		// Little-endian: LSB first
		if data[0] != 0x78 || data[1] != 0x56 || data[2] != 0x34 || data[3] != 0x12 {
			t.Errorf("Little-endian byte order incorrect: got [%02x %02x %02x %02x], want [78 56 34 12]",
				data[0], data[1], data[2], data[3])
		}

		// Verify roundtrip
		got := ReadUint32LE(data, 0)
		if got != value {
			t.Errorf("Roundtrip failed: got 0x%08x, want 0x%08x", got, value)
		}
	})

	t.Run("uint64 little-endian", func(t *testing.T) {
		value := uint64(0x0123456789ABCDEF)
		data := make([]byte, 8)
		WriteUint64LE(data, 0, value)

		// Little-endian: LSB first
		expected := []byte{0xEF, 0xCD, 0xAB, 0x89, 0x67, 0x45, 0x23, 0x01}
		for i, b := range expected {
			if data[i] != b {
				t.Errorf("Little-endian byte order incorrect at position %d: got 0x%02x, want 0x%02x", i, data[i], b)
			}
		}

		// Verify roundtrip
		got := ReadUint64LE(data, 0)
		if got != value {
			t.Errorf("Roundtrip failed: got 0x%016x, want 0x%016x", got, value)
		}
	})

	t.Run("3-byte little-endian in chunk header", func(t *testing.T) {
		// The chunk header uses 3-byte little-endian for sizes
		// Value 0x123456 should be stored as [0x56, 0x34, 0x12]
		header := &ChunkHeader{
			Version:          0x01,
			CompressedSize:   0x123456,
			CompressionType:  CompressionNone,
			UncompressedSize: 0x789ABC,
		}

		data := header.Serialize()

		// Check compressed size bytes (bytes 1-3)
		if data[1] != 0x56 || data[2] != 0x34 || data[3] != 0x12 {
			t.Errorf("Compressed size bytes incorrect: got [%02x %02x %02x], want [56 34 12]",
				data[1], data[2], data[3])
		}

		// Check uncompressed size bytes (bytes 5-7)
		if data[5] != 0xBC || data[6] != 0x9A || data[7] != 0x78 {
			t.Errorf("Uncompressed size bytes incorrect: got [%02x %02x %02x], want [BC 9A 78]",
				data[5], data[6], data[7])
		}
	})
}

func TestChunkHeaderBoundaryConditions(t *testing.T) {
	t.Run("maximum valid compressed size", func(t *testing.T) {
		// 3-byte max is 0xFFFFFF = 16777215
		header := &ChunkHeader{
			Version:          0x01,
			CompressedSize:   16777215,
			CompressionType:  CompressionNone,
			UncompressedSize: 16777215,
		}

		data := header.Serialize()
		parsed, err := ParseChunkHeader(data)
		if err != nil {
			t.Fatalf("Failed to parse header: %v", err)
		}

		if parsed.CompressedSize != 16777215 {
			t.Errorf("CompressedSize = %d, want 16777215", parsed.CompressedSize)
		}
	})

	t.Run("size that exceeds 3-byte max wraps", func(t *testing.T) {
		// If someone tries to set a size > 16777215, it will be truncated
		header := &ChunkHeader{
			Version:          0x01,
			CompressedSize:   16777216, // 0x1000000 - exceeds 3 bytes
			CompressionType:  CompressionNone,
			UncompressedSize: 16777216,
		}

		data := header.Serialize()
		parsed, err := ParseChunkHeader(data)
		if err != nil {
			t.Fatalf("Failed to parse header: %v", err)
		}

		// The 4th byte (bit 24+) will be lost
		if parsed.CompressedSize != 0 {
			t.Errorf("CompressedSize wrapped to %d, expected 0 (truncation)", parsed.CompressedSize)
		}
	})

	t.Run("version byte values", func(t *testing.T) {
		for v := 0; v <= 255; v++ {
			header := &ChunkHeader{
				Version:          byte(v),
				CompressionType:  CompressionNone,
				CompressedSize:   0,
				UncompressedSize: 0,
			}

			data := header.Serialize()
			parsed, err := ParseChunkHeader(data)
			if err != nil {
				t.Fatalf("Failed to parse header with version %d: %v", v, err)
			}

			if parsed.Version != byte(v) {
				t.Errorf("Version = %d, want %d", parsed.Version, v)
			}
		}
	})
}

// Helper function to compare byte slices
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
