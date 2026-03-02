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
	"os"
	"path/filepath"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/cecil-the-coder/inference-budget-controller/api/v1alpha1"
)

// DefaultTTL is the default time-to-live for unused cached models.
const DefaultTTL = 24 * time.Hour

// DefaultCleanupInterval is the default interval between cleanup runs.
const DefaultCleanupInterval = 1 * time.Hour

// CacheManager manages the model cache with TTL-based cleanup.
type CacheManager struct {
	client      client.Client
	namespace   string
	cacheDir    string
	ttl         time.Duration
	interval    time.Duration
	lastCleanup time.Time
	mu          sync.Mutex
}

// CacheManagerOption is a functional option for configuring the CacheManager.
type CacheManagerOption func(*CacheManager)

// WithCacheManagerDir sets the cache directory for the CacheManager.
func WithCacheManagerDir(dir string) CacheManagerOption {
	return func(m *CacheManager) {
		m.cacheDir = dir
	}
}

// WithTTL sets the time-to-live for unused cached models.
func WithTTL(ttl time.Duration) CacheManagerOption {
	return func(m *CacheManager) {
		m.ttl = ttl
	}
}

// WithCleanupInterval sets the interval between cleanup runs.
func WithCleanupInterval(interval time.Duration) CacheManagerOption {
	return func(m *CacheManager) {
		m.interval = interval
	}
}

// WithNamespace sets the namespace for listing InferenceModels.
func WithNamespace(ns string) CacheManagerOption {
	return func(m *CacheManager) {
		m.namespace = ns
	}
}

// WithClient sets the Kubernetes client.
func WithCacheClient(c client.Client) CacheManagerOption {
	return func(m *CacheManager) {
		m.client = c
	}
}

// NewCacheManager creates a new CacheManager with the given options.
func NewCacheManager(client client.Client, opts ...CacheManagerOption) *CacheManager {
	m := &CacheManager{
		client:   client,
		ttl:      DefaultTTL,
		interval: DefaultCleanupInterval,
	}

	for _, opt := range opts {
		opt(m)
	}

	return m
}

// Run starts the background cleanup loop. It runs until the context is cancelled.
func (m *CacheManager) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	log.FromContext(ctx).Info("Starting cache cleanup manager",
		"cacheDir", m.cacheDir,
		"ttl", m.ttl,
		"interval", m.interval)

	for {
		select {
		case <-ctx.Done():
			log.FromContext(ctx).Info("Stopping cache cleanup manager")
			return
		case <-ticker.C:
			if err := m.cleanup(ctx); err != nil {
				log.FromContext(ctx).Error(err, "Cache cleanup failed")
			}
		}
	}
}

// cleanup removes unused cached models older than TTL.
func (m *CacheManager) cleanup(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.lastCleanup = time.Now()

	logger := log.FromContext(ctx)

	// 1. List all InferenceModels to find active ones
	models := &inferencev1alpha1.InferenceModelList{}
	if err := m.client.List(ctx, models, client.InNamespace(m.namespace)); err != nil {
		return fmt.Errorf("failed to list models: %w", err)
	}

	activeModels := make(map[string]bool)
	for _, model := range models.Items {
		activeModels[model.Name] = true
	}

	// 2. Scan cache directory
	entries, err := os.ReadDir(m.cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			logger.Info("Cache directory does not exist yet, skipping cleanup")
			return nil // Cache dir doesn't exist yet
		}
		return fmt.Errorf("failed to read cache dir: %w", err)
	}

	// 3. Check each cached model
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		modelName := entry.Name()

		// Skip if model is active (has InferenceModel CRD)
		if activeModels[modelName] {
			continue
		}

		modelPath := filepath.Join(m.cacheDir, modelName)
		lastAccess := m.getLastAccessTime(modelPath)

		// Delete if older than TTL
		if !lastAccess.IsZero() && time.Since(lastAccess) > m.ttl {
			logger.Info("Cleaning up unused model cache",
				"model", modelName,
				"lastAccess", lastAccess,
				"age", time.Since(lastAccess),
				"ttl", m.ttl)

			if err := os.RemoveAll(modelPath); err != nil {
				logger.Error(err, "Failed to remove cached model", "model", modelName)
			}
		}
	}

	return nil
}

// getLastAccessTime returns the last access time of a cached model.
// It first checks for a .last-used marker file, then falls back to directory mtime.
func (m *CacheManager) getLastAccessTime(modelPath string) time.Time {
	// Check for .last-used marker file
	marker := filepath.Join(modelPath, ".last-used")
	if info, err := os.Stat(marker); err == nil {
		return info.ModTime()
	}

	// Fall back to directory mtime
	if info, err := os.Stat(modelPath); err == nil {
		return info.ModTime()
	}

	return time.Time{}
}

// TouchModel updates the last-used timestamp for a model.
// This creates or updates the .last-used marker file.
func (m *CacheManager) TouchModel(modelName string) error {
	marker := filepath.Join(m.cacheDir, modelName, ".last-used")

	// Create directory if needed
	dir := filepath.Dir(marker)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Touch the file (create if not exists, update mtime if exists)
	now := time.Now()
	// Create or truncate the file if it doesn't exist
	file, err := os.OpenFile(marker, os.O_CREATE|os.O_RDONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to create marker file: %w", err)
	}
	_ = file.Close()

	// Update the modification time
	if err := os.Chtimes(marker, now, now); err != nil {
		return fmt.Errorf("failed to update marker file time: %w", err)
	}

	return nil
}

// CacheStats contains statistics about the cache.
type CacheStats struct {
	TotalSize    int64
	ModelCount   int
	OldestAccess time.Time
	NewestAccess time.Time
}

// GetCacheStats returns statistics about the cache.
func (m *CacheManager) GetCacheStats() (CacheStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stats := CacheStats{}

	// Scan cache directory
	entries, err := os.ReadDir(m.cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return stats, nil // Cache dir doesn't exist yet
		}
		return stats, fmt.Errorf("failed to read cache dir: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		modelPath := filepath.Join(m.cacheDir, entry.Name())
		lastAccess := m.getLastAccessTime(modelPath)

		// Calculate directory size
		size, err := m.calculateDirSize(modelPath)
		if err != nil {
			continue // Skip directories we can't read
		}

		stats.TotalSize += size
		stats.ModelCount++

		// Track oldest and newest access times
		if !lastAccess.IsZero() {
			if stats.OldestAccess.IsZero() || lastAccess.Before(stats.OldestAccess) {
				stats.OldestAccess = lastAccess
			}
			if stats.NewestAccess.IsZero() || lastAccess.After(stats.NewestAccess) {
				stats.NewestAccess = lastAccess
			}
		}
	}

	return stats, nil
}

// calculateDirSize recursively calculates the size of a directory.
func (m *CacheManager) calculateDirSize(path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size, err
}

// GetLastCleanup returns the time of the last cleanup run.
func (m *CacheManager) GetLastCleanup() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastCleanup
}

// CleanupOnce performs a single cleanup run without starting the background loop.
// This is useful for testing or manual cleanup.
func (m *CacheManager) CleanupOnce(ctx context.Context) error {
	return m.cleanup(ctx)
}

// CleanupWithActive performs cleanup with a predefined set of active models.
// This is useful for testing without requiring a Kubernetes client.
func (m *CacheManager) CleanupWithActive(ctx context.Context, activeModels map[string]bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.lastCleanup = time.Now()

	logger := log.FromContext(ctx)

	// Scan cache directory
	entries, err := os.ReadDir(m.cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Cache dir doesn't exist yet
		}
		return fmt.Errorf("failed to read cache dir: %w", err)
	}

	// Check each cached model
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		modelName := entry.Name()

		// Skip if model is active
		if activeModels[modelName] {
			continue
		}

		modelPath := filepath.Join(m.cacheDir, modelName)
		lastAccess := m.getLastAccessTime(modelPath)

		// Delete if older than TTL
		if !lastAccess.IsZero() && time.Since(lastAccess) > m.ttl {
			logger.Info("Cleaning up unused model cache",
				"model", modelName,
				"lastAccess", lastAccess,
				"age", time.Since(lastAccess),
				"ttl", m.ttl)

			if err := os.RemoveAll(modelPath); err != nil {
				logger.Error(err, "Failed to remove cached model", "model", modelName)
			}
		}
	}

	return nil
}
