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
	"sync"
	"time"
)

// DownloadPhase represents the current phase of a download.
type DownloadPhase string

const (
	// PhasePending indicates the download is queued but not yet started.
	PhasePending DownloadPhase = "Pending"
	// PhaseDownloading indicates the download is in progress.
	PhaseDownloading DownloadPhase = "Downloading"
	// PhaseComplete indicates the download finished successfully.
	PhaseComplete DownloadPhase = "Complete"
	// PhaseFailed indicates the download failed with an error.
	PhaseFailed DownloadPhase = "Failed"
)

// Status represents the status of a model download.
type Status struct {
	// ModelName is the name/identifier of the model being downloaded.
	ModelName string

	// Phase is the current download phase.
	Phase DownloadPhase

	// Progress is the download progress as a percentage (0-100).
	Progress float64

	// BytesTotal is the total number of bytes to download.
	BytesTotal int64

	// BytesDone is the number of bytes already downloaded.
	BytesDone int64

	// Files contains the status of individual files being downloaded.
	Files []FileStatus

	// StartedAt is when the download started.
	StartedAt time.Time

	// CompletedAt is when the download finished (success or failure).
	CompletedAt *time.Time

	// Error contains the error message if the download failed.
	Error string

	// cancelFunc is used to cancel the download.
	cancelFunc context.CancelFunc

	mu sync.RWMutex
}

// FileStatus represents the status of a single file download.
type FileStatus struct {
	// Path is the file path within the repository.
	Path string

	// Phase is the current download phase for this file.
	Phase DownloadPhase

	// BytesDone is the number of bytes downloaded for this file.
	BytesDone int64

	// BytesTotal is the total size of this file.
	BytesTotal int64
}

// GetPhase returns the current download phase.
func (s *Status) GetPhase() DownloadPhase {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Phase
}

// GetProgress returns the current download progress as a percentage.
func (s *Status) GetProgress() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Progress
}

// GetBytesDone returns the number of bytes downloaded so far.
func (s *Status) GetBytesDone() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.BytesDone
}

// GetBytesTotal returns the total number of bytes to download.
func (s *Status) GetBytesTotal() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.BytesTotal
}

// GetError returns the error message if the download failed.
func (s *Status) GetError() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Error
}

// GetFiles returns a copy of the file statuses.
func (s *Status) GetFiles() []FileStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]FileStatus, len(s.Files))
	copy(result, s.Files)
	return result
}

// SetProgress updates the download progress.
func (s *Status) SetProgress(bytesDone, bytesTotal int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.BytesDone = bytesDone
	s.BytesTotal = bytesTotal

	if bytesTotal > 0 {
		s.Progress = float64(bytesDone) / float64(bytesTotal) * 100
	}
}

// SetPhase sets the download phase.
func (s *Status) SetPhase(phase DownloadPhase) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Phase = phase
}

// SetError sets the error message and phase to failed.
func (s *Status) SetError(err string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.Error = err
	s.Phase = PhaseFailed
	now := time.Now()
	s.CompletedAt = &now
}

// SetComplete marks the download as complete.
func (s *Status) SetComplete() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.Phase = PhaseComplete
	s.Progress = 100
	now := time.Now()
	s.CompletedAt = &now
}

// SetCancelFunc sets the cancel function for this download.
func (s *Status) SetCancelFunc(cancelFunc context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelFunc = cancelFunc
}

// Cancel cancels the download if it's in progress.
func (s *Status) Cancel() {
	s.mu.RLock()
	cancelFunc := s.cancelFunc
	s.mu.RUnlock()

	if cancelFunc != nil {
		cancelFunc()
	}
}

// UpdateFileProgress updates the progress of a specific file.
func (s *Status) UpdateFileProgress(path string, bytesDone, bytesTotal int64, phase DownloadPhase) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, f := range s.Files {
		if f.Path == path {
			s.Files[i].BytesDone = bytesDone
			s.Files[i].BytesTotal = bytesTotal
			s.Files[i].Phase = phase
			return
		}
	}

	// File not found, add it
	s.Files = append(s.Files, FileStatus{
		Path:       path,
		BytesDone:  bytesDone,
		BytesTotal: bytesTotal,
		Phase:      phase,
	})
}

// IsComplete returns true if the download is complete (success or failure).
func (s *Status) IsComplete() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Phase == PhaseComplete || s.Phase == PhaseFailed
}

// Duration returns the duration of the download.
// If the download is still in progress, it returns the time since start.
func (s *Status) Duration() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.CompletedAt != nil {
		return s.CompletedAt.Sub(s.StartedAt)
	}
	return time.Since(s.StartedAt)
}
