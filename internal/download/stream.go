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
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/cecil-the-coder/inference-budget-controller/internal/huggingface"
	"github.com/cecil-the-coder/inference-budget-controller/internal/xet"
)

// ProgressCallback is called periodically to report download progress.
// bytesDone is the number of bytes downloaded so far.
// bytesTotal is the total expected bytes (may be 0 if unknown).
type ProgressCallback func(bytesDone, bytesTotal int64)

// StreamDownloader handles memory-safe streaming downloads of large files.
// It streams data directly to disk without loading entire files into memory.
type StreamDownloader struct {
	hfClient   *huggingface.Client
	xetClient  *xet.Client
	bufferPool *BufferPool
}

// NewStreamDownloader creates a new stream downloader.
// If bufferPool is nil, a default pool with 64KB buffers is created.
func NewStreamDownloader(hfClient *huggingface.Client, xetClient *xet.Client, pool *BufferPool) *StreamDownloader {
	if pool == nil {
		pool = NewBufferPool(DefaultBufferSize)
	}
	return &StreamDownloader{
		hfClient:   hfClient,
		xetClient:  xetClient,
		bufferPool: pool,
	}
}

// DownloadLFS downloads a file via standard LFS, streaming to disk.
// This method is memory-safe and uses io.CopyBuffer with the buffer pool.
func (s *StreamDownloader) DownloadLFS(ctx context.Context, repo, file, destPath string, cb ProgressCallback) error {
	// 1. Create parent directories
	if err := EnsureDir(destPath); err != nil {
		return fmt.Errorf("failed to create parent directories: %w", err)
	}

	// 2. Create the huggingface repository handle
	hfRepo, err := s.hfClient.NewModelRepository(ctx, repo)
	if err != nil {
		return fmt.Errorf("failed to create repository handle: %w", err)
	}

	// 3. Get file info to determine size for progress reporting
	files, err := hfRepo.ListFiles(ctx)
	if err != nil {
		return fmt.Errorf("failed to list repository files: %w", err)
	}

	var totalSize int64
	for _, f := range files {
		if f.Name == file {
			totalSize = f.Size
			break
		}
	}

	// 4. Download the file using the huggingface client
	// The go-huggingface library handles streaming to its cache
	localPath, err := hfRepo.DownloadFile(ctx, file)
	if err != nil {
		return fmt.Errorf("failed to download file from huggingface: %w", err)
	}

	// 5. Open the downloaded file
	srcFile, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("failed to open downloaded file: %w", err)
	}
	defer srcFile.Close()

	// Get file size if not known
	if totalSize == 0 {
		info, err := srcFile.Stat()
		if err != nil {
			return fmt.Errorf("failed to stat source file: %w", err)
		}
		totalSize = info.Size()
	}

	// 6. Create destination file
	destFile, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer destFile.Close()

	// 7. Wrap with progress writer
	pw := &progressWriter{
		w:          destFile,
		total:      totalSize,
		onProgress: cb,
		lastUpdate: time.Now(),
	}

	// 8. Stream copy using buffer pool
	buf := s.bufferPool.Get()
	defer s.bufferPool.Put(buf)

	_, err = io.CopyBuffer(pw, srcFile, buf)
	if err != nil {
		return fmt.Errorf("failed to copy file: %w", err)
	}

	// 9. Sync to ensure data is on disk
	if err := destFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync file: %w", err)
	}

	// Final progress report
	if cb != nil {
		cb(totalSize, totalSize)
	}

	return nil
}

// DownloadXet downloads a file via Xet protocol, streaming to disk.
// This method uses the Xet Downloader's streaming capabilities.
func (s *StreamDownloader) DownloadXet(ctx context.Context, repo, branch, file, destPath string, cb ProgressCallback) error {
	// Parse repo into namespace and name
	namespace, repoName, err := parseRepo(repo)
	if err != nil {
		return err
	}

	// Default branch to "main" if empty
	if branch == "" {
		branch = "main"
	}

	// 1. Create parent directories
	if err := EnsureDir(destPath); err != nil {
		return fmt.Errorf("failed to create parent directories: %w", err)
	}

	// 2. Resolve file ID
	fileID, err := s.xetClient.ResolveFileID(ctx, namespace, repoName, branch, file)
	if err != nil {
		return fmt.Errorf("failed to resolve file ID: %w", err)
	}

	// 3. Get reconstruction metadata
	recon, err := s.xetClient.GetReconstruction(ctx, fileID)
	if err != nil {
		return fmt.Errorf("failed to get reconstruction: %w", err)
	}

	// 4. Calculate total size for progress reporting
	var totalSize int64
	for _, term := range recon.Terms {
		totalSize += term.UnpackedLength
	}

	// 5. Create destination file
	destFile, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer destFile.Close()

	// 6. Wrap with progress writer
	pw := &progressWriter{
		w:          destFile,
		total:      totalSize,
		onProgress: cb,
		lastUpdate: time.Now(),
	}

	// 7. Create xet downloader and stream to file
	downloader := xet.NewDownloader(s.xetClient)

	opts := xet.DownloadOptions{
		Namespace:  namespace,
		Repo:       repoName,
		Branch:     branch,
		FilePath:   file,
		FileID:     fileID,
		OutputPath: "", // We handle writing ourselves
	}

	// Use DownloadToWriter for streaming
	if err := downloader.DownloadToWriter(ctx, opts, pw); err != nil {
		return fmt.Errorf("failed to download via xet: %w", err)
	}

	// 8. Sync to ensure data is on disk
	if err := destFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync file: %w", err)
	}

	// Final progress report
	if cb != nil {
		cb(totalSize, totalSize)
	}

	return nil
}

// DownloadLFSWithProgress downloads a file via LFS with a reader that reports progress.
// This is useful when you need more control over the download process.
func (s *StreamDownloader) DownloadLFSWithProgress(ctx context.Context, repo, file string, destPath string, cb ProgressCallback) error {
	return s.DownloadLFS(ctx, repo, file, destPath, cb)
}

// progressWriter wraps an io.Writer and calls callback on each write.
// Progress updates are throttled to at most once per second to avoid overhead.
type progressWriter struct {
	w          io.Writer
	total      int64
	written    int64
	lastUpdate time.Time
	onProgress ProgressCallback
}

// Write implements io.Writer. It writes data to the underlying writer
// and reports progress periodically.
func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.w.Write(p)
	pw.written += int64(n)

	// Throttle progress updates to max 1 per second
	if time.Since(pw.lastUpdate) > time.Second {
		if pw.onProgress != nil {
			pw.onProgress(pw.written, pw.total)
		}
		pw.lastUpdate = time.Now()
	}

	return n, err
}

// parseRepo parses a repository ID into namespace and name.
// Repository IDs are typically in the format "namespace/repo-name".
func parseRepo(repoID string) (namespace, name string, err error) {
	parts := strings.SplitN(repoID, "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid repository ID format: %s (expected namespace/name)", repoID)
	}
	return parts[0], parts[1], nil
}
