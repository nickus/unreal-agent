package agentrunner

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm/clients/fireworks"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/ollama"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openai"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openrouter"
)

// llmExtraBodyEnvironment holds a JSON object of extra top-level fields for
// every Responses API request, like the OpenAI SDKs' extra_body: sampling or
// template parameters that a compatible server accepts beyond the standard set.
const llmExtraBodyEnvironment = "UNREAL_HARNESS_LLM_EXTRA_BODY"

// harnessRequestFields are the request fields the harness sets itself. Extra
// fields cannot replace them; the request builder also refuses at send time.
// max_output_tokens is left out on purpose: the builder sends it only for a
// model with MaxOutputTokens, which the runner never sets, so an extra field
// is how an output cap is configured. TestRunnerProvidersSendExtraBody fails
// if the runner starts setting it; reserve the field here when it does.
var harnessRequestFields = []string{"include", "input", "model", "prompt_cache_key", "reasoning", "store", "stream", "tools"}

func DefaultProviders() []Provider {
	return []Provider{
		{
			Name:    "ollama",
			BaseURL: ollama.BaseURL,
			NewClient: func(_, baseURL string, maxAttempts int, getenv func(string) string) (Client, error) {
				extraBody, err := requestExtraBody(getenv)
				if err != nil {
					return nil, err
				}
				return ollama.NewClient(ollama.Config{BaseURL: baseURL, MaxAttempts: &maxAttempts, Extensions: extraBody})
			},
		},
		{
			Name:              "openai",
			BaseURL:           "https://api.openai.com/v1",
			DefaultModel:      "gpt-6-astra",
			APIKeyEnvironment: "OPENAI_API_KEY",
			NewClient: func(apiKey, baseURL string, maxAttempts int, getenv func(string) string) (Client, error) {
				extraBody, err := requestExtraBody(getenv)
				if err != nil {
					return nil, err
				}
				return openai.NewClient(openai.Config{APIKey: apiKey, BaseURL: baseURL, MaxAttempts: &maxAttempts, Extensions: extraBody})
			},
		},
		{
			Name:    "openai-codex",
			BaseURL: openaicodex.BaseURL,
			NewClient: func(_, baseURL string, maxAttempts int, getenv func(string) string) (Client, error) {
				config, err := openaicodex.EnvironmentConfig(getenv)
				if err != nil {
					return nil, err
				}
				extraBody, err := requestExtraBody(getenv)
				if err != nil {
					return nil, err
				}
				config.BaseURL, config.MaxAttempts, config.Extensions = baseURL, &maxAttempts, extraBody
				return openaicodex.NewClient(config)
			},
		},

		{
			Name:              "openrouter",
			BaseURL:           "https://openrouter.ai/api/v1",
			APIKeyEnvironment: "OPENROUTER_API_KEY",
			NewClient: func(apiKey, baseURL string, maxAttempts int, getenv func(string) string) (Client, error) {
				extraBody, err := requestExtraBody(getenv)
				if err != nil {
					return nil, err
				}
				return openrouter.NewClient(openrouter.Config{APIKey: apiKey, BaseURL: baseURL, MaxAttempts: &maxAttempts, Extensions: extraBody})
			},
		},
		{
			Name:              "fireworks",
			BaseURL:           "https://api.fireworks.ai/inference/v1",
			APIKeyEnvironment: "FIREWORKS_API_KEY",
			NewClient: func(apiKey, baseURL string, maxAttempts int, getenv func(string) string) (Client, error) {
				extraBody, err := requestExtraBody(getenv)
				if err != nil {
					return nil, err
				}
				return fireworks.NewClient(fireworks.Config{APIKey: apiKey, BaseURL: baseURL, MaxAttempts: &maxAttempts, Extensions: extraBody})
			},
		},
	}
}

// requestExtraBody parses UNREAL_HARNESS_LLM_EXTRA_BODY. Unset or blank means
// no extra fields.
func requestExtraBody(getenv func(string) string) (map[string]jsontext.Value, error) {
	encoded := strings.TrimSpace(getenv(llmExtraBodyEnvironment))
	if encoded == "" {
		return nil, nil
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal([]byte(encoded), &fields); err != nil {
		return nil, fmt.Errorf("%s must be a JSON object of request fields: %w", llmExtraBodyEnvironment, err)
	}
	if fields == nil {
		return nil, fmt.Errorf("%s must be a JSON object of request fields, not null", llmExtraBodyEnvironment)
	}
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		if slices.Contains(harnessRequestFields, name) {
			return nil, fmt.Errorf("%s field %q is set by the harness and cannot be overridden", llmExtraBodyEnvironment, name)
		}
	}
	return fields, nil
}
