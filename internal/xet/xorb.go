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
	"encoding/binary"
	"fmt"
)

const (
	// ChunkHeaderSize is the size of a xorb chunk header in bytes.
	ChunkHeaderSize = 8

	// CompressionNone indicates no compression.
	CompressionNone = 0
	// CompressionLZ4 indicates LZ4 compression.
	CompressionLZ4 = 1
	// CompressionByteGrouping4LZ4 indicates ByteGrouping4 followed by LZ4 compression.
	CompressionByteGrouping4LZ4 = 2

	// DefaultChunkSize is the typical chunk size (~64KB).
	DefaultChunkSize = 64 * 1024
	// DefaultXorbSize is the typical xorb size (~64MB).
	DefaultXorbSize = 64 * 1024 * 1024
)

// ChunkHeader represents the header of a compressed chunk in a xorb file.
// Layout (8 bytes):
// - Version: 1 byte
// - Compressed size: 3 bytes (little-endian)
// - Compression type: 1 byte
// - Uncompressed size: 3 bytes (little-endian)
type ChunkHeader struct {
	Version         byte
	CompressedSize  uint32
	CompressionType byte
	UncompressedSize uint32
}

// ParseChunkHeader parses an 8-byte chunk header.
func ParseChunkHeader(data []byte) (*ChunkHeader, error) {
	if len(data) < ChunkHeaderSize {
		return nil, fmt.Errorf("header data too short: got %d bytes, need %d", len(data), ChunkHeaderSize)
	}

	header := &ChunkHeader{
		Version:          data[0],
		CompressionType:  data[4],
	}

	// 3-byte little-endian compressed size
	header.CompressedSize = uint32(data[1]) | uint32(data[2])<<8 | uint32(data[3])<<16

	// 3-byte little-endian uncompressed size
	header.UncompressedSize = uint32(data[5]) | uint32(data[6])<<8 | uint32(data[7])<<16

	return header, nil
}

// Serialize serializes the chunk header to an 8-byte buffer.
func (h *ChunkHeader) Serialize() []byte {
	data := make([]byte, ChunkHeaderSize)

	data[0] = h.Version

	// 3-byte little-endian compressed size
	data[1] = byte(h.CompressedSize)
	data[2] = byte(h.CompressedSize >> 8)
	data[3] = byte(h.CompressedSize >> 16)

	data[4] = h.CompressionType

	// 3-byte little-endian uncompressed size
	data[5] = byte(h.UncompressedSize)
	data[6] = byte(h.UncompressedSize >> 8)
	data[7] = byte(h.UncompressedSize >> 16)

	return data
}

// IsCompressed returns true if the chunk uses compression.
func (h *ChunkHeader) IsCompressed() bool {
	return h.CompressionType != CompressionNone
}

// CompressionName returns a human-readable name for the compression type.
func (h *ChunkHeader) CompressionName() string {
	switch h.CompressionType {
	case CompressionNone:
		return "none"
	case CompressionLZ4:
		return "lz4"
	case CompressionByteGrouping4LZ4:
		return "bytegrouping4+lz4"
	default:
		return fmt.Sprintf("unknown(%d)", h.CompressionType)
	}
}

// Chunk represents a decompressed chunk of data.
type Chunk struct {
	Header *ChunkHeader
	Data   []byte
}

// XorbIterator iterates over chunks in a xorb file.
type XorbIterator struct {
	data   []byte
	offset int
}

// NewXorbIterator creates a new iterator for xorb data.
func NewXorbIterator(data []byte) *XorbIterator {
	return &XorbIterator{
		data:   data,
		offset: 0,
	}
}

// Next returns the next chunk header and compressed data.
// Returns io.EOF when no more chunks are available.
func (it *XorbIterator) Next() (*ChunkHeader, []byte, error) {
	if it.offset+ChunkHeaderSize > len(it.data) {
		return nil, nil, nil // No more chunks
	}

	header, err := ParseChunkHeader(it.data[it.offset : it.offset+ChunkHeaderSize])
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse chunk header at offset %d: %w", it.offset, err)
	}

	chunkStart := it.offset + ChunkHeaderSize
	chunkEnd := chunkStart + int(header.CompressedSize)

	if chunkEnd > len(it.data) {
		return nil, nil, fmt.Errorf("chunk data extends beyond xorb boundary: need %d bytes, have %d", chunkEnd, len(it.data))
	}

	compressedData := it.data[chunkStart:chunkEnd]
	it.offset = chunkEnd

	return header, compressedData, nil
}

// HasNext returns true if there are more chunks to read.
func (it *XorbIterator) HasNext() bool {
	return it.offset+ChunkHeaderSize <= len(it.data)
}

// Offset returns the current offset in the xorb data.
func (it *XorbIterator) Offset() int {
	return it.offset
}

// ReadUint32LE reads a 4-byte little-endian uint32 from the given data at offset.
func ReadUint32LE(data []byte, offset int) uint32 {
	return binary.LittleEndian.Uint32(data[offset : offset+4])
}

// ReadUint64LE reads an 8-byte little-endian uint64 from the given data at offset.
func ReadUint64LE(data []byte, offset int) uint64 {
	return binary.LittleEndian.Uint64(data[offset : offset+8])
}

// WriteUint32LE writes a 4-byte little-endian uint32 to the given data at offset.
func WriteUint32LE(data []byte, offset int, value uint32) {
	binary.LittleEndian.PutUint32(data[offset:offset+4], value)
}

// WriteUint64LE writes an 8-byte little-endian uint64 to the given data at offset.
func WriteUint64LE(data []byte, offset int, value uint64) {
	binary.LittleEndian.PutUint64(data[offset:offset+8], value)
}
