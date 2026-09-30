package coordinator

import (
	"fmt"
	"reflect"
	"testing"
	"testing/synctest"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
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
