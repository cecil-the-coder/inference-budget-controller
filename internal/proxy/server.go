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
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/cecil-the-coder/inference-budget-controller/api/v1alpha1"
	"github.com/cecil-the-coder/inference-budget-controller/internal/budget"
	"github.com/cecil-the-coder/inference-budget-controller/internal/metrics"
)

// IdleTracker tracks idle time for models
type IdleTracker struct {
	mu sync.RWMutex

	// lastRequest tracks the last request time per model (namespace/name)
	lastRequest map[string]time.Time
}

// NewIdleTracker creates a new idle tracker
func NewIdleTracker() *IdleTracker {
	return &IdleTracker{
		lastRequest: make(map[string]time.Time),
	}
}

// RecordRequest records a request for a model
func (t *IdleTracker) RecordRequest(namespace, name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastRequest[namespace+"/"+name] = time.Now()
}

// GetIdleDuration returns how long a model has been idle
func (t *IdleTracker) GetIdleDuration(namespace, name string) time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()

	key := namespace + "/" + name
	if last, ok := t.lastRequest[key]; ok {
		return time.Since(last)
	}
	return 0
}

// GetIdleSince returns the time of the last request
func (t *IdleTracker) GetIdleSince(namespace, name string) *time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()

	key := namespace + "/" + name
	if last, ok := t.lastRequest[key]; ok {
		return &last
	}
	return nil
}

// Server is the HTTP proxy server
type Server struct {
	Addr        string
	Tracker     *budget.Tracker
	IdleTracker *IdleTracker
	K8sClient   client.Client
	Scheme      *runtime.Scheme
	Metrics     *metrics.Collector

	// Configuration
	Namespace       string // Default namespace to look for InferenceModels
	ScaleUpTimeout  time.Duration
	ReadyCheckDelay time.Duration

	server *http.Server
	router *gin.Engine
}

// ServerOption is a functional option for configuring the Server
type ServerOption func(*Server)

// WithNamespace sets the namespace for the server
func WithNamespace(namespace string) ServerOption {
	return func(s *Server) {
		s.Namespace = namespace
	}
}

// WithScaleUpTimeout sets the scale-up timeout
func WithScaleUpTimeout(timeout time.Duration) ServerOption {
	return func(s *Server) {
		s.ScaleUpTimeout = timeout
	}
}

// WithReadyCheckDelay sets the delay between ready checks
func WithReadyCheckDelay(delay time.Duration) ServerOption {
	return func(s *Server) {
		s.ReadyCheckDelay = delay
	}
}

// WithMetrics sets the metrics collector
func WithMetrics(collector *metrics.Collector) ServerOption {
	return func(s *Server) {
		s.Metrics = collector
	}
}

// NewServer creates a new proxy server
func NewServer(addr string, tracker *budget.Tracker, k8sClient client.Client, scheme *runtime.Scheme, opts ...ServerOption) *Server {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	s := &Server{
		Addr:            addr,
		Tracker:         tracker,
		IdleTracker:     NewIdleTracker(),
		K8sClient:       k8sClient,
		Scheme:          scheme,
		Namespace:       "default",
		ScaleUpTimeout:  5 * time.Minute,
		ReadyCheckDelay: 2 * time.Second,
		router:          router,
	}

	for _, opt := range opts {
		opt(s)
	}

	s.setupRoutes()
	return s
}

// setupRoutes configures the HTTP routes
func (s *Server) setupRoutes() {
	// Health endpoints
	s.router.GET("/healthz", s.healthzHandler)
	s.router.GET("/readyz", s.readyzHandler)

	// Metrics endpoint - Prometheus metrics
	s.router.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// OpenAI-compatible API
	v1 := s.router.Group("/v1")
	{
		v1.POST("/chat/completions", s.chatCompletionsHandler)
		v1.POST("/completions", s.completionsHandler)
		v1.GET("/models", s.listModelsHandler)
	}
}

// Start starts the HTTP server
func (s *Server) Start(ctx context.Context) error {
	s.server = &http.Server{
		Addr:         s.Addr,
		Handler:      s.router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 600 * time.Second, // Long timeout for inference
		IdleTimeout:  120 * time.Second,
	}

	logger := log.FromContext(ctx)
	logger.Info("Starting proxy server", "address", s.Addr)

	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}

	return nil
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown(ctx context.Context) error {
	if s.server != nil {
		return s.server.Shutdown(ctx)
	}
	return nil
}

func (s *Server) healthzHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Server) readyzHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ready"})
}

// lookupInferenceModelByName finds an InferenceModel by its modelName field
func (s *Server) lookupInferenceModelByName(ctx context.Context, modelName string) (*inferencev1alpha1.InferenceModel, error) {
	var modelList inferencev1alpha1.InferenceModelList
	if err := s.K8sClient.List(ctx, &modelList, client.InNamespace(s.Namespace)); err != nil {
		return nil, fmt.Errorf("failed to list InferenceModels: %w", err)
	}

	for i := range modelList.Items {
		if modelList.Items[i].Spec.ModelName == modelName {
			return &modelList.Items[i], nil
		}
	}

	return nil, nil
}

// listAllInferenceModels returns all InferenceModels in the configured namespace
func (s *Server) listAllInferenceModels(ctx context.Context) (*inferencev1alpha1.InferenceModelList, error) {
	var modelList inferencev1alpha1.InferenceModelList
	if err := s.K8sClient.List(ctx, &modelList, client.InNamespace(s.Namespace)); err != nil {
		return nil, fmt.Errorf("failed to list InferenceModels: %w", err)
	}
	return &modelList, nil
}
