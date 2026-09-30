package contextbuilder

import (
	_ "embed"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

// ToolCallRunningPayload is the result a running call shows until it completes.
const ToolCallRunningPayload = "Tool call is still running. Its result arrives in a later turn: continue with independent work, or end your turn to wait for it."

//go:embed prompts/preamble.md
var preambleFile string

var preamble = strings.TrimSpace(preambleFile)

type builder struct {
	request         llm.Request
	preamble        string
	systemPrompt    string
	committedPrefix []llm.Item
	stagedSuffix    []llm.Item
}

var _ Builder = (*builder)(nil)

func NewBuilder(skills ...tool.Skill) Builder {
	currentPreamble := preamble
	if skillPrompt := formatSkillsForPrompt(skills); skillPrompt != "" {
		currentPreamble += "\n\n" + skillPrompt
	}
	current := &builder{preamble: currentPreamble, committedPrefix: make([]llm.Item, 1)}
	current.SetSystemPrompt("")
	return current
}

func (current *builder) AddExternalInput(input inbox.Input) error {
	if input.Kind != inbox.InputExternal {
		return fmt.Errorf(
			"external input %q has input kind %q",
			input.ID,
			input.Kind,
		)
	}

	var text string
	if err := json.Unmarshal(input.Payload, &text); err != nil {
		return fmt.Errorf("decode external input %q: %w", input.ID, err)
	}
	current.stagedSuffix = append(current.stagedSuffix, llm.Item{
		Type: llm.ItemMessage,
		Data: llm.Message{Role: llm.RoleUser, Text: text},
	})
	return nil
}

func (current *builder) SetModel(model llm.Model) {
	current.request.Model = model
}

func (current *builder) AddControlMessage(request inbox.ControlMessage) {
	switch request.Mode {
	case inbox.UpdateSettings:
		settings := request.Parameters.(inbox.Settings)
		current.request.Model.ReasoningEffort = settings.ReasoningEffort
	case inbox.Heartbeat:
		current.stagedSuffix = append(current.stagedSuffix, llm.Item{
			Type: llm.ItemMessage,
			Data: llm.Message{Role: llm.RoleUser, Text: request.Reason},
		})
	}
}

func (current *builder) SetSystemPrompt(prompt string) {
	current.systemPrompt = prompt
	current.committedPrefix[0] = llm.Item{Type: llm.ItemMessage, Data: llm.Message{
		Role: llm.RoleSystem,
		Text: strings.TrimSpace(current.preamble + "\n\n" + current.systemPrompt),
	}}
}

func (current *builder) AddModelResponse(response llm.Response) {
	current.committedPrefix = append(current.committedPrefix, response.Output...)
}

func (current *builder) AddReasoning(reasoning llm.Reasoning) {
	current.stagedSuffix = append(current.stagedSuffix, llm.Item{
		Type: llm.ItemReasoning,
		Data: reasoning,
	})
}

func (current *builder) AddTool(tool llm.Tool) {
	current.request.Tools = append(current.request.Tools, tool)
}

func (current *builder) AddToolResult(
	callID string,
	payload []llm.ToolResultOutput,
	running bool,
) {
	if running {
		payload = []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: ToolCallRunningPayload}}
	}
	result := llm.Item{
		Type: llm.ItemToolResult,
		Data: llm.ToolResult{CallID: callID, Output: payload},
	}
	// A result takes the place of its call's staged placeholder, so results
	// keep the order in which the model issued the calls instead of the order
	// in which the calls finish. Many chat templates render tool results
	// without their call IDs, which leaves position as the only link between
	// a result and its call. At most one placeholder per call is staged,
	// because a new one also replaces the previous one.
	index := slices.IndexFunc(current.stagedSuffix, func(item llm.Item) bool {
		return isRunningResult(item, callID)
	})
	if index >= 0 {
		current.stagedSuffix[index] = result
		return
	}
	current.stagedSuffix = append(current.stagedSuffix, result)
}

// isRunningResult reports whether item is the running placeholder of callID.
func isRunningResult(item llm.Item, callID string) bool {
	if item.Type != llm.ItemToolResult {
		return false
	}
	result := item.Data.(llm.ToolResult)
	return result.CallID == callID && len(result.Output) == 1 &&
		result.Output[0] == llm.ToolResultOutput{Kind: llm.ToolResultText, Value: ToolCallRunningPayload}
}

func (current *builder) Commit() {
	current.committedPrefix = append(current.committedPrefix, current.stagedSuffix...)
	current.stagedSuffix = nil
}

func (current *builder) Build() (Result, error) {
	request := current.request
	input := make([]llm.Item, 0, len(current.committedPrefix)+len(current.stagedSuffix))
	input = append(input, current.committedPrefix...)
	request.Input = append(input, current.stagedSuffix...)
	request.Tools = append([]llm.Tool(nil), request.Tools...)
	return Result{Request: request}, nil
}
