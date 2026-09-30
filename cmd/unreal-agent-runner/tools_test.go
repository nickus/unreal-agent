package main

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/mcpclient"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestParseRequestConfiguresStaticTools(t *testing.T) {
	parsed, factory, err := parseRequest(strings.NewReader(`{"prompt":"hello","disallowed_tools":["Bash","SkillUse"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Prompt == nil || *parsed.Prompt != "hello" || factory == nil {
		t.Fatal("parser did not return the request and tool factory")
	}
	configured, err := factory(t.Context(), agentrunner.ToolConfig{Names: tool.StaticNames()})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, definition := range configured.Registry.StaticDefinitions() {
		names = append(names, definition.Tool.Name)
	}
	if !slices.Equal(names, []string{"ViewImage"}) {
		t.Fatalf("static tools = %v, want ViewImage", names)
	}
	for _, name := range []string{"Bash", "SkillUse", "McpSearch", "McpCall"} {
		if _, exists := configured.Registry.Resolve(name); exists {
			t.Errorf("unavailable tool %s resolves", name)
		}
	}
	// Only the MCP call handler, present without servers so that a resumed
	// session can settle calls recorded in an earlier run.
	if len(configured.RemoteJobs) != 1 || configured.RemoteJobs[0].RemoteJobPlanType() != mcpclient.PlanType {
		t.Fatalf("remote jobs = %#v", configured.RemoteJobs)
	}
	if len(configured.Warnings) != 0 {
		t.Fatalf("warnings = %v", configured.Warnings)
	}
	if err := configured.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerRejectsUnsupportedRequests(t *testing.T) {
	for _, input := range []string{
		`{"prompt":"hello","mcp_servers":{}}`,
		`{"prompt":"hello","mcp_servers":{"test":"http://localhost"}}`,
		`{"prompt":"hello","forward_auth":true}`,
		`{"prompt":"hello","unknown":true}`,
	} {
		var stderr bytes.Buffer
		code := agentrunner.RunMain(t.Context(), nil, func(string) string {
			t.Fatal("unsupported request reached environment setup")
			return ""
		}, func() []string { return nil }, strings.NewReader(input), io.Discard, &stderr,
			agentrunner.Config{Name: "unreal-agent-runner", ParseRequest: parseRequest})
		if code != 1 || !strings.Contains(stderr.String(), "unknown object member") {
			t.Fatalf("request = %s, exit = %d, stderr = %s", input, code, stderr.String())
		}
	}
}
