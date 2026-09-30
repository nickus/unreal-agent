package contextbuilder

import (
	_ "embed"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

// ToolCallRunningPayload is the result a running call shows until it completes.
const ToolCallRunningPayload = "Tool call is still running. Its result arrives in a later turn: continue with independent work, or end your turn to wait for it."

//go:embed prompts/preamble.md
var preambleFile string

var preamble = strings.TrimSpace(preambleFile)

// lateResultArgumentsLimit bounds the call arguments quoted in a late result's
// label: enough to tell parallel calls apart without repeating a long command.
const lateResultArgumentsLimit = 200

type builder struct {
	request         llm.Request
	preamble        string
	systemPrompt    string
	committedPrefix []llm.Item
	stagedSuffix    []llm.Item
	// shownRunning holds the calls whose running placeholder has been
	// committed and whose final result has not been added yet.
	shownRunning map[string]struct{}
}

var _ Builder = (*builder)(nil)

func NewBuilder(skills ...tool.Skill) Builder {
	currentPreamble := preamble
	if skillPrompt := formatSkillsForPrompt(skills); skillPrompt != "" {
		currentPreamble += "\n\n" + skillPrompt
	}
	current := &builder{
		preamble:        currentPreamble,
		committedPrefix: make([]llm.Item, 1),
		shownRunning:    make(map[string]struct{}),
	}
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
	} else if _, shown := current.shownRunning[callID]; shown {
		delete(current.shownRunning, callID)
		payload = labelLateResult(current.findToolCall(callID), payload)
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

// findToolCall returns the committed call with the given ID, searching from
// the most recent item because pending calls are usually recent. An unknown
// call is returned with only its ID set.
//
// The lookup assumes call IDs are unique across the conversation, as does the
// rest of the builder (shownRunning, placeholder replacement): the request
// pairs each tool result with its call by call ID alone, so a provider that
// reused an ID in a later response would make the request itself ambiguous.
func (current *builder) findToolCall(callID string) llm.ToolCall {
	for _, item := range slices.Backward(current.committedPrefix) {
		if item.Type != llm.ItemToolCall {
			continue
		}
		if call := item.Data.(llm.ToolCall); call.CallID == callID {
			return call
		}
	}
	return llm.ToolCall{CallID: callID}
}

// LateToolResultLabel is the line that starts the result of a call the model
// has already seen as still running. Such a result arrives in a later turn,
// often next to the calls and results of a newer response, where its position
// no longer identifies its call, and many chat templates omit call IDs. A
// call without a name is unknown, and only its ID is given.
func LateToolResultLabel(call llm.ToolCall) string {
	if call.Name == "" {
		return fmt.Sprintf("Result of the earlier call with call ID %q, previously shown as still running:", call.CallID)
	}
	return fmt.Sprintf(
		"Result of the earlier %s call %s (call ID %q), previously shown as still running:",
		call.Name, truncateArguments(call.Arguments), call.CallID,
	)
}

func labelLateResult(call llm.ToolCall, payload []llm.ToolResultOutput) []llm.ToolResultOutput {
	label := LateToolResultLabel(call)
	labeled := make([]llm.ToolResultOutput, 0, len(payload)+1)
	if len(payload) != 0 && payload[0].Kind == llm.ToolResultText {
		// Prefix the first text part rather than adding one: some servers
		// join text parts without a separator.
		labeled = append(labeled, llm.ToolResultOutput{
			Kind:  llm.ToolResultText,
			Value: label + "\n" + payload[0].Value,
		})
		return append(labeled, payload[1:]...)
	}
	labeled = append(labeled, llm.ToolResultOutput{Kind: llm.ToolResultText, Value: label})
	return append(labeled, payload...)
}

func truncateArguments(arguments string) string {
	if len(arguments) <= lateResultArgumentsLimit {
		return arguments
	}
	cut := lateResultArgumentsLimit
	for cut > 0 && !utf8.RuneStart(arguments[cut]) {
		cut--
	}
	return arguments[:cut] + "…"
}

func (current *builder) Commit() {
	for _, item := range current.stagedSuffix {
		if item.Type == llm.ItemToolResult {
			if callID := item.Data.(llm.ToolResult).CallID; isRunningResult(item, callID) {
				current.shownRunning[callID] = struct{}{}
			}
		}
	}
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
