package mcpclient

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/unreallabsai/unreal-agent/harness/operation"
)

const (
	// PlanType is the remote job plan of one MCP tool call.
	PlanType    operation.RemoteJobPlanType    = "mcp_tool_call"
	PlanVersion operation.RemoteJobPlanVersion = 1
)

// ToolCallPlan is the durable description of one tools/call request.
type ToolCallPlan struct {
	Server    string
	Tool      string
	Arguments jsontext.Value
}

// NewToolCallSpec returns the operation that calls plan.Tool on plan.Server.
func NewToolCallSpec(plan ToolCallPlan) (operation.Spec, error) {
	if err := validateToolCallPlan(&plan); err != nil {
		return operation.Spec{}, err
	}
	data, err := json.Marshal(plan)
	if err != nil {
		return operation.Spec{}, fmt.Errorf("encode MCP tool call plan: %w", err)
	}
	return operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: PlanType, Version: PlanVersion, Data: data})
}

// DecodeToolCallPlan reads the plan of an MCP tool call operation.
func DecodeToolCallPlan(plan operation.RemoteJobPlan) (ToolCallPlan, error) {
	if plan.Type != PlanType || plan.Version != PlanVersion {
		return ToolCallPlan{}, fmt.Errorf("remote job plan %q version %d is not an MCP tool call: %w",
			plan.Type, plan.Version, operation.ErrUnsupported)
	}
	var decoded ToolCallPlan
	if err := json.Unmarshal(plan.Data, &decoded); err != nil {
		return ToolCallPlan{}, fmt.Errorf("decode MCP tool call plan: %w", err)
	}
	if err := validateToolCallPlan(&decoded); err != nil {
		return ToolCallPlan{}, err
	}
	return decoded, nil
}

func validateToolCallPlan(plan *ToolCallPlan) error {
	if strings.TrimSpace(plan.Server) == "" {
		return errors.New("MCP tool call server must be set")
	}
	if strings.TrimSpace(plan.Tool) == "" {
		return errors.New("MCP tool call tool name must be set")
	}
	if len(plan.Arguments) == 0 {
		plan.Arguments = jsontext.Value("{}")
	}
	arguments := plan.Arguments.Clone()
	if err := arguments.Compact(); err != nil || arguments.Kind() != '{' {
		return errors.New("MCP tool call arguments must be a JSON object")
	}
	plan.Arguments = arguments
	return nil
}

// Jobs runs MCP tool call operations: an operation.RemoteJobHandler for
// PlanType. Each call runs in its own goroutine, bounded by its server's
// timeout.
type Jobs struct {
	ctx     context.Context
	stop    context.CancelFunc
	servers map[string]*Server
	http    *http.Client
	// directory keeps files a result refers to; empty keeps none.
	directory string
	updates   chan operation.Operation

	mu      sync.Mutex
	running map[operation.ID]context.CancelCauseFunc
	workers sync.WaitGroup
	closed  sync.Once
}

var _ operation.RemoteJobHandler = (*Jobs)(nil)

// NewJobs runs tool calls against servers until ctx ends or Close is called.
// Binary content and complete results that exceed an operation's output limit
// are saved under directory, one subdirectory per operation; an empty
// directory saves nothing. httpClient is the client the servers were connected
// with; Close releases its idle connections.
func NewJobs(ctx context.Context, servers []*Server, directory string, httpClient *http.Client) *Jobs {
	jobCtx, stop := context.WithCancel(ctx)
	jobs := &Jobs{
		ctx:       jobCtx,
		stop:      stop,
		servers:   make(map[string]*Server, len(servers)),
		http:      httpClient,
		directory: directory,
		updates:   make(chan operation.Operation),
		running:   make(map[operation.ID]context.CancelCauseFunc),
	}
	for _, server := range servers {
		jobs.servers[server.Config.Name] = server
	}
	return jobs
}

func (jobs *Jobs) RemoteJobPlanType() operation.RemoteJobPlanType { return PlanType }

func (jobs *Jobs) RemoteJobPlanVersion() operation.RemoteJobPlanVersion { return PlanVersion }

func (jobs *Jobs) RemoteJobUpdates() <-chan operation.Operation { return jobs.updates }

// AddRemoteJob starts a tool call. It never blocks on the server: problems
// with the call itself are reported as a failed operation.
func (jobs *Jobs) AddRemoteJob(current operation.Operation) error {
	state, err := operation.DecodeRemoteJobState(current)
	if err != nil {
		return err
	}
	if state.Plan.Type != PlanType || state.Plan.Version != PlanVersion {
		return fmt.Errorf("MCP jobs cannot run remote job plan %q version %d: %w",
			state.Plan.Type, state.Plan.Version, operation.ErrUnsupported)
	}
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	if err := jobs.ctx.Err(); err != nil {
		return fmt.Errorf("MCP jobs stopped: %w", err)
	}
	if _, exists := jobs.running[current.ID]; exists {
		return nil
	}
	ctx, cancel := context.WithCancelCause(jobs.ctx)
	jobs.running[current.ID] = cancel
	jobs.workers.Go(func() {
		defer func() {
			jobs.mu.Lock()
			delete(jobs.running, current.ID)
			jobs.mu.Unlock()
			cancel(nil)
		}()
		jobs.run(ctx, current, state)
	})
	return nil
}

// canceledError carries the reason a caller gave for canceling a call.
type canceledError struct {
	reason string
}

func (canceled *canceledError) Error() string {
	if canceled.reason == "" {
		return "tool call canceled"
	}
	return "tool call canceled: " + canceled.reason
}

// CancelRemoteJob aborts a running call; the operation then ends as canceled.
// A call that already finished is left alone.
func (jobs *Jobs) CancelRemoteJob(id operation.ID, reason string) error {
	jobs.mu.Lock()
	cancel := jobs.running[id]
	jobs.mu.Unlock()
	if cancel != nil {
		cancel(&canceledError{reason: reason})
	}
	return nil
}

// Close stops running calls, waits for them and ends every server session.
func (jobs *Jobs) Close() error {
	jobs.closed.Do(func() {
		jobs.stop()
		jobs.workers.Wait()
		var sessions sync.WaitGroup
		for _, server := range jobs.servers {
			sessions.Go(func() {
				ctx, cancel := context.WithTimeout(context.Background(), notificationTimeout)
				defer cancel()
				server.Client.Close(ctx)
			})
		}
		sessions.Wait()
		if jobs.http != nil {
			jobs.http.CloseIdleConnections()
		}
	})
	return nil
}

var errCallTimeout = errors.New("tool call timed out")

func (jobs *Jobs) run(ctx context.Context, current operation.Operation, state operation.RemoteJobState) {
	switch current.Status {
	case operation.StatusReady:
	case operation.StatusAwaiting:
		// Only a restored session hands over a call that was already sent.
		jobs.fail(current, errors.New("the runner stopped while this MCP tool call was in progress, so its result is unknown; "+
			"check whether it took effect before calling it again"))
		return
	case operation.StatusCanceling:
		jobs.cancel(current, state, "")
		return
	default:
		return
	}
	plan, err := DecodeToolCallPlan(state.Plan)
	if err != nil {
		jobs.fail(current, err)
		return
	}
	server := jobs.servers[plan.Server]
	if server == nil {
		jobs.fail(current, fmt.Errorf("MCP server %q is not connected in this run", plan.Server))
		return
	}
	var canceled *canceledError
	if errors.As(context.Cause(ctx), &canceled) {
		// Canceled before the request went out.
		jobs.cancel(current, state, canceled.reason)
		return
	}
	// Record that the request is about to be sent: a session restored after
	// a crash must not send it a second time.
	awaiting, err := operation.UpdateRemoteJob(current, state, operation.StatusAwaiting)
	if err != nil {
		jobs.fail(current, err)
		return
	}
	if !jobs.send(*awaiting.Operation) {
		return
	}
	current = *awaiting.Operation

	timeout := server.Config.callTimeout()
	callCtx, cancelCall := context.WithTimeoutCause(ctx, timeout, errCallTimeout)
	defer cancelCall()
	result, callErr := server.Client.CallTool(callCtx, plan.Tool, plan.Arguments)
	if jobs.ctx.Err() != nil {
		// The runner is shutting down; nothing is left to report to.
		return
	}
	if callErr != nil {
		switch cause := context.Cause(callCtx); {
		case errors.As(cause, &canceled):
			jobs.cancel(current, state, canceled.reason)
		case errors.Is(cause, errCallTimeout):
			jobs.fail(current, fmt.Errorf("MCP server %q did not answer the call to %q within %s",
				plan.Server, plan.Tool, timeout))
		default:
			jobs.fail(current, fmt.Errorf("MCP server %q, tool %q: %w", plan.Server, plan.Tool, callErr))
		}
		return
	}
	text := renderResult(result, jobs.saver(current.ID))
	text = jobs.keepComplete(current, text)
	state.TerminalResult, state.TerminalError = "", ""
	status := operation.StatusCompleted
	if result.IsError {
		state.TerminalError, status = text, operation.StatusFailed
	} else {
		state.TerminalResult = text
	}
	finished, err := operation.UpdateRemoteJob(current, state, status)
	if err != nil {
		jobs.fail(current, err)
		return
	}
	jobs.send(*finished.Operation)
}

func (jobs *Jobs) cancel(current operation.Operation, state operation.RemoteJobState, reason string) {
	state.TerminalResult, state.TerminalError = "", reason
	canceled, err := operation.UpdateRemoteJob(current, state, operation.StatusCanceled)
	if err != nil {
		jobs.fail(current, err)
		return
	}
	jobs.send(*canceled.Operation)
}

func (jobs *Jobs) fail(current operation.Operation, cause error) {
	failed, err := operation.FailRemoteJob(current, cause)
	if err != nil {
		// The manager validated the operation before handing it over.
		panic(fmt.Errorf("fail MCP tool call operation %q: %w", current.ID, err))
	}
	jobs.send(*failed.Operation)
}

func (jobs *Jobs) send(update operation.Operation) bool {
	select {
	case jobs.updates <- update:
		return true
	case <-jobs.ctx.Done():
		return false
	}
}
