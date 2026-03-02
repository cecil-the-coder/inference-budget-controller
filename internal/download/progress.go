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
	"sync"
	"sync/atomic"
	"time"
)

// ProgressTracker tracks overall and per-file download progress.
type ProgressTracker struct {
	totalBytes     atomic.Int64
	completedBytes atomic.Int64
	files          map[string]*FileProgress
	mu             sync.RWMutex
	startTime      time.Time
	lastUpdate     atomic.Int64 // Unix nano
	speeds         []int64      // Rolling window of speeds for ETA
	speedsMu       sync.Mutex
}

// FileProgress represents the progress of a single file download.
type FileProgress struct {
	Path        string
	Total       int64
	Completed   int64
	Phase       DownloadPhase
	Speed       int64 // bytes/sec
	StartTime   time.Time
	CompletedAt *time.Time
}

// ProgressSnapshot is an immutable snapshot of download progress.
type ProgressSnapshot struct {
	BytesTotal     int64
	BytesCompleted int64
	Percentage     float64
	Speed          int64 // bytes/sec
	ETA            time.Duration
	Files          []FileProgressSnapshot
	Elapsed        time.Duration
}

// FileProgressSnapshot is an immutable snapshot of file download progress.
type FileProgressSnapshot struct {
	Path       string
	Total      int64
	Completed  int64
	Percentage float64
	Phase      DownloadPhase
}

const (
	// speedWindowSize is the number of speed samples to keep for rolling average.
	speedWindowSize = 10
	// speedSampleInterval is the minimum interval between speed samples.
	speedSampleInterval = 500 * time.Millisecond
)

// NewProgressTracker creates a new ProgressTracker.
func NewProgressTracker() *ProgressTracker {
	return &ProgressTracker{
		files:  make(map[string]*FileProgress),
		speeds: make([]int64, 0, speedWindowSize),
	}
}

// Start initializes the progress tracker and sets the start time.
func (p *ProgressTracker) Start() {
	p.startTime = time.Now()
	p.lastUpdate.Store(time.Now().UnixNano())
}

// SetFileTotal sets the total size for a file.
func (p *ProgressTracker) SetFileTotal(path string, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	fp, exists := p.files[path]
	if !exists {
		fp = &FileProgress{
			Path:      path,
			StartTime: time.Now(),
		}
		p.files[path] = fp
	}

	fp.Total = total
	fp.Phase = PhasePending

	// Update total bytes
	p.totalBytes.Add(total)
}

// UpdateFileProgress updates the progress of a file download.
func (p *ProgressTracker) UpdateFileProgress(path string, completed int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	fp, exists := p.files[path]
	if !exists {
		fp = &FileProgress{
			Path:      path,
			StartTime: time.Now(),
		}
		p.files[path] = fp
	}

	// Calculate the delta for completed bytes
	delta := completed - fp.Completed
	fp.Completed = completed
	fp.Phase = PhaseDownloading

	// Update completed bytes
	p.completedBytes.Add(delta)

	// Record speed sample
	p.recordSpeedSample()
}

// CompleteFile marks a file as completed.
func (p *ProgressTracker) CompleteFile(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	fp, exists := p.files[path]
	if !exists {
		return
	}

	// Update completed bytes to match total if needed
	if fp.Completed < fp.Total {
		delta := fp.Total - fp.Completed
		p.completedBytes.Add(delta)
		fp.Completed = fp.Total
	}

	fp.Phase = PhaseComplete
	now := time.Now()
	fp.CompletedAt = &now
}

// FailFile marks a file as failed.
func (p *ProgressTracker) FailFile(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	fp, exists := p.files[path]
	if !exists {
		return
	}

	fp.Phase = PhaseFailed
}

// Snapshot returns an immutable snapshot of the current progress.
func (p *ProgressTracker) Snapshot() ProgressSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()

	total := p.totalBytes.Load()
	completed := p.completedBytes.Load()

	var percentage float64
	if total > 0 {
		percentage = float64(completed) / float64(total) * 100
	}

	elapsed := time.Duration(0)
	if !p.startTime.IsZero() {
		elapsed = time.Since(p.startTime)
	}

	files := make([]FileProgressSnapshot, 0, len(p.files))
	for _, fp := range p.files {
		var filePercentage float64
		if fp.Total > 0 {
			filePercentage = float64(fp.Completed) / float64(fp.Total) * 100
		}
		files = append(files, FileProgressSnapshot{
			Path:       fp.Path,
			Total:      fp.Total,
			Completed:  fp.Completed,
			Percentage: filePercentage,
			Phase:      fp.Phase,
		})
	}

	return ProgressSnapshot{
		BytesTotal:     total,
		BytesCompleted: completed,
		Percentage:     percentage,
		Speed:          p.calculateSpeed(),
		ETA:            p.calculateETA(),
		Files:          files,
		Elapsed:        elapsed,
	}
}

// recordSpeedSample records a speed sample for rolling average calculation.
func (p *ProgressTracker) recordSpeedSample() {
	now := time.Now()
	lastUpdate := time.Unix(0, p.lastUpdate.Load())

	// Only sample at specified intervals
	if now.Sub(lastUpdate) < speedSampleInterval {
		return
	}

	p.lastUpdate.Store(now.UnixNano())

	// Calculate instantaneous speed
	elapsed := now.Sub(p.startTime)
	if elapsed <= 0 {
		return
	}

	completed := p.completedBytes.Load()
	speed := int64(float64(completed) / elapsed.Seconds())

	p.speedsMu.Lock()
	defer p.speedsMu.Unlock()

	p.speeds = append(p.speeds, speed)
	if len(p.speeds) > speedWindowSize {
		p.speeds = p.speeds[1:]
	}
}

// calculateSpeed computes download speed using rolling average.
func (p *ProgressTracker) calculateSpeed() int64 {
	p.speedsMu.Lock()
	defer p.speedsMu.Unlock()

	if len(p.speeds) == 0 {
		// Calculate from total progress if no samples yet
		elapsed := time.Since(p.startTime)
		if elapsed <= 0 {
			return 0
		}
		return int64(float64(p.completedBytes.Load()) / elapsed.Seconds())
	}

	// Calculate rolling average
	var sum int64
	for _, s := range p.speeds {
		sum += s
	}
	return sum / int64(len(p.speeds))
}

// calculateETA estimates time remaining.
func (p *ProgressTracker) calculateETA() time.Duration {
	speed := p.calculateSpeed()
	if speed <= 0 {
		return 0
	}

	remaining := p.totalBytes.Load() - p.completedBytes.Load()
	if remaining <= 0 {
		return 0
	}

	etaSeconds := float64(remaining) / float64(speed)
	return time.Duration(etaSeconds * float64(time.Second))
}

// GetFileCount returns the number of files being tracked.
func (p *ProgressTracker) GetFileCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.files)
}

// GetCompletedFileCount returns the number of completed files.
func (p *ProgressTracker) GetCompletedFileCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var count int
	for _, fp := range p.files {
		if fp.Phase == PhaseComplete {
			count++
		}
	}
	return count
}

// Reset clears all progress data.
func (p *ProgressTracker) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.totalBytes.Store(0)
	p.completedBytes.Store(0)
	p.files = make(map[string]*FileProgress)
	p.startTime = time.Time{}
	p.lastUpdate.Store(0)

	p.speedsMu.Lock()
	p.speeds = make([]int64, 0, speedWindowSize)
	p.speedsMu.Unlock()
}
