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
	"net/http"

	"github.com/gin-gonic/gin"
)

// PropsResponse represents the server properties response for the llama.cpp web UI.
// This provides the configuration and capabilities that the UI needs to function.
type PropsResponse struct {
	DefaultGenerationSettings GenerationSettings `json:"default_generation_settings"`
	TotalSlots                int                `json:"total_slots"`
	ModelPath                 string             `json:"model_path"`
	Role                      string             `json:"role"`
	Modalities                Modalities         `json:"modalities"`
	ChatTemplate              string             `json:"chat_template"`
	BosToken                  string             `json:"bos_token"`
	EosToken                  string             `json:"eos_token"`
	BuildInfo                 string             `json:"build_info"`
}

// GenerationSettings contains default generation parameters.
type GenerationSettings struct {
	ID           int       `json:"id"`
	IDTask       int       `json:"id_task"`
	NCtx         int       `json:"n_ctx"`
	Speculative  bool      `json:"speculative"`
	IsProcessing bool      `json:"is_processing"`
	Params       Params    `json:"params"`
	Prompt       string    `json:"prompt"`
	NextToken    NextToken `json:"next_token"`
}

// Params contains the sampling and generation parameters.
type Params struct {
	NPredict            int         `json:"n_predict"`
	Seed                int         `json:"seed"`
	Temperature         float64     `json:"temperature"`
	DynatempRange       float64     `json:"dynatemp_range"`
	DynatempExponent    float64     `json:"dynatemp_exponent"`
	TopK                int         `json:"top_k"`
	TopP                float64     `json:"top_p"`
	MinP                float64     `json:"min_p"`
	TopNSigma           float64     `json:"top_n_sigma"`
	XtcProbability      float64     `json:"xtc_probability"`
	XtcThreshold        float64     `json:"xtc_threshold"`
	TypP                float64     `json:"typ_p"`
	RepeatLastN         int         `json:"repeat_last_n"`
	RepeatPenalty       float64     `json:"repeat_penalty"`
	PresencePenalty     float64     `json:"presence_penalty"`
	FrequencyPenalty    float64     `json:"frequency_penalty"`
	DryMultiplier       float64     `json:"dry_multiplier"`
	DryBase             float64     `json:"dry_base"`
	DryAllowedLength    int         `json:"dry_allowed_length"`
	DryPenaltyLastN     int         `json:"dry_penalty_last_n"`
	DrySequenceBreakers []string    `json:"dry_sequence_breakers"`
	Mirostat            int         `json:"mirostat"`
	MirostatTau         float64     `json:"mirostat_tau"`
	MirostatEta         float64     `json:"mirostat_eta"`
	Stop                []string    `json:"stop"`
	MaxTokens           int         `json:"max_tokens"`
	NKeep               int         `json:"n_keep"`
	NDiscard            int         `json:"n_discard"`
	IgnoreEos           bool        `json:"ignore_eos"`
	Stream              bool        `json:"stream"`
	LogitBias           [][]float64 `json:"logit_bias"`
	NProbs              int         `json:"n_probs"`
	MinKeep             int         `json:"min_keep"`
	Grammar             string      `json:"grammar"`
	GrammarLazy         bool        `json:"grammar_lazy"`
	GrammarTriggers     []string    `json:"grammar_triggers"`
	PreservedTokens     []int       `json:"preserved_tokens"`
	ChatFormat          string      `json:"chat_format"`
	ReasoningFormat     string      `json:"reasoning_format"`
	ReasoningInContent  bool        `json:"reasoning_in_content"`
	GenerationPrompt    string      `json:"generation_prompt"`
	Samplers            []string    `json:"samplers"`
	BackendSampling     bool        `json:"backend_sampling"`
	TimingsPerToken     bool        `json:"timings_per_token"`
	PostSamplingProbs   bool        `json:"post_sampling_probs"`
}

// NextToken contains token generation state.
type NextToken struct {
	HasNextToken bool   `json:"has_next_token"`
	HasNewLine   bool   `json:"has_new_line"`
	NRemain      int    `json:"n_remain"`
	NDecoded     int    `json:"n_decoded"`
	StoppingWord string `json:"stopping_word"`
}

// Modalities indicates what input types the model supports.
type Modalities struct {
	Vision bool `json:"vision"`
	Audio  bool `json:"audio"`
}

// DefaultProps returns the default server properties.
// These are reasonable defaults that work with the llama.cpp web UI.
func DefaultProps() PropsResponse {
	return PropsResponse{
		DefaultGenerationSettings: GenerationSettings{
			ID:           0,
			IDTask:       0,
			NCtx:         8192,
			Speculative:  false,
			IsProcessing: false,
			Params: Params{
				NPredict:            -1, // -1 means infinite
				Seed:                -1,
				Temperature:         0.7,
				DynatempRange:       0.0,
				DynatempExponent:    1.0,
				TopK:                40,
				TopP:                0.9,
				MinP:                0.05,
				TopNSigma:           -1.0,
				XtcProbability:      0.0,
				XtcThreshold:        0.1,
				TypP:                1.0,
				RepeatLastN:         64,
				RepeatPenalty:       1.0,
				PresencePenalty:     0.0,
				FrequencyPenalty:    0.0,
				DryMultiplier:       0.0,
				DryBase:             1.75,
				DryAllowedLength:    2,
				DryPenaltyLastN:     -1,
				DrySequenceBreakers: []string{"\n", "\"", "'"},
				Mirostat:            0,
				MirostatTau:         5.0,
				MirostatEta:         0.1,
				Stop:                []string{},
				MaxTokens:           -1,
				NKeep:               0,
				NDiscard:            0,
				IgnoreEos:           false,
				Stream:              true,
				LogitBias:           [][]float64{},
				NProbs:              0,
				MinKeep:             1,
				Grammar:             "",
				GrammarLazy:         false,
				GrammarTriggers:     []string{},
				PreservedTokens:     []int{},
				ChatFormat:          "chatml",
				ReasoningFormat:     "auto",
				ReasoningInContent:  false,
				GenerationPrompt:    "",
				Samplers:            []string{"top_k", "typ_p", "top_p", "min_p", "temperature"},
				BackendSampling:     false,
				TimingsPerToken:     false,
				PostSamplingProbs:   false,
			},
			Prompt: "",
			NextToken: NextToken{
				HasNextToken: false,
				HasNewLine:   false,
				NRemain:      0,
				NDecoded:     0,
				StoppingWord: "",
			},
		},
		TotalSlots:   0, // We don't use slot-based scheduling
		ModelPath:    "openyourmind-qwen3-6-35b-a3b-q4km",
		Role:         "inference-proxy",
		Modalities:   Modalities{Vision: false, Audio: false},
		ChatTemplate: "chatml",
		BosToken:     "<|endoftext|>",
		EosToken:     "<|endoftext|>",
		BuildInfo:    "inference-budget-controller v0.1.0",
	}
}

// propsHandler handles GET /props requests from the llama.cpp web UI.
func (s *Server) propsHandler(c *gin.Context) {
	props := DefaultProps()

	// Check if a model is specified in the query
	modelName := c.Query("model")
	if modelName != "" {
		// Look up the model to potentially customize modalities
		// For now, we return default props with the model path set
		props.ModelPath = modelName
	}

	c.JSON(http.StatusOK, props)
}

// slotsHandler handles GET /slots requests from the llama.cpp web UI.
// The operator doesn't use slot-based scheduling, so we return an empty array.
func (s *Server) slotsHandler(c *gin.Context) {
	// Return empty array - we don't have slots concept
	// The UI uses this to check if the server is busy
	c.JSON(http.StatusOK, []interface{}{})
}
