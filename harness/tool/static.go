package tool

import (
	"fmt"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

type unavailableTranslator struct {
	name string
}

func (translator unavailableTranslator) Translate(Context, llm.ToolCall) CallStatus {
	return CallStatus{Error: translator.errorMessage()}
}

func (translator unavailableTranslator) TranslateResult(
	callID string,
	_ CallStatus,
	_ []operation.Operation,
) (llm.ToolResult, error) {
	return llm.ToolResult{CallID: callID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: translator.errorMessage()}}}, nil
}

func (translator unavailableTranslator) errorMessage() string {
	return fmt.Sprintf("static tool %q is not configured", translator.name)
}

func StaticNames() []string {
	definitions := staticDefinitions()
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Tool.Name)
	}
	return names
}

func staticDefinitions() []Definition {
	return []Definition{
		{Tool: llm.Tool{
			Type:        llm.ToolFunction,
			Name:        BashName,
			Description: "Execute a shell command in background. Independent commands may be issued as parallel tool calls in one turn. Every call starts a new shell in the same working directory: variables, cd and other shell state do not carry over, so keep anything a later call needs in files. When the shell exits, the processes it started are terminated, including jobs put in the background with &, nohup or disown. To keep a long-running process such as a server, build or watcher going, run it in the foreground of its own call: calls run in the background, and its result arrives when it exits. Stop such a process when it is no longer needed; the session does not end while a call is running.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "The shell command to execute.",
					},
					"max_output_length": maxOutputLengthSchema(),
				},
				"required": []any{"command"},
			},
		}},
		{Tool: llm.Tool{
			Type:        llm.ToolFunction,
			Name:        ViewImageName,
			Description: "View a local JPEG, PNG, BMP, TIFF, or WebP image.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Image file path, absolute or relative to the workspace.",
					},
				},
				"required": []any{"path"},
			},
		}},
		{Tool: llm.Tool{
			Type:        llm.ToolFunction,
			Name:        SkillUseName,
			Description: "Load the instructions for a registered skill.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "The exact name of the skill to load.",
					},
				},
				"required": []any{"name"},
			},
		}},
	}
}

func maxOutputLengthSchema() map[string]any {
	return map[string]any{
		"type":        "integer",
		"description": fmt.Sprintf("Maximum characters per output text field. Truncated text keeps its head and tail, around a marker stating how much was omitted, and path to the file with the complete stream. Defaults to %d.", operation.DefaultMaxOutputLength),
		"minimum":     1,
		"maximum":     operation.MaxOutputLength,
		"default":     operation.DefaultMaxOutputLength,
	}
}
