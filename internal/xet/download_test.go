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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pierrec/lz4/v4"
)

// mockReconstructionServer creates a mock server that returns reconstruction responses
func mockReconstructionServer(t *testing.T, fileID string, recon *ReconstructionResponse) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/reconstructions/"+fileID {
			json.NewEncoder(w).Encode(recon)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

// mockXorbServer creates a mock server that serves xorb data
func mockXorbServer(t *testing.T, xorbData []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		if rangeHeader != "" {
			// Parse range header (simplified: bytes=start-end)
			var start, end int
			n, err := sscanfRange(rangeHeader, &start, &end)
			if err != nil || n != 2 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if end > len(xorbData) {
				end = len(xorbData)
			}
			w.Header().Set("Content-Range", r.Header.Get("Range"))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(xorbData[start : end+1])
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(xorbData)
	}))
}

// sscanfRange is a helper to parse range headers
func sscanfRange(rangeHeader string, start, end *int) (int, error) {
	var s, e int
	n, err := sscanf(rangeHeader, "bytes=%d-%d", &s, &e)
	if err != nil {
		return 0, err
	}
	*start = s
	*end = e
	return n, nil
}

// Simple sscanf implementation for range parsing
func sscanf(s, format string, a ...interface{}) (int, error) {
	// Very simplified implementation for "bytes=%d-%d" format
	if len(s) < 7 || s[:6] != "bytes=" {
		return 0, os.ErrInvalid
	}
 nums := s[6:]
	dash := -1
	for i, c := range nums {
		if c == '-' {
			dash = i
			break
		}
	}
	if dash == -1 {
		return 0, os.ErrInvalid
	}

	startStr := nums[:dash]
	endStr := nums[dash+1:]

	var start, end int
	for _, c := range startStr {
		if c >= '0' && c <= '9' {
			start = start*10 + int(c-'0')
		}
	}
	for _, c := range endStr {
		if c >= '0' && c <= '9' {
			end = end*10 + int(c-'0')
		}
	}

	if len(a) >= 2 {
		if ptr, ok := a[0].(*int); ok {
			*ptr = start
		}
		if ptr, ok := a[1].(*int); ok {
			*ptr = end
		}
	}
	return 2, nil
}

// createTestXorbData creates a simple xorb with uncompressed chunks
func createTestXorbData(t *testing.T, chunks [][]byte) []byte {
	var data []byte
	for _, chunk := range chunks {
		header := &ChunkHeader{
			Version:          0x01,
			CompressionType:  CompressionNone,
			CompressedSize:   uint32(len(chunk)),
			UncompressedSize: uint32(len(chunk)),
		}
		data = append(data, header.Serialize()...)
		data = append(data, chunk...)
	}
	return data
}

// createTestXorbDataLZ4 creates xorb data with LZ4 compressed chunks
func createTestXorbDataLZ4(t *testing.T, originalChunks [][]byte) []byte {
	var data []byte
	for _, chunk := range originalChunks {
		// Compress with LZ4
		var buf bytes.Buffer
		writer := lz4.NewWriter(&buf)
		writer.Write(chunk)
		writer.Close()
		compressed := buf.Bytes()

		header := &ChunkHeader{
			Version:          0x01,
			CompressionType:  CompressionLZ4,
			CompressedSize:   uint32(len(compressed)),
			UncompressedSize: uint32(len(chunk)),
		}
		data = append(data, header.Serialize()...)
		data = append(data, compressed...)
	}
	return data
}

func TestNewDownloader(t *testing.T) {
	client := NewClient()
	downloader := NewDownloader(client)

	if downloader == nil {
		t.Fatal("NewDownloader() returned nil")
	}
	if downloader.client != client {
		t.Error("Client not set correctly")
	}
	if downloader.concurrency != 4 {
		t.Errorf("Default concurrency = %d, want 4", downloader.concurrency)
	}
}

func TestDownloaderWithConcurrency(t *testing.T) {
	client := NewClient()
	downloader := NewDownloader(client, WithConcurrency(8))

	if downloader.concurrency != 8 {
		t.Errorf("Concurrency = %d, want 8", downloader.concurrency)
	}
}

func TestDownloaderDownloadData(t *testing.T) {
	// Create test data
	testChunks := [][]byte{
		[]byte("Hello, "),
		[]byte("World!"),
	}
	xorbData := createTestXorbData(t, testChunks)

	// Set up mock servers
	xorbServer := mockXorbServer(t, xorbData)
	defer xorbServer.Close()

	fileID := "test-file-id"
	recon := &ReconstructionResponse{
		OffsetIntoFirstRange: 0,
		Terms: []ReconstructionTerm{
			{
				Hash:           "hash1",
				UnpackedLength: 13, // "Hello, World!" length
				Range:          ByteRange{Start: 0, End: int64(len(xorbData))},
			},
		},
		FetchInfo: map[string][]FetchInfo{
			"hash1": {
				{
					URL:     xorbServer.URL,
					URLRange: ByteRange{Start: 0, End: int64(len(xorbData))},
					Range:   ByteRange{Start: 0, End: int64(len(xorbData))},
				},
			},
		},
	}

	reconServer := mockReconstructionServer(t, fileID, recon)
	defer reconServer.Close()

	client := NewClient(WithXetAPIEndpoint(reconServer.URL))
	downloader := NewDownloader(client)

	data, err := downloader.DownloadData(context.Background(), DownloadOptions{FileID: fileID})
	if err != nil {
		t.Fatalf("DownloadData() error: %v", err)
	}

	expected := "Hello, World!"
	if string(data) != expected {
		t.Errorf("DownloadData() = %q, want %q", string(data), expected)
	}
}

func TestDownloaderDownloadDataLZ4(t *testing.T) {
	// Create test data with LZ4 compression
	testChunks := [][]byte{
		[]byte("Compressed "),
		[]byte("Data Here!"),
	}
	xorbData := createTestXorbDataLZ4(t, testChunks)

	// Set up mock servers
	xorbServer := mockXorbServer(t, xorbData)
	defer xorbServer.Close()

	fileID := "test-lz4-file-id"
	recon := &ReconstructionResponse{
		OffsetIntoFirstRange: 0,
		Terms: []ReconstructionTerm{
			{
				Hash:           "hash-lz4",
				UnpackedLength: 23, // "Compressed Data Here!" length
				Range:          ByteRange{Start: 0, End: int64(len(xorbData))},
			},
		},
		FetchInfo: map[string][]FetchInfo{
			"hash-lz4": {
				{
					URL:     xorbServer.URL,
					URLRange: ByteRange{Start: 0, End: int64(len(xorbData))},
					Range:   ByteRange{Start: 0, End: int64(len(xorbData))},
				},
			},
		},
	}

	reconServer := mockReconstructionServer(t, fileID, recon)
	defer reconServer.Close()

	client := NewClient(WithXetAPIEndpoint(reconServer.URL))
	downloader := NewDownloader(client)

	data, err := downloader.DownloadData(context.Background(), DownloadOptions{FileID: fileID})
	if err != nil {
		t.Fatalf("DownloadData() error: %v", err)
	}

	expected := "Compressed Data Here!"
	if string(data) != expected {
		t.Errorf("DownloadData() = %q, want %q", string(data), expected)
	}
}

func TestDownloaderDownloadToWriter(t *testing.T) {
	testChunks := [][]byte{
		[]byte("Test "),
		[]byte("Writer"),
	}
	xorbData := createTestXorbData(t, testChunks)

	xorbServer := mockXorbServer(t, xorbData)
	defer xorbServer.Close()

	fileID := "test-writer-id"
	recon := &ReconstructionResponse{
		OffsetIntoFirstRange: 0,
		Terms: []ReconstructionTerm{
			{
				Hash:           "hash-writer",
				UnpackedLength: 11, // "Test Writer" length
				Range:          ByteRange{Start: 0, End: int64(len(xorbData))},
			},
		},
		FetchInfo: map[string][]FetchInfo{
			"hash-writer": {
				{
					URL:     xorbServer.URL,
					URLRange: ByteRange{Start: 0, End: int64(len(xorbData))},
					Range:   ByteRange{Start: 0, End: int64(len(xorbData))},
				},
			},
		},
	}

	reconServer := mockReconstructionServer(t, fileID, recon)
	defer reconServer.Close()

	client := NewClient(WithXetAPIEndpoint(reconServer.URL))
	downloader := NewDownloader(client)

	var buf bytes.Buffer
	err := downloader.DownloadToWriter(context.Background(), DownloadOptions{FileID: fileID}, &buf)
	if err != nil {
		t.Fatalf("DownloadToWriter() error: %v", err)
	}

	expected := "Test Writer"
	if buf.String() != expected {
		t.Errorf("DownloadToWriter() = %q, want %q", buf.String(), expected)
	}
}

func TestDownloaderDownloadToFile(t *testing.T) {
	testChunks := [][]byte{
		[]byte("File "),
		[]byte("Content"),
	}
	xorbData := createTestXorbData(t, testChunks)

	xorbServer := mockXorbServer(t, xorbData)
	defer xorbServer.Close()

	fileID := "test-file-download-id"
	recon := &ReconstructionResponse{
		OffsetIntoFirstRange: 0,
		Terms: []ReconstructionTerm{
			{
				Hash:           "hash-file",
				UnpackedLength: 12, // "File Content" length
				Range:          ByteRange{Start: 0, End: int64(len(xorbData))},
			},
		},
		FetchInfo: map[string][]FetchInfo{
			"hash-file": {
				{
					URL:     xorbServer.URL,
					URLRange: ByteRange{Start: 0, End: int64(len(xorbData))},
					Range:   ByteRange{Start: 0, End: int64(len(xorbData))},
				},
			},
		},
	}

	reconServer := mockReconstructionServer(t, fileID, recon)
	defer reconServer.Close()

	client := NewClient(WithXetAPIEndpoint(reconServer.URL))
	downloader := NewDownloader(client)

	// Create temp directory for output
	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "test-output.txt")

	err := downloader.Download(context.Background(), DownloadOptions{
		FileID:     fileID,
		OutputPath: outputPath,
	})
	if err != nil {
		t.Fatalf("Download() error: %v", err)
	}

	// Verify file was created
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("Failed to read output file: %v", err)
	}

	expected := "File Content"
	if string(data) != expected {
		t.Errorf("Download() file content = %q, want %q", string(data), expected)
	}
}

func TestDownloaderDownloadCreatesDirectory(t *testing.T) {
	testChunks := [][]byte{[]byte("test")}
	xorbData := createTestXorbData(t, testChunks)

	xorbServer := mockXorbServer(t, xorbData)
	defer xorbServer.Close()

	fileID := "test-mkdir-id"
	recon := &ReconstructionResponse{
		OffsetIntoFirstRange: 0,
		Terms: []ReconstructionTerm{
			{
				Hash:           "hash-mkdir",
				UnpackedLength: 4,
				Range:          ByteRange{Start: 0, End: int64(len(xorbData))},
			},
		},
		FetchInfo: map[string][]FetchInfo{
			"hash-mkdir": {
				{
					URL:     xorbServer.URL,
					URLRange: ByteRange{Start: 0, End: int64(len(xorbData))},
					Range:   ByteRange{Start: 0, End: int64(len(xorbData))},
				},
			},
		},
	}

	reconServer := mockReconstructionServer(t, fileID, recon)
	defer reconServer.Close()

	client := NewClient(WithXetAPIEndpoint(reconServer.URL))
	downloader := NewDownloader(client)

	// Create temp directory for output
	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "nested", "dir", "output.txt")

	err := downloader.Download(context.Background(), DownloadOptions{
		FileID:     fileID,
		OutputPath: outputPath,
	})
	if err != nil {
		t.Fatalf("Download() error: %v", err)
	}

	// Verify file was created in nested directory
	if _, err := os.Stat(outputPath); os.IsNotExist(err) {
		t.Error("Output file was not created")
	}
}

func TestDownloaderGetFileSize(t *testing.T) {
	fileID := "test-size-id"
	recon := &ReconstructionResponse{
		OffsetIntoFirstRange: 0,
		Terms: []ReconstructionTerm{
			{
				Hash:           "hash1",
				UnpackedLength: 1000,
				Range:          ByteRange{Start: 0, End: 500},
			},
			{
				Hash:           "hash2",
				UnpackedLength: 2000,
				Range:          ByteRange{Start: 0, End: 1000},
			},
		},
		FetchInfo: map[string][]FetchInfo{
			"hash1": {{URL: "http://example.com/xorb1"}},
			"hash2": {{URL: "http://example.com/xorb2"}},
		},
	}

	reconServer := mockReconstructionServer(t, fileID, recon)
	defer reconServer.Close()

	client := NewClient(WithXetAPIEndpoint(reconServer.URL))
	downloader := NewDownloader(client)

	size, err := downloader.GetFileSize(context.Background(), DownloadOptions{FileID: fileID})
	if err != nil {
		t.Fatalf("GetFileSize() error: %v", err)
	}

	expectedSize := int64(3000) // 1000 + 2000
	if size != expectedSize {
		t.Errorf("GetFileSize() = %d, want %d", size, expectedSize)
	}
}

func TestDownloaderMissingFetchInfo(t *testing.T) {
	fileID := "test-missing-fetch"
	recon := &ReconstructionResponse{
		OffsetIntoFirstRange: 0,
		Terms: []ReconstructionTerm{
			{
				Hash:           "missing-hash",
				UnpackedLength: 100,
				Range:          ByteRange{Start: 0, End: 50},
			},
		},
		FetchInfo: map[string][]FetchInfo{
			// Note: missing-hash is not in FetchInfo
		},
	}

	reconServer := mockReconstructionServer(t, fileID, recon)
	defer reconServer.Close()

	client := NewClient(WithXetAPIEndpoint(reconServer.URL))
	downloader := NewDownloader(client)

	_, err := downloader.DownloadData(context.Background(), DownloadOptions{FileID: fileID})
	if err == nil {
		t.Error("DownloadData() should fail with missing fetch info")
	}
}

func TestDownloaderEmptyFetchInfo(t *testing.T) {
	fileID := "test-empty-fetch"
	recon := &ReconstructionResponse{
		OffsetIntoFirstRange: 0,
		Terms: []ReconstructionTerm{
			{
				Hash:           "hash-empty",
				UnpackedLength: 100,
				Range:          ByteRange{Start: 0, End: 50},
			},
		},
		FetchInfo: map[string][]FetchInfo{
			"hash-empty": {}, // Empty fetch info array
		},
	}

	reconServer := mockReconstructionServer(t, fileID, recon)
	defer reconServer.Close()

	client := NewClient(WithXetAPIEndpoint(reconServer.URL))
	downloader := NewDownloader(client)

	_, err := downloader.DownloadData(context.Background(), DownloadOptions{FileID: fileID})
	if err == nil {
		t.Error("DownloadData() should fail with empty fetch info")
	}
}

func TestDownloaderNetworkError(t *testing.T) {
	fileID := "test-network-error"
	recon := &ReconstructionResponse{
		OffsetIntoFirstRange: 0,
		Terms: []ReconstructionTerm{
			{
				Hash:           "hash-network",
				UnpackedLength: 100,
				Range:          ByteRange{Start: 0, End: 50},
			},
		},
		FetchInfo: map[string][]FetchInfo{
			"hash-network": {
				{
					URL:     "http://nonexistent.invalid/xorb",
					URLRange: ByteRange{Start: 0, End: 50},
					Range:   ByteRange{Start: 0, End: 50},
				},
			},
		},
	}

	reconServer := mockReconstructionServer(t, fileID, recon)
	defer reconServer.Close()

	client := NewClient(WithXetAPIEndpoint(reconServer.URL))
	downloader := NewDownloader(client)

	_, err := downloader.DownloadData(context.Background(), DownloadOptions{FileID: fileID})
	if err == nil {
		t.Error("DownloadData() should fail with network error")
	}
}

func TestDownloaderReconstructionError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := NewClient(WithXetAPIEndpoint(server.URL))
	downloader := NewDownloader(client)

	_, err := downloader.DownloadData(context.Background(), DownloadOptions{FileID: "nonexistent"})
	if err == nil {
		t.Error("DownloadData() should fail with reconstruction error")
	}
}

func TestDownloaderContextCancellation(t *testing.T) {
	// Create a slow server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(WithXetAPIEndpoint(server.URL))
	downloader := NewDownloader(client)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := downloader.DownloadData(ctx, DownloadOptions{FileID: "test"})
	if err == nil {
		t.Error("DownloadData() should fail with cancelled context")
	}
}

func TestDownloaderMultipleTerms(t *testing.T) {
	// Create two separate xorb data sets
	chunk1 := [][]byte{[]byte("Part1-")}
	chunk2 := [][]byte{[]byte("Part2-")}
	xorbData1 := createTestXorbData(t, chunk1)
	xorbData2 := createTestXorbData(t, chunk2)

	// Create two xorb servers
	xorbServer1 := mockXorbServer(t, xorbData1)
	defer xorbServer1.Close()
	xorbServer2 := mockXorbServer(t, xorbData2)
	defer xorbServer2.Close()

	fileID := "test-multi-term"
	recon := &ReconstructionResponse{
		OffsetIntoFirstRange: 0,
		Terms: []ReconstructionTerm{
			{
				Hash:           "hash1",
				UnpackedLength: 6, // "Part1-"
				Range:          ByteRange{Start: 0, End: int64(len(xorbData1))},
			},
			{
				Hash:           "hash2",
				UnpackedLength: 6, // "Part2-"
				Range:          ByteRange{Start: 0, End: int64(len(xorbData2))},
			},
		},
		FetchInfo: map[string][]FetchInfo{
			"hash1": {
				{
					URL:     xorbServer1.URL,
					URLRange: ByteRange{Start: 0, End: int64(len(xorbData1))},
					Range:   ByteRange{Start: 0, End: int64(len(xorbData1))},
				},
			},
			"hash2": {
				{
					URL:     xorbServer2.URL,
					URLRange: ByteRange{Start: 0, End: int64(len(xorbData2))},
					Range:   ByteRange{Start: 0, End: int64(len(xorbData2))},
				},
			},
		},
	}

	reconServer := mockReconstructionServer(t, fileID, recon)
	defer reconServer.Close()

	client := NewClient(WithXetAPIEndpoint(reconServer.URL))
	downloader := NewDownloader(client)

	data, err := downloader.DownloadData(context.Background(), DownloadOptions{FileID: fileID})
	if err != nil {
		t.Fatalf("DownloadData() error: %v", err)
	}

	expected := "Part1-Part2-"
	if string(data) != expected {
		t.Errorf("DownloadData() = %q, want %q", string(data), expected)
	}
}

func TestDownloaderLargeData(t *testing.T) {
	// Create larger test data
	largeChunk := make([]byte, 64*1024) // 64KB
	for i := range largeChunk {
		largeChunk[i] = byte(i % 256)
	}

	testChunks := [][]byte{largeChunk}
	xorbData := createTestXorbData(t, testChunks)

	xorbServer := mockXorbServer(t, xorbData)
	defer xorbServer.Close()

	fileID := "test-large-id"
	recon := &ReconstructionResponse{
		OffsetIntoFirstRange: 0,
		Terms: []ReconstructionTerm{
			{
				Hash:           "hash-large",
				UnpackedLength: int64(len(largeChunk)),
				Range:          ByteRange{Start: 0, End: int64(len(xorbData))},
			},
		},
		FetchInfo: map[string][]FetchInfo{
			"hash-large": {
				{
					URL:     xorbServer.URL,
					URLRange: ByteRange{Start: 0, End: int64(len(xorbData))},
					Range:   ByteRange{Start: 0, End: int64(len(xorbData))},
				},
			},
		},
	}

	reconServer := mockReconstructionServer(t, fileID, recon)
	defer reconServer.Close()

	client := NewClient(WithXetAPIEndpoint(reconServer.URL))
	downloader := NewDownloader(client)

	data, err := downloader.DownloadData(context.Background(), DownloadOptions{FileID: fileID})
	if err != nil {
		t.Fatalf("DownloadData() error: %v", err)
	}

	if len(data) != len(largeChunk) {
		t.Errorf("DownloadData() length = %d, want %d", len(data), len(largeChunk))
	}

	// Verify content
	for i := range data {
		if data[i] != byte(i%256) {
			t.Errorf("Data mismatch at index %d: got %d, want %d", i, data[i], byte(i%256))
			break
		}
	}
}
