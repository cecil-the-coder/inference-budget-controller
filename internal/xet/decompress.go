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
	"bytes"
	"fmt"

	"github.com/pierrec/lz4/v4"
)

// Decompressor handles decompression of xorb chunks.
type Decompressor struct {
	lz4Reader *lz4.Reader
}

// NewDecompressor creates a new decompressor instance.
func NewDecompressor() *Decompressor {
	return &Decompressor{
		lz4Reader: lz4.NewReader(nil),
	}
}

// Decompress decompresses a chunk based on its compression type.
func (d *Decompressor) Decompress(header *ChunkHeader, compressedData []byte) ([]byte, error) {
	switch header.CompressionType {
	case CompressionNone:
		// No compression, return data as-is
		result := make([]byte, len(compressedData))
		copy(result, compressedData)
		return result, nil

	case CompressionLZ4:
		return d.decompressLZ4(compressedData, int(header.UncompressedSize))

	case CompressionByteGrouping4LZ4:
		// First decompress LZ4, then undo byte grouping
		decompressed, err := d.decompressLZ4(compressedData, -1) // Unknown size
		if err != nil {
			return nil, fmt.Errorf("lz4 decompression failed: %w", err)
		}
		return undoByteGrouping4(decompressed, int(header.UncompressedSize))

	default:
		return nil, fmt.Errorf("unsupported compression type: %d", header.CompressionType)
	}
}

// decompressLZ4 decompresses LZ4 data.
// If uncompressedSize is -1, it allocates a buffer dynamically.
func (d *Decompressor) decompressLZ4(compressedData []byte, uncompressedSize int) ([]byte, error) {
	d.lz4Reader.Reset(bytes.NewReader(compressedData))

	if uncompressedSize > 0 {
		// Known size - allocate exact buffer
		result := make([]byte, 0, uncompressedSize)
		buf := make([]byte, 4096)
		for {
			n, err := d.lz4Reader.Read(buf)
			if n > 0 {
				result = append(result, buf[:n]...)
			}
			if err != nil {
				if err.Error() == "EOF" {
					break
				}
				return nil, err
			}
		}
		return result, nil
	}

	// Unknown size - use bytes.Buffer
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(d.lz4Reader); err != nil {
		return nil, fmt.Errorf("failed to read lz4 stream: %w", err)
	}
	return buf.Bytes(), nil
}

// undoByteGrouping4 reverses the ByteGrouping4 transformation.
// ByteGrouping4 interleaves bytes from 4 consecutive values:
// Input:  [a0, a1, a2, a3, b0, b1, b2, b3, ...]
// Output: [a0, b0, a1, b1, a2, b2, a3, b3, ...]
//
// To undo, we de-interleave back to the original format.
func undoByteGrouping4(data []byte, expectedSize int) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}

	// Calculate the original size before grouping
	// ByteGrouping4 takes groups of 4 values and interleaves them
	// If the original had N values each of M bytes, the grouped size is still N*M
	// We need to figure out the structure

	// For simplicity, we'll de-interleave assuming the data represents
	// 4-way interleaved bytes
	n := len(data)
	if n%4 != 0 {
		// Data is not a multiple of 4, handle the remainder
		return undoByteGrouping4Partial(data, expectedSize)
	}

	numGroups := n / 4
	result := make([]byte, n)

	for i := 0; i < numGroups; i++ {
		// Original position i had 4 consecutive bytes
		// After grouping, byte j of position i is at i + j*numGroups
		for j := 0; j < 4; j++ {
			srcIdx := i + j*numGroups
			dstIdx := i*4 + j
			if srcIdx < n && dstIdx < n {
				result[dstIdx] = data[srcIdx]
			}
		}
	}

	// Trim to expected size if specified
	if expectedSize > 0 && len(result) > expectedSize {
		result = result[:expectedSize]
	}

	return result, nil
}

// undoByteGrouping4Partial handles de-interleaving when data length is not a multiple of 4.
func undoByteGrouping4Partial(data []byte, expectedSize int) ([]byte, error) {
	n := len(data)
	completeGroups := n / 4
	remainder := n % 4

	// Calculate original size
	originalSize := completeGroups * 4
	if remainder > 0 {
		originalSize += remainder
	}

	if expectedSize > 0 {
		originalSize = expectedSize
	}

	result := make([]byte, originalSize)

	// Handle complete groups
	for i := 0; i < completeGroups; i++ {
		for j := 0; j < 4; j++ {
			srcIdx := i + j*completeGroups
			dstIdx := i*4 + j
			if srcIdx < n && dstIdx < len(result) {
				result[dstIdx] = data[srcIdx]
			}
		}
	}

	// Handle remainder bytes (these were at the end of the original data)
	remainderStart := completeGroups * 4
	for j := 0; j < remainder; j++ {
		srcIdx := completeGroups + j*completeGroups
		dstIdx := remainderStart + j
		if srcIdx < n && dstIdx < len(result) {
			result[dstIdx] = data[srcIdx]
		}
	}

	return result, nil
}

// DecompressChunk is a convenience function to decompress a single chunk.
func DecompressChunk(header *ChunkHeader, compressedData []byte) ([]byte, error) {
	d := NewDecompressor()
	return d.Decompress(header, compressedData)
}
