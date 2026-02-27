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

// ChatCompletionRequest represents an OpenAI chat completion request
type ChatCompletionRequest struct {
	Model            string         `json:"model"`
	Messages         []ChatMessage  `json:"messages"`
	Temperature      *float64       `json:"temperature,omitempty"`
	TopP             *float64       `json:"top_p,omitempty"`
	N                *int           `json:"n,omitempty"`
	Stream           bool           `json:"stream,omitempty"`
	Stop             interface{}    `json:"stop,omitempty"`
	MaxTokens        *int           `json:"max_tokens,omitempty"`
	PresencePenalty  *float64       `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64       `json:"frequency_penalty,omitempty"`
	LogitBias        map[string]int `json:"logit_bias,omitempty"`
	User             string         `json:"user,omitempty"`
}

// ChatMessage represents a message in a chat completion
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
}

// CompletionRequest represents an OpenAI completion request
type CompletionRequest struct {
	Model            string         `json:"model"`
	Prompt           interface{}    `json:"prompt"`
	Suffix           string         `json:"suffix,omitempty"`
	MaxTokens        *int           `json:"max_tokens,omitempty"`
	Temperature      *float64       `json:"temperature,omitempty"`
	TopP             *float64       `json:"top_p,omitempty"`
	N                *int           `json:"n,omitempty"`
	Stream           bool           `json:"stream,omitempty"`
	Logprobs         *int           `json:"logprobs,omitempty"`
	Echo             bool           `json:"echo,omitempty"`
	Stop             interface{}    `json:"stop,omitempty"`
	PresencePenalty  *float64       `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64       `json:"frequency_penalty,omitempty"`
	BestOf           *int           `json:"best_of,omitempty"`
	LogitBias        map[string]int `json:"logit_bias,omitempty"`
	User             string         `json:"user,omitempty"`
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

// ChatCompletionResponse represents a chat completion response
type ChatCompletionResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   UsageInfo    `json:"usage"`
}

// ChatChoice represents a choice in chat completion
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// UsageInfo represents token usage information
type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
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

// StreamingChatCompletionChunk represents a streaming response chunk
type StreamingChatCompletionChunk struct {
	ID      string                     `json:"id"`
	Object  string                     `json:"object"`
	Created int64                      `json:"created"`
	Model   string                     `json:"model"`
	Choices []StreamingChatChoiceChunk `json:"choices"`
}

// StreamingChatChoiceChunk represents a choice in streaming response
type StreamingChatChoiceChunk struct {
	Index        int              `json:"index"`
	Delta        ChatMessageDelta `json:"delta"`
	FinishReason *string          `json:"finish_reason,omitempty"`
}

// ChatMessageDelta represents a partial message in streaming
type ChatMessageDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// CompletionResponse represents a legacy completion response
type CompletionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []CompletionChoice `json:"choices"`
	Usage   UsageInfo          `json:"usage"`
}

// CompletionChoice represents a choice in completion response
type CompletionChoice struct {
	Text         string    `json:"text"`
	Index        int       `json:"index"`
	FinishReason string    `json:"finish_reason"`
	Logprobs     *Logprobs `json:"logprobs,omitempty"`
}

// Logprobs represents log probability information
type Logprobs struct {
	Tokens        []string             `json:"tokens,omitempty"`
	TokenLogprobs []float64            `json:"token_logprobs,omitempty"`
	TopLogprobs   []map[string]float64 `json:"top_logprobs,omitempty"`
	TextOffset    []int                `json:"text_offset,omitempty"`
}

// GetModelName extracts the model name from a chat completion request
func (r *ChatCompletionRequest) GetModelName() string {
	return r.Model
}

// GetModelName extracts the model name from a completion request
func (r *CompletionRequest) GetModelName() string {
	return r.Model
}

// IsStream returns whether the request is for streaming response
func (r *ChatCompletionRequest) IsStream() bool {
	return r.Stream
}

// IsStream returns whether the request is for streaming response
func (r *CompletionRequest) IsStream() bool {
	return r.Stream
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
