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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

func TestTracker_CanAllocate(t *testing.T) {
	tracker := NewTracker()

	// Set up a node pool with 128Gi memory
	tracker.SetNodeBudget("inference-pool=default", resource.MustParse("128Gi"))

	nodeSelector := map[string]string{"inference-pool": "default"}

	tests := []struct {
		name      string
		modelName string
		memory    string
		want      bool
	}{
		{
			name:      "small model fits",
			modelName: "model-a",
			memory:    "60Gi",
			want:      true,
		},
		{
			name:      "large model fits",
			modelName: "model-b",
			memory:    "128Gi",
			want:      true,
		},
		{
			name:      "model too large",
			modelName: "model-c",
			memory:    "129Gi",
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tracker.CanAllocate(tt.modelName, "default", tt.memory, nodeSelector)
			if got != tt.want {
				t.Errorf("Tracker.CanAllocate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTracker_AllocateAndRelease(t *testing.T) {
	tracker := NewTracker()

	// Set up a node pool with 128Gi memory
	tracker.SetNodeBudget("inference-pool=default", resource.MustParse("128Gi"))

	nodeSelector := map[string]string{"inference-pool": "default"}

	// Allocate 80Gi for model-a
	if !tracker.Allocate("model-a", "default", "80Gi", nodeSelector) {
		t.Error("Failed to allocate 80Gi for model-a")
	}

	// Check remaining memory (should be 48Gi)
	available := tracker.GetAvailableMemory(nodeSelector)
	expected := resource.MustParse("48Gi")
	if available.Cmp(expected) != 0 {
		t.Errorf("Expected available memory %v, got %v", expected, available)
	}

	// Try to allocate 60Gi for model-b (should fail - only 48Gi available)
	if tracker.CanAllocate("model-b", "default", "60Gi", nodeSelector) {
		t.Error("Should not be able to allocate 60Gi when only 48Gi available")
	}

	// Release model-a
	tracker.ReleaseModel("model-a", "default")

	// Now 60Gi allocation should work
	if !tracker.CanAllocate("model-b", "default", "60Gi", nodeSelector) {
		t.Error("Should be able to allocate 60Gi after releasing model-a")
	}
}

func TestTracker_GetBlockingModels(t *testing.T) {
	tracker := NewTracker()

	// Set up a node pool with 128Gi memory
	tracker.SetNodeBudget("inference-pool=default", resource.MustParse("128Gi"))

	nodeSelector := map[string]string{"inference-pool": "default"}

	// Allocate memory for multiple models
	tracker.Allocate("model-a", "default", "60Gi", nodeSelector)
	tracker.Allocate("model-b", "default", "40Gi", nodeSelector)

	// Request 50Gi (only 28Gi available, so should be blocked)
	requested := resource.MustParse("50Gi")
	blocking := tracker.GetBlockingModels(nodeSelector, &requested)

	if len(blocking) == 0 {
		t.Error("Expected blocking models, got none")
	}

	// Release all models
	tracker.ReleaseModel("model-a", "default")
	tracker.ReleaseModel("model-b", "default")

	// Now should have no blocking models
	blocking = tracker.GetBlockingModels(nodeSelector, &requested)
	if len(blocking) != 0 {
		t.Errorf("Expected no blocking models, got %d", len(blocking))
	}
}

func TestTracker_RecordUsage(t *testing.T) {
	tracker := NewTracker()

	// Record initial usage
	initialMemory := resource.MustParse("50Gi")
	tracker.RecordUsage("model-a", "default", &initialMemory)

	// Get utilization - should show the observed peak
	info := tracker.GetUtilization("model-a", "default", "80Gi")
	if info.ObservedPeak == nil {
		t.Error("Expected observed peak to be set")
	}
	if info.ObservedPeak.Cmp(initialMemory) != 0 {
		t.Errorf("Expected observed peak %v, got %v", initialMemory, info.ObservedPeak)
	}

	// Record higher usage - should update peak
	higherMemory := resource.MustParse("75Gi")
	tracker.RecordUsage("model-a", "default", &higherMemory)

	info = tracker.GetUtilization("model-a", "default", "80Gi")
	if info.ObservedPeak.Cmp(higherMemory) != 0 {
		t.Errorf("Expected observed peak to be updated to %v, got %v", higherMemory, info.ObservedPeak)
	}

	// Record lower usage - should NOT update peak
	lowerMemory := resource.MustParse("60Gi")
	tracker.RecordUsage("model-a", "default", &lowerMemory)

	info = tracker.GetUtilization("model-a", "default", "80Gi")
	if info.ObservedPeak.Cmp(higherMemory) != 0 {
		t.Errorf("Expected observed peak to remain at %v, got %v", higherMemory, info.ObservedPeak)
	}
}

func TestTracker_GetUtilization(t *testing.T) {
	tracker := NewTracker()

	// Test model with no recorded usage
	info := tracker.GetUtilization("model-a", "default", "80Gi")
	if info.Name != "model-a" {
		t.Errorf("Expected name 'model-a', got %s", info.Name)
	}
	if info.Namespace != "default" {
		t.Errorf("Expected namespace 'default', got %s", info.Namespace)
	}
	if info.Declared == nil || info.Declared.Cmp(resource.MustParse("80Gi")) != 0 {
		t.Error("Expected declared memory to be 80Gi")
	}
	if info.ObservedPeak != nil {
		t.Error("Expected observed peak to be nil for model with no usage")
	}
	if info.Utilization != 0.0 {
		t.Errorf("Expected utilization to be 0.0, got %f", info.Utilization)
	}
	if info.Recommendation != "" {
		t.Error("Expected no recommendation for model with no usage")
	}

	// Record usage and test utilization calculation
	observedMemory := resource.MustParse("70Gi") // 87.5% of 80Gi, should get recommendation
	tracker.RecordUsage("model-a", "default", &observedMemory)

	info = tracker.GetUtilization("model-a", "default", "80Gi")
	expectedUtilization := float64(70) / float64(80)
	if info.Utilization != expectedUtilization {
		t.Errorf("Expected utilization %f, got %f", expectedUtilization, info.Utilization)
	}

	// Should have recommendation since utilization < 90%
	if info.Recommendation == "" {
		t.Error("Expected recommendation for underutilized model")
	}
	if !strings.Contains(info.Recommendation, "70Gi") {
		t.Errorf("Expected recommendation to contain observed peak '70Gi', got: %s", info.Recommendation)
	}
}

func TestTracker_GetUtilization_HighUtilization(t *testing.T) {
	tracker := NewTracker()

	// Record usage that is >= 90% of declared
	observedMemory := resource.MustParse("73Gi") // 91.25% of 80Gi
	tracker.RecordUsage("model-a", "default", &observedMemory)

	info := tracker.GetUtilization("model-a", "default", "80Gi")
	expectedUtilization := float64(73) / float64(80)
	if info.Utilization != expectedUtilization {
		t.Errorf("Expected utilization %f, got %f", expectedUtilization, info.Utilization)
	}

	// Should NOT have recommendation since utilization >= 90%
	if info.Recommendation != "" {
		t.Errorf("Expected no recommendation for well-utilized model, got: %s", info.Recommendation)
	}
}

func TestTracker_GetUtilization_RecommendationCalculation(t *testing.T) {
	tracker := NewTracker()

	// Test recommendation calculation: 75Gi observed * 1.1 = 82.5Gi -> rounded up
	observedMemory := resource.MustParse("75Gi")
	tracker.RecordUsage("model-a", "default", &observedMemory)

	info := tracker.GetUtilization("model-a", "default", "100Gi")

	// Verify utilization
	expectedUtilization := 0.75
	if info.Utilization != expectedUtilization {
		t.Errorf("Expected utilization %f, got %f", expectedUtilization, info.Utilization)
	}

	// Recommendation should suggest something around 82Gi (75Gi * 1.1 = 82.5Gi rounded up)
	// The value might be in Mi or Gi depending on formatting
	if info.Recommendation == "" {
		t.Error("Expected recommendation for underutilized model")
	}
	if !strings.Contains(info.Recommendation, "82") && !strings.Contains(info.Recommendation, "83") && !strings.Contains(info.Recommendation, "84480") {
		t.Errorf("Expected recommendation to contain recommended size, got: %s", info.Recommendation)
	}
}

func TestTracker_GetOverprovisionedModels(t *testing.T) {
	tracker := NewTracker()

	nodeSelector := map[string]string{"inference-pool": "default"}

	// Allocate and record usage for model-a (overprovisioned)
	tracker.Allocate("model-a", "default", "80Gi", nodeSelector)
	observedA := resource.MustParse("60Gi") // 75% utilization
	tracker.RecordUsage("model-a", "default", &observedA)

	// Allocate and record usage for model-b (well-provisioned)
	tracker.Allocate("model-b", "default", "60Gi", nodeSelector)
	observedB := resource.MustParse("58Gi") // ~97% utilization
	tracker.RecordUsage("model-b", "default", &observedB)

	// Allocate and record usage for model-c (overprovisioned)
	tracker.Allocate("model-c", "default", "100Gi", nodeSelector)
	observedC := resource.MustParse("70Gi") // 70% utilization
	tracker.RecordUsage("model-c", "default", &observedC)

	// Get overprovisioned models
	overprovisioned := tracker.GetOverprovisionedModels()

	// Should find model-a and model-c (both < 90% utilization)
	if len(overprovisioned) != 2 {
		t.Errorf("Expected 2 overprovisioned models, got %d", len(overprovisioned))
	}

	// Verify the models are correct
	foundA := false
	foundC := false
	for _, info := range overprovisioned {
		if info.Name == "model-a" {
			foundA = true
			if info.Utilization != 0.75 {
				t.Errorf("Expected model-a utilization 0.75, got %f", info.Utilization)
			}
			if info.Recommendation == "" {
				t.Error("Expected recommendation for model-a")
			}
		}
		if info.Name == "model-c" {
			foundC = true
			if info.Utilization != 0.7 {
				t.Errorf("Expected model-c utilization 0.7, got %f", info.Utilization)
			}
		}
		if info.Name == "model-b" {
			t.Error("model-b should not be in overprovisioned list (97% utilization)")
		}
	}

	if !foundA {
		t.Error("Expected to find model-a in overprovisioned list")
	}
	if !foundC {
		t.Error("Expected to find model-c in overprovisioned list")
	}
}

func TestTracker_GetOverprovisionedModels_NoUsage(t *testing.T) {
	tracker := NewTracker()

	nodeSelector := map[string]string{"inference-pool": "default"}

	// Allocate models without recording usage
	tracker.Allocate("model-a", "default", "80Gi", nodeSelector)
	tracker.Allocate("model-b", "default", "60Gi", nodeSelector)

	// Should return empty list since no usage has been recorded
	overprovisioned := tracker.GetOverprovisionedModels()
	if len(overprovisioned) != 0 {
		t.Errorf("Expected no overprovisioned models without usage data, got %d", len(overprovisioned))
	}
}

func TestTracker_RecordUsage_ConcurrentSafety(t *testing.T) {
	tracker := NewTracker()

	// Run concurrent RecordUsage calls to verify thread safety
	done := make(chan bool)

	for i := 0; i < 10; i++ {
		go func() {
			memory := resource.MustParse("50Gi")
			tracker.RecordUsage("model-a", "default", &memory)
			done <- true
		}()
	}

	// Wait for all goroutines to complete
	for i := 0; i < 10; i++ {
		<-done
	}

	// Verify the peak is still correct
	info := tracker.GetUtilization("model-a", "default", "80Gi")
	if info.ObservedPeak == nil {
		t.Error("Expected observed peak to be set after concurrent writes")
	}
}

func TestTracker_GetUtilization_DifferentNamespaces(t *testing.T) {
	tracker := NewTracker()

	// Record usage for models with same name but different namespaces
	observedA := resource.MustParse("60Gi")
	tracker.RecordUsage("model-a", "namespace-1", &observedA)

	observedB := resource.MustParse("70Gi")
	tracker.RecordUsage("model-a", "namespace-2", &observedB)

	// Get utilization for each - should be independent
	info1 := tracker.GetUtilization("model-a", "namespace-1", "80Gi")
	info2 := tracker.GetUtilization("model-a", "namespace-2", "80Gi")

	if info1.ObservedPeak.Cmp(observedA) != 0 {
		t.Errorf("Expected namespace-1 peak %v, got %v", observedA, info1.ObservedPeak)
	}
	if info2.ObservedPeak.Cmp(observedB) != 0 {
		t.Errorf("Expected namespace-2 peak %v, got %v", observedB, info2.ObservedPeak)
	}
}
