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

package registry

import (
	"sync"
	"time"
)

// DeploymentState represents the current state of a deployment
type DeploymentState int

const (
	// StateNonexistent indicates the deployment doesn't exist
	StateNonexistent DeploymentState = iota
	// StateCreating indicates the deployment was created but pods are not ready
	StateCreating
	// StateReady indicates the deployment has ready pods
	StateReady
	// StateDeleting indicates the deployment is being deleted
	StateDeleting
)

// String returns the string representation of the deployment state
func (s DeploymentState) String() string {
	switch s {
	case StateNonexistent:
		return "Nonexistent"
	case StateCreating:
		return "Creating"
	case StateReady:
		return "Ready"
	case StateDeleting:
		return "Deleting"
	default:
		return "Unknown"
	}
}

// DeploymentEntry represents a tracked deployment in the registry
type DeploymentEntry struct {
	Name            string
	Namespace       string
	State           DeploymentState
	LastRequestTime time.Time
	ActiveRequests  int
	CreatedAt       time.Time
}

// DeploymentRegistry tracks deployment state in memory
type DeploymentRegistry struct {
	mu      sync.RWMutex
	entries map[string]*DeploymentEntry
}

// NewRegistry creates a new DeploymentRegistry
func NewRegistry() *DeploymentRegistry {
	return &DeploymentRegistry{
		entries: make(map[string]*DeploymentEntry),
	}
}

// entryKey generates a unique key for a deployment
func entryKey(namespace, name string) string {
	return namespace + "/" + name
}

// Get retrieves a deployment entry by namespace and name.
// Returns nil if the entry is not found.
func (r *DeploymentRegistry) Get(namespace, name string) *DeploymentEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	key := entryKey(namespace, name)
	if entry, exists := r.entries[key]; exists {
		return entry
	}
	return nil
}

// SetState updates the state of a deployment.
// If the deployment doesn't exist, it creates a new entry.
func (r *DeploymentRegistry) SetState(namespace, name string, state DeploymentState) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := entryKey(namespace, name)
	now := time.Now()

	if entry, exists := r.entries[key]; exists {
		entry.State = state
	} else {
		r.entries[key] = &DeploymentEntry{
			Name:            name,
			Namespace:       namespace,
			State:           state,
			LastRequestTime: now,
			ActiveRequests:  0,
			CreatedAt:       now,
		}
	}
}

// RecordRequest increments the active request count and updates the last request time.
// If the deployment doesn't exist, it creates a new entry in Creating state.
func (r *DeploymentRegistry) RecordRequest(namespace, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := entryKey(namespace, name)
	now := time.Now()

	if entry, exists := r.entries[key]; exists {
		entry.ActiveRequests++
		entry.LastRequestTime = now
	} else {
		r.entries[key] = &DeploymentEntry{
			Name:            name,
			Namespace:       namespace,
			State:           StateCreating,
			LastRequestTime: now,
			ActiveRequests:  1,
			CreatedAt:       now,
		}
	}
}

// FinishRequest decrements the active request count for a deployment.
// If the deployment doesn't exist or the count is already zero, this is a no-op.
func (r *DeploymentRegistry) FinishRequest(namespace, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := entryKey(namespace, name)

	if entry, exists := r.entries[key]; exists {
		if entry.ActiveRequests > 0 {
			entry.ActiveRequests--
		}
	}
}

// Delete removes a deployment entry from the registry.
func (r *DeploymentRegistry) Delete(namespace, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := entryKey(namespace, name)
	delete(r.entries, key)
}

// ListByState returns all deployment entries matching the given state.
func (r *DeploymentRegistry) ListByState(state DeploymentState) []*DeploymentEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*DeploymentEntry
	for _, entry := range r.entries {
		if entry.State == state {
			result = append(result, entry)
		}
	}
	return result
}

// GetIdleEntries returns entries with no active requests and LastRequestTime older than idleTimeout.
func (r *DeploymentRegistry) GetIdleEntries(idleTimeout time.Duration) []*DeploymentEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*DeploymentEntry
	cutoff := time.Now().Add(-idleTimeout)

	for _, entry := range r.entries {
		if entry.ActiveRequests == 0 && entry.LastRequestTime.Before(cutoff) {
			result = append(result, entry)
		}
	}
	return result
}
