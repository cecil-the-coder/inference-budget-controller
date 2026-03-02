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
	"fmt"
	"os"
	"path/filepath"
)

const (
	// ReadyMarkerFile is the name of the marker file indicating a complete download.
	ReadyMarkerFile = ".ready"
)

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
			os.Remove(tmpPath)
		}
	}()

	// Write data
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write data: %w", err)
	}

	// Sync to ensure data is on disk
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
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
