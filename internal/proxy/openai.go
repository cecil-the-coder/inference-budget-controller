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

import "time"

// inferenceRequest extracts only the fields the proxy needs from any
// OpenAI-compatible request body. The full body is forwarded as raw bytes.
type inferenceRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// ErrorResponse represents an OpenAI error response
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail contains error details
type ErrorDetail struct {
	Message string         `json:"message"`
	Type    string         `json:"type"`
	Param   string         `json:"param,omitempty"`
	Code    string         `json:"code,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

// ModelsResponse represents the response for listing models
type ModelsResponse struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}

// ModelInfo represents model information
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// InsufficientMemoryDetails contains details about memory allocation failure
type InsufficientMemoryDetails struct {
	Requested RequestedInfo   `json:"requested"`
	Available string          `json:"available"`
	Blocking  []BlockingModel `json:"blocking,omitempty"`
}

// RequestedInfo contains information about the requested model
type RequestedInfo struct {
	Model  string `json:"model"`
	Memory string `json:"memory"`
}

// BlockingModel contains information about a model blocking allocation
type BlockingModel struct {
	Model   string `json:"model"`
	Memory  string `json:"memory"`
	IdleFor string `json:"idle_for,omitempty"`
}

// InsufficientMemoryResponse represents a 429 response due to insufficient memory
type InsufficientMemoryResponse struct {
	Error struct {
		Type    string                     `json:"type"`
		Message string                     `json:"message"`
		Details *InsufficientMemoryDetails `json:"details,omitempty"`
	} `json:"error"`
}

// NewInsufficientMemoryResponse creates a new insufficient memory error response
func NewInsufficientMemoryResponse(modelName, memoryReq, availableMem string, blocking []BlockingModel) ErrorResponse {
	blockingModels := make([]map[string]any, len(blocking))
	for i, b := range blocking {
		blockingModels[i] = map[string]any{
			"model":    b.Model,
			"memory":   b.Memory,
			"idle_for": b.IdleFor,
		}
	}

	return ErrorResponse{
		Error: ErrorDetail{
			Type:    "insufficient_memory",
			Message: "Cannot schedule " + modelName + ": insufficient memory",
			Code:    "insufficient_memory",
			Param:   "",
			Details: map[string]any{
				"requested": map[string]any{
					"model":  modelName,
					"memory": memoryReq,
				},
				"available": availableMem,
				"blocking":  blockingModels,
			},
		},
	}
}

// ModelListEntry represents a model in the list response with additional metadata
type ModelListEntry struct {
	ID        string    `json:"id"`
	Object    string    `json:"object"`
	Created   int64     `json:"created"`
	OwnedBy   string    `json:"owned_by"`
	Ready     bool      `json:"ready,omitempty"`
	Replicas  int32     `json:"replicas,omitempty"`
	Memory    string    `json:"memory,omitempty"`
	CreatedAt time.Time `json:"-"`
}
