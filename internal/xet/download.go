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
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Downloader handles downloading files from Xet storage.
type Downloader struct {
	client       *Client
	decompressor *Decompressor
	concurrency  int
}

// DownloaderOption is a functional option for configuring the Downloader.
type DownloaderOption func(*Downloader)

// WithConcurrency sets the number of concurrent downloads.
func WithConcurrency(n int) DownloaderOption {
	return func(d *Downloader) {
		if n > 0 {
			d.concurrency = n
		}
	}
}

// NewDownloader creates a new Xet downloader.
func NewDownloader(client *Client, opts ...DownloaderOption) *Downloader {
	d := &Downloader{
		client:       client,
		decompressor: NewDecompressor(),
		concurrency:  4, // Default concurrency
	}

	for _, opt := range opts {
		opt(d)
	}

	return d
}

// DownloadOptions contains options for a single file download.
type DownloadOptions struct {
	// Namespace is the HuggingFace namespace (organization/user).
	Namespace string
	// Repo is the repository name.
	Repo string
	// Branch is the git branch (default: "main").
	Branch string
	// FilePath is the path to the file in the repository.
	FilePath string
	// OutputPath is the local path to save the file.
	OutputPath string
	// FileID is an optional pre-resolved file ID.
	FileID string
}

// Download downloads a file from Xet storage.
func (d *Downloader) Download(ctx context.Context, opts DownloadOptions) error {
	// Resolve file ID if not provided
	fileID := opts.FileID
	if fileID == "" {
		var err error
		fileID, err = d.client.ResolveFileID(ctx, opts.Namespace, opts.Repo, opts.Branch, opts.FilePath)
		if err != nil {
			return fmt.Errorf("failed to resolve file ID: %w", err)
		}
	}

	// Get reconstruction metadata
	recon, err := d.client.GetReconstruction(ctx, fileID)
	if err != nil {
		return fmt.Errorf("failed to get reconstruction: %w", err)
	}

	// Reconstruct the file
	data, err := d.reconstruct(ctx, recon)
	if err != nil {
		return fmt.Errorf("failed to reconstruct file: %w", err)
	}

	// Ensure output directory exists
	if opts.OutputPath != "" {
		dir := filepath.Dir(opts.OutputPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create output directory: %w", err)
		}

		// Write to file
		if err := os.WriteFile(opts.OutputPath, data, 0644); err != nil {
			return fmt.Errorf("failed to write output file: %w", err)
		}
	}

	return nil
}

// DownloadToWriter downloads a file and writes it to the provided writer.
func (d *Downloader) DownloadToWriter(ctx context.Context, opts DownloadOptions, w io.Writer) error {
	// Resolve file ID if not provided
	fileID := opts.FileID
	if fileID == "" {
		var err error
		fileID, err = d.client.ResolveFileID(ctx, opts.Namespace, opts.Repo, opts.Branch, opts.FilePath)
		if err != nil {
			return fmt.Errorf("failed to resolve file ID: %w", err)
		}
	}

	// Get reconstruction metadata
	recon, err := d.client.GetReconstruction(ctx, fileID)
	if err != nil {
		return fmt.Errorf("failed to get reconstruction: %w", err)
	}

	// Reconstruct and write chunks
	return d.reconstructToWriter(ctx, recon, w)
}

// DownloadData downloads a file and returns its contents.
func (d *Downloader) DownloadData(ctx context.Context, opts DownloadOptions) ([]byte, error) {
	// Resolve file ID if not provided
	fileID := opts.FileID
	if fileID == "" {
		var err error
		fileID, err = d.client.ResolveFileID(ctx, opts.Namespace, opts.Repo, opts.Branch, opts.FilePath)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve file ID: %w", err)
		}
	}

	// Get reconstruction metadata
	recon, err := d.client.GetReconstruction(ctx, fileID)
	if err != nil {
		return nil, fmt.Errorf("failed to get reconstruction: %w", err)
	}

	// Reconstruct the file
	return d.reconstruct(ctx, recon)
}

// reconstruct reconstructs a file from reconstruction metadata.
func (d *Downloader) reconstruct(ctx context.Context, recon *ReconstructionResponse) ([]byte, error) {
	// Calculate total size
	var totalSize int64
	for _, term := range recon.Terms {
		totalSize += term.UnpackedLength
	}

	// Allocate buffer for the entire file
	result := make([]byte, 0, totalSize)

	// Fetch and process each xorb
	for _, term := range recon.Terms {
		fetchInfos, ok := recon.FetchInfo[term.Hash]
		if !ok {
			return nil, fmt.Errorf("no fetch info for hash %s", term.Hash)
		}

		// Fetch xorb data
		xorbData, err := d.fetchXorbData(ctx, fetchInfos)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch xorb %s: %w", term.Hash, err)
		}

		// Extract chunks from the xorb range
		chunkData, err := d.extractChunks(xorbData, term.Range)
		if err != nil {
			return nil, fmt.Errorf("failed to extract chunks: %w", err)
		}

		result = append(result, chunkData...)
	}

	return result, nil
}

// reconstructToWriter reconstructs a file and writes it to a writer.
func (d *Downloader) reconstructToWriter(ctx context.Context, recon *ReconstructionResponse, w io.Writer) error {
	for _, term := range recon.Terms {
		fetchInfos, ok := recon.FetchInfo[term.Hash]
		if !ok {
			return fmt.Errorf("no fetch info for hash %s", term.Hash)
		}

		// Fetch xorb data
		xorbData, err := d.fetchXorbData(ctx, fetchInfos)
		if err != nil {
			return fmt.Errorf("failed to fetch xorb %s: %w", term.Hash, err)
		}

		// Extract and decompress chunks
		if err := d.extractChunksToWriter(xorbData, term.Range, w); err != nil {
			return fmt.Errorf("failed to extract chunks: %w", err)
		}
	}

	return nil
}

// fetchXorbData fetches xorb data from multiple fetch infos.
func (d *Downloader) fetchXorbData(ctx context.Context, fetchInfos []FetchInfo) ([]byte, error) {
	if len(fetchInfos) == 0 {
		return nil, fmt.Errorf("no fetch info provided")
	}

	// For simplicity, fetch from the first URL
	// In a production implementation, we might want to parallelize this
	// and handle multiple ranges
	return d.client.FetchXorbFromFetchInfo(ctx, fetchInfos[0])
}

// extractChunks extracts and decompresses chunks from xorb data.
func (d *Downloader) extractChunks(xorbData []byte, r ByteRange) ([]byte, error) {
	// Extract the relevant range
	rangeData := xorbData[r.Start:r.End]

	// Iterate over chunks and decompress
	var result []byte
	iter := NewXorbIterator(rangeData)

	for iter.HasNext() {
		header, compressedData, err := iter.Next()
		if err != nil {
			return nil, err
		}
		if header == nil {
			break
		}

		// Decompress the chunk
		decompressed, err := d.decompressor.Decompress(header, compressedData)
		if err != nil {
			return nil, fmt.Errorf("failed to decompress chunk: %w", err)
		}

		result = append(result, decompressed...)
	}

	return result, nil
}

// extractChunksToWriter extracts and decompresses chunks, writing to the output.
func (d *Downloader) extractChunksToWriter(xorbData []byte, r ByteRange, w io.Writer) error {
	// Extract the relevant range
	rangeData := xorbData[r.Start:r.End]

	// Iterate over chunks and decompress
	iter := NewXorbIterator(rangeData)

	for iter.HasNext() {
		header, compressedData, err := iter.Next()
		if err != nil {
			return err
		}
		if header == nil {
			break
		}

		// Decompress the chunk
		decompressed, err := d.decompressor.Decompress(header, compressedData)
		if err != nil {
			return fmt.Errorf("failed to decompress chunk: %w", err)
		}

		// Write to output
		if _, err := w.Write(decompressed); err != nil {
			return fmt.Errorf("failed to write chunk: %w", err)
		}
	}

	return nil
}

// DownloadMultiple downloads multiple files concurrently.
func (d *Downloader) DownloadMultiple(ctx context.Context, files []DownloadOptions) error {
	var wg sync.WaitGroup
	errChan := make(chan error, len(files))
	semaphore := make(chan struct{}, d.concurrency)

	for _, file := range files {
		wg.Add(1)
		go func(f DownloadOptions) {
			defer wg.Done()

			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			if err := d.Download(ctx, f); err != nil {
				errChan <- fmt.Errorf("failed to download %s: %w", f.FilePath, err)
			}
		}(file)
	}

	wg.Wait()
	close(errChan)

	// Collect any errors
	var errors []error
	for err := range errChan {
		errors = append(errors, err)
	}

	if len(errors) > 0 {
		return fmt.Errorf("download errors: %v", errors)
	}

	return nil
}

// GetFileSize returns the size of a file without downloading it.
func (d *Downloader) GetFileSize(ctx context.Context, opts DownloadOptions) (int64, error) {
	// Resolve file ID if not provided
	fileID := opts.FileID
	if fileID == "" {
		var err error
		fileID, err = d.client.ResolveFileID(ctx, opts.Namespace, opts.Repo, opts.Branch, opts.FilePath)
		if err != nil {
			return 0, fmt.Errorf("failed to resolve file ID: %w", err)
		}
	}

	// Get reconstruction metadata
	recon, err := d.client.GetReconstruction(ctx, fileID)
	if err != nil {
		return 0, fmt.Errorf("failed to get reconstruction: %w", err)
	}

	// Calculate total size
	var totalSize int64
	for _, term := range recon.Terms {
		totalSize += term.UnpackedLength
	}

	return totalSize, nil
}
