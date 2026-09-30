package coordinator

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

// orderTestCalls is enough calls to fill more than one map group, so that a
// map of them is not iterated in the order the calls were added.
const orderTestCalls = 16

type orderedResult struct {
	callID string
	text   string
}

func orderTestCallIDs() []string {
	callIDs := make([]string, orderTestCalls)
	for index := range callIDs {
		callIDs[index] = fmt.Sprintf("call-%02d", index)
	}
	return callIDs
}

func TestCoordinatorKeepsParallelResultsInCallOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newToolGraceTestRun(t)
		run.start(t)
		run.input(t, externalEvent(t, 0, "input", "run many tools"))
		callIDs := orderTestCallIDs()
		response := toolGraceResponse(callIDs...)
		run.respond(t, 0, response)
		// The calls finish in reverse order, each with an outcome that tells
		// it apart from its neighbours, before the grace period ends so that
		// every result arrives in one turn.
		outcomes := []operation.Status{operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled}
		want := make([]orderedResult, len(callIDs))
		for index, callID := range callIDs {
			want[index] = orderedResult{callID, string(outcomes[index%len(outcomes)])}
		}
		for index := len(callIDs) - 1; index >= 0; index-- {
			updateToolGraceCall(t, run, callIDs[index], outcomes[index%len(outcomes)])
		}
		if run.requestCount() != 2 {
			t.Fatalf("requests = %d, want the results batched in one turn", run.requestCount())
		}
		if got := resultsAfter(t, run.calls[1].request, response); !reflect.DeepEqual(got, want) {
			t.Fatalf("results = %v\nwant %v", got, want)
		}
	})
}

func TestCoordinatorShowsRunningCallsInCallOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newToolGraceTestRun(t)
		run.start(t)
		run.input(t, externalEvent(t, 0, "input", "run many tools"))
		callIDs := orderTestCallIDs()
		response := toolGraceResponse(callIDs...)
		run.respond(t, 0, response)
		finished := callIDs[5]
		updateToolGraceCall(t, run, finished, operation.StatusCompleted)
		synctest.Sleep(toolCallRunGracePeriod)
		if run.requestCount() != 2 {
			t.Fatalf("requests = %d, want grace expiry to deliver the finished call", run.requestCount())
		}
		want := make([]orderedResult, 0, len(callIDs))
		for _, callID := range callIDs {
			text := contextbuilder.ToolCallRunningPayload
			if callID == finished {
				text = string(operation.StatusCompleted)
			}
			want = append(want, orderedResult{callID, text})
		}
		if got := resultsAfter(t, run.calls[1].request, response); !reflect.DeepEqual(got, want) {
			t.Fatalf("results = %v\nwant %v", got, want)
		}
	})
}

func TestCoordinatorNamesLateResultsInCallOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newToolGraceTestRun(t)
		run.start(t)
		run.input(t, externalEvent(t, 0, "input", "run many tools"))
		callIDs := orderTestCallIDs()
		run.respond(t, 0, toolGraceResponse(callIDs...))
		run.input(t, externalEvent(t, 1, "steering", "check progress"))
		if run.requestCount() != 2 {
			t.Fatal("steering did not start a turn")
		}
		waiting := textResponse("Waiting.")
		run.respond(t, 1, waiting)
		// The calls finish together after the model has seen them running.
		for _, callID := range callIDs {
			for _, status := range run.store.appendedStatuses {
				if status.CallID == callID {
					value := status.Operations[0]
					value.Status = operation.StatusCompleted
					run.operations.updates <- value
					break
				}
			}
		}
		synctest.Wait()
		synctest.Sleep(2 * slurpIdleTimeout)
		if run.requestCount() != 3 {
			t.Fatalf("requests = %d, want the late results in one turn", run.requestCount())
		}
		want := make([]orderedResult, 0, len(callIDs))
		for _, callID := range callIDs {
			label := contextbuilder.LateToolResultLabel(llm.ToolCall{CallID: callID, Name: tool.BashName, Arguments: `{}`})
			want = append(want, orderedResult{callID, label + "\n" + string(operation.StatusCompleted)})
		}
		if got := resultsAfter(t, run.calls[2].request, waiting); !reflect.DeepEqual(got, want) {
			t.Fatalf("results = %v\nwant %v", got, want)
		}
	})
}

// resultsAfter lists the tool results that follow a response's output.
func resultsAfter(t *testing.T, request llm.Request, response llm.Response) []orderedResult {
	t.Helper()
	start := -1
	for index := range request.Input {
		if index+len(response.Output) <= len(request.Input) &&
			reflect.DeepEqual(request.Input[index:index+len(response.Output)], response.Output) {
			start = index + len(response.Output)
		}
	}
	if start < 0 {
		t.Fatal("request lacks the response output")
	}
	var results []orderedResult
	for _, item := range request.Input[start:] {
		if item.Type == llm.ItemToolResult {
			result := item.Data.(llm.ToolResult)
			results = append(results, orderedResult{result.CallID, result.Output[0].Value})
		}
	}
	return results
}

// toolResultText returns a result's first text part without the label that
// names the call of a result the model earlier saw as still running.
func toolResultText(request llm.Request, result llm.ToolResult) string {
	text := result.Output[0].Value
	for _, item := range request.Input {
		if item.Type != llm.ItemToolCall {
			continue
		}
		if call := item.Data.(llm.ToolCall); call.CallID == result.CallID {
			if payload, labeled := strings.CutPrefix(text, contextbuilder.LateToolResultLabel(call)+"\n"); labeled {
				return payload
			}
		}
	}
	return text
}

// independentCall is the call that independentToolCalls records at index.
func independentCall(index int) llm.ToolCall {
	return llm.ToolCall{CallID: fmt.Sprintf("call-%d", index), Name: tool.ViewImageName, Arguments: `{}`}
}

// lateToolResult is the result of a call the model earlier saw as still running.
func lateToolResult(call llm.ToolCall, text string) llm.Item {
	return llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: call.CallID, Output: []llm.ToolResultOutput{{
		Kind: llm.ToolResultText, Value: contextbuilder.LateToolResultLabel(call) + "\n" + text,
	}}}}
}
