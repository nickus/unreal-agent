package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// dictionaryServer is a streamable HTTP MCP server with one tool, lookup.
type dictionaryServer struct {
	*httptest.Server
	mu             sync.Mutex
	authorizations []string
	calls          []string
}

func newDictionaryServer(t *testing.T) *dictionaryServer {
	server := &dictionaryServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete {
			return
		}
		var message struct {
			ID     jsontext.Value `json:"id,omitzero"`
			Method string         `json:"method"`
			Params struct {
				Name      string            `json:"name"`
				Arguments map[string]string `json:"arguments"`
			} `json:"params"`
		}
		if err := json.UnmarshalRead(request.Body, &message); err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		server.mu.Lock()
		server.authorizations = append(server.authorizations, request.Header.Get("Authorization"))
		server.mu.Unlock()
		var result any
		switch message.Method {
		case "initialize":
			response.Header().Set("Mcp-Session-Id", "dictionary-session")
			result = map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "dictionary", "version": "1"},
			}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name":        "lookup",
				"description": "Look up a word.",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"word": map[string]any{"type": "string"}},
					"required":   []any{"word"},
				},
			}}}
		case "tools/call":
			server.mu.Lock()
			server.calls = append(server.calls, message.Params.Name+"("+message.Params.Arguments["word"]+")")
			server.mu.Unlock()
			word := message.Params.Arguments["word"]
			result = map[string]any{
				"content":           []any{map[string]any{"type": "text", "text": "definition of " + word}},
				"structuredContent": map[string]any{"word": word, "senses": 1},
			}
		default:
			response.WriteHeader(http.StatusAccepted)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(response, map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result})
	}))
	t.Cleanup(server.Close)
	return server
}

type scriptedClient struct {
	mu      sync.Mutex
	respond func(int, llm.Request) (llm.Response, error)
	calls   int
}

func (client *scriptedClient) Respond(_ context.Context, request llm.Request, _ llm.RequestOptions) (llm.Response, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.calls++
	return client.respond(client.calls, request)
}

func (client *scriptedClient) Close() error { return nil }

func scriptedConfig(client *scriptedClient) agentrunner.Config {
	return agentrunner.Config{Name: "unreal-agent-runner", ParseRequest: parseRequest, Providers: []agentrunner.Provider{{
		Name: "openai", BaseURL: "https://llm.example.test", DefaultModel: "test-model", APIKeyEnvironment: "OPENAI_API_KEY",
		NewClient: func(string, string, int, func(string) string) (agentrunner.Client, error) { return client, nil },
	}}}
}

func message(text string) llm.Response {
	return llm.Response{ID: "response", Stop: llm.StopComplete, Output: []llm.Item{{
		Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: text},
	}}}
}

func toolResult(request llm.Request, callID string) (string, bool) {
	for _, item := range request.Input {
		if item.Type != llm.ItemToolResult {
			continue
		}
		result := item.Data.(llm.ToolResult)
		if result.CallID == callID && result.Output[0].Value != contextbuilder.ToolCallRunningPayload {
			return result.Output[0].Value, true
		}
	}
	return "", false
}

func writeConfig(t *testing.T, config string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunnerCallsMCPToolsAndResumesWithoutThem(t *testing.T) {
	dictionary := newDictionaryServer(t)
	down := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, "maintenance", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	configPath := writeConfig(t, fmt.Sprintf(`{"mcpServers":{
		"dictionary":{"type":"http","url":%q,"headers":{"Authorization":"Bearer ${DICTIONARY_TOKEN}"},"timeout":330000},
		"down":{"type":"http","url":%q}
	}}`, dictionary.URL, down.URL))
	getenv := func(name string) string {
		return map[string]string{"OPENAI_API_KEY": "secret", "DICTIONARY_TOKEN": "token-123"}[name]
	}
	workspace, sessions := t.TempDir(), t.TempDir()

	client := &scriptedClient{respond: func(call int, request llm.Request) (llm.Response, error) {
		switch call {
		case 1:
			var lookup *llm.Tool
			for index := range request.Tools {
				if request.Tools[index].Name == "mcp__dictionary__lookup" {
					lookup = &request.Tools[index]
				}
			}
			if lookup == nil || lookup.Description != "Look up a word." || fmt.Sprint(lookup.Parameters["required"]) != "[word]" {
				return llm.Response{}, fmt.Errorf("MCP tool is missing from %v", request.Tools)
			}
			return llm.Response{ID: "response-1", Stop: llm.StopComplete, Output: []llm.Item{{
				Type: llm.ItemToolCall,
				Data: llm.ToolCall{CallID: "call-1", Name: "mcp__dictionary__lookup", Arguments: `{"word":"apple"}`},
			}}}, nil
		default:
			result, found := toolResult(request, "call-1")
			structured, _ := strings.CutPrefix(result, "definition of apple\nStructured content: ")
			if !found || !jsontext.Value(structured).IsValid() ||
				!strings.Contains(structured, `"word":"apple"`) || !strings.Contains(structured, `"senses":1`) {
				return llm.Response{}, fmt.Errorf("tool result = %q (found %v)", result, found)
			}
			return message("apple is a fruit"), nil
		}
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := agentrunner.RunMain(ctx,
		[]string{"-workspace", workspace, "-session-directory", sessions, "-mcp-config", configPath},
		getenv, func() []string { return nil },
		strings.NewReader(`{"prompt":"define apple","session_id":"mcp-session"}`), &stdout, &stderr, scriptedConfig(client))
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s, stdout = %s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stderr.String(), `tool error> MCP server "down": initialize: HTTP 503`) {
		t.Fatalf("stderr = %s", stderr.String())
	}
	dictionary.mu.Lock()
	calls, authorizations := dictionary.calls, dictionary.authorizations
	dictionary.mu.Unlock()
	if len(calls) != 1 || calls[0] != "lookup(apple)" {
		t.Fatalf("calls = %v", calls)
	}
	for _, authorization := range authorizations {
		if authorization != "Bearer token-123" {
			t.Fatalf("authorization = %q", authorization)
		}
	}
	if strings.Contains(stdout.String()+stderr.String(), "token-123") {
		t.Fatal("the runner wrote the header value")
	}

	// The next run has no MCP servers; the recorded call must not stop the
	// session from resuming.
	resumed := &scriptedClient{respond: func(_ int, request llm.Request) (llm.Response, error) {
		if result, found := toolResult(request, "call-1"); !found || !strings.HasPrefix(result, "definition of apple") {
			return llm.Response{}, errors.New("resumed context lost the MCP result")
		}
		for _, definition := range request.Tools {
			if strings.HasPrefix(definition.Name, "mcp__") {
				return llm.Response{}, fmt.Errorf("unconfigured MCP tool %s offered", definition.Name)
			}
		}
		return message("still here"), nil
	}}
	stdout.Reset()
	stderr.Reset()
	code = agentrunner.RunMain(ctx,
		[]string{"-workspace", workspace, "-session-directory", sessions},
		getenv, func() []string { return nil },
		strings.NewReader(`{"prompt":"anything else?","session_id":"mcp-session","resume":true}`), &stdout, &stderr, scriptedConfig(resumed))
	if code != 0 || resumed.calls != 1 {
		t.Fatalf("resume exit = %d, calls = %d, stderr = %s", code, resumed.calls, stderr.String())
	}
}

func TestRunnerRejectsInvalidMCPConfiguration(t *testing.T) {
	for _, test := range []struct{ config, want string }{
		{`{"mcpServers":{"local":{"type":"stdio","command":"server"}}}`, "invalid JSON"},
		{`{"mcpServers":{"local":{"type":"stdio"}}}`, `server "local": type "stdio" is not supported`},
		{`{"mcpServers":{"remote":{"url":"https://x.test","headers":{"Authorization":"Bearer ${UNSET_TOKEN}"}}}}`, "environment variable UNSET_TOKEN is not set"},
	} {
		client := &scriptedClient{respond: func(int, llm.Request) (llm.Response, error) {
			return llm.Response{}, errors.New("model called despite an invalid configuration")
		}}
		var stderr bytes.Buffer
		code := agentrunner.RunMain(t.Context(),
			[]string{"-workspace", t.TempDir(), "-session-directory", t.TempDir(), "-mcp-config", writeConfig(t, test.config)},
			func(name string) string { return map[string]string{"OPENAI_API_KEY": "secret"}[name] },
			func() []string { return nil }, strings.NewReader(`{"prompt":"hello"}`), &bytes.Buffer{}, &stderr, scriptedConfig(client))
		if code != 1 || !strings.Contains(stderr.String(), test.want) || client.calls != 0 {
			t.Fatalf("config %s: exit = %d, stderr = %s", test.config, code, stderr.String())
		}
	}
}
