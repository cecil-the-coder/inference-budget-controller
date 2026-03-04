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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cecil-the-coder/inference-budget-controller/internal/huggingface"
	"github.com/cecil-the-coder/inference-budget-controller/internal/xet"
)

// DefaultMaxConcurrent is the default maximum number of concurrent downloads.
const DefaultMaxConcurrent = 3

// Manager handles concurrent model downloads with proper resource management.
type Manager struct {
	client            client.Client
	hfClient          *huggingface.Client
	xetDownloader     *xet.Downloader
	chunkedDownloader *chunkedDownloader
	cacheDir          string
	maxConcurrent     int
	semaphore         chan struct{}
	downloads         sync.Map // modelName -> *Status
	bufferPool        *BufferPool
}

// Option is a functional option for configuring the Manager.
type Option func(*Manager)

// WithMaxConcurrent sets the maximum number of concurrent downloads.
func WithMaxConcurrent(n int) Option {
	return func(m *Manager) {
		if n > 0 {
			m.maxConcurrent = n
		}
	}
}

// WithCacheDir sets the cache directory for downloaded files.
func WithCacheDir(dir string) Option {
	return func(m *Manager) {
		m.cacheDir = dir
	}
}

// WithHFToken sets the HuggingFace API token.
func WithHFToken(token string) Option {
	return func(m *Manager) {
		if m.hfClient != nil {
			m.hfClient.SetToken(token)
		}
	}
}

// WithBufferPoolSize sets the buffer pool size for downloads.
func WithBufferPoolSize(size int) Option {
	return func(m *Manager) {
		m.bufferPool = NewBufferPool(size)
	}
}

// WithClient sets the Kubernetes client.
func WithClient(c client.Client) Option {
	return func(m *Manager) {
		m.client = c
	}
}

// WithHFClient sets the HuggingFace client.
func WithHFClient(c *huggingface.Client) Option {
	return func(m *Manager) {
		m.hfClient = c
	}
}

// WithXetDownloader sets the Xet downloader.
func WithXetDownloader(d *xet.Downloader) Option {
	return func(m *Manager) {
		m.xetDownloader = d
	}
}

// NewManager creates a new download Manager with the given options.
func NewManager(opts ...Option) *Manager {
	m := &Manager{
		maxConcurrent: DefaultMaxConcurrent,
		cacheDir:      os.Getenv("HF_HOME"),
		bufferPool:    NewBufferPool(DefaultBufferSize),
	}

	for _, opt := range opts {
		opt(m)
	}

	// Initialize semaphore for concurrency control
	m.semaphore = make(chan struct{}, m.maxConcurrent)

	// Initialize HuggingFace client if not provided
	if m.hfClient == nil {
		hfConfig := huggingface.Config{}
		if m.cacheDir != "" {
			hfConfig.CacheDir = m.cacheDir
		}
		m.hfClient = huggingface.NewClient(hfConfig)
	}

	// Initialize Xet downloader if not provided
	if m.xetDownloader == nil {
		xetClient := xet.NewClient()
		m.xetDownloader = xet.NewDownloader(xetClient)
	}

	// Initialize chunked downloader
	m.chunkedDownloader = newChunkedDownloader(m.hfClient, m.bufferPool)

	return m
}

// Download starts downloading a model with the given specification.
// This method is non-blocking - it starts the download in a goroutine and returns immediately.
// Use GetStatus to check the download progress.
func (m *Manager) Download(ctx context.Context, modelName string, spec *DownloadSpec) error {
	fmt.Printf("[download:%s] Download() called, repo=%s\n", modelName, spec.Repo)

	// Create download status
	status := &Status{
		ModelName: modelName,
		Phase:     PhasePending,
		StartedAt: time.Now(),
	}

	// Atomically store only if no entry exists — prevents duplicate downloads
	if existing, loaded := m.downloads.LoadOrStore(modelName, status); loaded {
		existingStatus := existing.(*Status)
		fmt.Printf("[download:%s] Download already tracked (phase=%s)\n", modelName, existingStatus.GetPhase())
		return fmt.Errorf("download already tracked for model: %s (phase=%s)", modelName, existingStatus.GetPhase())
	}

	// Create cancellable context
	dlCtx, cancel := context.WithCancel(context.Background())
	status.SetCancelFunc(cancel)

	fmt.Printf("[download:%s] Status stored, starting goroutine\n", modelName)

	// Start download in background
	go m.performDownload(dlCtx, modelName, spec, status)

	return nil
}

// performDownload executes the actual download in a goroutine.
func (m *Manager) performDownload(ctx context.Context, modelName string, spec *DownloadSpec, status *Status) {
	logf := func(format string, args ...interface{}) {
		fmt.Printf("[download:%s] %s\n", modelName, fmt.Sprintf(format, args...))
	}

	logf("Waiting for semaphore slot...")
	// Acquire semaphore slot
	m.semaphore <- struct{}{}
	defer func() { <-m.semaphore }()
	logf("Semaphore slot acquired, starting download")

	// Check for cancellation
	select {
	case <-ctx.Done():
		logf("Download cancelled while waiting for semaphore")
		status.SetError("download cancelled")
		return
	default:
	}

	// Update status to downloading
	status.SetPhase(PhaseDownloading)
	logf("Status set to Downloading")

	// Determine destination directory
	destDir := spec.DestDir
	if destDir == "" {
		destDir = filepath.Join(m.cacheDir, "models", strings.ReplaceAll(modelName, "/", "--"))
	}
	logf("Destination directory: %s", destDir)

	// Create destination directory
	if err := os.MkdirAll(destDir, 0755); err != nil {
		logf("ERROR: failed to create destination directory: %v", err)
		status.SetError(fmt.Sprintf("failed to create destination directory: %v", err))
		return
	}

	// Create repository handle
	logf("Creating repository handle for: %s", spec.Repo)
	repo, err := m.hfClient.NewModelRepository(ctx, spec.Repo)
	if err != nil {
		logf("ERROR: failed to create repository handle: %v", err)
		status.SetError(fmt.Sprintf("failed to create repository handle: %v", err))
		return
	}

	// Get list of files to download
	logf("Resolving files to download...")
	filesToDownload, err := m.resolveFiles(ctx, repo, spec)
	if err != nil {
		logf("ERROR: failed to resolve files: %v", err)
		status.SetError(fmt.Sprintf("failed to resolve files: %v", err))
		return
	}

	if len(filesToDownload) == 0 {
		logf("ERROR: no files to download")
		status.SetError("no files to download")
		return
	}
	logf("Files to download: %v", filesToDownload)

	// Initialize file statuses
	fileSizes := make(map[string]int64)
	for _, file := range filesToDownload {
		status.UpdateFileProgress(file, 0, 0, PhasePending)
		fileSizes[file] = 0 // Will be updated during download
	}
	status.SetProgress(0, int64(len(filesToDownload)))

	// Download files
	var completedFiles int64
	var downloadedBytes int64
	var totalBytes int64
	startTime := time.Now()

	for i, filePath := range filesToDownload {
		select {
		case <-ctx.Done():
			logf("Download cancelled during file %s", filePath)
			status.SetError("download cancelled")
			return
		default:
		}

		logf("Downloading file %d/%d: %s", i+1, len(filesToDownload), filePath)
		// Update file status to downloading
		status.UpdateFileProgress(filePath, 0, 0, PhaseDownloading)

		// Download the file
		localPath := filepath.Join(destDir, filePath)
		fileDir := filepath.Dir(localPath)
		if err := os.MkdirAll(fileDir, 0755); err != nil {
			logf("ERROR: failed to create directory for %s: %v", filePath, err)
			status.SetError(fmt.Sprintf("failed to create directory for %s: %v", filePath, err))
			return
		}

		// Start progress monitoring goroutine
		progressCtx, progressCancel := context.WithCancel(context.Background())
		progressDone := make(chan struct{})
		go func() {
			defer close(progressDone)
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			lastSize := int64(0)
			lastTime := time.Now()

			for {
				select {
				case <-progressCtx.Done():
					return
				case <-ticker.C:
					info, err := os.Stat(localPath)
					if err != nil {
						continue // File doesn't exist yet
					}
					currentSize := info.Size()
					now := time.Now()
					elapsed := now.Sub(lastTime).Seconds()
					if elapsed > 0 {
						rate := float64(currentSize-lastSize) / elapsed / 1024 / 1024 // MB/s
						elapsed_total := now.Sub(startTime).Seconds()
						overallRate := float64(currentSize) / elapsed_total / 1024 / 1024 // MB/s
						logf("Progress: %s - %.1f MB (%.1f MB/s current, %.1f MB/s overall)",
							filePath, float64(currentSize)/1024/1024, rate, overallRate)
					}
					lastSize = currentSize
					lastTime = now
				}
			}
		}()

		// Create progress callback for chunked downloads
		onProgress := func(bytesDone int64) {
			status.UpdateFileProgress(filePath, bytesDone, 0, PhaseDownloading)
			logf("Progress: %s - %.1f MB downloaded", filePath, float64(bytesDone)/1024/1024)
		}

		// Try Xet download first, fall back to regular download
		downloadedSize, err := m.downloadFile(ctx, spec, filePath, localPath, onProgress)

		// Stop progress monitoring
		progressCancel()
		<-progressDone

		if err != nil {
			logf("ERROR: failed to download %s: %v", filePath, err)
			status.SetError(fmt.Sprintf("failed to download %s: %v", filePath, err))
			return
		}

		// Update progress
		fileSizes[filePath] = downloadedSize
		downloadedBytes += downloadedSize
		totalBytes += downloadedSize
		completedFiles++

		// Calculate overall stats
		overallDuration := time.Since(startTime)
		overallRate := float64(downloadedBytes) / overallDuration.Seconds() / 1024 / 1024 // MB/s
		logf("Completed file %d/%d: %s (%.1f MB in %.1fs, %.1f MB/s overall)",
			i+1, len(filesToDownload), filePath, float64(downloadedSize)/1024/1024,
			overallDuration.Seconds(), overallRate)
		status.UpdateFileProgress(filePath, downloadedSize, downloadedSize, PhaseComplete)
		status.SetProgress(downloadedBytes, totalBytes+int64(len(filesToDownload)-int(completedFiles))*1024*1024) // Estimate remaining
	}

	// Fetch LFS metadata for SHA256 checksums
	manifest := &FileManifest{Files: fileSizes}
	logf("Fetching file metadata for SHA256 verification...")
	fileMetadata, err := m.hfClient.GetFileMetadata(ctx, spec.Repo)
	if err != nil {
		logf("WARNING: failed to fetch file metadata (SHA256 verification skipped): %v", err)
	} else {
		checksums := make(map[string]string)
		metaByName := make(map[string]string)
		for _, fi := range fileMetadata {
			if fi.SHA256 != "" {
				metaByName[fi.Name] = fi.SHA256
			}
		}
		for _, filePath := range filesToDownload {
			if hash, ok := metaByName[filePath]; ok {
				checksums[filePath] = hash
			}
		}
		if len(checksums) > 0 {
			manifest.Checksums = checksums
			// Verify downloaded files against expected checksums
			logf("Verifying SHA256 checksums for %d files...", len(checksums))
			for filePath, expectedHash := range checksums {
				localPath := filepath.Join(destDir, filePath)
				if err := VerifyChecksum(localPath, expectedHash); err != nil {
					logf("ERROR: SHA256 verification failed for %s: %v", filePath, err)
					status.SetError(fmt.Sprintf("SHA256 verification failed for %s: %v", filePath, err))
					return
				}
			}
			logf("SHA256 verification passed for all files")
		}
	}

	// Write manifest for verification before marking complete
	if err := WriteManifest(destDir, manifest); err != nil {
		logf("ERROR: failed to write manifest: %v", err)
		status.SetError(fmt.Sprintf("failed to write manifest: %v", err))
		return
	}

	// Mark as complete
	status.SetProgress(totalBytes, totalBytes)
	status.SetComplete()
	logf("Download complete! Total bytes: %d", totalBytes)
}

// resolveFiles determines which files to download based on the spec.
func (m *Manager) resolveFiles(ctx context.Context, repo *huggingface.Repository, spec *DownloadSpec) ([]string, error) {
	// List all files in the repository
	allFiles, err := repo.ListFiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list repository files: %w", err)
	}

	fmt.Printf("[download] resolveFiles: spec.Files=%v, spec.Patterns=%v\n", spec.Files, spec.Patterns)
	fmt.Printf("[download] resolveFiles: found %d files in repo\n", len(allFiles))

	var filesToDownload []string
	seen := make(map[string]bool) // Track which files we've already added

	for _, file := range allFiles {
		// Check if file should be excluded
		if m.shouldExclude(file.Name, spec.Exclude) {
			continue
		}

		// Check if file matches any explicit file list
		matched := false
		for _, f := range spec.Files {
			if file.Name == f {
				if !seen[file.Name] {
					filesToDownload = append(filesToDownload, file.Name)
					seen[file.Name] = true
				}
				matched = true
				break
			}
		}
		if matched {
			continue
		}

		// Check if file matches any pattern
		for _, pattern := range spec.Patterns {
			matched, err := filepath.Match(pattern, file.Name)
			if err != nil {
				return nil, fmt.Errorf("invalid pattern %q: %w", pattern, err)
			}
			if matched {
				fmt.Printf("[download] resolveFiles: pattern %q matched file %q\n", pattern, file.Name)
				if !seen[file.Name] {
					filesToDownload = append(filesToDownload, file.Name)
					seen[file.Name] = true
				}
				break
			}
		}
	}

	// If no files or patterns specified, download all files
	if len(spec.Files) == 0 && len(spec.Patterns) == 0 {
		for _, file := range allFiles {
			if !m.shouldExclude(file.Name, spec.Exclude) {
				filesToDownload = append(filesToDownload, file.Name)
			}
		}
	}

	return filesToDownload, nil
}

// shouldExclude checks if a file should be excluded based on exclude patterns.
func (m *Manager) shouldExclude(filePath string, excludePatterns []string) bool {
	for _, pattern := range excludePatterns {
		matched, err := filepath.Match(pattern, filePath)
		if err == nil && matched {
			return true
		}
		// Also check if the pattern matches any part of the path
		parts := strings.Split(filePath, "/")
		for _, part := range parts {
			matched, err := filepath.Match(pattern, part)
			if err == nil && matched {
				return true
			}
		}
	}
	return false
}

// downloadFile downloads a single file, trying Xet first, then resume, then falling back to regular download.
func (m *Manager) downloadFile(ctx context.Context, spec *DownloadSpec, filePath, localPath string, onProgress ProgressFunc) (int64, error) {
	// Parse namespace and repo name from the full repo path
	parts := strings.SplitN(spec.Repo, "/", 2)
	namespace := parts[0]
	repoName := parts[0]
	if len(parts) > 1 {
		repoName = parts[1]
	}
	branch := spec.Branch
	if branch == "" {
		branch = "main"
	}

	// Try Xet download first
	dlOpts := xet.DownloadOptions{
		Namespace:  namespace,
		Repo:       repoName,
		Branch:     branch,
		FilePath:   filePath,
		OutputPath: localPath,
	}

	err := m.xetDownloader.Download(ctx, dlOpts)
	if err == nil {
		// Get file size
		info, statErr := os.Stat(localPath)
		if statErr != nil {
			return 0, fmt.Errorf("failed to stat downloaded file: %w", statErr)
		}
		return info.Size(), nil
	}

	// Try chunked multipart download for large files with Range support
	size, chunkedErr := m.chunkedDownloader.Download(ctx, spec.Repo, filePath, localPath, onProgress)
	if chunkedErr == nil {
		return size, nil
	}
	if !errors.Is(chunkedErr, ErrRangeNotSupported) {
		fmt.Printf("[download] Chunked download failed for %s: %v, falling back\n", filePath, chunkedErr)
	}

	// Download directly via HTTP with Range support and stall detection.
	// This bypasses the HF library's cache (which downloads to its own dir first)
	// and writes directly to localPath, enabling progress monitoring and resume.
	size, dlErr := m.tryResumeDownload(ctx, spec.Repo, filePath, localPath)
	if dlErr == nil {
		return size, nil
	}

	// Resume failed — try a fresh direct download (offset=0)
	fmt.Printf("[download] Direct download for %s (resume failed: %v)\n", filePath, dlErr)
	f, fErr := os.Create(localPath)
	if fErr != nil {
		return 0, fmt.Errorf("failed to create file %s: %w", localPath, fErr)
	}

	written, dlErr := m.hfClient.ResumeDownloadFile(ctx, spec.Repo, filePath, 0, f)
	if syncErr := f.Sync(); syncErr != nil && dlErr == nil {
		dlErr = fmt.Errorf("failed to sync file: %w", syncErr)
	}
	_ = f.Close()

	if dlErr != nil {
		return 0, fmt.Errorf("direct download failed for %s: %w", filePath, dlErr)
	}

	return written, nil
}

// tryResumeDownload attempts to resume a partial download using HTTP Range headers.
// Returns the final file size on success, or an error if resume is not possible.
func (m *Manager) tryResumeDownload(ctx context.Context, repoID, filePath, localPath string) (int64, error) {
	info, err := os.Stat(localPath)
	if err != nil || info.Size() == 0 {
		return 0, fmt.Errorf("no partial file to resume")
	}

	localSize := info.Size()
	fmt.Printf("[download] Found partial file %s (%d bytes), attempting resume\n", filePath, localSize)

	// Open the file for appending
	f, err := os.OpenFile(localPath, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return 0, fmt.Errorf("failed to open file for resume: %w", err)
	}
	defer func() { _ = f.Close() }()

	written, err := m.hfClient.ResumeDownloadFile(ctx, repoID, filePath, localSize, f)
	if err != nil {
		return 0, fmt.Errorf("resume download failed: %w", err)
	}

	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("failed to sync resumed file: %w", err)
	}

	totalSize := localSize + written
	fmt.Printf("[download] Resume complete for %s: %d bytes appended, total %d bytes\n", filePath, written, totalSize)
	return totalSize, nil
}

// IsDownloading checks if a download is in progress for the given model.
func (m *Manager) IsDownloading(modelName string) bool {
	value, exists := m.downloads.Load(modelName)
	if !exists {
		return false
	}

	status := value.(*Status)
	return !status.IsComplete()
}

// GetStatus returns the download status for the given model.
// Returns nil if no download exists for the model.
func (m *Manager) GetStatus(modelName string) *Status {
	value, exists := m.downloads.Load(modelName)
	if !exists {
		return nil
	}
	return value.(*Status)
}

// Cancel cancels an ongoing download for the given model.
// If no download is in progress, this is a no-op.
func (m *Manager) Cancel(modelName string) {
	value, exists := m.downloads.Load(modelName)
	if !exists {
		return
	}

	status := value.(*Status)
	status.Cancel()
}

// RemoveStatus removes the download status for a completed download.
// This should only be called after the download is complete.
func (m *Manager) RemoveStatus(modelName string) {
	m.downloads.Delete(modelName)
}

// ListDownloads returns all current download statuses.
func (m *Manager) ListDownloads() map[string]*Status {
	result := make(map[string]*Status)
	m.downloads.Range(func(key, value interface{}) bool {
		modelName := key.(string)
		status := value.(*Status)
		result[modelName] = status
		return true
	})
	return result
}

// WaitForDownload waits for a download to complete.
// Returns the final status of the download.
func (m *Manager) WaitForDownload(ctx context.Context, modelName string) (*Status, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			status := m.GetStatus(modelName)
			if status == nil {
				return nil, fmt.Errorf("no download found for model: %s", modelName)
			}
			if status.IsComplete() {
				return status, nil
			}
		}
	}
}

// GetModelPath returns the local path where a model would be/is downloaded.
func (m *Manager) GetModelPath(modelName string) string {
	return filepath.Join(m.cacheDir, "models", strings.ReplaceAll(modelName, "/", "--"))
}

// GetBufferPool returns the buffer pool used by the manager.
func (m *Manager) GetBufferPool() *BufferPool {
	return m.bufferPool
}

// CleanupCompleted removes all completed download statuses.
func (m *Manager) CleanupCompleted() {
	m.downloads.Range(func(key, value interface{}) bool {
		status := value.(*Status)
		if status.IsComplete() {
			m.downloads.Delete(key)
		}
		return true
	})
}

// Note: The client.Client and types.NamespacedName are imported for potential
// future use with Kubernetes integration (e.g., updating custom resource status).
// Currently unused but kept for API compatibility.
var _ = client.Client(nil)
var _ = types.NamespacedName{}
