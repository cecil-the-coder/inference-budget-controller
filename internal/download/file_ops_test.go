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

package download

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// writeLE is a test helper that writes binary data in little-endian format, failing the test on error.
func writeLE(t *testing.T, buf *bytes.Buffer, data interface{}) {
	t.Helper()
	if err := binary.Write(buf, binary.LittleEndian, data); err != nil {
		t.Fatalf("binary.Write failed: %v", err)
	}
}

// buildGGUFFile builds a minimal valid GGUF v3 file with the given tensors.
// Each tensor entry specifies name, dimensions, type ID, and data.
func buildGGUFFile(t *testing.T, tensors []testTensor, alignment uint64) []byte {
	t.Helper()
	var buf bytes.Buffer

	// Magic
	buf.WriteString("GGUF")
	// Version 3
	writeLE(t, &buf, uint32(3))
	// Tensor count
	writeLE(t, &buf, uint64(len(tensors)))

	// Metadata KV count — include general.alignment if non-default
	if alignment != 32 {
		writeLE(t, &buf, uint64(1))
		// Key: "general.alignment"
		key := "general.alignment"
		writeLE(t, &buf, uint64(len(key)))
		buf.WriteString(key)
		// Value type: uint32
		writeLE(t, &buf, uint32(ggufTypeUint32))
		writeLE(t, &buf, uint32(alignment))
	} else {
		writeLE(t, &buf, uint64(0))
	}

	// Compute tensor data section: offset for each tensor, total data size
	// First pass: compute sizes and offsets
	type tensorLayout struct {
		offset   uint64
		dataSize uint64
	}
	layouts := make([]tensorLayout, len(tensors))
	var currentOffset uint64
	for i, tensor := range tensors {
		dataSize := uint64(len(tensor.data))
		layouts[i] = tensorLayout{offset: currentOffset, dataSize: dataSize}
		currentOffset += dataSize
	}

	// Write tensor info entries
	for i, tensor := range tensors {
		// Name
		writeLE(t, &buf, uint64(len(tensor.name)))
		buf.WriteString(tensor.name)
		// nDims
		writeLE(t, &buf, uint32(len(tensor.dims)))
		// Dimensions
		for _, d := range tensor.dims {
			writeLE(t, &buf, d)
		}
		// Type
		writeLE(t, &buf, tensor.typeID)
		// Offset
		writeLE(t, &buf, layouts[i].offset)
	}

	// Pad to alignment
	headerSize := buf.Len()
	if alignment > 0 {
		remainder := uint64(headerSize) % alignment
		if remainder != 0 {
			padding := alignment - remainder
			buf.Write(make([]byte, padding))
		}
	}

	// Write tensor data
	for _, tensor := range tensors {
		buf.Write(tensor.data)
	}

	return buf.Bytes()
}

type testTensor struct {
	name   string
	dims   []uint64
	typeID uint32
	data   []byte
}

func TestValidateGGUFHeader_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model.gguf")

	// F32 tensor, 4 elements = 16 bytes
	data := make([]byte, 16)
	ggufData := buildGGUFFile(t, []testTensor{
		{name: "weight", dims: []uint64{4}, typeID: 0, data: data},
	}, 32)

	if err := os.WriteFile(path, ggufData, 0644); err != nil {
		t.Fatal(err)
	}

	if err := validateGGUFHeader(path, int64(len(ggufData))); err != nil {
		t.Fatalf("expected valid GGUF file, got error: %v", err)
	}
}

func TestValidateGGUFHeader_TruncatedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model.gguf")

	// F32 tensor, 4 elements = 16 bytes of data needed
	data := make([]byte, 16)
	ggufData := buildGGUFFile(t, []testTensor{
		{name: "weight", dims: []uint64{4}, typeID: 0, data: data},
	}, 32)

	// Truncate the file — remove some tensor data
	truncated := ggufData[:len(ggufData)-8]
	if err := os.WriteFile(path, truncated, 0644); err != nil {
		t.Fatal(err)
	}

	err := validateGGUFHeader(path, int64(len(truncated)))
	if err == nil {
		t.Fatal("expected error for truncated GGUF file")
	}
	t.Logf("Got expected error: %v", err)
}

func TestValidateGGUFHeader_CorruptOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model.gguf")

	// Build a valid file first
	data := make([]byte, 16)
	ggufData := buildGGUFFile(t, []testTensor{
		{name: "weight", dims: []uint64{4}, typeID: 0, data: data},
	}, 32)

	// Report the actual file size but the data won't match because we'll claim
	// a larger offset by building with correct size but claiming the file is smaller
	if err := os.WriteFile(path, ggufData, 0644); err != nil {
		t.Fatal(err)
	}

	// Simulate the corruption: file is correct size but we pass wrong size
	// (simulating a file that says it should be bigger)
	err := validateGGUFHeader(path, int64(len(ggufData)-10))
	if err == nil {
		t.Fatal("expected error when file size is too small for tensor data")
	}
	t.Logf("Got expected error: %v", err)
}

func TestValidateGGUFHeader_InvalidMagic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model.gguf")

	data := []byte("NOT_GGUF_DATA_HERE")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	err := validateGGUFHeader(path, int64(len(data)))
	if err == nil {
		t.Fatal("expected error for invalid magic")
	}
}

func TestValidateGGUFHeader_MultipleTensors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model.gguf")

	// Two F32 tensors
	data1 := make([]byte, 16) // 4 F32 elements
	data2 := make([]byte, 32) // 8 F32 elements

	ggufData := buildGGUFFile(t, []testTensor{
		{name: "weight1", dims: []uint64{4}, typeID: 0, data: data1},
		{name: "weight2", dims: []uint64{8}, typeID: 0, data: data2},
	}, 32)

	if err := os.WriteFile(path, ggufData, 0644); err != nil {
		t.Fatal(err)
	}

	if err := validateGGUFHeader(path, int64(len(ggufData))); err != nil {
		t.Fatalf("expected valid GGUF file with multiple tensors, got error: %v", err)
	}
}

func TestValidateGGUFHeader_Q4_0Type(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model.gguf")

	// Q4_0: block_size=32, bytes_per_block=18
	// 32 elements = 1 block = 18 bytes
	data := make([]byte, 18)

	ggufData := buildGGUFFile(t, []testTensor{
		{name: "weight", dims: []uint64{32}, typeID: 2, data: data},
	}, 32)

	if err := os.WriteFile(path, ggufData, 0644); err != nil {
		t.Fatal(err)
	}

	if err := validateGGUFHeader(path, int64(len(ggufData))); err != nil {
		t.Fatalf("expected valid Q4_0 GGUF file, got error: %v", err)
	}
}

func TestComputeTensorDataSize(t *testing.T) {
	tests := []struct {
		name     string
		tensor   ggufTensorInfo
		expected uint64
	}{
		{
			name:     "F32 scalar",
			tensor:   ggufTensorInfo{dimensions: []uint64{1}, typeID: 0},
			expected: 4,
		},
		{
			name:     "F32 vector",
			tensor:   ggufTensorInfo{dimensions: []uint64{10}, typeID: 0},
			expected: 40,
		},
		{
			name:     "F16 matrix",
			tensor:   ggufTensorInfo{dimensions: []uint64{4, 4}, typeID: 1},
			expected: 32,
		},
		{
			name:     "Q4_0 vector 32 elements",
			tensor:   ggufTensorInfo{dimensions: []uint64{32}, typeID: 2},
			expected: 18,
		},
		{
			name:     "Q4_0 vector 64 elements",
			tensor:   ggufTensorInfo{dimensions: []uint64{64}, typeID: 2},
			expected: 36,
		},
		{
			name:     "Q6_K 256 elements",
			tensor:   ggufTensorInfo{dimensions: []uint64{256}, typeID: 14},
			expected: 210,
		},
		{
			name:     "empty dimensions",
			tensor:   ggufTensorInfo{dimensions: []uint64{0}, typeID: 0},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			size, err := computeTensorDataSize(tt.tensor)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if size != tt.expected {
				t.Errorf("expected %d, got %d", tt.expected, size)
			}
		})
	}
}

func TestComputeTensorDataSize_UnknownType(t *testing.T) {
	tensor := ggufTensorInfo{dimensions: []uint64{10}, typeID: 999}
	_, err := computeTensorDataSize(tensor)
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
}

func TestVerifyChecksum(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "testfile")
	content := []byte("hello world")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	h := sha256.Sum256(content)
	expected := hex.EncodeToString(h[:])

	// Should pass
	if err := VerifyChecksum(path, expected); err != nil {
		t.Fatalf("expected checksum to match: %v", err)
	}

	// Should fail with wrong hash
	if err := VerifyChecksum(path, "0000000000000000000000000000000000000000000000000000000000000000"); err == nil {
		t.Fatal("expected checksum mismatch error")
	}
}

func TestComputeSHA256(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "testfile")
	content := []byte("test content for hashing")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	hash, err := ComputeSHA256(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	h := sha256.Sum256(content)
	expected := hex.EncodeToString(h[:])
	if hash != expected {
		t.Errorf("hash mismatch: got %s, expected %s", hash, expected)
	}
}

func TestVerifyFilesDeep_NoChecksums(t *testing.T) {
	dir := t.TempDir()

	// Write manifest without checksums
	manifest := &FileManifest{
		Files: map[string]int64{"test.bin": 10},
	}
	if err := WriteManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}

	// Should succeed (no checksums to verify)
	if err := VerifyFilesDeep(dir); err != nil {
		t.Fatalf("expected no error with no checksums: %v", err)
	}
}

func TestVerifyFilesDeep_WithValidChecksums(t *testing.T) {
	dir := t.TempDir()
	content := []byte("file content here")
	path := filepath.Join(dir, "model.bin")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	h := sha256.Sum256(content)
	hash := hex.EncodeToString(h[:])

	manifest := &FileManifest{
		Files:     map[string]int64{"model.bin": int64(len(content))},
		Checksums: map[string]string{"model.bin": hash},
	}
	if err := WriteManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}

	if err := VerifyFilesDeep(dir); err != nil {
		t.Fatalf("expected verification to pass: %v", err)
	}
}

func TestVerifyFilesDeep_WithInvalidChecksum(t *testing.T) {
	dir := t.TempDir()
	content := []byte("file content here")
	path := filepath.Join(dir, "model.bin")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	manifest := &FileManifest{
		Files:     map[string]int64{"model.bin": int64(len(content))},
		Checksums: map[string]string{"model.bin": "badhash0000000000000000000000000000000000000000000000000000000000"},
	}
	if err := WriteManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}

	if err := VerifyFilesDeep(dir); err == nil {
		t.Fatal("expected verification to fail with bad checksum")
	}
}

func TestCheckAndVerifyReadyMarkerDetailed_NoMarker(t *testing.T) {
	dir := t.TempDir()
	result := CheckAndVerifyReadyMarkerDetailed(dir)
	if result.MarkerExists {
		t.Error("expected MarkerExists=false for empty directory")
	}
	if result.Verified {
		t.Error("expected Verified=false for empty directory")
	}
}

func TestCheckAndVerifyReadyMarkerDetailed_MarkerWithValidFiles(t *testing.T) {
	dir := t.TempDir()

	// Create a file and manifest
	content := []byte("model data")
	if err := os.WriteFile(filepath.Join(dir, "model.bin"), content, 0644); err != nil {
		t.Fatal(err)
	}
	manifest := &FileManifest{
		Files: map[string]int64{"model.bin": int64(len(content))},
	}
	if err := WriteManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}
	if err := WriteReadyMarker(dir); err != nil {
		t.Fatal(err)
	}

	result := CheckAndVerifyReadyMarkerDetailed(dir)
	if !result.MarkerExists {
		t.Error("expected MarkerExists=true")
	}
	if !result.Verified {
		t.Error("expected Verified=true")
	}
	if result.Error != nil {
		t.Errorf("expected no error, got: %v", result.Error)
	}
}

func TestFileManifest_Checksums(t *testing.T) {
	dir := t.TempDir()

	manifest := &FileManifest{
		Files:     map[string]int64{"file1.bin": 100, "file2.bin": 200},
		Checksums: map[string]string{"file1.bin": "abc123", "file2.bin": "def456"},
	}

	if err := WriteManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}

	loaded, err := ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(loaded.Checksums) != 2 {
		t.Fatalf("expected 2 checksums, got %d", len(loaded.Checksums))
	}
	if loaded.Checksums["file1.bin"] != "abc123" {
		t.Errorf("expected abc123, got %s", loaded.Checksums["file1.bin"])
	}
	if loaded.Checksums["file2.bin"] != "def456" {
		t.Errorf("expected def456, got %s", loaded.Checksums["file2.bin"])
	}
}
