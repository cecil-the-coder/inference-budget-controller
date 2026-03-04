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
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// ReadyMarkerFile is the name of the marker file indicating a complete download.
	ReadyMarkerFile = ".ready"
	// ManifestFile is the name of the file storing download metadata for verification.
	ManifestFile = ".manifest"
	// GGUF magic number for validation
	ggufMagic = "GGUF"
)

// FileManifest contains metadata about downloaded files for verification.
type FileManifest struct {
	Files     map[string]int64  `json:"files"`               // file path -> size in bytes
	Checksums map[string]string `json:"checksums,omitempty"` // file path -> sha256 hex
}

// EnsureDir creates parent directories for a file path.
// It creates all necessary parent directories with standard permissions (0755).
func EnsureDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0755)
}

// AtomicWrite writes data to a temporary file then renames it to the target path.
// This operation is atomic on POSIX systems - the file will either exist completely
// or not exist at all, preventing partial writes from being visible.
func AtomicWrite(path string, data []byte) error {
	// Ensure parent directory exists
	if err := EnsureDir(path); err != nil {
		return fmt.Errorf("failed to create parent directories: %w", err)
	}

	// Create temp file in the same directory to ensure same filesystem
	dir := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(dir, ".tmp-download-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	// Ensure cleanup on error
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpPath)
		}
	}()

	// Write data
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to write data: %w", err)
	}

	// Sync to ensure data is on disk
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to sync temp file: %w", err)
	}

	// Close before rename
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	// Set permissions
	if err := os.Chmod(tmpPath, 0644); err != nil {
		return fmt.Errorf("failed to set permissions: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	success = true
	return nil
}

// WriteReadyMarker creates the .ready file indicating download is complete.
// The model directory should be the root directory of the downloaded model.
func WriteReadyMarker(modelDir string) error {
	markerPath := filepath.Join(modelDir, ReadyMarkerFile)
	return AtomicWrite(markerPath, []byte{})
}

// CheckReadyMarker checks if the model is ready by looking for the .ready marker file.
// Returns true if the marker exists, false otherwise.
func CheckReadyMarker(modelDir string) bool {
	markerPath := filepath.Join(modelDir, ReadyMarkerFile)
	_, err := os.Stat(markerPath)
	return err == nil
}

// RemoveReadyMarker removes the ready marker file if it exists.
// This is useful when starting a fresh download or cleaning up.
func RemoveReadyMarker(modelDir string) error {
	markerPath := filepath.Join(modelDir, ReadyMarkerFile)
	err := os.Remove(markerPath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// CreateDownloadMarker creates a .downloading marker file to indicate
// an in-progress download. The marker contains the PID of the downloading process.
func CreateDownloadMarker(modelDir string) error {
	markerPath := filepath.Join(modelDir, ".downloading")
	pid := os.Getpid()
	data := []byte(fmt.Sprintf("%d", pid))
	return AtomicWrite(markerPath, data)
}

// CheckDownloadMarker checks if there's an in-progress download marker.
// Returns the PID of the downloading process if the marker exists, or 0 if not.
func CheckDownloadMarker(modelDir string) (int, error) {
	markerPath := filepath.Join(modelDir, ".downloading")
	data, err := os.ReadFile(markerPath)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("failed to read download marker: %w", err)
	}

	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
		return 0, fmt.Errorf("invalid download marker content: %w", err)
	}
	return pid, nil
}

// RemoveDownloadMarker removes the download marker file.
func RemoveDownloadMarker(modelDir string) error {
	markerPath := filepath.Join(modelDir, ".downloading")
	err := os.Remove(markerPath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// WriteManifest saves the file manifest for later verification.
func WriteManifest(modelDir string, manifest *FileManifest) error {
	manifestPath := filepath.Join(modelDir, ManifestFile)
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("failed to marshal manifest: %w", err)
	}
	return AtomicWrite(manifestPath, data)
}

// ReadManifest loads the file manifest.
func ReadManifest(modelDir string) (*FileManifest, error) {
	manifestPath := filepath.Join(modelDir, ManifestFile)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest: %w", err)
	}
	var manifest FileManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("failed to unmarshal manifest: %w", err)
	}
	return &manifest, nil
}

// VerifyFiles checks that all files in the manifest exist and have correct sizes.
// For GGUF files, also validates the file header to detect corruption.
// Returns an error describing any issues found.
func VerifyFiles(modelDir string) error {
	manifest, err := ReadManifest(modelDir)
	if err != nil {
		return fmt.Errorf("failed to read manifest: %w", err)
	}

	var issues []string
	for filePath, expectedSize := range manifest.Files {
		fullPath := filepath.Join(modelDir, filePath)
		info, err := os.Stat(fullPath)
		if err != nil {
			issues = append(issues, fmt.Sprintf("file %s: %v", filePath, err))
			continue
		}
		if info.Size() != expectedSize {
			issues = append(issues, fmt.Sprintf("file %s: size mismatch (expected %d, got %d)", filePath, expectedSize, info.Size()))
			continue
		}

		// Validate GGUF files by checking header
		if filepath.Ext(filePath) == ".gguf" {
			if err := validateGGUFHeader(fullPath, expectedSize); err != nil {
				issues = append(issues, fmt.Sprintf("file %s: GGUF validation failed: %v", filePath, err))
			}
		}
	}

	if len(issues) > 0 {
		return fmt.Errorf("file verification failed: %v", issues)
	}
	return nil
}

// VerifyChecksum computes the SHA256 of a file and compares it to the expected hash.
func VerifyChecksum(filePath, expectedHash string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("failed to hash file: %w", err)
	}

	actualHash := hex.EncodeToString(h.Sum(nil))
	if actualHash != expectedHash {
		return fmt.Errorf("SHA256 mismatch: expected %s, got %s", expectedHash, actualHash)
	}
	return nil
}

// VerifyFilesDeep performs full SHA256 verification of all files in the manifest
// that have checksums stored. This is more expensive than VerifyFiles but catches
// corruption that preserves file size.
func VerifyFilesDeep(modelDir string) error {
	manifest, err := ReadManifest(modelDir)
	if err != nil {
		return fmt.Errorf("failed to read manifest: %w", err)
	}

	if len(manifest.Checksums) == 0 {
		return nil // no checksums to verify
	}

	var issues []string
	for filePath, expectedHash := range manifest.Checksums {
		fullPath := filepath.Join(modelDir, filePath)
		if err := VerifyChecksum(fullPath, expectedHash); err != nil {
			issues = append(issues, fmt.Sprintf("file %s: %v", filePath, err))
		}
	}

	if len(issues) > 0 {
		return fmt.Errorf("SHA256 verification failed: %v", issues)
	}
	return nil
}

// ComputeSHA256 computes the SHA256 hash of a file and returns it as a hex string.
func ComputeSHA256(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("failed to hash file: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ggufTypeSize maps GGUF quantization type IDs to their block_size and bytes_per_block.
// This allows computing exact tensor data sizes for validation.
type ggufQuantInfo struct {
	blockSize     uint64
	bytesPerBlock uint64
}

var ggufQuantTypes = map[uint32]ggufQuantInfo{
	0:  {1, 4},     // F32
	1:  {1, 2},     // F16
	2:  {32, 18},   // Q4_0
	3:  {32, 20},   // Q4_1
	6:  {32, 22},   // Q5_0
	7:  {32, 24},   // Q5_1
	8:  {32, 34},   // Q8_0
	9:  {32, 40},   // Q8_1
	10: {256, 84},  // Q2_K
	11: {256, 110}, // Q3_K
	12: {256, 144}, // Q4_K
	13: {256, 176}, // Q5_K
	14: {256, 210}, // Q6_K
	16: {256, 66},  // IQ2_XXS
	17: {256, 74},  // IQ2_XS
	18: {256, 98},  // IQ3_XXS
	19: {256, 56},  // IQ1_S
	20: {32, 18},   // IQ4_NL
	21: {256, 110}, // IQ3_S
	22: {256, 82},  // IQ2_S
	23: {256, 136}, // IQ4_XS
	24: {1, 1},     // I8
	25: {1, 2},     // I16
	26: {1, 4},     // I32
	27: {1, 8},     // I64
	28: {1, 8},     // F64
	29: {256, 56},  // IQ1_M
	30: {1, 2},     // BF16
}

// GGUF metadata value type IDs
const (
	ggufTypeUint8   uint32 = 0
	ggufTypeInt8    uint32 = 1
	ggufTypeUint16  uint32 = 2
	ggufTypeInt16   uint32 = 3
	ggufTypeUint32  uint32 = 4
	ggufTypeInt32   uint32 = 5
	ggufTypeFloat32 uint32 = 6
	ggufTypeBool    uint32 = 7
	ggufTypeString  uint32 = 8
	ggufTypeArray   uint32 = 9
	ggufTypeUint64  uint32 = 10
	ggufTypeInt64   uint32 = 11
	ggufTypeFloat64 uint32 = 12
)

// ggufTensorInfo holds parsed tensor info from the GGUF file.
type ggufTensorInfo struct {
	name       string
	nDims      uint32
	dimensions []uint64
	typeID     uint32
	offset     uint64
}

// ggufHeader holds parsed GGUF file header fields.
type ggufHeader struct {
	version       uint32
	tensorCount   uint64
	metadataCount uint64
}

// validateGGUFHeader validates that a GGUF file has a valid header and that every
// tensor's data fits within the file bounds. This catches the exact corruption pattern
// of "tensor data not within file bounds".
func validateGGUFHeader(path string, actualSize int64) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer func() { _ = file.Close() }()

	r := bufio.NewReaderSize(file, 256*1024) // 256KB buffer for efficient reading

	header, err := readGGUFFileHeader(r)
	if err != nil {
		return err
	}

	// Track bytes read so far: 4 (magic) + 4 (version) + 8 (tensors) + 8 (metadata) = 24
	var bytesRead int64 = 24

	alignment, n, err := parseGGUFMetadata(r, header.metadataCount)
	if err != nil {
		return err
	}
	bytesRead += n

	tensors, n, err := parseGGUFTensorInfos(r, header.tensorCount)
	if err != nil {
		return err
	}
	bytesRead += n

	return validateGGUFTensorBounds(tensors, bytesRead, alignment, actualSize)
}

// readGGUFFileHeader reads and validates the GGUF magic, version, tensor count, and metadata count.
func readGGUFFileHeader(r io.Reader) (ggufHeader, error) {
	magic := make([]byte, 4)
	if _, err := io.ReadFull(r, magic); err != nil {
		return ggufHeader{}, fmt.Errorf("failed to read magic: %w", err)
	}
	if string(magic) != ggufMagic {
		return ggufHeader{}, fmt.Errorf("invalid GGUF magic: got %q, expected %q", string(magic), ggufMagic)
	}

	var h ggufHeader
	if err := binary.Read(r, binary.LittleEndian, &h.version); err != nil {
		return ggufHeader{}, fmt.Errorf("failed to read version: %w", err)
	}
	if h.version < 2 || h.version > 4 {
		return ggufHeader{}, fmt.Errorf("unsupported GGUF version: %d (expected 2-4)", h.version)
	}

	if err := binary.Read(r, binary.LittleEndian, &h.tensorCount); err != nil {
		return ggufHeader{}, fmt.Errorf("failed to read tensor count: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &h.metadataCount); err != nil {
		return ggufHeader{}, fmt.Errorf("failed to read metadata count: %w", err)
	}

	if h.tensorCount > 1_000_000 {
		return ggufHeader{}, fmt.Errorf("implausible tensor count: %d", h.tensorCount)
	}
	if h.metadataCount > 1_000_000 {
		return ggufHeader{}, fmt.Errorf("implausible metadata KV count: %d", h.metadataCount)
	}

	return h, nil
}

// parseGGUFMetadata parses all metadata KV pairs, extracting the alignment value.
// Returns the alignment and total bytes consumed.
func parseGGUFMetadata(r io.Reader, count uint64) (uint64, int64, error) {
	alignment := uint64(32) // default
	var bytesRead int64

	for i := uint64(0); i < count; i++ {
		key, n, err := readGGUFString(r)
		if err != nil {
			return 0, bytesRead, fmt.Errorf("failed to read metadata key %d: %w", i, err)
		}
		bytesRead += n

		var valueType uint32
		if err := binary.Read(r, binary.LittleEndian, &valueType); err != nil {
			return 0, bytesRead, fmt.Errorf("failed to read metadata value type for key %q: %w", key, err)
		}
		bytesRead += 4

		if key == "general.alignment" {
			val, n, err := readGGUFMetadataValue(r, valueType)
			if err != nil {
				return 0, bytesRead, fmt.Errorf("failed to read alignment value: %w", err)
			}
			bytesRead += n
			switch v := val.(type) {
			case uint64:
				alignment = v
			case uint32:
				alignment = uint64(v)
			}
		} else {
			n, err := skipGGUFMetadataValue(r, valueType)
			if err != nil {
				return 0, bytesRead, fmt.Errorf("failed to skip metadata value for key %q (type %d): %w", key, valueType, err)
			}
			bytesRead += n
		}
	}

	return alignment, bytesRead, nil
}

// parseGGUFTensorInfos parses tensor info entries from the GGUF file.
// Returns the tensor infos and total bytes consumed.
func parseGGUFTensorInfos(r io.Reader, count uint64) ([]ggufTensorInfo, int64, error) {
	tensors := make([]ggufTensorInfo, count)
	var bytesRead int64

	for i := uint64(0); i < count; i++ {
		name, n, err := readGGUFString(r)
		if err != nil {
			return nil, bytesRead, fmt.Errorf("failed to read tensor %d name: %w", i, err)
		}
		bytesRead += n

		var nDims uint32
		if err := binary.Read(r, binary.LittleEndian, &nDims); err != nil {
			return nil, bytesRead, fmt.Errorf("failed to read tensor %d ndims: %w", i, err)
		}
		bytesRead += 4

		if nDims > 8 {
			return nil, bytesRead, fmt.Errorf("tensor %q has implausible %d dimensions", name, nDims)
		}

		dims := make([]uint64, nDims)
		for d := uint32(0); d < nDims; d++ {
			if err := binary.Read(r, binary.LittleEndian, &dims[d]); err != nil {
				return nil, bytesRead, fmt.Errorf("failed to read tensor %q dimension %d: %w", name, d, err)
			}
			bytesRead += 8
		}

		var typeID uint32
		if err := binary.Read(r, binary.LittleEndian, &typeID); err != nil {
			return nil, bytesRead, fmt.Errorf("failed to read tensor %q type: %w", name, err)
		}
		bytesRead += 4

		var offset uint64
		if err := binary.Read(r, binary.LittleEndian, &offset); err != nil {
			return nil, bytesRead, fmt.Errorf("failed to read tensor %q offset: %w", name, err)
		}
		bytesRead += 8

		tensors[i] = ggufTensorInfo{
			name:       name,
			nDims:      nDims,
			dimensions: dims,
			typeID:     typeID,
			offset:     offset,
		}
	}

	return tensors, bytesRead, nil
}

// validateGGUFTensorBounds checks that every tensor's data fits within the file.
func validateGGUFTensorBounds(tensors []ggufTensorInfo, headerSize int64, alignment uint64, fileSize int64) error {
	tensorDataStart := headerSize
	if alignment > 0 {
		remainder := uint64(tensorDataStart) % alignment
		if remainder != 0 {
			tensorDataStart += int64(alignment - remainder)
		}
	}

	for _, t := range tensors {
		dataSize, err := computeTensorDataSize(t)
		if err != nil {
			return fmt.Errorf("tensor %q: %w", t.name, err)
		}

		endOffset := tensorDataStart + int64(t.offset) + int64(dataSize)
		if endOffset > fileSize {
			return fmt.Errorf("tensor %q data not within file bounds: offset=%d, size=%d, end=%d, file_size=%d",
				t.name, t.offset, dataSize, endOffset, fileSize)
		}
	}

	return nil
}

// computeTensorDataSize computes the byte size of a tensor's data from its dimensions and type.
func computeTensorDataSize(t ggufTensorInfo) (uint64, error) {
	qinfo, ok := ggufQuantTypes[t.typeID]
	if !ok {
		return 0, fmt.Errorf("unknown quantization type: %d", t.typeID)
	}

	// Total number of elements
	numElements := uint64(1)
	for _, d := range t.dimensions {
		if d == 0 {
			return 0, nil
		}
		numElements *= d
	}

	// Number of blocks = ceil(numElements / blockSize)
	numBlocks := (numElements + qinfo.blockSize - 1) / qinfo.blockSize
	return numBlocks * qinfo.bytesPerBlock, nil
}

// readGGUFString reads a GGUF string (uint64 length + bytes) and returns the string and bytes consumed.
func readGGUFString(r io.Reader) (string, int64, error) {
	var length uint64
	if err := binary.Read(r, binary.LittleEndian, &length); err != nil {
		return "", 0, err
	}
	if length > 1<<20 { // 1MB max string length sanity check
		return "", 0, fmt.Errorf("string length %d exceeds sanity limit", length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", 0, err
	}
	return string(buf), 8 + int64(length), nil
}

// readGGUFMetadataValue reads a metadata value and returns it along with bytes consumed.
// Only used for values we actually need (like alignment).
func readGGUFMetadataValue(r io.Reader, valueType uint32) (interface{}, int64, error) {
	switch valueType {
	case ggufTypeUint32:
		var v uint32
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, 0, err
		}
		return v, 4, nil
	case ggufTypeUint64:
		var v uint64
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, 0, err
		}
		return v, 8, nil
	default:
		// For other types, skip and return nil
		n, err := skipGGUFMetadataValue(r, valueType)
		return nil, n, err
	}
}

// skipGGUFMetadataValue skips a metadata value and returns bytes consumed.
func skipGGUFMetadataValue(r io.Reader, valueType uint32) (int64, error) {
	switch valueType {
	case ggufTypeUint8, ggufTypeInt8, ggufTypeBool:
		_, err := readBytes(r, 1)
		return 1, err
	case ggufTypeUint16, ggufTypeInt16:
		_, err := readBytes(r, 2)
		return 2, err
	case ggufTypeUint32, ggufTypeInt32, ggufTypeFloat32:
		_, err := readBytes(r, 4)
		return 4, err
	case ggufTypeUint64, ggufTypeInt64, ggufTypeFloat64:
		_, err := readBytes(r, 8)
		return 8, err
	case ggufTypeString:
		_, n, err := readGGUFString(r)
		return n, err
	case ggufTypeArray:
		// Read element type and count
		var elemType uint32
		if err := binary.Read(r, binary.LittleEndian, &elemType); err != nil {
			return 0, err
		}
		var count uint64
		if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
			return 0, err
		}
		if count > 100_000_000 {
			return 0, fmt.Errorf("array count %d exceeds sanity limit", count)
		}
		totalRead := int64(12) // 4 (type) + 8 (count)
		for i := uint64(0); i < count; i++ {
			n, err := skipGGUFMetadataValue(r, elemType)
			if err != nil {
				return totalRead, err
			}
			totalRead += n
		}
		return totalRead, nil
	default:
		return 0, fmt.Errorf("unknown metadata value type: %d", valueType)
	}
}

// readBytes reads exactly n bytes from r and discards them.
func readBytes(r io.Reader, n int) (int, error) {
	buf := make([]byte, n)
	_, err := io.ReadFull(r, buf)
	return n, err
}

// VerificationResult represents the result of checking and verifying the ready marker.
type VerificationResult struct {
	// MarkerExists is true if the .ready marker file exists.
	MarkerExists bool
	// Verified is true if the marker exists and all files passed verification.
	Verified bool
	// Error contains the verification error if MarkerExists is true but Verified is false.
	Error error
}

// CheckAndVerifyReadyMarker checks if the model is ready and verifies files.
// Returns true only if the ready marker exists AND all files verify correctly.
// If verification fails, the ready marker is removed and corrupted files are deleted
// to trigger a fresh re-download.
func CheckAndVerifyReadyMarker(modelDir string) bool {
	result := CheckAndVerifyReadyMarkerDetailed(modelDir)
	return result.Verified
}

// CheckAndVerifyReadyMarkerDetailed checks the ready marker and verifies files,
// returning detailed results so callers can distinguish between "no marker" (first download)
// and "marker exists but verification failed" (corruption).
func CheckAndVerifyReadyMarkerDetailed(modelDir string) VerificationResult {
	if !CheckReadyMarker(modelDir) {
		return VerificationResult{MarkerExists: false, Verified: false}
	}

	// Verify files exist and have correct sizes
	if err := VerifyFiles(modelDir); err != nil {
		// Verification failed - remove ready marker and corrupted files
		fmt.Printf("[download] File verification failed, cleaning up: %v\n", err)
		_ = RemoveReadyMarker(modelDir)

		// Delete the corrupted model directory to force fresh download
		if err := os.RemoveAll(modelDir); err != nil {
			fmt.Printf("[download] Warning: failed to remove corrupted model directory: %v\n", err)
		}

		return VerificationResult{MarkerExists: true, Verified: false, Error: err}
	}

	return VerificationResult{MarkerExists: true, Verified: true}
}

// CleanHuggingFaceCache removes the HuggingFace cache for a specific repository.
// The cache is stored in HF_HOME/hub/models--org--repo-name/
// This should be called when verification fails to ensure a fresh download.
func CleanHuggingFaceCache(hfHome, repoID string) error {
	// Convert repo ID (org/repo-name) to cache directory name (models--org--repo-name)
	cacheDirName := "models--" + strings.ReplaceAll(repoID, "/", "--")
	cachePath := filepath.Join(hfHome, cacheDirName)

	// Also check in hub/ subdirectory (older HF cache structure)
	hubCachePath := filepath.Join(hfHome, "hub", cacheDirName)

	removed := false

	// Remove main cache path
	if _, err := os.Stat(cachePath); err == nil {
		if err := os.RemoveAll(cachePath); err != nil {
			return fmt.Errorf("failed to remove HF cache at %s: %w", cachePath, err)
		}
		fmt.Printf("[download] Removed HuggingFace cache: %s\n", cachePath)
		removed = true
	}

	// Remove hub cache path
	if _, err := os.Stat(hubCachePath); err == nil {
		if err := os.RemoveAll(hubCachePath); err != nil {
			return fmt.Errorf("failed to remove HF hub cache at %s: %w", hubCachePath, err)
		}
		fmt.Printf("[download] Removed HuggingFace hub cache: %s\n", hubCachePath)
		removed = true
	}

	if !removed {
		fmt.Printf("[download] No HuggingFace cache found for repo: %s\n", repoID)
	}

	return nil
}
