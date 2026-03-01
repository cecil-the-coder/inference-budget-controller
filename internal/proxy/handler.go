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
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/cecil-the-coder/inference-budget-controller/api/v1alpha1"
	"github.com/cecil-the-coder/inference-budget-controller/internal/metrics"
	"github.com/cecil-the-coder/inference-budget-controller/internal/registry"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// HeaderContentType is the content type header
	HeaderContentType = "Content-Type"
	// ContentTypeJSON is the JSON content type
	ContentTypeJSON = "application/json"
	// ContentTypeSSE is the Server-Sent Events content type
	ContentTypeSSE = "text/event-stream"
)

// openaiPassthroughHandler returns a gin handler that extracts the model name,
// performs budget/scale-up checks, and forwards the raw request body to the
// given backend path.
func (s *Server) openaiPassthroughHandler(backendPath string) gin.HandlerFunc {
	return func(c *gin.Context) {
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
		var req inferenceRequest
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
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
		logger.Info("Processing inference request",
			"model", req.Model,
			"memory_declared", model.Spec.Resources.Memory,
			"memory_observed", model.Status.ObservedPeakMemory,
			"utilization_percent", model.Status.UtilizationPercent,
		)

		namespace := model.Namespace
		modelName := model.Name

		// 2. Check registry for deployment state and ensure deployment exists
		if s.Registry != nil {
			// IMPORTANT: Record request BEFORE ensuring deployment to prevent
			// the controller from deleting the deployment while we wait for it
			// to become ready. This increments ActiveRequests, which the controller
			// checks before deleting idle deployments.
			s.Registry.RecordRequest(namespace, modelName)
			requestRecorded := true

			entry := s.Registry.Get(namespace, modelName)
			var state registry.DeploymentState
			if entry == nil {
				state = registry.StateNonexistent
			} else {
				state = entry.State
			}

			switch state {
			case registry.StateNonexistent:
				// Deployment doesn't exist, need to create it
				logger.Info("Deployment nonexistent, creating on-demand",
					"namespace", namespace,
					"model", modelName,
				)

				if err := s.ensureDeployment(ctx, model); err != nil {
					logger.Error(err, "Failed to ensure deployment")
					// Decrement since we won't proceed to forward
					s.Registry.FinishRequest(namespace, modelName)
					requestRecorded = false
					c.JSON(http.StatusServiceUnavailable, ErrorResponse{
						Error: ErrorDetail{
							Message: "Failed to create deployment: " + err.Error(),
							Type:    "server_error",
							Code:    "deployment_failed",
						},
					})
					return
				}

			case registry.StateCreating:
				// Deployment is being created, wait for it to be ready
				logger.Info("Deployment creating, waiting for ready state",
					"namespace", namespace,
					"model", modelName,
				)

				if err := s.waitForDeploymentReady(ctx, model); err != nil {
					logger.Error(err, "Timed out waiting for deployment to become ready")
					// Decrement since we won't proceed to forward
					s.Registry.FinishRequest(namespace, modelName)
					requestRecorded = false
					c.JSON(http.StatusServiceUnavailable, ErrorResponse{
						Error: ErrorDetail{
							Message: "Deployment is taking too long to become ready: " + err.Error(),
							Type:    "server_error",
							Code:    "deployment_timeout",
						},
					})
					return
				}

			case registry.StateReady:
				// Deployment is ready, proceed
				logger.V(1).Info("Deployment already ready",
					"namespace", namespace,
					"model", modelName,
				)

			case registry.StateDeleting:
				// Deployment is being deleted, wait and recreate
				logger.Info("Deployment is being deleted, waiting and recreating",
					"namespace", namespace,
					"model", modelName,
				)

				if err := s.waitForDeploymentDeleted(ctx, model); err != nil {
					logger.Error(err, "Failed waiting for deployment deletion")
				}

				if err := s.ensureDeployment(ctx, model); err != nil {
					logger.Error(err, "Failed to recreate deployment")
					// Decrement since we won't proceed to forward
					s.Registry.FinishRequest(namespace, modelName)
					requestRecorded = false
					c.JSON(http.StatusServiceUnavailable, ErrorResponse{
						Error: ErrorDetail{
							Message: "Failed to recreate deployment: " + err.Error(),
							Type:    "server_error",
							Code:    "deployment_failed",
						},
					})
					return
				}
			}

			// Don't call RecordRequest again at line 243-246 since we already did
			_ = requestRecorded // used for tracking
		} else {
			// Fallback to legacy behavior when registry is not available
			if !model.Status.Ready {
				if !s.canScaleUp(ctx, model) {
					s.handleInsufficientMemory(c, model)
					return
				}

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
		}

		// 3. Forward request to backend (with defer to finish request tracking)
		// Note: RecordRequest was already called earlier if Registry is available
		defer func() {
			if s.Registry != nil {
				s.Registry.FinishRequest(namespace, modelName)
			}
		}()
		s.forwardToBackend(c, model, bodyBytes, &req, backendPath, startTime)
	}
}

// multipartPassthroughHandler returns a gin handler for multipart/form-data
// endpoints (e.g. audio transcriptions) where the model field is in form data
// rather than a JSON body.
func (s *Server) multipartPassthroughHandler(backendPath string) gin.HandlerFunc {
	return func(c *gin.Context) {
		startTime := time.Now()

		modelName := c.Request.FormValue("model")
		if modelName == "" {
			c.JSON(http.StatusBadRequest, ErrorResponse{
				Error: ErrorDetail{
					Message: "Missing required field: model",
					Type:    "invalid_request_error",
					Code:    "invalid_request",
				},
			})
			return
		}

		ctx := c.Request.Context()
		logger := log.FromContext(ctx).WithValues("model", modelName)

		// 1. Look up the InferenceModel CRD by model name
		model, err := s.lookupInferenceModelByName(ctx, modelName)
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
					Message: fmt.Sprintf("Model '%s' not found", modelName),
					Type:    "invalid_request_error",
					Code:    "model_not_found",
				},
			})
			return
		}

		logger = logger.WithValues("inferenceModel", model.Name, "namespace", model.Namespace)

		logger.Info("Processing inference request",
			"model", modelName,
			"memory_declared", model.Spec.Resources.Memory,
			"memory_observed", model.Status.ObservedPeakMemory,
			"utilization_percent", model.Status.UtilizationPercent,
		)

		namespace := model.Namespace
		deploymentName := model.Name

		// 2. Check registry for deployment state and ensure deployment exists
		if s.Registry != nil {
			// IMPORTANT: Record request BEFORE ensuring deployment to prevent
			// the controller from deleting the deployment while we wait for it
			// to become ready. This increments ActiveRequests, which the controller
			// checks before deleting idle deployments.
			s.Registry.RecordRequest(namespace, deploymentName)
			requestRecorded := true

			entry := s.Registry.Get(namespace, deploymentName)
			var state registry.DeploymentState
			if entry == nil {
				state = registry.StateNonexistent
			} else {
				state = entry.State
			}

			switch state {
			case registry.StateNonexistent:
				// Deployment doesn't exist, need to create it
				logger.Info("Deployment nonexistent, creating on-demand",
					"namespace", namespace,
					"model", deploymentName,
				)

				if err := s.ensureDeployment(ctx, model); err != nil {
					logger.Error(err, "Failed to ensure deployment")
					// Decrement since we won't proceed to forward
					s.Registry.FinishRequest(namespace, deploymentName)
					requestRecorded = false
					c.JSON(http.StatusServiceUnavailable, ErrorResponse{
						Error: ErrorDetail{
							Message: "Failed to create deployment: " + err.Error(),
							Type:    "server_error",
							Code:    "deployment_failed",
						},
					})
					return
				}

			case registry.StateCreating:
				// Deployment is being created, wait for it to be ready
				logger.Info("Deployment creating, waiting for ready state",
					"namespace", namespace,
					"model", deploymentName,
				)

				if err := s.waitForDeploymentReady(ctx, model); err != nil {
					logger.Error(err, "Timed out waiting for deployment to become ready")
					// Decrement since we won't proceed to forward
					s.Registry.FinishRequest(namespace, deploymentName)
					requestRecorded = false
					c.JSON(http.StatusServiceUnavailable, ErrorResponse{
						Error: ErrorDetail{
							Message: "Deployment is taking too long to become ready: " + err.Error(),
							Type:    "server_error",
							Code:    "deployment_timeout",
						},
					})
					return
				}

			case registry.StateReady:
				// Deployment is ready, proceed
				logger.V(1).Info("Deployment already ready",
					"namespace", namespace,
					"model", deploymentName,
				)

			case registry.StateDeleting:
				// Deployment is being deleted, wait and recreate
				logger.Info("Deployment is being deleted, waiting and recreating",
					"namespace", namespace,
					"model", deploymentName,
				)

				if err := s.waitForDeploymentDeleted(ctx, model); err != nil {
					logger.Error(err, "Failed waiting for deployment deletion")
				}

				if err := s.ensureDeployment(ctx, model); err != nil {
					logger.Error(err, "Failed to recreate deployment")
					// Decrement since we won't proceed to forward
					s.Registry.FinishRequest(namespace, deploymentName)
					requestRecorded = false
					c.JSON(http.StatusServiceUnavailable, ErrorResponse{
						Error: ErrorDetail{
							Message: "Failed to recreate deployment: " + err.Error(),
							Type:    "server_error",
							Code:    "deployment_failed",
						},
					})
					return
				}
			}

			_ = requestRecorded // used for tracking
		} else {
			// Fallback to legacy behavior when registry is not available
			if !model.Status.Ready {
				if !s.canScaleUp(ctx, model) {
					s.handleInsufficientMemory(c, model)
					return
				}

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
		}

		// 3. Forward the original request to backend (preserving multipart body)
		// Note: RecordRequest was already called earlier if Registry is available
		defer func() {
			if s.Registry != nil {
				s.Registry.FinishRequest(namespace, deploymentName)
			}
		}()
		s.forwardRawRequest(c, model, backendPath, startTime)
	}
}

// forwardRawRequest forwards the original HTTP request to the backend,
// preserving the original body, content-type, and headers. Used for
// multipart/form-data requests where we cannot re-read the body from bytes.
func (s *Server) forwardRawRequest(c *gin.Context, model *inferencev1alpha1.InferenceModel, backendPath string, startTime time.Time) {
	ctx := c.Request.Context()
	logger := log.FromContext(ctx)

	backendURL := s.getBackendURL(model)

	backendReq, err := http.NewRequestWithContext(ctx, c.Request.Method, backendURL+backendPath, c.Request.Body)
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

	for key, values := range c.Request.Header {
		for _, value := range values {
			backendReq.Header.Add(key, value)
		}
	}
	backendReq.ContentLength = c.Request.ContentLength

	httpClient := &http.Client{
		Timeout: 10 * time.Minute,
	}

	resp, err := httpClient.Do(backendReq)
	if err != nil {
		logger.Error(err, "Failed to forward request to backend",
			"model", model.Name,
			"namespace", model.Namespace,
			"backend_url", backendURL,
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
	defer func() { _ = resp.Body.Close() }()

	s.IdleTracker.RecordRequest(model.Namespace, model.Name)
	s.recordRequestMetrics(model, startTime, metrics.StatusSuccess)

	duration := time.Since(startTime)
	logger.Info("Request completed",
		"model", model.Name,
		"namespace", model.Namespace,
		"status_code", resp.StatusCode,
		"duration_ms", duration.Milliseconds(),
	)

	s.handleNonStreamingResponse(c, resp)
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
	return s.Tracker.CanAllocate(model.Name, model.Namespace, model.Spec.Resources.Memory, model.Spec.NodeSelector)
}

// handleInsufficientMemory handles the case when there's not enough memory
func (s *Server) handleInsufficientMemory(c *gin.Context, model *inferencev1alpha1.InferenceModel) {
	ctx := c.Request.Context()
	logger := log.FromContext(ctx)

	requestedMemory := resource.MustParse(model.Spec.Resources.Memory)
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
		"requested", model.Spec.Resources.Memory,
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
					"memory": model.Spec.Resources.Memory,
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
	if !s.Tracker.Allocate(model.Name, model.Namespace, model.Spec.Resources.Memory, model.Spec.NodeSelector) {
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
				"memory_declared", model.Spec.Resources.Memory,
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
			"memory_declared", model.Spec.Resources.Memory,
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
					"memory_declared", model.Spec.Resources.Memory,
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

// getBackendURL constructs the backend URL for a model
func (s *Server) getBackendURL(model *inferencev1alpha1.InferenceModel) string {
	port := int32(8080)
	if model.Spec.Service.Port != nil {
		port = *model.Spec.Service.Port
	}
	return fmt.Sprintf("http://%s.%s.svc:%d", model.Name, model.Namespace, port)
}

// forwardToBackend forwards a request to the given backend path
func (s *Server) forwardToBackend(c *gin.Context, model *inferencev1alpha1.InferenceModel, bodyBytes []byte, req *inferenceRequest, backendPath string, startTime time.Time) {
	ctx := c.Request.Context()
	logger := log.FromContext(ctx)

	// Construct backend URL from model's service
	backendURL := s.getBackendURL(model)

	// Create request to backend
	backendReq, err := http.NewRequestWithContext(ctx, "POST", backendURL+backendPath, bytes.NewReader(bodyBytes))
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
			"backend_url", backendURL,
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
	defer func() { _ = resp.Body.Close() }()

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
		"memory_declared", model.Spec.Resources.Memory,
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

		if _, err := c.Writer.WriteString(line); err != nil {
			log.FromContext(c.Request.Context()).Error(err, "Failed to write streaming response")
		}
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

	if _, err := c.Writer.Write(body); err != nil {
		log.FromContext(c.Request.Context()).Error(err, "Failed to write response body")
	}
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

// deploymentMutex provides single-flight protection for deployment creation
// to prevent duplicate deployments when multiple concurrent requests come in
// for the same cold model.
var deploymentMutex sync.Map

// ensureDeployment ensures that a deployment exists and is ready for the given model.
// It uses a single-flight pattern to prevent duplicate deployment creation.
func (s *Server) ensureDeployment(ctx context.Context, model *inferencev1alpha1.InferenceModel) error {
	logger := log.FromContext(ctx)
	namespace := model.Namespace
	name := model.Name
	key := namespace + "/" + name

	// Use single-flight pattern to prevent duplicate deployment creation
	// LoadOrStore returns the existing mutex if another goroutine is already
	// creating the deployment, or stores a new mutex if this is the first.
	muRaw, _ := deploymentMutex.LoadOrStore(key, &sync.Mutex{})
	mu := muRaw.(*sync.Mutex)
	mu.Lock()
	defer func() {
		mu.Unlock()
		// Clean up the mutex after we're done
		deploymentMutex.Delete(key)
	}()

	// Double-check the registry state after acquiring the lock
	entry := s.Registry.Get(namespace, name)
	if entry != nil && entry.State == registry.StateReady {
		logger.V(1).Info("Deployment already ready after acquiring lock")
		return nil
	}

	// Check if we can allocate memory budget
	if !s.Tracker.CanAllocate(name, namespace, model.Spec.Resources.Memory, model.Spec.NodeSelector) {
		return fmt.Errorf("insufficient memory budget to create deployment")
	}

	// Get the InferenceBackend CRD
	backend := &inferencev1alpha1.InferenceBackend{}
	backendKey := client.ObjectKey{Name: model.Spec.Backend, Namespace: namespace}
	if err := s.K8sClient.Get(ctx, backendKey, backend); err != nil {
		return fmt.Errorf("failed to get InferenceBackend %s: %w", model.Spec.Backend, err)
	}

	// Set registry state to Creating before starting
	s.Registry.SetState(namespace, name, registry.StateCreating)

	// Build the deployment spec
	deployment, err := s.buildDeploymentSpec(model, backend)
	if err != nil {
		s.Registry.SetState(namespace, name, registry.StateNonexistent)
		return fmt.Errorf("failed to build deployment spec: %w", err)
	}

	// Set owner reference
	if err := controllerutil.SetControllerReference(model, deployment, s.Scheme); err != nil {
		s.Registry.SetState(namespace, name, registry.StateNonexistent)
		return fmt.Errorf("failed to set controller reference: %w", err)
	}

	// Create the deployment
	if err := s.K8sClient.Create(ctx, deployment); err != nil {
		if !errors.IsAlreadyExists(err) {
			s.Registry.SetState(namespace, name, registry.StateNonexistent)
			return fmt.Errorf("failed to create deployment: %w", err)
		}
		logger.Info("Deployment already exists, proceeding")
	}

	// Build and create the Service
	service := s.buildServiceSpec(model, backend)
	if err := controllerutil.SetControllerReference(model, service, s.Scheme); err != nil {
		logger.Error(err, "failed to set controller reference for service")
	} else if err := s.K8sClient.Create(ctx, service); err != nil {
		if !errors.IsAlreadyExists(err) {
			logger.Error(err, "failed to create service")
		}
	}

	// Allocate memory budget
	if !s.Tracker.Allocate(name, namespace, model.Spec.Resources.Memory, model.Spec.NodeSelector) {
		logger.Error(nil, "failed to allocate memory after creating deployment")
		// Try to clean up the deployment
		_ = s.K8sClient.Delete(ctx, deployment)
		s.Registry.SetState(namespace, name, registry.StateNonexistent)
		return fmt.Errorf("failed to allocate memory budget")
	}

	logger.Info("Created deployment for on-demand model",
		"namespace", namespace,
		"model", name,
		"memory", model.Spec.Resources.Memory,
		"backend", model.Spec.Backend,
	)

	// Wait for deployment to become ready
	if err := s.waitForDeploymentReady(ctx, model); err != nil {
		return fmt.Errorf("deployment failed to become ready: %w", err)
	}

	return nil
}

// waitForDeploymentReady waits for the deployment to become ready with timeout.
// It polls the deployment status until all replicas are ready or timeout is reached.
func (s *Server) waitForDeploymentReady(ctx context.Context, model *inferencev1alpha1.InferenceModel) error {
	logger := log.FromContext(ctx)
	namespace := model.Namespace
	name := model.Name

	timeout := s.ScaleUpTimeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}

	timeoutChan := time.After(timeout)
	ticker := time.NewTicker(s.ReadyCheckDelay)
	if ticker == nil {
		ticker = time.NewTicker(2 * time.Second)
	}
	defer ticker.Stop()

	deploymentKey := types.NamespacedName{Name: name, Namespace: namespace}

	for {
		select {
		case <-timeoutChan:
			s.Registry.SetState(namespace, name, registry.StateNonexistent)
			return fmt.Errorf("timeout waiting for deployment to become ready after %v", timeout)

		case <-ctx.Done():
			return ctx.Err()

		case <-ticker.C:
			deployment := &appsv1.Deployment{}
			if err := s.K8sClient.Get(ctx, deploymentKey, deployment); err != nil {
				if errors.IsNotFound(err) {
					logger.V(1).Info("Deployment not found yet, continuing to wait")
					continue
				}
				logger.Error(err, "Failed to get deployment status during wait")
				continue
			}

			// Check if deployment is ready
			if deployment.Status.ReadyReplicas > 0 &&
				deployment.Status.ReadyReplicas == deployment.Status.Replicas {
				logger.Info("Deployment is now ready",
					"namespace", namespace,
					"model", name,
					"ready_replicas", deployment.Status.ReadyReplicas,
					"replicas", deployment.Status.Replicas,
				)
				s.Registry.SetState(namespace, name, registry.StateReady)
				return nil
			}

			logger.V(1).Info("Waiting for deployment to become ready",
				"namespace", namespace,
				"model", name,
				"ready_replicas", deployment.Status.ReadyReplicas,
				"replicas", deployment.Status.Replicas,
				"updated_replicas", deployment.Status.UpdatedReplicas,
				"available_replicas", deployment.Status.AvailableReplicas,
			)
		}
	}
}

// waitForDeploymentDeleted waits for the deployment to be deleted.
func (s *Server) waitForDeploymentDeleted(ctx context.Context, model *inferencev1alpha1.InferenceModel) error {
	logger := log.FromContext(ctx)
	namespace := model.Namespace
	name := model.Name

	timeout := 30 * time.Second
	timeoutChan := time.After(timeout)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	deploymentKey := types.NamespacedName{Name: name, Namespace: namespace}

	for {
		select {
		case <-timeoutChan:
			return fmt.Errorf("timeout waiting for deployment to be deleted")

		case <-ctx.Done():
			return ctx.Err()

		case <-ticker.C:
			deployment := &appsv1.Deployment{}
			if err := s.K8sClient.Get(ctx, deploymentKey, deployment); err != nil {
				if errors.IsNotFound(err) {
					logger.Info("Deployment has been deleted",
						"namespace", namespace,
						"model", name,
					)
					s.Registry.Delete(namespace, name)
					return nil
				}
				logger.Error(err, "Failed to get deployment status during deletion wait")
				continue
			}
		}
	}
}

// buildDeploymentSpec creates a Deployment spec from an InferenceModel and InferenceBackend.
// This reuses the logic from the controller.
func (s *Server) buildDeploymentSpec(model *inferencev1alpha1.InferenceModel, backend *inferencev1alpha1.InferenceBackend) (*appsv1.Deployment, error) {
	labels := buildDeploymentLabels(model)
	port := resolvePort(model, backend)
	readinessPath, livenessPath := resolveProbePaths(model, backend)

	envVars := buildContainerEnv(model, backend, port)
	volumes, volumeMounts := buildVolumeConfig(model, backend)

	// Add HF_SOURCE env var for HuggingFace models
	if model.Spec.Source.HuggingFace != nil && model.Spec.Storage.PVC != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "HF_SOURCE",
			Value: getModelDir(model),
		})
	}

	paths := resolveSourcePaths(model)
	args := buildContainerArgs(model, backend, paths)

	container := s.buildMainContainer(model, backend, port, readinessPath, livenessPath, envVars, volumeMounts, args)
	podSpec := buildPodSpec(model, backend, container, volumes)

	replicas := model.Spec.Scaling.MaxReplicas
	if replicas == 0 {
		replicas = 1
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      model.Name,
			Namespace: model.Namespace,
			Labels:    labels,
			Annotations: map[string]string{
				"inference.eh-ops.io/memory":  model.Spec.Resources.Memory,
				"inference.eh-ops.io/backend": model.Spec.Backend,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app.kubernetes.io/name": model.Name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						"inference.eh-ops.io/last-request-time": time.Now().UTC().Format(time.RFC3339),
					},
				},
				Spec: podSpec,
			},
		},
	}

	return deployment, nil
}

// buildServiceSpec creates a Service for the inference deployment.
func (s *Server) buildServiceSpec(model *inferencev1alpha1.InferenceModel, backend *inferencev1alpha1.InferenceBackend) *corev1.Service {
	port := resolvePort(model, backend)

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      model.Name,
			Namespace: model.Namespace,
			Labels:    buildDeploymentLabels(model),
		},
		Spec: corev1.ServiceSpec{
			Type: model.Spec.Service.Type,
			Selector: map[string]string{
				"app.kubernetes.io/name": model.Name,
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       port,
					TargetPort: intstr.FromInt(int(port)),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// buildDeploymentLabels creates standard labels for the deployment and pods.
func buildDeploymentLabels(model *inferencev1alpha1.InferenceModel) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      model.Name,
		"app.kubernetes.io/component": "inference-server",
		"inference.eh-ops.io/model":   model.Spec.ModelName,
		"inference.eh-ops.io/backend": model.Spec.Backend,
		"inference.eh-ops.io/managed": "true",
	}
}

// resolvePort determines the port to use from backend or model overrides.
func resolvePort(model *inferencev1alpha1.InferenceModel, backend *inferencev1alpha1.InferenceBackend) int32 {
	port := backend.Spec.Port
	if port == 0 {
		port = 8080
	}
	if model.Spec.BackendOverrides != nil && model.Spec.BackendOverrides.Port != nil {
		port = *model.Spec.BackendOverrides.Port
	}
	return port
}

// resolveImage determines the image to use from backend or model overrides.
func resolveImage(model *inferencev1alpha1.InferenceModel, backend *inferencev1alpha1.InferenceBackend) string {
	imageRef := &backend.Spec.Image
	if model.Spec.BackendOverrides != nil && model.Spec.BackendOverrides.Image != nil {
		imageRef = model.Spec.BackendOverrides.Image
	}
	return imageRef.GetImage()
}

// resolveProbePaths determines readiness and liveness probe paths.
func resolveProbePaths(model *inferencev1alpha1.InferenceModel, backend *inferencev1alpha1.InferenceBackend) (readiness, liveness string) {
	readiness = backend.Spec.ReadinessPath
	if readiness == "" {
		readiness = "/health"
	}
	if model.Spec.BackendOverrides != nil && model.Spec.BackendOverrides.ReadinessPath != nil {
		readiness = *model.Spec.BackendOverrides.ReadinessPath
	}

	liveness = backend.Spec.LivenessPath
	if liveness == "" {
		liveness = readiness
	}
	return readiness, liveness
}

// buildContainerEnv builds environment variables for the container.
func buildContainerEnv(model *inferencev1alpha1.InferenceModel, backend *inferencev1alpha1.InferenceBackend, port int32) []corev1.EnvVar {
	envVars := []corev1.EnvVar{
		{Name: "MODEL_NAME", Value: model.Spec.ModelName},
		{Name: "BACKEND_URL", Value: fmt.Sprintf("http://localhost:%d", port)},
		{Name: "PORT", Value: fmt.Sprintf("%d", port)},
	}
	envVars = append(envVars, backend.Spec.Env...)
	envVars = append(envVars, model.Spec.Env...)
	return envVars
}

// buildVolumeConfig builds volumes and volume mounts for the pod.
func buildVolumeConfig(model *inferencev1alpha1.InferenceModel, backend *inferencev1alpha1.InferenceBackend) ([]corev1.Volume, []corev1.VolumeMount) {
	volumes := append([]corev1.Volume{}, backend.Spec.Volumes...)
	volumeMounts := append([]corev1.VolumeMount{}, backend.Spec.VolumeMounts...)

	if model.Spec.Source.HuggingFace != nil && model.Spec.Storage.PVC != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "model-cache",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: model.Spec.Storage.PVC,
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "model-cache",
			MountPath: "/models",
		})
	}
	return volumes, volumeMounts
}

// getModelDir returns the directory path for the model within the PVC.
func getModelDir(model *inferencev1alpha1.InferenceModel) string {
	modelDir := model.Spec.Storage.ModelDir
	if modelDir == "" {
		// Default to model name, replacing any slashes
		modelDir = strings.ReplaceAll(model.Spec.ModelName, "/", "-")
	}
	return "/models/" + modelDir
}

// sourcePaths contains the resolved paths for model files.
type sourcePaths struct {
	hfSource     string
	mmprojSource string
}

// resolveSourcePaths calculates HF_SOURCE and MMPROJ_SOURCE paths for arg substitution.
func resolveSourcePaths(model *inferencev1alpha1.InferenceModel) sourcePaths {
	var paths sourcePaths

	if model.Spec.Source.HuggingFace != nil {
		modelDir := getModelDir(model)
		hf := model.Spec.Source.HuggingFace
		if hf.ModelFile != "" {
			paths.hfSource = fmt.Sprintf("%s/%s", modelDir, hf.ModelFile)
		} else {
			paths.hfSource = modelDir
		}
		if hf.MmprojFile != "" {
			paths.mmprojSource = fmt.Sprintf("%s/%s", modelDir, hf.MmprojFile)
		}
	} else if model.Spec.Source.PVC != nil {
		pvc := model.Spec.Source.PVC
		if pvc.ModelFile != "" {
			paths.hfSource = fmt.Sprintf("%s/%s", pvc.Path, pvc.ModelFile)
		} else {
			paths.hfSource = pvc.Path
		}
		if pvc.MmprojFile != "" {
			paths.mmprojSource = fmt.Sprintf("%s/%s", pvc.Path, pvc.MmprojFile)
		}
	}
	return paths
}

// buildContainerArgs builds args with variable substitution.
func buildContainerArgs(model *inferencev1alpha1.InferenceModel, backend *inferencev1alpha1.InferenceBackend, paths sourcePaths) []string {
	args := append(backend.Spec.Args, model.Spec.Args...)
	for i, arg := range args {
		if paths.hfSource != "" {
			args[i] = strings.ReplaceAll(arg, "$(HF_SOURCE)", paths.hfSource)
		}
		if paths.mmprojSource != "" {
			args[i] = strings.ReplaceAll(arg, "$(MMPROJ_SOURCE)", paths.mmprojSource)
		}
	}
	return args
}

// buildMainContainer creates the main inference container.
func (s *Server) buildMainContainer(
	model *inferencev1alpha1.InferenceModel,
	backend *inferencev1alpha1.InferenceBackend,
	port int32,
	readinessPath, livenessPath string,
	envVars []corev1.EnvVar,
	volumeMounts []corev1.VolumeMount,
	args []string,
) corev1.Container {
	container := corev1.Container{
		Name:            "inference",
		Image:           resolveImage(model, backend),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Ports: []corev1.ContainerPort{
			{Name: "http", ContainerPort: port, Protocol: corev1.ProtocolTCP},
		},
		Env:          envVars,
		VolumeMounts: volumeMounts,
		Resources:    s.buildResourceRequirements(model, backend),
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: readinessPath,
					Port: intstr.FromInt(int(port)),
				},
			},
			InitialDelaySeconds: 30, PeriodSeconds: 10, TimeoutSeconds: 5,
			SuccessThreshold: 1, FailureThreshold: 3,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: livenessPath,
					Port: intstr.FromInt(int(port)),
				},
			},
			InitialDelaySeconds: 60, PeriodSeconds: 30, TimeoutSeconds: 5, FailureThreshold: 3,
		},
		Args: args,
	}

	if len(backend.Spec.Command) > 0 {
		container.Command = backend.Spec.Command
	}
	if backend.Spec.SecurityContext != nil {
		container.SecurityContext = backend.Spec.SecurityContext
	}
	return container
}

// buildPodSpec creates the pod spec for the deployment.
func buildPodSpec(
	model *inferencev1alpha1.InferenceModel,
	backend *inferencev1alpha1.InferenceBackend,
	container corev1.Container,
	volumes []corev1.Volume,
) corev1.PodSpec {
	podSpec := corev1.PodSpec{
		Containers:   []corev1.Container{container},
		NodeSelector: model.Spec.NodeSelector,
		Volumes:      volumes,
	}

	if len(model.Spec.Tolerations) > 0 {
		podSpec.Tolerations = model.Spec.Tolerations
	}
	if model.Spec.Affinity != nil {
		podSpec.Affinity = model.Spec.Affinity
	}
	if len(backend.Spec.InitContainers) > 0 {
		podSpec.InitContainers = backend.Spec.InitContainers
	}

	// Add sidecar if specified
	if model.Spec.Sidecar != nil {
		sidecar := corev1.Container{
			Name:  model.Spec.Sidecar.Name,
			Image: model.Spec.Sidecar.Image,
			Ports: model.Spec.Sidecar.Ports,
			Args:  model.Spec.Sidecar.Args,
			Env:   model.Spec.Sidecar.Env,
		}
		if model.Spec.Sidecar.Resources != nil {
			sidecar.Resources = *model.Spec.Sidecar.Resources
		}
		if sidecar.Name == "" {
			sidecar.Name = "sidecar"
		}
		podSpec.Containers = append(podSpec.Containers, sidecar)
	}

	// Add GPU tolerations if needed
	gpuConfig := backend.Spec.GPU
	if model.Spec.BackendOverrides != nil && model.Spec.BackendOverrides.GPU != nil {
		gpuConfig = model.Spec.BackendOverrides.GPU
	}
	if gpuConfig != nil && (gpuConfig.Exclusive || gpuConfig.Shared) && len(podSpec.Tolerations) == 0 {
		podSpec.Tolerations = []corev1.Toleration{
			{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		}
	}

	return podSpec
}

// buildResourceRequirements creates ResourceRequirements from the InferenceModel spec and InferenceBackend.
func (s *Server) buildResourceRequirements(model *inferencev1alpha1.InferenceModel, backend *inferencev1alpha1.InferenceBackend) corev1.ResourceRequirements {
	requirements := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}

	// Set memory request from spec
	if model.Spec.Resources.Memory != "" {
		if q, err := resource.ParseQuantity(model.Spec.Resources.Memory); err == nil {
			requirements.Requests[corev1.ResourceMemory] = q
		}
	}

	// Set CPU request from spec
	if model.Spec.Resources.CPU != "" {
		if q, err := resource.ParseQuantity(model.Spec.Resources.CPU); err == nil {
			requirements.Requests[corev1.ResourceCPU] = q
		}
	}

	// Set memory limit (defaults to memory request if not specified)
	if model.Spec.Resources.MemoryLimit != "" {
		if q, err := resource.ParseQuantity(model.Spec.Resources.MemoryLimit); err == nil {
			requirements.Limits[corev1.ResourceMemory] = q
		}
	} else if model.Spec.Resources.Memory != "" {
		if q, err := resource.ParseQuantity(model.Spec.Resources.Memory); err == nil {
			requirements.Limits[corev1.ResourceMemory] = q
		}
	}

	// Set GPU resources from backend configuration (can be overridden by model)
	gpuConfig := backend.Spec.GPU
	if model.Spec.BackendOverrides != nil && model.Spec.BackendOverrides.GPU != nil {
		gpuConfig = model.Spec.BackendOverrides.GPU
	}

	if gpuConfig != nil {
		gpuResourceName := corev1.ResourceName("nvidia.com/gpu")
		if gpuConfig.ResourceName != "" {
			gpuResourceName = corev1.ResourceName(gpuConfig.ResourceName)
		} else if gpuConfig.Shared {
			gpuResourceName = corev1.ResourceName("amd.com/gpu-shared")
		}

		if gpuConfig.Exclusive || gpuConfig.Shared {
			// Set GPU to 1 for both exclusive and shared modes
			requirements.Requests[gpuResourceName] = resource.MustParse("1")
			requirements.Limits[gpuResourceName] = resource.MustParse("1")
		}
	}

	return requirements
}

// IsConditionTrue returns true if the condition with the given type is true.
func IsConditionTrue(conditions []metav1.Condition, conditionType string) bool {
	condition := meta.FindStatusCondition(conditions, conditionType)
	return condition != nil && condition.Status == metav1.ConditionTrue
}
