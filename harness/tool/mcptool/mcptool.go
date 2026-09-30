// Package mcptool offers the tools of MCP servers to the model as ordinary
// function tools, one per server tool, and translates their calls into MCP
// tool call operations.
package mcptool

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/mcpclient"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

// NamePrefix starts the model-facing name of every MCP tool:
// mcp__<server>__<tool>.
const NamePrefix = "mcp__"

// maxNameLength is the longest function name providers accept.
const maxNameLength = 64

// Tool is a tool that an MCP server listed.
type Tool struct {
	Server string
	Tool   mcpclient.Tool
}

type registry struct {
	tool.Registry
	definitions []tool.Definition
	translators map[string]*translator
}

// NewRegistry returns base with tools added after its own definitions,
// sorted by name so that the model sees the same tool list in every run that
// has the same tools (which keeps provider prompt caches valid). Tools named
// in disallowed are left out. Every other name that starts with
// NamePrefix still resolves, to a translator that rejects new calls but can
// read the results of calls recorded in a session, so that a session stays
// readable when a server is gone. The returned errors describe tools that
// were left out.
func NewRegistry(base tool.Registry, tools []Tool, disallowed []string) (tool.Registry, []error) {
	current := &registry{Registry: base, translators: make(map[string]*translator)}
	taken := make(map[string]struct{})
	for _, definition := range base.StaticDefinitions() {
		taken[definition.Tool.Name] = struct{}{}
	}
	sorted := slices.Clone(tools)
	slices.SortStableFunc(sorted, func(a, b Tool) int {
		return cmp.Or(
			strings.Compare(Name(a.Server, a.Tool.Name), Name(b.Server, b.Tool.Name)),
			strings.Compare(a.Server, b.Server),
			strings.Compare(a.Tool.Name, b.Tool.Name),
		)
	})
	var problems []error
	for _, listed := range sorted {
		name := Name(listed.Server, listed.Tool.Name)
		if slices.Contains(disallowed, name) {
			continue
		}
		if _, exists := taken[name]; exists {
			problems = append(problems, fmt.Errorf("MCP server %q tool %q: name %q is already taken", listed.Server, listed.Tool.Name, name))
			continue
		}
		parameters, err := parameters(listed.Tool.InputSchema)
		if err != nil {
			problems = append(problems, fmt.Errorf("MCP server %q tool %q: %w", listed.Server, listed.Tool.Name, err))
			continue
		}
		taken[name] = struct{}{}
		current.translators[name] = &translator{server: listed.Server, tool: listed.Tool.Name}
		current.definitions = append(current.definitions, tool.Definition{Tool: llm.Tool{
			Type:        llm.ToolFunction,
			Name:        name,
			Description: description(listed),
			Parameters:  parameters,
		}})
	}
	return current, problems
}

func (current *registry) StaticDefinitions() []tool.Definition {
	return append(current.Registry.StaticDefinitions(), current.definitions...)
}

func (current *registry) Resolve(name string) (tool.Translator, bool) {
	if translator, exists := current.Registry.Resolve(name); exists {
		return translator, true
	}
	if translator, exists := current.translators[name]; exists {
		return translator, true
	}
	if strings.HasPrefix(name, NamePrefix) {
		return &translator{}, true
	}
	return nil, false
}

// Name is the model-facing name of an MCP tool. Characters providers do not
// accept become '_'; a name longer than providers accept is shortened and
// given a hash suffix, which keeps it unique and stable.
func Name(server, name string) string {
	full := NamePrefix + sanitize(server) + "__" + sanitize(name)
	if len(full) <= maxNameLength {
		return full
	}
	sum := sha256.Sum256([]byte(server + "\x00" + name))
	suffix := "_" + hex.EncodeToString(sum[:])[:8]
	return full[:maxNameLength-len(suffix)] + suffix
}

func sanitize(name string) string {
	var sanitized strings.Builder
	for _, character := range name {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9', character == '_', character == '-':
			sanitized.WriteRune(character)
		default:
			sanitized.WriteByte('_')
		}
	}
	return sanitized.String()
}

func description(listed Tool) string {
	switch {
	case strings.TrimSpace(listed.Tool.Description) != "":
		return listed.Tool.Description
	case strings.TrimSpace(listed.Tool.Title) != "":
		return listed.Tool.Title
	default:
		return fmt.Sprintf("Tool %q of MCP server %q.", listed.Tool.Name, listed.Server)
	}
}

// parameters turns a tool's input schema into function parameters. A schema
// must describe an object; a missing schema accepts an empty object.
func parameters(schema jsontext.Value) (map[string]any, error) {
	parameters := map[string]any{}
	if len(schema) != 0 && string(schema) != "null" {
		if err := json.Unmarshal(schema, &parameters); err != nil {
			return nil, fmt.Errorf("input schema is not a JSON object: %w", err)
		}
	}
	switch kind := parameters["type"]; kind {
	case nil:
		parameters["type"] = "object"
	case "object":
	default:
		return nil, fmt.Errorf("input schema type is %v, want object", kind)
	}
	if _, exists := parameters["properties"]; !exists {
		parameters["properties"] = map[string]any{}
	}
	return parameters, nil
}

// translator calls one MCP tool. The zero value stands for a tool that is not
// available: it rejects calls and reads recorded results.
type translator struct {
	server string
	tool   string
}

func (current *translator) Translate(ctx tool.Context, call llm.ToolCall) tool.CallStatus {
	if current.server == "" {
		return tool.ErrorStatus(fmt.Sprintf("tool %q is not available", call.Name), 0)
	}
	arguments := strings.TrimSpace(call.Arguments)
	if arguments == "" {
		arguments = "{}"
	}
	spec, err := mcpclient.NewToolCallSpec(mcpclient.ToolCallPlan{
		Server:    current.server,
		Tool:      current.tool,
		Arguments: jsontext.Value(arguments),
	})
	if err != nil {
		return tool.ErrorStatus(fmt.Sprintf("%s: %v", call.Name, err), 0)
	}
	return tool.CallStatus{WaitingFor: []operation.ID{ctx.Submit(spec)}}
}

func (current *translator) TranslateResult(
	callID string,
	status tool.CallStatus,
	operations []operation.Operation,
) (llm.ToolResult, error) {
	result := llm.ToolResult{CallID: callID}
	text := func(value string) (llm.ToolResult, error) {
		result.Output = []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: value}}
		return result, nil
	}
	if status.Error != "" {
		if len(operations) != 0 {
			return result, fmt.Errorf("MCP tool call %q has both a validation error and operations", callID)
		}
		return text("Error: " + status.Error)
	}
	if len(operations) != 1 {
		return result, fmt.Errorf("MCP tool call %q has %d operations, want 1", callID, len(operations))
	}
	state, err := operation.DecodeRemoteJobState(operations[0])
	if err != nil {
		return result, fmt.Errorf("decode MCP tool call %q result: %w", callID, err)
	}
	switch operations[0].Status {
	case operation.StatusReady, operation.StatusAwaiting, operation.StatusCanceling:
		return text("The tool call is still running.")
	case operation.StatusCompleted:
		return text(state.TerminalResult)
	case operation.StatusFailed:
		return text("Error: " + state.TerminalError)
	case operation.StatusCanceled:
		if state.TerminalError != "" {
			return text("The tool call was canceled: " + state.TerminalError)
		}
		return text("The tool call was canceled.")
	default:
		return result, fmt.Errorf("MCP tool call %q operation %q has unsupported status %q",
			callID, operations[0].ID, operations[0].Status)
	}
}
