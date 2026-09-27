package agentrunner

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunnerProviderRetries(t *testing.T) {
	for _, provider := range DefaultProviders() {
		for _, maxAttempts := range []int{1, 2} {
			t.Run(provider.Name+"/"+strconv.Itoa(maxAttempts), func(t *testing.T) {
				t.Parallel()
				var attempts atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					var body struct {
						MaxAttempts *int `json:"max_attempts"`
					}
					if err := json.UnmarshalRead(request.Body, &body); err != nil {
						t.Error(err)
					}
					if body.MaxAttempts != nil {
						t.Error("harness retry configuration leaked into provider request")
					}
					attempts.Add(1)
					writer.WriteHeader(http.StatusServiceUnavailable)
				}))
				defer server.Close()
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				code := RunMain(ctx,
					[]string{"-workspace", t.TempDir(), "-session-directory", t.TempDir()},
					func(name string) string {
						switch name {
						case "UNREAL_HARNESS_LLM_PROVIDER":
							return provider.Name
						case "UNREAL_HARNESS_LLM_BASE_URL":
							return server.URL
						case "OPENAI_CODEX_ACCESS_TOKEN":
							return "subscription-token"
						case "OPENAI_CODEX_ACCOUNT_ID":
							return "account-1"
						case "UNREAL_HARNESS_LLM_API_KEY":
							return "test-key"
						default:
							return ""
						}
					}, func() []string { return nil },
					strings.NewReader(`{"prompt":"hello","model":"test","max_attempts":`+strconv.Itoa(maxAttempts)+`}`),
					io.Discard, io.Discard, Config{Name: "unreal-agent-runner", ParseRequest: parseTestRequest, Providers: DefaultProviders()})
				if code != 1 || attempts.Load() != int64(maxAttempts) {
					t.Fatalf("exit = %d, attempts = %d, want %d", code, attempts.Load(), maxAttempts)
				}
			})
		}
	}
}

func TestRunnerProviderDefaultModels(t *testing.T) {
	for _, provider := range DefaultProviders() {
		want := ""
		if provider.Name == "openai" {
			want = "gpt-6-astra"
		}
		if provider.DefaultModel != want {
			t.Errorf("%s default model = %q, want %q", provider.Name, provider.DefaultModel, want)
		}
	}
}

func TestRunnerCodexUsesSubscriptionWithoutAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer subscription-token" || r.Header.Get("ChatGPT-Account-ID") != "account" {
			t.Error("wrong authentication")
		}
		var body struct {
			Stream bool `json:"stream"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Error(err)
		}
		if !body.Stream {
			t.Errorf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[{\"id\":\"msg-1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"subscription works\"}]}]}}\n\n")
	}))
	defer server.Close()
	var output, stderr strings.Builder
	code := RunMain(t.Context(), []string{"-workspace", t.TempDir(), "-session-directory", t.TempDir()}, func(key string) string {
		return map[string]string{
			"UNREAL_HARNESS_LLM_PROVIDER": "openai-codex",
			"UNREAL_HARNESS_LLM_BASE_URL": server.URL,
			"OPENAI_CODEX_ACCESS_TOKEN":   "subscription-token",
			"OPENAI_CODEX_ACCOUNT_ID":     "account",
		}[key]
	}, func() []string { return nil }, strings.NewReader(`{"prompt":"hello","model":"gpt-test","system_prompt":"my system prompt"}`), &output, &stderr, Config{Name: "unreal-agent-runner", ParseRequest: parseTestRequest, Providers: DefaultProviders()})
	if code != 0 || !strings.Contains(output.String(), "subscription works") {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if strings.Contains(output.String(), "subscription-token") {
		t.Fatal("credential leaked into session output")
	}
}

func TestRequestExtraBody(t *testing.T) {
	parse := func(value string) (map[string]jsontext.Value, error) {
		return requestExtraBody(func(name string) string {
			if name == llmExtraBodyEnvironment {
				return value
			}
			return ""
		})
	}
	if fields, err := parse(" "); err != nil || fields != nil {
		t.Fatalf("blank: fields = %v, error = %v", fields, err)
	}
	fields, err := parse(`{"top_k":20,"max_output_tokens":4096,"chat_template_kwargs":{"enable_thinking":true}}`)
	if err != nil || len(fields) != 3 || string(fields["top_k"]) != "20" || string(fields["chat_template_kwargs"]) != `{"enable_thinking":true}` {
		t.Fatalf("fields = %v, error = %v", fields, err)
	}
	for _, value := range []string{`[1]`, `null`, `"top_k"`, `{"top_k":}`, `{"a":1,"a":2}`} {
		if _, err := parse(value); err == nil || !strings.Contains(err.Error(), llmExtraBodyEnvironment+" must be a JSON object") {
			t.Errorf("%s: error = %v", value, err)
		}
	}
	for _, field := range harnessRequestFields {
		if _, err := parse(`{"` + field + `":true}`); err == nil || !strings.Contains(err.Error(), `field "`+field+`" is set by the harness`) {
			t.Errorf("%s: error = %v", field, err)
		}
	}
}

func TestRunnerProvidersSendExtraBody(t *testing.T) {
	for _, provider := range DefaultProviders() {
		for _, extraBody := range []string{
			`{"top_k":20,"max_output_tokens":64,"chat_template_kwargs":{"enable_thinking":false}}`,
			`{"top_k":20,"max_output_tokens":64,"chat_template_kwargs":{"enable_thinking":false},"cache_control":{"type":"ephemeral"}}`,
		} {
			t.Run(provider.Name+"/"+extraBody, func(t *testing.T) {
				t.Parallel()
				bodies := make(chan map[string]jsontext.Value, 1)
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					var body map[string]jsontext.Value
					if err := json.UnmarshalRead(request.Body, &body); err != nil {
						t.Error(err)
					}
					bodies <- body
					writer.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
				}))
				defer server.Close()
				var stderr strings.Builder
				code := RunMain(t.Context(),
					[]string{"-workspace", t.TempDir(), "-session-directory", t.TempDir()},
					func(name string) string {
						return map[string]string{
							"UNREAL_HARNESS_LLM_PROVIDER": provider.Name,
							"UNREAL_HARNESS_LLM_BASE_URL": server.URL,
							"UNREAL_HARNESS_LLM_API_KEY":  "test-key",
							"OPENAI_CODEX_ACCESS_TOKEN":   "subscription-token",
							"OPENAI_CODEX_ACCOUNT_ID":     "account-1",
							llmExtraBodyEnvironment:       extraBody,
						}[name]
					}, func() []string { return nil },
					strings.NewReader(`{"prompt":"hello","model":"test","max_attempts":1}`),
					io.Discard, &stderr, Config{Name: "unreal-agent-runner", ParseRequest: parseTestRequest, Providers: DefaultProviders()})
				if code != 0 {
					t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
				}
				body := <-bodies
				if string(body["top_k"]) != "20" || string(body["chat_template_kwargs"]) != `{"enable_thinking":false}` || string(body["model"]) != `"test"` {
					t.Fatalf("body = %v", body)
				}
				// The output cap is the operator's: the runner must not set it
				// itself (see harnessRequestFields).
				if string(body["max_output_tokens"]) != "64" {
					t.Fatalf("max_output_tokens = %s, want the configured 64", body["max_output_tokens"])
				}
				cacheControl := string(body["cache_control"])
				switch {
				case strings.Contains(extraBody, "cache_control") && cacheControl != `{"type":"ephemeral"}`:
					t.Fatalf("cache_control = %s, want the configured value", cacheControl)
				case !strings.Contains(extraBody, "cache_control") && provider.Name == "openrouter" && cacheControl != `{"type":"ephemeral","ttl":"1h"}`:
					t.Fatalf("cache_control = %s, want the OpenRouter default", cacheControl)
				case !strings.Contains(extraBody, "cache_control") && provider.Name != "openrouter" && cacheControl != "":
					t.Fatalf("cache_control = %s, want none", cacheControl)
				}
			})
		}
	}
}

func TestRunnerRejectsInvalidExtraBody(t *testing.T) {
	var stderr strings.Builder
	code := RunMain(t.Context(), []string{"-workspace", t.TempDir(), "-session-directory", t.TempDir()}, func(name string) string {
		return map[string]string{"UNREAL_HARNESS_LLM_API_KEY": "test-key", llmExtraBodyEnvironment: `{"model":"other"}`}[name]
	}, func() []string { return nil }, strings.NewReader(`{"prompt":"hello"}`), io.Discard, &stderr,
		Config{Name: "unreal-agent-runner", ParseRequest: parseTestRequest, Providers: DefaultProviders()})
	if code != 1 || !strings.Contains(stderr.String(), llmExtraBodyEnvironment+` field "model" is set by the harness`) {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
}
