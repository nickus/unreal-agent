package mcptool_test

import (
	"encoding/json/jsontext"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/mcpclient"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/mcptool"
)

type recordingContext struct {
	specs []operation.Spec
}

func (ctx *recordingContext) Submit(spec operation.Spec) operation.ID {
	ctx.specs = append(ctx.specs, spec)
	return operation.ID("op-" + string(rune('0'+len(ctx.specs))))
}

func listed(server, name, schema string) mcptool.Tool {
	return mcptool.Tool{Server: server, Tool: mcpclient.Tool{
		Name: name, Description: "Does " + name + ".", InputSchema: jsontext.Value(schema),
	}}
}

func TestName(t *testing.T) {
	for _, test := range []struct{ server, tool, want string }{
		{"github", "create_issue", "mcp__github__create_issue"},
		{"Team docs", "pages.search", "mcp__Team_docs__pages_search"},
		{"srv", "werkzeug-über", "mcp__srv__werkzeug-_ber"},
	} {
		if got := mcptool.Name(test.server, test.tool); got != test.want {
			t.Errorf("Name(%q, %q) = %q, want %q", test.server, test.tool, got, test.want)
		}
	}
	long := mcptool.Name("server", strings.Repeat("x", 100))
	other := mcptool.Name("server", strings.Repeat("x", 99)+"y")
	if len(long) != 64 || len(other) != 64 || long == other || !strings.HasPrefix(long, "mcp__server__xxx") {
		t.Fatalf("long names = %q, %q", long, other)
	}
	if again := mcptool.Name("server", strings.Repeat("x", 100)); again != long {
		t.Fatalf("long name is not stable: %q, %q", long, again)
	}
}

func TestRegistryAddsToolsAfterStaticTools(t *testing.T) {
	base := tool.NewRegistry(tool.StaticTranslators{}, tool.ViewImageName)
	registry, problems := mcptool.NewRegistry(base, []mcptool.Tool{
		listed("srv", "search", `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
		listed("srv", "no_schema", ``),
		listed("srv", "untyped", `{"properties":{"a":{"type":"number"}}}`),
		listed("srv", "hidden", `{"type":"object"}`),
		listed("srv", "array", `{"type":"array"}`),
		listed("srv", "not.unique", `{"type":"object"}`),
		listed("srv", "not_unique", `{"type":"object"}`),
		{Server: "srv", Tool: mcpclient.Tool{Name: "titled", Title: "Titled tool"}},
	}, []string{"mcp__srv__hidden"})
	var names []string
	for _, definition := range registry.StaticDefinitions() {
		names = append(names, definition.Tool.Name)
	}
	// Static tools first, then MCP tools by name.
	want := []string{"ViewImage", "mcp__srv__no_schema", "mcp__srv__not_unique", "mcp__srv__search", "mcp__srv__titled", "mcp__srv__untyped"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("definitions = %v, want %v", names, want)
	}
	if len(problems) != 2 ||
		!strings.Contains(problems[0].Error(), `tool "array": input schema type is array, want object`) ||
		!strings.Contains(problems[1].Error(), `tool "not_unique": name "mcp__srv__not_unique" is already taken`) {
		t.Fatalf("problems = %v", problems)
	}
	definitions := make(map[string]llm.Tool)
	for _, definition := range registry.StaticDefinitions() {
		definitions[definition.Tool.Name] = definition.Tool
	}
	search := definitions["mcp__srv__search"]
	if search.Type != llm.ToolFunction || search.Description != "Does search." ||
		!reflect.DeepEqual(search.Parameters["required"], []any{"query"}) {
		t.Fatalf("search definition = %#v", search)
	}
	if got := definitions["mcp__srv__no_schema"].Parameters; !reflect.DeepEqual(got, map[string]any{"type": "object", "properties": map[string]any{}}) {
		t.Fatalf("parameters without a schema = %#v", got)
	}
	if got := definitions["mcp__srv__untyped"].Parameters["type"]; got != "object" {
		t.Fatalf("untyped schema type = %v", got)
	}
	if got := definitions["mcp__srv__titled"].Description; got != "Titled tool" {
		t.Fatalf("description from title = %q", got)
	}
	// The same tools listed in another order give the same definitions.
	reordered, _ := mcptool.NewRegistry(base, []mcptool.Tool{
		{Server: "srv", Tool: mcpclient.Tool{Name: "titled", Title: "Titled tool"}},
		listed("srv", "not_unique", `{"type":"object"}`),
		listed("srv", "untyped", `{"properties":{"a":{"type":"number"}}}`),
		listed("srv", "not.unique", `{"type":"object"}`),
		listed("srv", "array", `{"type":"array"}`),
		listed("srv", "hidden", `{"type":"object"}`),
		listed("srv", "no_schema", ``),
		listed("srv", "search", `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
	}, []string{"mcp__srv__hidden"})
	if !reflect.DeepEqual(reordered.StaticDefinitions(), registry.StaticDefinitions()) {
		t.Fatal("tool order depends on the listing order")
	}
	if _, exists := registry.Resolve("ViewImage"); !exists {
		t.Fatal("static tool does not resolve")
	}
	if _, exists := registry.Resolve("Bash"); exists {
		t.Fatal("disabled static tool resolves")
	}
	if _, exists := registry.Resolve("McpCall"); exists {
		t.Fatal("unrelated name resolves")
	}
}

func TestTranslateSubmitsToolCallOperations(t *testing.T) {
	registry, _ := mcptool.NewRegistry(tool.NewRegistry(tool.StaticTranslators{}), []mcptool.Tool{
		listed("srv", "search", `{"type":"object"}`),
	}, nil)
	translator, exists := registry.Resolve("mcp__srv__search")
	if !exists {
		t.Fatal("tool does not resolve")
	}
	ctx := &recordingContext{}
	status := translator.Translate(ctx, llm.ToolCall{CallID: "c", Name: "mcp__srv__search", Arguments: ` {"query": "x"} `})
	if status.Error != "" || len(status.WaitingFor) != 1 || len(ctx.specs) != 1 {
		t.Fatalf("status = %#v, specs = %d", status, len(ctx.specs))
	}
	state, err := operation.DecodeRemoteJobState(operation.Operation{
		ID: "op-1", MaxOutputLength: ctx.specs[0].MaxOutputLength,
		Type: ctx.specs[0].Type, Version: ctx.specs[0].Version, State: ctx.specs[0].State,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := mcpclient.DecodeToolCallPlan(state.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Server != "srv" || plan.Tool != "search" || string(plan.Arguments) != `{"query":"x"}` {
		t.Fatalf("plan = %#v", plan)
	}

	for _, arguments := range []string{"", "  "} {
		ctx := &recordingContext{}
		if status := translator.Translate(ctx, llm.ToolCall{Name: "mcp__srv__search", Arguments: arguments}); status.Error != "" || len(ctx.specs) != 1 {
			t.Fatalf("empty arguments: status = %#v", status)
		}
	}
	for _, arguments := range []string{`[1]`, `{"a":`, `"text"`} {
		ctx := &recordingContext{}
		status := translator.Translate(ctx, llm.ToolCall{Name: "mcp__srv__search", Arguments: arguments})
		if !strings.Contains(status.Error, "arguments must be a JSON object") || len(ctx.specs) != 0 {
			t.Fatalf("arguments %s: status = %#v", arguments, status)
		}
	}
}

func TestUnavailableToolsRejectCallsButReadRecordedResults(t *testing.T) {
	registry, _ := mcptool.NewRegistry(tool.NewRegistry(tool.StaticTranslators{}), nil, nil)
	translator, exists := registry.Resolve("mcp__gone__search")
	if !exists {
		t.Fatal("recorded MCP tool name does not resolve")
	}
	ctx := &recordingContext{}
	status := translator.Translate(ctx, llm.ToolCall{Name: "mcp__gone__search", Arguments: `{}`})
	if status.Error != `tool "mcp__gone__search" is not available` || len(ctx.specs) != 0 {
		t.Fatalf("status = %#v", status)
	}

	spec, err := mcpclient.NewToolCallSpec(mcpclient.ToolCallPlan{Server: "gone", Tool: "search"})
	if err != nil {
		t.Fatal(err)
	}
	current := operation.Operation{
		ID: "op", MaxOutputLength: spec.MaxOutputLength, Type: spec.Type, Version: spec.Version,
		Status: operation.StatusReady, State: spec.State,
	}
	state, err := operation.DecodeRemoteJobState(current)
	if err != nil {
		t.Fatal(err)
	}
	finish := func(status operation.Status, result, message string) operation.Operation {
		state.TerminalResult, state.TerminalError = result, message
		step, err := operation.UpdateRemoteJob(current, state, status)
		if err != nil {
			t.Fatal(err)
		}
		return *step.Operation
	}
	for _, test := range []struct {
		name      string
		operation operation.Operation
		want      string
	}{
		{"running", current, "The tool call is still running."},
		{"completed", finish(operation.StatusCompleted, "42 results", ""), "42 results"},
		{"failed", finish(operation.StatusFailed, "", "server exploded"), "Error: server exploded"},
		{"canceled", finish(operation.StatusCanceled, "", "stop requested"), "The tool call was canceled: stop requested"},
		{"canceled without reason", finish(operation.StatusCanceled, "", ""), "The tool call was canceled."},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := translator.TranslateResult("call", tool.CallStatus{WaitingFor: []operation.ID{"op"}}, []operation.Operation{test.operation})
			if err != nil {
				t.Fatal(err)
			}
			if result.CallID != "call" || len(result.Output) != 1 || result.Output[0].Value != test.want {
				t.Fatalf("result = %#v", result)
			}
		})
	}
	result, err := translator.TranslateResult("call", tool.CallStatus{Error: "bad"}, nil)
	if err != nil || result.Output[0].Value != "Error: bad" {
		t.Fatalf("validation error result = %#v, %v", result, err)
	}
	if _, err := translator.TranslateResult("call", tool.CallStatus{}, nil); err == nil {
		t.Fatal("result without an operation was accepted")
	}
}
