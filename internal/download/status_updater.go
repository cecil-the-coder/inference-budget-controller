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
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/cecil-the-coder/inference-budget-controller/api/v1alpha1"
)

// StatusUpdater updates InferenceModel CRD status.
type StatusUpdater struct {
	client      client.Client
	lastUpdate  map[string]time.Time
	minInterval time.Duration // Minimum time between status updates
	mu          sync.Mutex
}

// NewStatusUpdater creates a new StatusUpdater.
func NewStatusUpdater(client client.Client, minInterval time.Duration) *StatusUpdater {
	if minInterval <= 0 {
		minInterval = 5 * time.Second // Default to 5 seconds
	}
	return &StatusUpdater{
		client:      client,
		lastUpdate:  make(map[string]time.Time),
		minInterval: minInterval,
	}
}

// UpdateStatus updates the InferenceModel status with download progress.
// Throttled to avoid overwhelming the API server.
func (u *StatusUpdater) UpdateStatus(ctx context.Context, modelName string, namespace string, status *Status, snapshot ProgressSnapshot) error {
	// 1. Check throttle
	u.mu.Lock()
	last, exists := u.lastUpdate[modelName]
	if exists && time.Since(last) < u.minInterval {
		u.mu.Unlock()
		return nil // Skip update, too soon
	}
	u.lastUpdate[modelName] = time.Now()
	u.mu.Unlock()

	// 2. Fetch current model
	model := &inferencev1alpha1.InferenceModel{}
	if err := u.client.Get(ctx, types.NamespacedName{Name: modelName, Namespace: namespace}, model); err != nil {
		return fmt.Errorf("failed to get model: %w", err)
	}

	// 3. Update status fields
	model.Status.DownloadPhase = convertPhase(status.Phase)
	model.Status.DownloadProgress = int32(snapshot.Percentage)
	model.Status.DownloadBytesTotal = snapshot.BytesTotal
	model.Status.DownloadBytesDone = snapshot.BytesCompleted
	model.Status.DownloadSpeed = snapshot.Speed
	model.Status.DownloadETA = snapshot.ETA.String()
	model.Status.DownloadMessage = formatDownloadMessage(snapshot)

	// 4. Update file statuses
	model.Status.DownloadFiles = make([]inferencev1alpha1.FileDownloadStatus, len(snapshot.Files))
	for i, f := range snapshot.Files {
		model.Status.DownloadFiles[i] = inferencev1alpha1.FileDownloadStatus{
			Path:       f.Path,
			Phase:      string(f.Phase),
			BytesDone:  f.Completed,
			BytesTotal: f.Total,
		}
	}

	// 5. Update timestamps
	if !status.StartedAt.IsZero() {
		model.Status.DownloadStartedAt = &metav1.Time{Time: status.StartedAt}
	}
	if status.CompletedAt != nil {
		model.Status.DownloadCompletedAt = &metav1.Time{Time: *status.CompletedAt}
	}

	// 6. Update error
	if status.Error != "" {
		model.Status.DownloadError = status.Error
	}

	// 7. Patch status
	return u.client.Status().Update(ctx, model)
}

// UpdateStatusForce updates the InferenceModel status without throttle checking.
// Use this for important state transitions like completion or failure.
func (u *StatusUpdater) UpdateStatusForce(ctx context.Context, modelName string, namespace string, status *Status, snapshot ProgressSnapshot) error {
	// Update last update time
	u.mu.Lock()
	u.lastUpdate[modelName] = time.Now()
	u.mu.Unlock()

	// Fetch current model
	model := &inferencev1alpha1.InferenceModel{}
	if err := u.client.Get(ctx, types.NamespacedName{Name: modelName, Namespace: namespace}, model); err != nil {
		return fmt.Errorf("failed to get model: %w", err)
	}

	// Update status fields
	model.Status.DownloadPhase = convertPhase(status.Phase)
	model.Status.DownloadProgress = int32(snapshot.Percentage)
	model.Status.DownloadBytesTotal = snapshot.BytesTotal
	model.Status.DownloadBytesDone = snapshot.BytesCompleted
	model.Status.DownloadSpeed = snapshot.Speed
	model.Status.DownloadETA = snapshot.ETA.String()
	model.Status.DownloadMessage = formatDownloadMessage(snapshot)

	// Update file statuses
	model.Status.DownloadFiles = make([]inferencev1alpha1.FileDownloadStatus, len(snapshot.Files))
	for i, f := range snapshot.Files {
		model.Status.DownloadFiles[i] = inferencev1alpha1.FileDownloadStatus{
			Path:       f.Path,
			Phase:      string(f.Phase),
			BytesDone:  f.Completed,
			BytesTotal: f.Total,
		}
	}

	// Update timestamps
	if !status.StartedAt.IsZero() {
		model.Status.DownloadStartedAt = &metav1.Time{Time: status.StartedAt}
	}
	if status.CompletedAt != nil {
		model.Status.DownloadCompletedAt = &metav1.Time{Time: *status.CompletedAt}
	}

	// Update error
	if status.Error != "" {
		model.Status.DownloadError = status.Error
	}

	// Patch status
	return u.client.Status().Update(ctx, model)
}

// convertPhase converts download.DownloadPhase to inferencev1alpha1.DownloadPhase.
func convertPhase(p DownloadPhase) inferencev1alpha1.DownloadPhase {
	switch p {
	case PhasePending:
		return inferencev1alpha1.DownloadPhasePending
	case PhaseDownloading:
		return inferencev1alpha1.DownloadPhaseDownloading
	case PhaseComplete:
		return inferencev1alpha1.DownloadPhaseComplete
	case PhaseFailed:
		return inferencev1alpha1.DownloadPhaseFailed
	default:
		return inferencev1alpha1.DownloadPhasePending
	}
}

// formatDownloadMessage creates a human-readable download message.
func formatDownloadMessage(snapshot ProgressSnapshot) string {
	if snapshot.BytesTotal == 0 {
		return "Preparing download..."
	}

	completedMB := float64(snapshot.BytesCompleted) / 1024 / 1024
	totalMB := float64(snapshot.BytesTotal) / 1024 / 1024
	speedMBps := float64(snapshot.Speed) / 1024 / 1024

	if snapshot.Percentage >= 100 {
		return fmt.Sprintf("Download complete: %.1f MB", totalMB)
	}

	if snapshot.Speed > 0 {
		return fmt.Sprintf("Downloading: %.1f/%.1f MB (%.1f%%) at %.2f MB/s, ETA: %s",
			completedMB, totalMB, snapshot.Percentage, speedMBps, formatDuration(snapshot.ETA))
	}

	return fmt.Sprintf("Downloading: %.1f/%.1f MB (%.1f%%)",
		completedMB, totalMB, snapshot.Percentage)
}

// formatDuration formats a duration in a human-readable way.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "calculating..."
	}

	seconds := int(d.Seconds())
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}

	minutes := seconds / 60
	seconds = seconds % 60
	if minutes < 60 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}

	hours := minutes / 60
	minutes = minutes % 60
	return fmt.Sprintf("%dh %dm", hours, minutes)
}

// ClearThrottle clears the throttle for a specific model, allowing immediate update.
func (u *StatusUpdater) ClearThrottle(modelName string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.lastUpdate, modelName)
}

// SetMinInterval sets the minimum interval between status updates.
func (u *StatusUpdater) SetMinInterval(interval time.Duration) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.minInterval = interval
}
