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

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/cecil-the-coder/inference-budget-controller/api/v1alpha1"
	"github.com/cecil-the-coder/inference-budget-controller/internal/metrics"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// HeaderContentType is the content type header
	HeaderContentType = "Content-Type"
	// ContentTypeJSON is the JSON content type
	ContentTypeJSON = "application/json"
	// ContentTypeSSE is the Server-Sent Events content type
	ContentTypeSSE = "text/event-stream"
)

// chatCompletionsHandler handles OpenAI-compatible chat completion requests
func (s *Server) chatCompletionsHandler(c *gin.Context) {
	startTime := time.Now()

	// Read the raw body first so we can forward it later
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: ErrorDetail{
				Message: "Failed to read request body: " + err.Error(),
				Type:    "invalid_request_error",
				Code:    "invalid_request",
			},
		})
		return
	}
	// Restore the body for ShouldBindJSON
	c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	var req ChatCompletionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: ErrorDetail{
				Message: "Invalid request: " + err.Error(),
				Type:    "invalid_request_error",
				Code:    "invalid_request",
			},
		})
		return
	}

	ctx := c.Request.Context()
	logger := log.FromContext(ctx).WithValues("model", req.Model)

	// 1. Look up the InferenceModel CRD by model name
	model, err := s.lookupInferenceModelByName(ctx, req.Model)
	if err != nil {
		logger.Error(err, "Failed to lookup InferenceModel")
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: ErrorDetail{
				Message: "Failed to lookup model: " + err.Error(),
				Type:    "server_error",
				Code:    "internal_error",
			},
		})
		return
	}

	if model == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: ErrorDetail{
				Message: fmt.Sprintf("Model '%s' not found", req.Model),
				Type:    "invalid_request_error",
				Code:    "model_not_found",
			},
		})
		return
	}

	logger = logger.WithValues("inferenceModel", model.Name, "namespace", model.Namespace)

	// Log with structured fields including memory info
	logger.Info("Processing chat completion request",
		"model", req.Model,
		"memory_declared", model.Spec.Memory,
		"memory_observed", model.Status.ObservedPeakMemory,
		"utilization_percent", model.Status.UtilizationPercent,
	)

	// 2. Check if model is ready
	if !model.Status.Ready {
		// Model is not ready, check if we can scale it up
		if !s.canScaleUp(ctx, model) {
			// Cannot allocate memory, return 429
			s.handleInsufficientMemory(c, model)
			return
		}

		// Trigger scale-up and wait for readiness
		if err := s.triggerScaleUp(ctx, model); err != nil {
			logger.Error(err, "Failed to trigger scale-up")
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Error: ErrorDetail{
					Message: "Failed to scale up model: " + err.Error(),
					Type:    "server_error",
					Code:    "scale_up_failed",
				},
			})
			return
		}

		// Wait for model to become ready
		if err := s.waitForModelReady(ctx, model); err != nil {
			logger.Error(err, "Model failed to become ready")
			c.JSON(http.StatusServiceUnavailable, ErrorResponse{
				Error: ErrorDetail{
					Message: "Model failed to become ready: " + err.Error(),
					Type:    "server_error",
					Code:    "model_not_ready",
				},
			})
			return
		}
	}

	// 3. Forward request to backend
	s.forwardRequest(c, model, bodyBytes, &req, startTime)
}

// completionsHandler handles OpenAI-compatible completion requests
func (s *Server) completionsHandler(c *gin.Context) {
	startTime := time.Now()

	// Read the raw body first so we can forward it later
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: ErrorDetail{
				Message: "Failed to read request body: " + err.Error(),
				Type:    "invalid_request_error",
				Code:    "invalid_request",
			},
		})
		return
	}
	// Restore the body for ShouldBindJSON
	c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	var req CompletionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: ErrorDetail{
				Message: "Invalid request: " + err.Error(),
				Type:    "invalid_request_error",
				Code:    "invalid_request",
			},
		})
		return
	}

	ctx := c.Request.Context()
	logger := log.FromContext(ctx).WithValues("model", req.Model)

	// 1. Look up the InferenceModel CRD by model name
	model, err := s.lookupInferenceModelByName(ctx, req.Model)
	if err != nil {
		logger.Error(err, "Failed to lookup InferenceModel")
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: ErrorDetail{
				Message: "Failed to lookup model: " + err.Error(),
				Type:    "server_error",
				Code:    "internal_error",
			},
		})
		return
	}

	if model == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: ErrorDetail{
				Message: fmt.Sprintf("Model '%s' not found", req.Model),
				Type:    "invalid_request_error",
				Code:    "model_not_found",
			},
		})
		return
	}

	logger = logger.WithValues("inferenceModel", model.Name, "namespace", model.Namespace)

	// Log with structured fields including memory info
	logger.Info("Processing completion request",
		"model", req.Model,
		"memory_declared", model.Spec.Memory,
		"memory_observed", model.Status.ObservedPeakMemory,
		"utilization_percent", model.Status.UtilizationPercent,
	)

	// 2. Check if model is ready
	if !model.Status.Ready {
		// Model is not ready, check if we can scale it up
		if !s.canScaleUp(ctx, model) {
			// Cannot allocate memory, return 429
			s.handleInsufficientMemory(c, model)
			return
		}

		// Trigger scale-up and wait for readiness
		if err := s.triggerScaleUp(ctx, model); err != nil {
			logger.Error(err, "Failed to trigger scale-up")
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Error: ErrorDetail{
					Message: "Failed to scale up model: " + err.Error(),
					Type:    "server_error",
					Code:    "scale_up_failed",
				},
			})
			return
		}

		// Wait for model to become ready
		if err := s.waitForModelReady(ctx, model); err != nil {
			logger.Error(err, "Model failed to become ready")
			c.JSON(http.StatusServiceUnavailable, ErrorResponse{
				Error: ErrorDetail{
					Message: "Model failed to become ready: " + err.Error(),
					Type:    "server_error",
					Code:    "model_not_ready",
				},
			})
			return
		}
	}

	// 3. Forward request to backend
	s.forwardCompletionRequest(c, model, bodyBytes, &req, startTime)
}

// listModelsHandler returns list of available models
func (s *Server) listModelsHandler(c *gin.Context) {
	ctx := c.Request.Context()

	modelList, err := s.listAllInferenceModels(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: ErrorDetail{
				Message: "Failed to list models: " + err.Error(),
				Type:    "server_error",
				Code:    "internal_error",
			},
		})
		return
	}

	var models []ModelInfo
	for _, item := range modelList.Items {
		models = append(models, ModelInfo{
			ID:      item.Spec.ModelName,
			Object:  "model",
			Created: item.CreationTimestamp.Unix(),
			OwnedBy: "eh-ops",
		})
	}

	c.JSON(http.StatusOK, ModelsResponse{
		Object: "list",
		Data:   models,
	})
}

// canScaleUp checks if there is enough memory budget to scale up a model
func (s *Server) canScaleUp(ctx context.Context, model *inferencev1alpha1.InferenceModel) bool {
	return s.Tracker.CanAllocate(model.Name, model.Namespace, model.Spec.Memory, model.Spec.NodeSelector)
}

// handleInsufficientMemory handles the case when there's not enough memory
func (s *Server) handleInsufficientMemory(c *gin.Context, model *inferencev1alpha1.InferenceModel) {
	ctx := c.Request.Context()
	logger := log.FromContext(ctx)

	requestedMemory := resource.MustParse(model.Spec.Memory)
	availableMemory := s.Tracker.GetAvailableMemory(model.Spec.NodeSelector)
	blockingModels := s.Tracker.GetBlockingModels(model.Spec.NodeSelector, &requestedMemory)

	// Build blocking models info with idle times
	var blocking []BlockingModel
	for _, b := range blockingModels {
		idleFor := s.IdleTracker.GetIdleDuration(b.Namespace, b.Name)
		blocking = append(blocking, BlockingModel{
			Model:   b.Name,
			Memory:  b.Memory.String(),
			IdleFor: idleFor.String(),
		})
	}

	// Log with structured fields
	logger.Info("Insufficient memory for model",
		"model", model.Spec.ModelName,
		"namespace", model.Namespace,
		"requested", model.Spec.Memory,
		"available", availableMemory.String(),
		"blocking", len(blocking),
		"node", getNodeSelectorKey(model.Spec.NodeSelector),
	)

	// Record admission rejection metrics
	if s.Metrics != nil {
		s.Metrics.RecordAdmission(model.Name, model.Namespace, metrics.AdmissionRejected)
		s.Metrics.RecordAdmissionRejection(model.Name, model.Namespace, metrics.ReasonInsufficientMemory)
	}

	c.JSON(http.StatusTooManyRequests, gin.H{
		"error": gin.H{
			"type":    "insufficient_memory",
			"message": fmt.Sprintf("Cannot schedule %s: insufficient memory", model.Spec.ModelName),
			"code":    "insufficient_memory",
			"details": gin.H{
				"requested": gin.H{
					"model":  model.Spec.ModelName,
					"memory": model.Spec.Memory,
				},
				"available": availableMemory.String(),
				"blocking":  blocking,
			},
		},
	})
}

// triggerScaleUp triggers the scale-up of a model by updating its deployment
func (s *Server) triggerScaleUp(ctx context.Context, model *inferencev1alpha1.InferenceModel) error {
	logger := log.FromContext(ctx)

	// Allocate memory budget first
	if !s.Tracker.Allocate(model.Name, model.Namespace, model.Spec.Memory, model.Spec.NodeSelector) {
		return fmt.Errorf("failed to allocate memory budget")
	}

	// Record admission acceptance metrics
	if s.Metrics != nil {
		s.Metrics.RecordAdmission(model.Name, model.Namespace, metrics.AdmissionAccepted)
	}

	// Check if deployment exists
	deployment := &appsv1.Deployment{}
	deploymentKey := client.ObjectKey{Name: model.Name, Namespace: model.Namespace}

	err := s.K8sClient.Get(ctx, deploymentKey, deployment)
	if err != nil {
		if errors.IsNotFound(err) {
			// Deployment doesn't exist, we need to create it
			// This would typically be handled by the controller, but we can signal it
			logger.Info("Deployment not found, controller should create it",
				"deployment", model.Name,
				"namespace", model.Namespace,
				"memory_declared", model.Spec.Memory,
				"node", getNodeSelectorKey(model.Spec.NodeSelector),
			)
			return nil
		}
		return fmt.Errorf("failed to get deployment: %w", err)
	}

	// If deployment exists but is scaled to zero, scale it up
	if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 0 {
		replicas := int32(1)
		deployment.Spec.Replicas = &replicas
		if err := s.K8sClient.Update(ctx, deployment); err != nil {
			return fmt.Errorf("failed to scale up deployment: %w", err)
		}
		logger.Info("Triggered scale-up for deployment",
			"deployment", model.Name,
			"namespace", model.Namespace,
			"memory_declared", model.Spec.Memory,
		)
	}

	return nil
}

// waitForModelReady waits for the model to become ready with timeout
func (s *Server) waitForModelReady(ctx context.Context, model *inferencev1alpha1.InferenceModel) error {
	logger := log.FromContext(ctx)
	timeout := time.After(s.ScaleUpTimeout)
	ticker := time.NewTicker(s.ReadyCheckDelay)
	defer ticker.Stop()

	modelKey := types.NamespacedName{Name: model.Name, Namespace: model.Namespace}

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for model to become ready")
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// Re-fetch the model to check status
			currentModel := &inferencev1alpha1.InferenceModel{}
			if err := s.K8sClient.Get(ctx, modelKey, currentModel); err != nil {
				logger.Error(err, "Failed to get model status during wait")
				continue
			}

			if currentModel.Status.Ready {
				logger.Info("Model is now ready",
					"model", model.Name,
					"namespace", model.Namespace,
					"memory_declared", model.Spec.Memory,
					"memory_observed", currentModel.Status.ObservedPeakMemory,
					"utilization_percent", currentModel.Status.UtilizationPercent,
				)
				return nil
			}

			logger.V(1).Info("Waiting for model to become ready",
				"model", model.Name,
				"namespace", model.Namespace,
				"replicas", currentModel.Status.Replicas,
				"ready_replicas", currentModel.Status.Ready,
			)
		}
	}
}

// forwardRequest forwards a chat completion request to the backend
func (s *Server) forwardRequest(c *gin.Context, model *inferencev1alpha1.InferenceModel, bodyBytes []byte, req *ChatCompletionRequest, startTime time.Time) {
	ctx := c.Request.Context()
	logger := log.FromContext(ctx)

	// Create request to backend
	backendReq, err := http.NewRequestWithContext(ctx, "POST", model.Spec.BackendURL+"/v1/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		s.recordRequestMetrics(model, startTime, metrics.StatusError)
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: ErrorDetail{
				Message: "Failed to create backend request: " + err.Error(),
				Type:    "server_error",
				Code:    "internal_error",
			},
		})
		return
	}

	// Copy headers
	for key, values := range c.Request.Header {
		for _, value := range values {
			backendReq.Header.Add(key, value)
		}
	}

	// Send request to backend
	httpClient := &http.Client{
		Timeout: 10 * time.Minute, // Long timeout for inference
	}

	resp, err := httpClient.Do(backendReq)
	if err != nil {
		logger.Error(err, "Failed to forward request to backend",
			"model", model.Name,
			"namespace", model.Namespace,
			"backend_url", model.Spec.BackendURL,
		)
		s.recordRequestMetrics(model, startTime, metrics.StatusError)
		c.JSON(http.StatusBadGateway, ErrorResponse{
			Error: ErrorDetail{
				Message: "Failed to connect to backend: " + err.Error(),
				Type:    "server_error",
				Code:    "backend_unavailable",
			},
		})
		return
	}
	defer resp.Body.Close()

	// Update idle tracker
	s.IdleTracker.RecordRequest(model.Namespace, model.Name)

	// Record successful request metrics
	s.recordRequestMetrics(model, startTime, metrics.StatusSuccess)

	// Log request completion
	duration := time.Since(startTime)
	logger.Info("Request completed",
		"model", model.Name,
		"namespace", model.Namespace,
		"status_code", resp.StatusCode,
		"duration_ms", duration.Milliseconds(),
		"memory_declared", model.Spec.Memory,
		"memory_observed", model.Status.ObservedPeakMemory,
	)

	// Handle streaming response
	if req.Stream {
		s.handleStreamingResponse(c, resp)
		return
	}

	// Handle non-streaming response
	s.handleNonStreamingResponse(c, resp)
}

// recordRequestMetrics records metrics for a request
func (s *Server) recordRequestMetrics(model *inferencev1alpha1.InferenceModel, startTime time.Time, status metrics.RequestStatus) {
	if s.Metrics == nil {
		return
	}
	duration := time.Since(startTime)
	s.Metrics.RecordRequest(model.Name, model.Namespace, status, duration)
}

// forwardCompletionRequest forwards a completion request to the backend
func (s *Server) forwardCompletionRequest(c *gin.Context, model *inferencev1alpha1.InferenceModel, bodyBytes []byte, req *CompletionRequest, startTime time.Time) {
	ctx := c.Request.Context()
	logger := log.FromContext(ctx)

	// Create request to backend
	backendReq, err := http.NewRequestWithContext(ctx, "POST", model.Spec.BackendURL+"/v1/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		s.recordRequestMetrics(model, startTime, metrics.StatusError)
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: ErrorDetail{
				Message: "Failed to create backend request: " + err.Error(),
				Type:    "server_error",
				Code:    "internal_error",
			},
		})
		return
	}

	// Copy headers
	for key, values := range c.Request.Header {
		for _, value := range values {
			backendReq.Header.Add(key, value)
		}
	}

	// Send request to backend
	httpClient := &http.Client{
		Timeout: 10 * time.Minute,
	}

	resp, err := httpClient.Do(backendReq)
	if err != nil {
		logger.Error(err, "Failed to forward request to backend",
			"model", model.Name,
			"namespace", model.Namespace,
			"backend_url", model.Spec.BackendURL,
		)
		s.recordRequestMetrics(model, startTime, metrics.StatusError)
		c.JSON(http.StatusBadGateway, ErrorResponse{
			Error: ErrorDetail{
				Message: "Failed to connect to backend: " + err.Error(),
				Type:    "server_error",
				Code:    "backend_unavailable",
			},
		})
		return
	}
	defer resp.Body.Close()

	// Update idle tracker
	s.IdleTracker.RecordRequest(model.Namespace, model.Name)

	// Record successful request metrics
	s.recordRequestMetrics(model, startTime, metrics.StatusSuccess)

	// Log request completion
	duration := time.Since(startTime)
	logger.Info("Request completed",
		"model", model.Name,
		"namespace", model.Namespace,
		"status_code", resp.StatusCode,
		"duration_ms", duration.Milliseconds(),
		"memory_declared", model.Spec.Memory,
		"memory_observed", model.Status.ObservedPeakMemory,
	)

	// Handle streaming response
	if req.Stream {
		s.handleStreamingResponse(c, resp)
		return
	}

	// Handle non-streaming response
	s.handleNonStreamingResponse(c, resp)
}

// handleStreamingResponse handles a streaming response from the backend
func (s *Server) handleStreamingResponse(c *gin.Context, resp *http.Response) {
	// Set headers for SSE
	c.Header(HeaderContentType, ContentTypeSSE)
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("Transfer-Encoding", "chunked")

	// Stream the response
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: ErrorDetail{
				Message: "Streaming not supported",
				Type:    "server_error",
				Code:    "streaming_not_supported",
			},
		})
		return
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			break
		}

		c.Writer.WriteString(line)
		flusher.Flush()
	}
}

// handleNonStreamingResponse handles a non-streaming response from the backend
func (s *Server) handleNonStreamingResponse(c *gin.Context, resp *http.Response) {
	// Copy status code
	c.Status(resp.StatusCode)

	// Copy headers
	for key, values := range resp.Header {
		for _, value := range values {
			c.Header(key, value)
		}
	}

	// Copy body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: ErrorDetail{
				Message: "Failed to read backend response: " + err.Error(),
				Type:    "server_error",
				Code:    "internal_error",
			},
		})
		return
	}

	c.Writer.Write(body)
}

// unmarshalChatRequest unmarshals a chat completion request from raw bytes
func unmarshalChatRequest(body []byte) (*ChatCompletionRequest, error) {
	var req ChatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// unmarshalCompletionRequest unmarshals a completion request from raw bytes
func unmarshalCompletionRequest(body []byte) (*CompletionRequest, error) {
	var req CompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// getNodeSelectorKey generates a key from node selector for logging
func getNodeSelectorKey(nodeSelector map[string]string) string {
	if nodeSelector == nil {
		return "default"
	}
	// Simple implementation - use first key-value pair
	for k, v := range nodeSelector {
		return k + "=" + v
	}
	return "default"
}
