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
	"path/filepath"
	"sync"

	"github.com/cecil-the-coder/inference-budget-controller/internal/xet"
)

// MultiFileDownloader handles parallel downloads of multiple files from a HuggingFace repository.
// It uses a semaphore to limit concurrent downloads and supports both Xet and LFS storage backends.
type MultiFileDownloader struct {
	manager   *Manager
	resolver  *FileResolver
	detector  *Detector
	streamer  *StreamDownloader
	semaphore chan struct{}
}

// MultiFileDownloaderOption is a functional option for configuring a MultiFileDownloader.
type MultiFileDownloaderOption func(*MultiFileDownloader)

// WithMaxParallelDownloads sets the maximum number of parallel file downloads.
func WithMaxParallelDownloads(n int) MultiFileDownloaderOption {
	return func(m *MultiFileDownloader) {
		if n > 0 {
			m.semaphore = make(chan struct{}, n)
		}
	}
}

// NewMultiFileDownloader creates a new MultiFileDownloader with the given Manager.
// The Manager provides the necessary clients and configuration for downloads.
func NewMultiFileDownloader(manager *Manager, opts ...MultiFileDownloaderOption) *MultiFileDownloader {
	m := &MultiFileDownloader{
		manager:   manager,
		resolver:  NewFileResolver(manager.hfClient),
		detector:  NewDetector(),
		streamer:  NewStreamDownloader(manager.hfClient, xet.NewClient(), manager.bufferPool),
		semaphore: make(chan struct{}, DefaultMaxConcurrent),
	}

	for _, opt := range opts {
		opt(m)
	}

	return m
}

// DownloadAll downloads all files for a model spec in parallel.
// It resolves the file list from the spec, initializes status tracking,
// and downloads files concurrently within the semaphore limit.
func (m *MultiFileDownloader) DownloadAll(ctx context.Context, spec *DownloadSpec, status *Status) error {
	// 1. Resolve file list
	files, err := m.resolver.Resolve(ctx, spec.Repo, spec)
	if err != nil {
		return fmt.Errorf("failed to resolve files: %w", err)
	}

	if len(files) == 0 {
		return fmt.Errorf("no files to download")
	}

	// 2. Initialize status for each file
	m.initFileStatus(status, files)

	// 3. Download files in parallel (within semaphore limit)
	var wg sync.WaitGroup
	errChan := make(chan error, len(files))
	var hasError bool
	var firstError error
	var errMu sync.Mutex

	for _, file := range files {
		// Check for context cancellation before starting each download
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		wg.Add(1)
		go func(filename string) {
			defer wg.Done()

			// Acquire semaphore slot
			m.semaphore <- struct{}{}
			defer func() { <-m.semaphore }()

			// Check for context cancellation after acquiring semaphore
			select {
			case <-ctx.Done():
				status.UpdateFileProgress(filename, 0, 0, PhaseFailed)
				errChan <- ctx.Err()
				return
			default:
			}

			err := m.downloadSingleFile(ctx, spec, filename, status)
			if err != nil {
				errMu.Lock()
				if !hasError {
					hasError = true
					firstError = err
				}
				errMu.Unlock()
				errChan <- err
				return
			}
		}(file)
	}

	// Wait for all downloads to complete
	wg.Wait()
	close(errChan)

	// Collect any errors (we return the first one)
	for err := range errChan {
		if err != nil {
			// We already tracked the first error
			continue
		}
	}

	if hasError {
		return firstError
	}
	return nil
}

// initFileStatus initializes the status for each file to be downloaded.
func (m *MultiFileDownloader) initFileStatus(status *Status, files []string) {
	for _, file := range files {
		status.UpdateFileProgress(file, 0, 0, PhasePending)
	}
}

// downloadSingleFile downloads a single file and updates the status.
func (m *MultiFileDownloader) downloadSingleFile(ctx context.Context, spec *DownloadSpec, filename string, status *Status) error {
	destPath := filepath.Join(spec.DestDir, filename)

	// Detect storage type
	detection, err := m.detector.Detect(ctx, spec.Repo, spec.Branch, filename)
	if err != nil {
		status.UpdateFileProgress(filename, 0, 0, PhaseFailed)
		return fmt.Errorf("%s: detection failed: %w", filename, err)
	}

	// Update file status to downloading
	status.UpdateFileProgress(filename, 0, 0, PhaseDownloading)

	var dlErr error
	if detection.Type == StorageTypeXet {
		dlErr = m.streamer.DownloadXet(ctx, spec.Repo, spec.Branch, filename, destPath,
			func(done, total int64) {
				status.UpdateFileProgress(filename, done, total, PhaseDownloading)
			})
	} else {
		dlErr = m.streamer.DownloadLFS(ctx, spec.Repo, filename, destPath,
			func(done, total int64) {
				status.UpdateFileProgress(filename, done, total, PhaseDownloading)
			})
	}

	if dlErr != nil {
		status.UpdateFileProgress(filename, 0, 0, PhaseFailed)
		return fmt.Errorf("%s: %w", filename, dlErr)
	}

	status.UpdateFileProgress(filename, 0, 0, PhaseComplete)
	return nil
}

// DownloadFiles downloads a specific list of files in parallel.
// This is a convenience method when you already know which files to download.
func (m *MultiFileDownloader) DownloadFiles(ctx context.Context, repo, branch, destDir string, files []string, status *Status) error {
	spec := &DownloadSpec{
		Repo:    repo,
		Branch:  branch,
		Files:   files,
		DestDir: destDir,
	}
	return m.DownloadAll(ctx, spec, status)
}

// DownloadWithDetector downloads files using a custom detector for storage type detection.
// This is useful when you have pre-computed detection results.
func (m *MultiFileDownloader) DownloadWithDetector(ctx context.Context, spec *DownloadSpec, status *Status, detector *Detector) error {
	originalDetector := m.detector
	m.detector = detector
	defer func() { m.detector = originalDetector }()

	return m.DownloadAll(ctx, spec, status)
}

// GetResolver returns the file resolver used by this downloader.
func (m *MultiFileDownloader) GetResolver() *FileResolver {
	return m.resolver
}

// GetDetector returns the storage detector used by this downloader.
func (m *MultiFileDownloader) GetDetector() *Detector {
	return m.detector
}

// GetStreamer returns the stream downloader used by this downloader.
func (m *MultiFileDownloader) GetStreamer() *StreamDownloader {
	return m.streamer
}
