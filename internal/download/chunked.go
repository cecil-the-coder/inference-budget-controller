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
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cecil-the-coder/inference-budget-controller/internal/huggingface"
)

const (
	// MultipartThreshold is the minimum file size to use chunked downloads (256MB).
	MultipartThreshold = 256 * 1024 * 1024
	// DefaultChunkSize is the target size per chunk (1 GB).
	DefaultChunkSize = 1 * 1024 * 1024 * 1024
	// MaxConcurrentChunks is the maximum number of chunks downloading in parallel.
	MaxConcurrentChunks = 8
	// DefaultMaxRetries is the maximum retry attempts per chunk.
	DefaultMaxRetries = 4
)

// ErrRangeNotSupported is returned when the server does not support Range requests
// or the file is below the multipart threshold.
var ErrRangeNotSupported = errors.New("server does not support Range requests")

type backoffConfig struct {
	Initial    time.Duration
	Max        time.Duration
	Multiplier float64
	Jitter     time.Duration
	MaxRetries int
}

var defaultBackoff = backoffConfig{
	Initial:    400 * time.Millisecond,
	Max:        10 * time.Second,
	Multiplier: 1.6,
	Jitter:     120 * time.Millisecond,
	MaxRetries: DefaultMaxRetries,
}

type chunkRange struct {
	Index int
	Start int64
	End   int64 // inclusive
}

type chunkedDownloader struct {
	hfClient   *huggingface.Client
	bufferPool *BufferPool
	chunkSize  int64
	maxWorkers int
	backoff    backoffConfig
}

// ProgressFunc is called with the number of bytes downloaded so far.
type ProgressFunc func(bytesDone int64)

func newChunkedDownloader(hfClient *huggingface.Client, bufferPool *BufferPool) *chunkedDownloader {
	return &chunkedDownloader{
		hfClient:   hfClient,
		bufferPool: bufferPool,
		chunkSize:  DefaultChunkSize,
		maxWorkers: MaxConcurrentChunks,
		backoff:    defaultBackoff,
	}
}

// Download performs a multipart chunked download. Returns the file size or an error.
// Returns ErrRangeNotSupported if the server doesn't support Range or the file is too small.
func (cd *chunkedDownloader) Download(ctx context.Context, repoID, filePath, localPath string, onProgress ProgressFunc) (int64, error) {
	// HEAD request to get file size and check range support
	fileSize, supportsRange, err := cd.headRequest(ctx, repoID, filePath)
	if err != nil {
		return 0, fmt.Errorf("HEAD request failed: %w", err)
	}

	if !supportsRange || fileSize < MultipartThreshold {
		return 0, ErrRangeNotSupported
	}

	chunks := cd.computeChunks(fileSize)

	fmt.Printf("[chunked] Starting chunked download: %s (%d bytes, %d chunks of ~%d MB, %d workers)\n",
		filePath, fileSize, len(chunks), cd.chunkSize/1024/1024, cd.maxWorkers)

	// Atomic progress counter across all chunks
	var totalDownloaded atomic.Int64

	// Worker pool: feed chunks through a channel, limited concurrency
	chunkCh := make(chan chunkRange, len(chunks))
	for _, c := range chunks {
		chunkCh <- c
	}
	close(chunkCh)

	errCh := make(chan error, len(chunks))
	var wg sync.WaitGroup

	workers := cd.maxWorkers
	if workers > len(chunks) {
		workers = len(chunks)
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range chunkCh {
				chunkProgress := func(n int64) {
					newTotal := totalDownloaded.Add(n)
					if onProgress != nil {
						onProgress(newTotal)
					}
				}
				if err := cd.downloadChunkWithRetry(ctx, repoID, filePath, localPath, c, chunkProgress); err != nil {
					errCh <- fmt.Errorf("chunk %d failed: %w", c.Index, err)
					return
				}
			}
		}()
	}

	// Wait for all workers then close error channel
	go func() {
		wg.Wait()
		close(errCh)
	}()

	// Collect first error
	for err := range errCh {
		if err != nil {
			return 0, err
		}
	}

	// Assemble chunks into final file
	if err := cd.assembleChunks(localPath, chunks); err != nil {
		return 0, fmt.Errorf("assembly failed: %w", err)
	}

	// Clean up part files
	cd.cleanupParts(localPath, chunks)

	fmt.Printf("[chunked] Download complete: %s (%d bytes)\n", filePath, fileSize)
	return fileSize, nil
}

// headRequest sends a HEAD to get file size and check Accept-Ranges support.
func (cd *chunkedDownloader) headRequest(ctx context.Context, repoID, filePath string) (size int64, supportsRange bool, err error) {
	url := huggingface.ResolveDownloadURL(repoID, filePath)

	req, err := cd.hfClient.NewAuthenticatedRequest(ctx, http.MethodHead, url)
	if err != nil {
		return 0, false, err
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false, err
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, false, fmt.Errorf("HEAD returned status %d", resp.StatusCode)
	}

	// Parse Content-Length
	clStr := resp.Header.Get("Content-Length")
	if clStr == "" {
		return 0, false, nil
	}
	size, err = strconv.ParseInt(clStr, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("invalid Content-Length %q: %w", clStr, err)
	}

	// Check Accept-Ranges
	ar := resp.Header.Get("Accept-Ranges")
	supportsRange = strings.Contains(ar, "bytes")

	return size, supportsRange, nil
}

// computeChunks divides fileSize into chunks of cd.chunkSize bytes.
func (cd *chunkedDownloader) computeChunks(fileSize int64) []chunkRange {
	chunkSize := cd.chunkSize
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}

	count := int((fileSize + chunkSize - 1) / chunkSize) // ceiling division
	if count <= 0 {
		count = 1
	}

	chunks := make([]chunkRange, count)
	for i := 0; i < count; i++ {
		start := int64(i) * chunkSize
		end := start + chunkSize - 1
		if end >= fileSize {
			end = fileSize - 1
		}
		chunks[i] = chunkRange{
			Index: i,
			Start: start,
			End:   end,
		}
	}

	return chunks
}

func partPath(localPath string, index int) string {
	return fmt.Sprintf("%s.part-%02d", localPath, index)
}

// downloadChunk downloads a single chunk to a .part-NN file.
func (cd *chunkedDownloader) downloadChunk(ctx context.Context, repoID, filePath, localPath string, chunk chunkRange, onProgress func(int64)) error {
	pp := partPath(localPath, chunk.Index)
	expectedSize := chunk.End - chunk.Start + 1

	// Check if part already exists with correct size (resume)
	if info, err := os.Stat(pp); err == nil && info.Size() == expectedSize {
		fmt.Printf("[chunked] Chunk %d already complete, skipping\n", chunk.Index)
		return nil
	}

	url := huggingface.ResolveDownloadURL(repoID, filePath)
	req, err := cd.hfClient.NewAuthenticatedRequest(ctx, http.MethodGet, url)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", chunk.Start, chunk.End))

	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GET chunk %d failed: %w", chunk.Index, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("chunk %d: unexpected status %d", chunk.Index, resp.StatusCode)
	}

	f, err := os.Create(pp)
	if err != nil {
		return fmt.Errorf("create part file: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Wrap with stall detection
	dlCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stallReader := huggingface.NewStallTimeoutReader(resp.Body, cancel, 5*time.Minute)
	defer stallReader.Close()

	// Wrap with progress reader
	var body io.Reader = stallReader
	if onProgress != nil {
		body = &progressReader{
			r:          stallReader,
			onProgress: onProgress,
			interval:   200 * time.Millisecond,
		}
	}

	buf := cd.bufferPool.Get()
	defer cd.bufferPool.Put(buf)

	written, err := io.CopyBuffer(f, body, buf)
	if err != nil {
		// Check if context was cancelled (stall or parent cancel)
		if dlCtx.Err() != nil {
			return fmt.Errorf("chunk %d: download stalled or cancelled after %d bytes: %w", chunk.Index, written, dlCtx.Err())
		}
		return fmt.Errorf("chunk %d: write failed after %d bytes: %w", chunk.Index, written, err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("chunk %d: fsync failed: %w", chunk.Index, err)
	}

	return nil
}

// downloadChunkWithRetry wraps downloadChunk with exponential backoff retry.
func (cd *chunkedDownloader) downloadChunkWithRetry(ctx context.Context, repoID, filePath, localPath string, chunk chunkRange, onProgress func(int64)) error {
	delay := cd.backoff.Initial
	var lastErr error

	for attempt := 0; attempt <= cd.backoff.MaxRetries; attempt++ {
		if attempt > 0 {
			fmt.Printf("[chunked] Chunk %d: retry %d/%d after %v\n",
				chunk.Index, attempt, cd.backoff.MaxRetries, delay)
			if !sleepCtx(ctx, delay) {
				return ctx.Err()
			}
			// Exponential backoff with jitter
			delay = time.Duration(float64(delay) * cd.backoff.Multiplier)
			if cd.backoff.Jitter > 0 {
				delay += time.Duration(rand.Int63n(int64(cd.backoff.Jitter)))
			}
			if delay > cd.backoff.Max {
				delay = cd.backoff.Max
			}
		}

		lastErr = cd.downloadChunk(ctx, repoID, filePath, localPath, chunk, onProgress)
		if lastErr == nil {
			return nil
		}

		// Don't retry on context cancellation
		if ctx.Err() != nil {
			return lastErr
		}

		fmt.Printf("[chunked] Chunk %d attempt %d failed: %v\n", chunk.Index, attempt, lastErr)
	}

	return fmt.Errorf("chunk %d: all %d retries exhausted: %w", chunk.Index, cd.backoff.MaxRetries, lastErr)
}

// assembleChunks concatenates .part-NN files into localPath via a .part temp file,
// then atomically renames to the final path.
func (cd *chunkedDownloader) assembleChunks(localPath string, chunks []chunkRange) error {
	tmpPath := localPath + ".part"

	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create temp assembly file: %w", err)
	}
	defer func() { _ = f.Close() }()

	buf := cd.bufferPool.Get()
	defer cd.bufferPool.Put(buf)

	for _, chunk := range chunks {
		pp := partPath(localPath, chunk.Index)
		pf, err := os.Open(pp)
		if err != nil {
			return fmt.Errorf("open part %d: %w", chunk.Index, err)
		}
		_, err = io.CopyBuffer(f, pf, buf)
		_ = pf.Close()
		if err != nil {
			return fmt.Errorf("copy part %d: %w", chunk.Index, err)
		}
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync assembly: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close assembly: %w", err)
	}

	return os.Rename(tmpPath, localPath)
}

// cleanupParts removes .part-NN files and the .part temp file.
func (cd *chunkedDownloader) cleanupParts(localPath string, chunks []chunkRange) {
	for _, chunk := range chunks {
		_ = os.Remove(partPath(localPath, chunk.Index))
	}
	_ = os.Remove(localPath + ".part")
}

// sleepCtx sleeps for the given duration or until the context is cancelled.
// Returns true if the sleep completed, false if cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// progressReader wraps an io.Reader and calls onProgress with the delta bytes read,
// throttled to at most one call per interval.
type progressReader struct {
	r          io.Reader
	onProgress func(int64)
	interval   time.Duration
	pending    int64
	lastFlush  time.Time
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	if n > 0 {
		pr.pending += int64(n)
		now := time.Now()
		if now.Sub(pr.lastFlush) >= pr.interval || err != nil {
			pr.onProgress(pr.pending)
			pr.pending = 0
			pr.lastFlush = now
		}
	}
	return n, err
}
