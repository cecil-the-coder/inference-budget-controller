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
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheManagerCleanup(t *testing.T) {
	// Create temp cache dir
	tmpDir := t.TempDir()

	// Create a model directory with old marker
	oldModel := filepath.Join(tmpDir, "old-model")
	if err := os.MkdirAll(oldModel, 0755); err != nil {
		t.Fatalf("failed to create old model dir: %v", err)
	}

	// Set .last-used to 2 days ago
	marker := filepath.Join(oldModel, ".last-used")
	if err := os.WriteFile(marker, []byte{}, 0644); err != nil {
		t.Fatalf("failed to create marker file: %v", err)
	}
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(marker, oldTime, oldTime); err != nil {
		t.Fatalf("failed to set marker time: %v", err)
	}

	// Create a recent model
	newModel := filepath.Join(tmpDir, "new-model")
	if err := os.MkdirAll(newModel, 0755); err != nil {
		t.Fatalf("failed to create new model dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(newModel, ".last-used"), []byte{}, 0644); err != nil {
		t.Fatalf("failed to create new marker file: %v", err)
	}

	// Run cleanup with 24h TTL
	mgr := &CacheManager{
		cacheDir: tmpDir,
		ttl:      24 * time.Hour,
	}

	// Use empty active models (no InferenceModel CRDs)
	err := mgr.CleanupWithActive(context.Background(), map[string]bool{})
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}

	// Old model should be deleted
	if _, err := os.Stat(oldModel); !os.IsNotExist(err) {
		t.Error("old model should have been deleted")
	}

	// New model should still exist
	if _, err := os.Stat(newModel); os.IsNotExist(err) {
		t.Error("new model should still exist")
	}
}

func TestCacheManagerCleanupActiveModel(t *testing.T) {
	// Create temp cache dir
	tmpDir := t.TempDir()

	// Create a model directory with old marker
	modelDir := filepath.Join(tmpDir, "active-model")
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		t.Fatalf("failed to create model dir: %v", err)
	}

	// Set .last-used to 2 days ago (older than TTL)
	marker := filepath.Join(modelDir, ".last-used")
	if err := os.WriteFile(marker, []byte{}, 0644); err != nil {
		t.Fatalf("failed to create marker file: %v", err)
	}
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(marker, oldTime, oldTime); err != nil {
		t.Fatalf("failed to set marker time: %v", err)
	}

	// Run cleanup with 24h TTL
	mgr := &CacheManager{
		cacheDir: tmpDir,
		ttl:      24 * time.Hour,
	}

	// Mark model as active
	activeModels := map[string]bool{"active-model": true}

	err := mgr.CleanupWithActive(context.Background(), activeModels)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}

	// Active model should NOT be deleted even though it's older than TTL
	if _, err := os.Stat(modelDir); os.IsNotExist(err) {
		t.Error("active model should not have been deleted")
	}
}

func TestCacheManagerTouchModel(t *testing.T) {
	// Create temp cache dir
	tmpDir := t.TempDir()

	mgr := &CacheManager{
		cacheDir: tmpDir,
		ttl:      24 * time.Hour,
	}

	// Touch a model that doesn't exist yet
	err := mgr.TouchModel("test-model")
	if err != nil {
		t.Fatalf("TouchModel failed: %v", err)
	}

	// Check that marker file was created
	marker := filepath.Join(tmpDir, "test-model", ".last-used")
	if _, err := os.Stat(marker); os.IsNotExist(err) {
		t.Error("marker file should have been created")
	}

	// Get last access time
	lastAccess := mgr.getLastAccessTime(filepath.Join(tmpDir, "test-model"))
	if lastAccess.IsZero() {
		t.Error("last access time should not be zero")
	}

	// Check that the time is recent
	if time.Since(lastAccess) > time.Second {
		t.Error("last access time should be very recent")
	}
}

func TestCacheManagerGetCacheStats(t *testing.T) {
	// Create temp cache dir
	tmpDir := t.TempDir()

	// Create two model directories
	model1 := filepath.Join(tmpDir, "model-1")
	if err := os.MkdirAll(model1, 0755); err != nil {
		t.Fatalf("failed to create model dir: %v", err)
	}
	// Add a file with known size
	if err := os.WriteFile(filepath.Join(model1, "model.bin"), make([]byte, 1000), 0644); err != nil {
		t.Fatalf("failed to create model file: %v", err)
	}

	model2 := filepath.Join(tmpDir, "model-2")
	if err := os.MkdirAll(model2, 0755); err != nil {
		t.Fatalf("failed to create model dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(model2, "model.bin"), make([]byte, 2000), 0644); err != nil {
		t.Fatalf("failed to create model file: %v", err)
	}

	mgr := &CacheManager{
		cacheDir: tmpDir,
		ttl:      24 * time.Hour,
	}

	stats, err := mgr.GetCacheStats()
	if err != nil {
		t.Fatalf("GetCacheStats failed: %v", err)
	}

	if stats.ModelCount != 2 {
		t.Errorf("expected 2 models, got %d", stats.ModelCount)
	}

	// Total size should be at least 3000 bytes (1000 + 2000)
	// Note: marker files and directory entries add some overhead
	if stats.TotalSize < 3000 {
		t.Errorf("expected total size >= 3000, got %d", stats.TotalSize)
	}
}

func TestCacheManagerGetLastAccessTime(t *testing.T) {
	// Create temp cache dir
	tmpDir := t.TempDir()

	mgr := &CacheManager{
		cacheDir: tmpDir,
		ttl:      24 * time.Hour,
	}

	// Test with non-existent directory
	lastAccess := mgr.getLastAccessTime("/nonexistent/path")
	if !lastAccess.IsZero() {
		t.Error("last access time should be zero for non-existent path")
	}

	// Test with directory but no marker file
	modelDir := filepath.Join(tmpDir, "no-marker")
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		t.Fatalf("failed to create model dir: %v", err)
	}

	lastAccess = mgr.getLastAccessTime(modelDir)
	if lastAccess.IsZero() {
		t.Error("last access time should fall back to directory mtime")
	}

	// Test with marker file
	modelDirWithMarker := filepath.Join(tmpDir, "with-marker")
	if err := os.MkdirAll(modelDirWithMarker, 0755); err != nil {
		t.Fatalf("failed to create model dir: %v", err)
	}
	expectedTime := time.Now().Add(-1 * time.Hour)
	marker := filepath.Join(modelDirWithMarker, ".last-used")
	if err := os.WriteFile(marker, []byte{}, 0644); err != nil {
		t.Fatalf("failed to create marker file: %v", err)
	}
	if err := os.Chtimes(marker, expectedTime, expectedTime); err != nil {
		t.Fatalf("failed to set marker time: %v", err)
	}

	lastAccess = mgr.getLastAccessTime(modelDirWithMarker)
	if lastAccess.IsZero() {
		t.Error("last access time should not be zero")
	}

	// Check that the time matches the marker file time (within 1 second tolerance)
	diff := lastAccess.Sub(expectedTime)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("last access time should match marker file time, diff=%v", diff)
	}
}

func TestCacheManagerNonExistentCacheDir(t *testing.T) {
	mgr := &CacheManager{
		cacheDir: "/nonexistent/cache/dir",
		ttl:      24 * time.Hour,
	}

	// Cleanup should not fail for non-existent cache directory
	err := mgr.CleanupWithActive(context.Background(), map[string]bool{})
	if err != nil {
		t.Errorf("cleanup should not fail for non-existent cache dir: %v", err)
	}

	// GetCacheStats should return empty stats for non-existent cache directory
	stats, err := mgr.GetCacheStats()
	if err != nil {
		t.Errorf("GetCacheStats should not fail for non-existent cache dir: %v", err)
	}
	if stats.ModelCount != 0 {
		t.Errorf("expected 0 models, got %d", stats.ModelCount)
	}
}

func TestNewCacheManager(t *testing.T) {
	// Test with defaults
	mgr := NewCacheManager(nil)
	if mgr.ttl != DefaultTTL {
		t.Errorf("expected default TTL %v, got %v", DefaultTTL, mgr.ttl)
	}
	if mgr.interval != DefaultCleanupInterval {
		t.Errorf("expected default interval %v, got %v", DefaultCleanupInterval, mgr.interval)
	}

	// Test with options
	customTTL := 48 * time.Hour
	customInterval := 30 * time.Minute
	customDir := "/custom/cache"
	customNS := "test-namespace"

	mgr = NewCacheManager(nil,
		WithTTL(customTTL),
		WithCleanupInterval(customInterval),
		WithCacheManagerDir(customDir),
		WithNamespace(customNS),
	)

	if mgr.ttl != customTTL {
		t.Errorf("expected TTL %v, got %v", customTTL, mgr.ttl)
	}
	if mgr.interval != customInterval {
		t.Errorf("expected interval %v, got %v", customInterval, mgr.interval)
	}
	if mgr.cacheDir != customDir {
		t.Errorf("expected cache dir %s, got %s", customDir, mgr.cacheDir)
	}
	if mgr.namespace != customNS {
		t.Errorf("expected namespace %s, got %s", customNS, mgr.namespace)
	}
}

func TestCacheManagerGetLastCleanup(t *testing.T) {
	mgr := &CacheManager{
		cacheDir: t.TempDir(),
		ttl:      24 * time.Hour,
	}

	// Initially, last cleanup should be zero
	if !mgr.GetLastCleanup().IsZero() {
		t.Error("initial last cleanup time should be zero")
	}

	// Run cleanup
	_ = mgr.CleanupWithActive(context.Background(), map[string]bool{})

	// Now last cleanup should be set
	lastCleanup := mgr.GetLastCleanup()
	if lastCleanup.IsZero() {
		t.Error("last cleanup time should be set after cleanup")
	}

	// Check that the time is recent
	if time.Since(lastCleanup) > time.Second {
		t.Error("last cleanup time should be very recent")
	}
}
