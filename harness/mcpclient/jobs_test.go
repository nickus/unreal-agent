package mcpclient

import (
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func connectedServer(t *testing.T, fake *fakeServer, name string, timeout time.Duration) *Server {
	t.Helper()
	config := fake.config(name)
	config.Timeout = timeout
	servers, errs := Connect(t.Context(), []ServerConfig{config}, fake.Client())
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	return servers[0]
}

func startJobs(t *testing.T, directory string, servers ...*Server) (*Jobs, *operation.LocalOperationManager) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	jobs := NewJobs(ctx, servers, directory, nil)
	t.Cleanup(func() {
		cancel()
		if err := jobs.Close(); err != nil {
			t.Error(err)
		}
	})
	return jobs, operation.NewLocalOperationManager(ctx, jobs)
}

func toolCallOperation(t *testing.T, id operation.ID, plan ToolCallPlan) operation.Operation {
	t.Helper()
	spec, err := NewToolCallSpec(plan)
	if err != nil {
		t.Fatal(err)
	}
	return operation.Operation{
		MaxOutputLength: spec.MaxOutputLength,
		ID:              id,
		Type:            spec.Type,
		Version:         spec.Version,
		Status:          operation.StatusReady,
		State:           spec.State,
	}
}

// awaitTerminal collects updates until the operation ends and returns the
// statuses it went through with the final state.
func awaitTerminal(t *testing.T, manager *operation.LocalOperationManager) ([]operation.Status, operation.RemoteJobState) {
	t.Helper()
	var statuses []operation.Status
	timeout := time.After(10 * time.Second)
	for {
		select {
		case update := <-manager.Updates():
			statuses = append(statuses, update.Status)
			switch update.Status {
			case operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled:
				state, err := operation.DecodeRemoteJobState(update)
				if err != nil {
					t.Fatal(err)
				}
				return statuses, state
			}
		case <-timeout:
			t.Fatalf("operation did not finish; statuses so far: %v", statuses)
		}
	}
}

func textResult(text string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
}

func TestJobsCompleteToolCalls(t *testing.T) {
	fake := newFakeServer(t)
	fake.handle("echo", func(_ context.Context, arguments map[string]any) (any, *fakeError) {
		return textResult("echo: " + arguments["text"].(string)), nil
	})
	_, manager := startJobs(t, "", connectedServer(t, fake, "fake", DefaultTimeout))
	if err := manager.Add(toolCallOperation(t, "call-1", ToolCallPlan{
		Server: "fake", Tool: "echo", Arguments: jsontext.Value(`{ "text" : "hello" }`),
	})); err != nil {
		t.Fatal(err)
	}
	statuses, state := awaitTerminal(t, manager)
	if !slices.Equal(statuses, []operation.Status{operation.StatusAwaiting, operation.StatusCompleted}) {
		t.Fatalf("statuses = %v", statuses)
	}
	if state.TerminalResult != "echo: hello" || state.TerminalError != "" {
		t.Fatalf("state = %#v", state)
	}
	last := fake.recorded()[len(fake.recorded())-1]
	if last.Method != "tools/call" || string(last.Params) != `{"name":"echo","arguments":{"text":"hello"}}` {
		t.Fatalf("call = %s %s", last.Method, last.Params)
	}
}

func TestJobsReportFailures(t *testing.T) {
	fake := newFakeServer(t)
	fake.handle("refuse", func(context.Context, map[string]any) (any, *fakeError) {
		result := textResult("the tool refused")
		result["isError"] = true
		return result, nil
	})
	fake.handle("broken", func(context.Context, map[string]any) (any, *fakeError) {
		return nil, &fakeError{Code: -32603, Message: "internal failure"}
	})
	fake.handle("slow", func(ctx context.Context, _ map[string]any) (any, *fakeError) {
		<-ctx.Done()
		return nil, &fakeError{Code: -1, Message: "gone"}
	})
	server := connectedServer(t, fake, "fake", 100*time.Millisecond)
	for _, test := range []struct {
		name   string
		plan   ToolCallPlan
		status operation.Status
		want   string
	}{
		{"tool error", ToolCallPlan{Server: "fake", Tool: "refuse"}, operation.StatusFailed, "the tool refused"},
		{"JSON-RPC error", ToolCallPlan{Server: "fake", Tool: "broken"}, operation.StatusFailed,
			`MCP server "fake", tool "broken": JSON-RPC error -32603: internal failure`},
		{"timeout", ToolCallPlan{Server: "fake", Tool: "slow"}, operation.StatusFailed,
			`MCP server "fake" did not answer the call to "slow" within 100ms`},
		{"unknown server", ToolCallPlan{Server: "gone", Tool: "echo"}, operation.StatusFailed,
			`MCP server "gone" is not connected in this run`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, manager := startJobs(t, "", server)
			if err := manager.Add(toolCallOperation(t, "call", test.plan)); err != nil {
				t.Fatal(err)
			}
			statuses, state := awaitTerminal(t, manager)
			if statuses[len(statuses)-1] != test.status || state.TerminalError != test.want || state.TerminalResult != "" {
				t.Fatalf("statuses = %v, state = %#v", statuses, state)
			}
		})
	}
}

func TestJobsCancelRunningCalls(t *testing.T) {
	fake := newFakeServer(t)
	started := make(chan struct{})
	fake.handle("slow", func(ctx context.Context, _ map[string]any) (any, *fakeError) {
		close(started)
		<-ctx.Done()
		return nil, &fakeError{Code: -1, Message: "gone"}
	})
	server := connectedServer(t, fake, "fake", DefaultTimeout)
	_, manager := startJobs(t, "", server)
	if err := manager.Add(toolCallOperation(t, "call", ToolCallPlan{Server: "fake", Tool: "slow"})); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := manager.Cancel("call", "stop requested"); err != nil {
		t.Fatal(err)
	}
	statuses, state := awaitTerminal(t, manager)
	if statuses[len(statuses)-1] != operation.StatusCanceled || state.TerminalError != "stop requested" {
		t.Fatalf("statuses = %v, state = %#v", statuses, state)
	}
	server.Client.Close(t.Context())
	cancelled := slices.ContainsFunc(fake.recordedNotifications(), func(request fakeRequest) bool {
		return request.Method == "notifications/cancelled"
	})
	if !cancelled {
		t.Fatal("server was not told about the cancellation")
	}
}

func TestJobsDoNotRepeatInterruptedCalls(t *testing.T) {
	fake := newFakeServer(t)
	calls := 0
	fake.handle("create", func(context.Context, map[string]any) (any, *fakeError) {
		calls++
		return textResult("created"), nil
	})
	_, manager := startJobs(t, "", connectedServer(t, fake, "fake", DefaultTimeout))
	current := toolCallOperation(t, "restored", ToolCallPlan{Server: "fake", Tool: "create"})
	state, err := operation.DecodeRemoteJobState(current)
	if err != nil {
		t.Fatal(err)
	}
	awaiting, err := operation.UpdateRemoteJob(current, state, operation.StatusAwaiting)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Add(*awaiting.Operation); err != nil {
		t.Fatal(err)
	}
	statuses, final := awaitTerminal(t, manager)
	if !slices.Equal(statuses, []operation.Status{operation.StatusFailed}) ||
		!strings.Contains(final.TerminalError, "its result is unknown") || calls != 0 {
		t.Fatalf("statuses = %v, state = %#v, calls = %d", statuses, final, calls)
	}
}

func TestJobsSaveBinaryContentAndCompleteLongResults(t *testing.T) {
	fake := newFakeServer(t)
	long := strings.Repeat("0123456789", 5_000)
	fake.handle("report", func(context.Context, map[string]any) (any, *fakeError) {
		return map[string]any{"content": []any{
			map[string]any{"type": "text", "text": long},
			map[string]any{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString([]byte("png bytes"))},
		}}, nil
	})
	directory := t.TempDir()
	_, manager := startJobs(t, directory, connectedServer(t, fake, "fake", DefaultTimeout))
	if err := manager.Add(toolCallOperation(t, "report-call", ToolCallPlan{Server: "fake", Tool: "report"})); err != nil {
		t.Fatal(err)
	}
	statuses, state := awaitTerminal(t, manager)
	if statuses[len(statuses)-1] != operation.StatusCompleted || !state.ResultTruncated {
		t.Fatalf("statuses = %v, state = %#v", statuses, state)
	}
	resultPath := filepath.Join(directory, "report-call", "result.txt")
	imagePath := filepath.Join(directory, "report-call", "content-2.png")
	if !strings.HasPrefix(state.TerminalResult, "The complete result (") || !strings.Contains(state.TerminalResult, resultPath) {
		t.Fatalf("result does not name the saved copy: %.200s", state.TerminalResult)
	}
	if !strings.HasSuffix(state.TerminalResult, "[image content (image/png, 9 bytes) saved to "+imagePath+"]") {
		t.Fatalf("result does not end with the image: %.200s", state.TerminalResult[len(state.TerminalResult)-200:])
	}
	if len([]rune(state.TerminalResult)) > operation.DefaultMaxOutputLength+100 {
		t.Fatalf("result has %d characters", len([]rune(state.TerminalResult)))
	}
	saved, err := os.ReadFile(resultPath)
	if err != nil || !strings.Contains(string(saved), long) {
		t.Fatalf("saved result: %v", err)
	}
	image, err := os.ReadFile(imagePath)
	if err != nil || string(image) != "png bytes" {
		t.Fatalf("saved image = %q, %v", image, err)
	}
	for _, path := range []string{filepath.Dir(resultPath), resultPath, imagePath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s mode = %v", path, info.Mode())
		}
	}
}

func TestJobsCloseStopsRunningCalls(t *testing.T) {
	fake := newFakeServer(t)
	started := make(chan struct{})
	fake.handle("slow", func(ctx context.Context, _ map[string]any) (any, *fakeError) {
		close(started)
		<-ctx.Done()
		return nil, &fakeError{Code: -1, Message: "gone"}
	})
	server := connectedServer(t, fake, "fake", DefaultTimeout)
	jobs := NewJobs(t.Context(), []*Server{server}, "", nil)
	manager := operation.NewLocalOperationManager(t.Context(), jobs)
	if err := manager.Add(toolCallOperation(t, "call", ToolCallPlan{Server: "fake", Tool: "slow"})); err != nil {
		t.Fatal(err)
	}
	if update := <-manager.Updates(); update.Status != operation.StatusAwaiting {
		t.Fatalf("status = %q", update.Status)
	}
	<-started
	done := make(chan struct{})
	go func() {
		_ = jobs.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return while a call was running")
	}
	if err := jobs.AddRemoteJob(toolCallOperation(t, "late", ToolCallPlan{Server: "fake", Tool: "slow"})); err == nil {
		t.Fatal("closed jobs accepted a call")
	}
	if !slices.Contains(fake.deletedSessions(), "session-1") {
		t.Fatalf("session was not closed: %v", fake.deletedSessions())
	}
}

func TestToolCallPlanValidation(t *testing.T) {
	for _, arguments := range []string{`[]`, `"text"`, `{`, `null`} {
		if _, err := NewToolCallSpec(ToolCallPlan{Server: "s", Tool: "t", Arguments: jsontext.Value(arguments)}); err == nil {
			t.Errorf("arguments %s were accepted", arguments)
		}
	}
	if _, err := NewToolCallSpec(ToolCallPlan{Tool: "t"}); err == nil {
		t.Error("a plan without a server was accepted")
	}
	spec, err := NewToolCallSpec(ToolCallPlan{Server: "s", Tool: "t"})
	if err != nil {
		t.Fatal(err)
	}
	state, err := operation.DecodeRemoteJobState(operation.Operation{
		MaxOutputLength: spec.MaxOutputLength, ID: "x", Type: spec.Type, Version: spec.Version, State: spec.State,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := DecodeToolCallPlan(state.Plan)
	if err != nil || string(plan.Arguments) != "{}" {
		t.Fatalf("plan = %#v, %v", plan, err)
	}
	if _, err := DecodeToolCallPlan(operation.RemoteJobPlan{Type: "other", Version: 1, Data: jsontext.Value("{}")}); err == nil {
		t.Fatal("decoded a plan of another type")
	}
}
