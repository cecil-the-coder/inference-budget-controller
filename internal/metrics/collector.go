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

package metrics

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	inferencev1alpha1 "github.com/cecil-the-coder/inference-budget-controller/api/v1alpha1"
)

// Collector manages Prometheus metrics for the inference budget controller
type Collector struct {
	// Memory metrics
	memoryDeclared     *prometheus.GaugeVec
	memoryObservedPeak *prometheus.GaugeVec
	memoryUtilization  *prometheus.GaugeVec

	// Controller metrics
	modelReplicas        *prometheus.GaugeVec
	modelRequestsTotal   *prometheus.CounterVec
	modelRequestDuration *prometheus.HistogramVec

	// Admission control metrics
	admissionRequestsTotal *prometheus.CounterVec
	admissionRejectedTotal *prometheus.CounterVec

	// Budget metrics (node-level)
	nodeMemoryTotal     *prometheus.GaugeVec
	nodeMemoryAvailable *prometheus.GaugeVec
	nodeMemoryUsed      *prometheus.GaugeVec

	// Model state metrics
	modelReady *prometheus.GaugeVec

	// Legacy budget metrics (kept for backward compatibility)
	budgetTotal     *prometheus.GaugeVec
	budgetAllocated *prometheus.GaugeVec
	budgetAvailable *prometheus.GaugeVec

	// Recommendation logger
	recommendationLogger *RecommendationLogger
}

// AdmissionResult represents the result of an admission request
type AdmissionResult string

const (
	AdmissionAccepted AdmissionResult = "accepted"
	AdmissionRejected AdmissionResult = "rejected"
)

// AdmissionReason represents the reason for rejection
type AdmissionReason string

const (
	ReasonInsufficientMemory AdmissionReason = "insufficient_memory"
	ReasonBudgetExceeded     AdmissionReason = "budget_exceeded"
	ReasonNodeCapacity       AdmissionReason = "node_capacity"
)

// RequestStatus represents the status of a request
type RequestStatus string

const (
	StatusSuccess RequestStatus = "success"
	StatusError   RequestStatus = "error"
)

// RecommendationLogger handles periodic logging of overprovisioning recommendations
type RecommendationLogger struct {
	mu       sync.RWMutex
	models   map[string]modelUtilization
	interval time.Duration
	stopCh   chan struct{}
}

type modelUtilization struct {
	name              string
	namespace         string
	declaredBytes     int64
	observedPeakBytes int64
	utilization       float64
}

// NewRecommendationLogger creates a new recommendation logger
func NewRecommendationLogger(interval time.Duration) *RecommendationLogger {
	if interval == 0 {
		interval = 5 * time.Minute
	}
	return &RecommendationLogger{
		models:   make(map[string]modelUtilization),
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

// Start begins the periodic logging of recommendations
func (rl *RecommendationLogger) Start(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("recommendation-logger")

	ticker := time.NewTicker(rl.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-rl.stopCh:
			return
		case <-ticker.C:
			rl.logRecommendations(logger)
		}
	}
}

// Stop stops the recommendation logger
func (rl *RecommendationLogger) Stop() {
	close(rl.stopCh)
}

// UpdateModel updates the utilization data for a model
func (rl *RecommendationLogger) UpdateModel(name, namespace string, declaredBytes, observedPeakBytes int64, utilization float64) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	key := name + "/" + namespace
	rl.models[key] = modelUtilization{
		name:              name,
		namespace:         namespace,
		declaredBytes:     declaredBytes,
		observedPeakBytes: observedPeakBytes,
		utilization:       utilization,
	}
}

// RemoveModel removes a model from tracking
func (rl *RecommendationLogger) RemoveModel(name, namespace string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	key := name + "/" + namespace
	delete(rl.models, key)
}

func (rl *RecommendationLogger) logRecommendations(logger logr.Logger) {
	rl.mu.RLock()
	defer rl.mu.RUnlock()

	for _, m := range rl.models {
		// Only log for models with significant overprovisioning (> 10%)
		if m.utilization < 0.9 && m.utilization > 0 {
			overprovisionedPercent := int((1 - m.utilization) * 100)
			declaredStr := formatBytes(m.declaredBytes)
			observedStr := formatBytes(m.observedPeakBytes)

			logger.Info("Overprovisioning hint",
				"model", m.name,
				"namespace", m.namespace,
				"declared", declaredStr,
				"observed_peak", observedStr,
				"utilization_percent", int(m.utilization*100),
				"overprovisioned_percent", overprovisionedPercent,
			)
		}
	}
}

// formatBytes formats bytes to human-readable string
func formatBytes(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
		TB = GB * 1024
	)

	switch {
	case bytes >= TB:
		return fmt.Sprintf("%dTi", bytes/TB)
	case bytes >= GB:
		return fmt.Sprintf("%dGi", bytes/GB)
	case bytes >= MB:
		return fmt.Sprintf("%dMi", bytes/MB)
	case bytes >= KB:
		return fmt.Sprintf("%dKi", bytes/KB)
	default:
		return fmt.Sprintf("%d", bytes)
	}
}

// NewCollector creates a new metrics collector
func NewCollector() *Collector {
	c := &Collector{
		// Memory metrics
		memoryDeclared: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "inference_model_memory_declared_bytes",
				Help: "Memory declared in the CRD",
			},
			[]string{"model", "namespace"},
		),
		memoryObservedPeak: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "inference_model_memory_observed_peak_bytes",
				Help: "Peak memory observed at runtime",
			},
			[]string{"model", "namespace"},
		),
		memoryUtilization: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "inference_model_memory_utilization_ratio",
				Help: "Ratio of observed to declared memory (0-1)",
			},
			[]string{"model", "namespace"},
		),

		// Controller metrics
		modelReplicas: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "inference_model_replicas",
				Help: "Current number of replicas for the model",
			},
			[]string{"model", "namespace"},
		),
		modelRequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "inference_model_requests_total",
				Help: "Total number of requests to models",
			},
			[]string{"model", "namespace", "status"},
		),
		modelRequestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "inference_model_request_duration_seconds",
				Help:    "Request latency histogram",
				Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600},
			},
			[]string{"model", "namespace"},
		),

		// Admission control metrics
		admissionRequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "inference_admission_requests_total",
				Help: "Total number of admission requests",
			},
			[]string{"model", "namespace", "result"},
		),
		admissionRejectedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "inference_admission_rejected_total",
				Help: "Total number of rejected admission requests by reason",
			},
			[]string{"model", "namespace", "reason"},
		),

		// Budget metrics (node-level)
		nodeMemoryTotal: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "inference_node_memory_total_bytes",
				Help: "Total memory capacity for a node pool",
			},
			[]string{"node"},
		),
		nodeMemoryAvailable: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "inference_node_memory_available_bytes",
				Help: "Available memory in a node pool",
			},
			[]string{"node"},
		),
		nodeMemoryUsed: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "inference_node_memory_used_bytes",
				Help: "Used memory in a node pool",
			},
			[]string{"node"},
		),

		// Model state metrics
		modelReady: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "inference_model_ready",
				Help: "Whether the model is ready to serve requests (1=ready, 0=not ready)",
			},
			[]string{"model", "namespace"},
		),

		// Legacy budget metrics (kept for backward compatibility)
		budgetTotal: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "inference_budget_total_bytes",
				Help: "Total memory budget for a node pool",
			},
			[]string{"node_pool"},
		),
		budgetAllocated: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "inference_budget_allocated_bytes",
				Help: "Allocated memory in a node pool",
			},
			[]string{"node_pool"},
		),
		budgetAvailable: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "inference_budget_available_bytes",
				Help: "Available memory in a node pool",
			},
			[]string{"node_pool"},
		),

		// Initialize recommendation logger
		recommendationLogger: NewRecommendationLogger(5 * time.Minute),
	}

	// Register metrics with the controller-runtime metrics registry
	metrics.Registry.MustRegister(
		c.memoryDeclared,
		c.memoryObservedPeak,
		c.memoryUtilization,
		c.modelReplicas,
		c.modelRequestsTotal,
		c.modelRequestDuration,
		c.admissionRequestsTotal,
		c.admissionRejectedTotal,
		c.nodeMemoryTotal,
		c.nodeMemoryAvailable,
		c.nodeMemoryUsed,
		c.modelReady,
		c.budgetTotal,
		c.budgetAllocated,
		c.budgetAvailable,
	)

	return c
}

// StartRecommendationLogger starts the periodic recommendation logger
func (c *Collector) StartRecommendationLogger(ctx context.Context) {
	go c.recommendationLogger.Start(ctx)
}

// UpdateModelMetrics updates metrics for a specific model
func (c *Collector) UpdateModelMetrics(model *inferencev1alpha1.InferenceModel) {
	labels := prometheus.Labels{
		"model":     model.Name,
		"namespace": model.Namespace,
	}

	// Parse declared memory
	declaredBytes := parseMemoryToBytes(model.Spec.Resources.Memory)
	c.memoryDeclared.With(labels).Set(float64(declaredBytes))

	// Update ready status
	if model.Status.Ready {
		c.modelReady.With(labels).Set(1)
	} else {
		c.modelReady.With(labels).Set(0)
	}

	// Update replica count
	c.modelReplicas.With(labels).Set(float64(model.Status.Replicas))

	// Update observed peak if available
	var observedBytes int64
	if model.Status.ObservedPeakMemory != "" {
		observedBytes = parseMemoryToBytes(model.Status.ObservedPeakMemory)
		c.memoryObservedPeak.With(labels).Set(float64(observedBytes))

		// Calculate utilization
		if declaredBytes > 0 {
			utilization := float64(observedBytes) / float64(declaredBytes)
			c.memoryUtilization.With(labels).Set(utilization)

			// Update recommendation logger
			c.recommendationLogger.UpdateModel(
				model.Name,
				model.Namespace,
				declaredBytes,
				observedBytes,
				utilization,
			)
		}
	} else {
		// Clear from recommendation logger if no observed data
		c.recommendationLogger.RemoveModel(model.Name, model.Namespace)
	}
}

// DeleteModelMetrics removes metrics for a deleted model
func (c *Collector) DeleteModelMetrics(name, namespace string) {
	labels := prometheus.Labels{
		"model":     name,
		"namespace": namespace,
	}

	c.memoryDeclared.Delete(labels)
	c.memoryObservedPeak.Delete(labels)
	c.memoryUtilization.Delete(labels)
	c.modelReady.Delete(labels)
	c.modelReplicas.Delete(labels)
	c.modelRequestsTotal.DeleteLabelValues(name, namespace, string(StatusSuccess))
	c.modelRequestsTotal.DeleteLabelValues(name, namespace, string(StatusError))
	c.modelRequestDuration.Delete(labels)
	c.admissionRequestsTotal.DeleteLabelValues(name, namespace, string(AdmissionAccepted))
	c.admissionRequestsTotal.DeleteLabelValues(name, namespace, string(AdmissionRejected))

	// Remove from recommendation logger
	c.recommendationLogger.RemoveModel(name, namespace)
}

// RecordRequest records a request to a model
func (c *Collector) RecordRequest(name, namespace string, status RequestStatus, duration time.Duration) {
	labels := prometheus.Labels{
		"model":     name,
		"namespace": namespace,
		"status":    string(status),
	}

	c.modelRequestsTotal.With(labels).Inc()

	durationLabels := prometheus.Labels{
		"model":     name,
		"namespace": namespace,
	}
	c.modelRequestDuration.With(durationLabels).Observe(duration.Seconds())
}

// RecordAdmission records an admission request result
func (c *Collector) RecordAdmission(name, namespace string, result AdmissionResult) {
	labels := prometheus.Labels{
		"model":     name,
		"namespace": namespace,
		"result":    string(result),
	}

	c.admissionRequestsTotal.With(labels).Inc()
}

// RecordAdmissionRejection records an admission rejection with reason
func (c *Collector) RecordAdmissionRejection(name, namespace string, reason AdmissionReason) {
	labels := prometheus.Labels{
		"model":     name,
		"namespace": namespace,
		"reason":    string(reason),
	}

	c.admissionRejectedTotal.With(labels).Inc()
}

// UpdateNodeMemoryMetrics updates node-level memory metrics
func (c *Collector) UpdateNodeMemoryMetrics(node string, total, used, available int64) {
	labels := prometheus.Labels{
		"node": node,
	}

	c.nodeMemoryTotal.With(labels).Set(float64(total))
	c.nodeMemoryUsed.With(labels).Set(float64(used))
	c.nodeMemoryAvailable.With(labels).Set(float64(available))
}

// UpdateBudgetMetrics updates budget metrics for a node pool
func (c *Collector) UpdateBudgetMetrics(nodePool string, total, allocated, available int64) {
	labels := prometheus.Labels{
		"node_pool": nodePool,
	}

	c.budgetTotal.With(labels).Set(float64(total))
	c.budgetAllocated.With(labels).Set(float64(allocated))
	c.budgetAvailable.With(labels).Set(float64(available))

	// Also update node-level metrics with node_pool as node identifier
	c.UpdateNodeMemoryMetrics(nodePool, total, allocated, available)
}

// SetupWithManager sets up the collector with the controller manager
func (c *Collector) SetupWithManager(mgr interface{}) error {
	// Metrics are already registered, nothing else needed
	return nil
}

// parseMemoryToBytes converts Kubernetes memory string to bytes
func parseMemoryToBytes(memory string) int64 {
	if memory == "" {
		return 0
	}

	// Try using Kubernetes resource quantity parsing
	if q, err := resource.ParseQuantity(memory); err == nil {
		return q.Value()
	}

	// Fallback to simple implementation - handle common formats
	switch {
	case len(memory) >= 3 && memory[len(memory)-3:] == "TiB":
		var tb int64
		if _, err := fmt.Sscanf(memory, "%dTiB", &tb); err == nil {
			return tb * 1024 * 1024 * 1024 * 1024
		}
	case len(memory) >= 2 && memory[len(memory)-2:] == "Ti":
		var tb int64
		if _, err := fmt.Sscanf(memory, "%dTi", &tb); err == nil {
			return tb * 1024 * 1024 * 1024 * 1024
		}
	case len(memory) >= 3 && memory[len(memory)-3:] == "GiB":
		var gb int64
		if _, err := fmt.Sscanf(memory, "%dGiB", &gb); err == nil {
			return gb * 1024 * 1024 * 1024
		}
	case len(memory) >= 2 && memory[len(memory)-2:] == "Gi":
		var gb int64
		if _, err := fmt.Sscanf(memory, "%dGi", &gb); err == nil {
			return gb * 1024 * 1024 * 1024
		}
	case len(memory) >= 3 && memory[len(memory)-3:] == "MiB":
		var mb int64
		if _, err := fmt.Sscanf(memory, "%dMiB", &mb); err == nil {
			return mb * 1024 * 1024
		}
	case len(memory) >= 2 && memory[len(memory)-2:] == "Mi":
		var mb int64
		if _, err := fmt.Sscanf(memory, "%dMi", &mb); err == nil {
			return mb * 1024 * 1024
		}
	case len(memory) >= 3 && memory[len(memory)-3:] == "KiB":
		var kb int64
		if _, err := fmt.Sscanf(memory, "%dKiB", &kb); err == nil {
			return kb * 1024
		}
	case len(memory) >= 2 && memory[len(memory)-2:] == "Ki":
		var kb int64
		if _, err := fmt.Sscanf(memory, "%dKi", &kb); err == nil {
			return kb * 1024
		}
	}

	return 0
}
