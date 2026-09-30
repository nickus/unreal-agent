package mcpclient

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func connectFake(t *testing.T, fake *fakeServer) *Client {
	t.Helper()
	client := NewClient(fake.config("fake"), fake.Client(), Implementation{Name: "test", Version: "1"})
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	t.Cleanup(func() { client.Close(context.Background()) })
	return client
}

func TestClientInitializesAndListsEveryPage(t *testing.T) {
	fake := newFakeServer(t)
	fake.pageSize = 2
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		fake.handle(name, nil)
	}
	client := connectFake(t, fake)
	tools, err := client.ListTools(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, listed := range tools {
		names = append(names, listed.Name)
	}
	if !slices.Equal(names, []string{"a", "b", "c", "d", "e"}) {
		t.Fatalf("tools = %v", names)
	}

	requests := fake.recorded()
	if len(requests) != 4 || requests[0].Method != "initialize" {
		t.Fatalf("requests = %#v", requests)
	}
	for index, request := range requests {
		if got := request.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("request %d Authorization = %q", index, got)
		}
		if got := request.Header.Get("Accept"); got != "application/json, text/event-stream" {
			t.Errorf("request %d Accept = %q", index, got)
		}
		if index == 0 {
			if request.Session != "" || request.Header.Get("MCP-Protocol-Version") != "" {
				t.Errorf("initialize carried session headers: %v", request.Header)
			}
			continue
		}
		if request.Session != "session-1" || request.Header.Get("MCP-Protocol-Version") != LatestProtocolVersion {
			t.Errorf("request %d session = %q, protocol version = %q", index, request.Session, request.Header.Get("MCP-Protocol-Version"))
		}
	}
	notifications := fake.recordedNotifications()
	if len(notifications) != 1 || notifications[0].Method != "notifications/initialized" || notifications[0].Session != "session-1" {
		t.Fatalf("notifications = %#v", notifications)
	}

	client.Close(t.Context())
	if !slices.Equal(fake.deletedSessions(), []string{"session-1"}) {
		t.Fatalf("deleted sessions = %v", fake.deletedSessions())
	}
}

func TestClientWithoutToolsCapabilityListsNothing(t *testing.T) {
	fake := newFakeServer(t)
	fake.noTools = true
	fake.handle("hidden", nil)
	tools, err := connectFake(t, fake).ListTools(t.Context())
	if err != nil || len(tools) != 0 {
		t.Fatalf("ListTools() = %v, %v", tools, err)
	}
}

func TestClientRejectsUnsupportedProtocolVersion(t *testing.T) {
	fake := newFakeServer(t)
	fake.protocolVersion = "1999-01-01"
	client := NewClient(fake.config("fake"), fake.Client(), ClientInfo())
	err := client.Initialize(t.Context())
	if err == nil || !strings.Contains(err.Error(), `protocol version "1999-01-01"`) {
		t.Fatalf("Initialize() error = %v", err)
	}
}

func TestClientAcceptsOlderProtocolVersionsAndServersWithoutSessions(t *testing.T) {
	fake := newFakeServer(t)
	fake.protocolVersion = "2025-03-26"
	fake.noSession = true
	fake.handle("echo", func(_ context.Context, arguments map[string]any) (any, *fakeError) {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": arguments["text"]}}}, nil
	})
	client := connectFake(t, fake)
	result, err := client.CallTool(t.Context(), "echo", jsontext.Value(`{"text":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := renderResult(result, nil); got != "hi" {
		t.Fatalf("result = %q", got)
	}
	last := fake.recorded()[len(fake.recorded())-1]
	if last.Session != "" || last.Header.Get("MCP-Protocol-Version") != "2025-03-26" {
		t.Fatalf("call headers = %v", last.Header)
	}
	client.Close(t.Context())
	if len(fake.deletedSessions()) != 0 {
		t.Fatalf("closed a session the server never opened: %v", fake.deletedSessions())
	}
}

func TestClientReadsEventStreamResponses(t *testing.T) {
	fake := newFakeServer(t)
	fake.stream = true
	fake.handle("echo", func(_ context.Context, arguments map[string]any) (any, *fakeError) {
		return map[string]any{
			"content":           []any{map[string]any{"type": "text", "text": fmt.Sprint(arguments["text"])}},
			"structuredContent": map[string]any{"echo": arguments["text"]},
		}, nil
	})
	client := connectFake(t, fake)
	result, err := client.CallTool(t.Context(), "echo", jsontext.Value(`{"text":"streamed"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := renderResult(result, nil); got != "streamed\nStructured content: {\"echo\":\"streamed\"}" {
		t.Fatalf("result = %q", got)
	}
	answers := fake.recordedAnswers()
	if len(answers) != 1 || string(answers[0].ID) != `"server-ping"` || string(answers[0].Result) != "{}" {
		t.Fatalf("answers to server requests = %#v", answers)
	}
}

func TestClientReportsErrors(t *testing.T) {
	for _, test := range []struct {
		name    string
		handler http.HandlerFunc
		want    string
		check   func(error) bool
	}{
		{
			name: "JSON-RPC error",
			handler: func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				fmt.Fprint(response, `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"bad arguments","data":{"field":"text"}}}`)
			},
			want: `JSON-RPC error -32602: bad arguments (data: {"field":"text"})`,
			check: func(err error) bool {
				var rpcErr *RPCError
				return errors.As(err, &rpcErr) && rpcErr.Code == -32602
			},
		},
		{
			name: "JSON-RPC error with HTTP status",
			handler: func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				response.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(response, `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"params.name is required"}}`)
			},
			want: "HTTP 400: JSON-RPC error -32602: params.name is required",
		},
		{
			name: "HTTP error",
			handler: func(response http.ResponseWriter, _ *http.Request) {
				http.Error(response, "  bearer\n token   is required ", http.StatusUnauthorized)
			},
			want: "HTTP 401 Unauthorized: bearer token is required",
			check: func(err error) bool {
				var statusErr *HTTPStatusError
				return errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusUnauthorized
			},
		},
		{
			name: "no response",
			handler: func(response http.ResponseWriter, _ *http.Request) {
				response.WriteHeader(http.StatusAccepted)
			},
			want: "server answered HTTP 202 without a response",
		},
		{
			name: "other content type",
			handler: func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "text/html")
				fmt.Fprint(response, "<html>")
			},
			want: `unexpected response content type "text/html"`,
		},
		{
			name: "stream without the response",
			handler: func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(response, "data: {\"jsonrpc\":\"2.0\",\"id\":99,\"result\":{}}\n\n")
			},
			want: "event stream ended before the response",
		},
		{
			name: "invalid JSON",
			handler: func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				fmt.Fprint(response, "{")
			},
			want: "server sent invalid JSON",
		},
		{
			name: "oversized response",
			handler: func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				chunk := strings.Repeat(" ", 1<<20)
				for range maxResponseBytes/len(chunk) + 1 {
					if _, err := fmt.Fprint(response, chunk); err != nil {
						return
					}
				}
			},
			want: fmt.Sprintf("response exceeds %d bytes", maxResponseBytes),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			client := NewClient(ServerConfig{Name: "broken", URL: server.URL}, server.Client(), ClientInfo())
			// Skip initialize: its session state is not needed to read an answer.
			_, err := client.CallTool(t.Context(), "tool", nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("CallTool() error = %v, want %q", err, test.want)
			}
			if test.check != nil && !test.check(err) {
				t.Fatalf("CallTool() error %#v has the wrong type", err)
			}
		})
	}
}

func TestClientRenewsExpiredSession(t *testing.T) {
	fake := newFakeServer(t)
	fake.handle("echo", func(context.Context, map[string]any) (any, *fakeError) {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}, nil
	})
	client := connectFake(t, fake)
	fake.mu.Lock()
	fake.expireSession = true
	fake.mu.Unlock()
	result, err := client.CallTool(t.Context(), "echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := renderResult(result, nil); got != "ok" {
		t.Fatalf("result = %q", got)
	}
	var methods []string
	for _, request := range fake.recorded() {
		methods = append(methods, request.Method+"@"+request.Session)
	}
	if !slices.Equal(methods, []string{"initialize@", "initialize@", "tools/call@session-2"}) {
		t.Fatalf("requests = %v", methods)
	}
}

func TestClientReportsNotFoundAnswersInASession(t *testing.T) {
	fake := newFakeServer(t)
	client := connectFake(t, fake)
	unknown := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var message struct {
			ID jsontext.Value `json:"id"`
		}
		_ = json.UnmarshalRead(request.Body, &message)
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(response, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"Unknown tool: nope"}}`, message.ID)
	}))
	defer unknown.Close()
	// The session is open; the 404 still answers the call itself.
	client.endpoint = unknown.URL
	_, err := client.CallTool(t.Context(), "nope", nil)
	if err == nil || err.Error() != "HTTP 404: JSON-RPC error -32601: Unknown tool: nope" {
		t.Fatalf("CallTool() error = %v", err)
	}
	if requests := fake.recorded(); len(requests) != 1 {
		t.Fatalf("the client opened another session: %v", requests)
	}
}

func TestClientTellsServerAboutCancelledCalls(t *testing.T) {
	fake := newFakeServer(t)
	started := make(chan struct{})
	fake.handle("slow", func(ctx context.Context, _ map[string]any) (any, *fakeError) {
		close(started)
		<-ctx.Done()
		return nil, &fakeError{Code: -1, Message: "gone"}
	})
	client := connectFake(t, fake)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-started
		cancel()
	}()
	if _, err := client.CallTool(ctx, "slow", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("CallTool() error = %v", err)
	}
	client.Close(t.Context())
	var cancelled []fakeRequest
	for _, notification := range fake.recordedNotifications() {
		if notification.Method == "notifications/cancelled" {
			cancelled = append(cancelled, notification)
		}
	}
	call := fake.recorded()[len(fake.recorded())-1]
	if len(cancelled) != 1 || call.Method != "tools/call" {
		t.Fatalf("cancellations = %#v", cancelled)
	}
	var callID struct {
		RequestID int64  `json:"requestId"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal(cancelled[0].Params, &callID); err != nil || callID.RequestID == 0 || callID.Reason != "request cancelled" {
		t.Fatalf("cancellation params = %s (%v)", cancelled[0].Params, err)
	}
}

func TestConnectKeepsWorkingServersAndReportsFailures(t *testing.T) {
	working := newFakeServer(t)
	working.handle("echo", nil)
	unauthorized := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, "denied", http.StatusUnauthorized)
	}))
	defer unauthorized.Close()
	release := make(chan struct{})
	hanging := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	defer hanging.Close()
	defer close(release)

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()

	servers, errs := Connect(t.Context(), []ServerConfig{
		{Name: "denied", URL: unauthorized.URL, Timeout: DefaultTimeout},
		{Name: "hanging", URL: hanging.URL, Timeout: 50 * time.Millisecond},
		{Name: "unreachable", URL: closed.URL + "/mcp?key=secret-in-url", Timeout: DefaultTimeout},
		working.config("working"),
	}, nil)
	defer func() {
		for _, server := range servers {
			server.Client.Close(context.Background())
		}
	}()
	if len(servers) != 1 || servers[0].Config.Name != "working" || len(servers[0].Tools) != 1 {
		t.Fatalf("servers = %#v", servers)
	}
	if len(errs) != 3 ||
		!strings.Contains(errs[0].Error(), `MCP server "denied": initialize: HTTP 401`) ||
		!strings.Contains(errs[1].Error(), `MCP server "hanging": no answer within 50ms`) ||
		!strings.Contains(errs[2].Error(), `MCP server "unreachable": initialize: send request: Post: dial tcp`) {
		t.Fatalf("errors = %v", errs)
	}
	for _, err := range errs {
		if strings.Contains(err.Error(), "secret-in-url") {
			t.Fatalf("error quotes the URL: %v", err)
		}
	}
}
