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

package budget

import (
	"fmt"
	"math"
	"sync"

	"k8s.io/apimachinery/pkg/api/resource"
)

// ModelAllocation represents a model's memory allocation
type ModelAllocation struct {
	Name         string
	Namespace    string
	Memory       *resource.Quantity
	NodeSelector map[string]string
}

// UtilizationInfo represents memory utilization data for a model
type UtilizationInfo struct {
	Name           string
	Namespace      string
	Declared       *resource.Quantity
	ObservedPeak   *resource.Quantity
	Utilization    float64 // 0.0 - 1.0
	Recommendation string
}

// modelUsage tracks observed memory usage for a model
type modelUsage struct {
	name         string
	namespace    string
	observedPeak *resource.Quantity
}

// NodeBudget represents memory budget for a node pool
type NodeBudget struct {
	Total     *resource.Quantity
	Allocated *resource.Quantity
}

// Tracker tracks memory budgets for inference models across node pools
type Tracker struct {
	mu sync.RWMutex

	// nodeBudgets maps node selector key to budget
	nodeBudgets map[string]*NodeBudget

	// allocations maps model namespace/name to allocation
	allocations map[string]*ModelAllocation

	// usageTracking maps model namespace/name to observed peak memory usage
	usageTracking map[string]*modelUsage
}

// NewTracker creates a new budget tracker
func NewTracker() *Tracker {
	return &Tracker{
		nodeBudgets:   make(map[string]*NodeBudget),
		allocations:   make(map[string]*ModelAllocation),
		usageTracking: make(map[string]*modelUsage),
	}
}

// SetNodeBudget sets the total memory budget for a node pool
func (t *Tracker) SetNodeBudget(nodeSelectorKey string, total resource.Quantity) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if budget, exists := t.nodeBudgets[nodeSelectorKey]; exists {
		budget.Total = &total
	} else {
		t.nodeBudgets[nodeSelectorKey] = &NodeBudget{
			Total:     &total,
			Allocated: resource.NewQuantity(0, resource.BinarySI),
		}
	}
}

// CanAllocate checks if there is enough memory budget for a model
func (t *Tracker) CanAllocate(name, namespace, memoryStr string, nodeSelector map[string]string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	memory := resource.MustParse(memoryStr)
	key := modelKey(name, namespace)

	// If already allocated, it can be re-allocated (update case)
	if existing, exists := t.allocations[key]; exists {
		nodeBudgetKey := nodeSelectorKey(nodeSelector)
		if budget, ok := t.nodeBudgets[nodeBudgetKey]; ok {
			currentAllocated := budget.Allocated.DeepCopy()
			currentAllocated.Sub(*existing.Memory)
			currentAllocated.Add(memory)
			return currentAllocated.Cmp(*budget.Total) <= 0
		}
	}

	// Check new allocation
	nodeBudgetKey := nodeSelectorKey(nodeSelector)
	budget, ok := t.nodeBudgets[nodeBudgetKey]
	if !ok {
		// If no budget set, use a default (e.g., 128Gi)
		return true
	}

	available := budget.Total.DeepCopy()
	available.Sub(*budget.Allocated)
	return available.Cmp(memory) >= 0
}

// Allocate allocates memory for a model
func (t *Tracker) Allocate(name, namespace, memoryStr string, nodeSelector map[string]string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.canAllocateLocked(name, namespace, memoryStr, nodeSelector) {
		return false
	}

	memory := resource.MustParse(memoryStr)
	key := modelKey(name, namespace)

	// Release existing allocation if any
	if existing, exists := t.allocations[key]; exists {
		nodeBudgetKey := nodeSelectorKey(existing.NodeSelector)
		if budget, ok := t.nodeBudgets[nodeBudgetKey]; ok {
			budget.Allocated.Sub(*existing.Memory)
		}
	}

	// Create new allocation
	t.allocations[key] = &ModelAllocation{
		Name:         name,
		Namespace:    namespace,
		Memory:       &memory,
		NodeSelector: nodeSelector,
	}

	// Update node budget
	nodeBudgetKey := nodeSelectorKey(nodeSelector)
	if budget, ok := t.nodeBudgets[nodeBudgetKey]; ok {
		budget.Allocated.Add(memory)
	}

	return true
}

// canAllocateLocked checks allocation without lock (internal use)
func (t *Tracker) canAllocateLocked(name, namespace, memoryStr string, nodeSelector map[string]string) bool {
	memory := resource.MustParse(memoryStr)
	key := modelKey(name, namespace)

	// If already allocated, it can be re-allocated (update case)
	if existing, exists := t.allocations[key]; exists {
		nodeBudgetKey := nodeSelectorKey(nodeSelector)
		if budget, ok := t.nodeBudgets[nodeBudgetKey]; ok {
			currentAllocated := budget.Allocated.DeepCopy()
			currentAllocated.Sub(*existing.Memory)
			currentAllocated.Add(memory)
			return currentAllocated.Cmp(*budget.Total) <= 0
		}
		return true
	}

	// Check new allocation
	nodeBudgetKey := nodeSelectorKey(nodeSelector)
	budget, ok := t.nodeBudgets[nodeBudgetKey]
	if !ok {
		// If no budget set, allow allocation
		return true
	}

	available := budget.Total.DeepCopy()
	available.Sub(*budget.Allocated)
	return available.Cmp(memory) >= 0
}

// ReleaseModel releases memory allocation for a model
func (t *Tracker) ReleaseModel(name, namespace string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	key := modelKey(name, namespace)
	if allocation, exists := t.allocations[key]; exists {
		nodeBudgetKey := nodeSelectorKey(allocation.NodeSelector)
		if budget, ok := t.nodeBudgets[nodeBudgetKey]; ok {
			budget.Allocated.Sub(*allocation.Memory)
		}
		delete(t.allocations, key)
	}
}

// GetAvailableMemory returns available memory for a node pool
func (t *Tracker) GetAvailableMemory(nodeSelector map[string]string) *resource.Quantity {
	t.mu.RLock()
	defer t.mu.RUnlock()

	nodeBudgetKey := nodeSelectorKey(nodeSelector)
	budget, ok := t.nodeBudgets[nodeBudgetKey]
	if !ok {
		// Return zero if no budget configured
		return resource.NewQuantity(0, resource.BinarySI)
	}

	available := budget.Total.DeepCopy()
	available.Sub(*budget.Allocated)
	return &available
}

// GetAllocatedMemory returns total allocated memory for a node pool
func (t *Tracker) GetAllocatedMemory(nodeSelector map[string]string) *resource.Quantity {
	t.mu.RLock()
	defer t.mu.RUnlock()

	nodeBudgetKey := nodeSelectorKey(nodeSelector)
	if budget, ok := t.nodeBudgets[nodeBudgetKey]; ok {
		val := budget.Allocated.DeepCopy()
		return &val
	}
	return resource.NewQuantity(0, resource.BinarySI)
}

// GetBlockingModels returns models blocking allocation for the requested memory
func (t *Tracker) GetBlockingModels(nodeSelector map[string]string, requestedMemory *resource.Quantity) []ModelAllocation {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var blocking []ModelAllocation
	nodeBudgetKey := nodeSelectorKey(nodeSelector)

	for _, allocation := range t.allocations {
		if nodeSelectorKey(allocation.NodeSelector) == nodeBudgetKey {
			blocking = append(blocking, *allocation)
		}
	}

	return blocking
}

// modelKey generates a unique key for a model
func modelKey(name, namespace string) string {
	return namespace + "/" + name
}

// nodeSelectorKey generates a key from node selector
func nodeSelectorKey(nodeSelector map[string]string) string {
	if nodeSelector == nil {
		return "default"
	}
	// Simple implementation - use first key-value pair
	for k, v := range nodeSelector {
		return k + "=" + v
	}
	return "default"
}

// RecordUsage records observed memory usage for a model, updating peak if necessary
func (t *Tracker) RecordUsage(name, namespace string, observedMemory *resource.Quantity) {
	t.mu.Lock()
	defer t.mu.Unlock()

	key := modelKey(name, namespace)

	if usage, exists := t.usageTracking[key]; exists {
		// Update peak if observed memory is greater
		if observedMemory != nil && (usage.observedPeak == nil || observedMemory.Cmp(*usage.observedPeak) > 0) {
			val := observedMemory.DeepCopy()
			usage.observedPeak = &val
		}
	} else {
		// Create new usage tracking entry
		observedCopy := observedMemory.DeepCopy()
		t.usageTracking[key] = &modelUsage{
			name:         name,
			namespace:    namespace,
			observedPeak: &observedCopy,
		}
	}
}

// GetUtilization returns utilization information for a model, including recommendations
func (t *Tracker) GetUtilization(name, namespace, declaredMemory string) *UtilizationInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()

	declared := resource.MustParse(declaredMemory)
	key := modelKey(name, namespace)

	info := &UtilizationInfo{
		Name:         name,
		Namespace:    namespace,
		Declared:     &declared,
		ObservedPeak: nil,
		Utilization:  0.0,
	}

	if usage, exists := t.usageTracking[key]; exists && usage.observedPeak != nil {
		val := usage.observedPeak.DeepCopy()
		info.ObservedPeak = &val

		// Calculate utilization ratio
		declaredValue := float64(declared.Value())
		observedValue := float64(usage.observedPeak.Value())
		if declaredValue > 0 {
			info.Utilization = observedValue / declaredValue
		}

		// Generate recommendation if utilization is below 90%
		if info.Utilization < 0.9 {
			// Add 10% safety buffer to observed peak
			recommendedValue := int64(math.Ceil(observedValue * 1.1))
			recommended := resource.NewQuantity(recommendedValue, resource.BinarySI)

			// Format as readable string (e.g., "78Gi")
			recommendedStr := recommended.String()

			// Calculate overprovisioning percentage
			overprovisionedPercent := int((1 - info.Utilization) * 100)

			info.Recommendation = fmt.Sprintf(
				"Consider reducing memory from %s to %s (observed peak %s, %d%% overprovisioned)",
				declared.String(),
				recommendedStr,
				usage.observedPeak.String(),
				overprovisionedPercent,
			)
		}
	}

	return info
}

// GetOverprovisionedModels returns a list of models that are significantly overprovisioned
func (t *Tracker) GetOverprovisionedModels() []UtilizationInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var overprovisioned []UtilizationInfo

	for key, allocation := range t.allocations {
		// Get observed usage for this model
		usage, hasUsage := t.usageTracking[key]

		if !hasUsage || usage == nil || usage.observedPeak == nil {
			continue
		}

		declaredValue := float64(allocation.Memory.Value())
		observedValue := float64(usage.observedPeak.Value())

		if declaredValue <= 0 {
			continue
		}

		utilization := observedValue / declaredValue

		// Include models that are less than 90% utilized
		if utilization < 0.9 {
			// Add 10% safety buffer to observed peak
			recommendedValue := int64(math.Ceil(observedValue * 1.1))
			recommended := resource.NewQuantity(recommendedValue, resource.BinarySI)

			declaredCopy := allocation.Memory.DeepCopy()
			observedCopy := usage.observedPeak.DeepCopy()
			info := UtilizationInfo{
				Name:         allocation.Name,
				Namespace:    allocation.Namespace,
				Declared:     &declaredCopy,
				ObservedPeak: &observedCopy,
				Utilization:  utilization,
				Recommendation: fmt.Sprintf(
					"Consider reducing memory from %s to %s",
					allocation.Memory.String(),
					recommended.String(),
				),
			}
			overprovisioned = append(overprovisioned, info)
		}
	}

	return overprovisioned
}
