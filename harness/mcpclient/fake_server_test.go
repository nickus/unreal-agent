package mcpclient

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

// fakeServer is a streamable HTTP MCP server for tests. It opens a session on
// initialize, pages tools/list, and answers tools/call from handlers, as JSON
// or, with stream set, as an event stream preceded by a progress
// notification and a ping request.
type fakeServer struct {
	*httptest.Server
	t *testing.T

	mu              sync.Mutex
	protocolVersion string
	noSession       bool
	noTools         bool
	pageSize        int
	tools           []Tool
	stream          bool
	handlers        map[string]func(context.Context, map[string]any) (any, *fakeError)
	sessions        int
	session         string
	expireSession   bool
	requests        []fakeRequest
	notifications   []fakeRequest
	answers         []fakeAnswer
	deleted         []string
}

type fakeError struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

type fakeAnswer struct {
	ID     jsontext.Value
	Result jsontext.Value
	Error  jsontext.Value
}

type fakeRequest struct {
	Method  string
	Params  jsontext.Value
	Header  http.Header
	Session string
}

func newFakeServer(t *testing.T) *fakeServer {
	fake := &fakeServer{
		t:               t,
		protocolVersion: LatestProtocolVersion,
		handlers:        make(map[string]func(context.Context, map[string]any) (any, *fakeError)),
	}
	fake.Server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.Close)
	return fake
}

func (fake *fakeServer) config(name string) ServerConfig {
	return ServerConfig{Name: name, URL: fake.URL, Headers: map[string]string{"Authorization": "Bearer test-token"}, Timeout: DefaultTimeout}
}

func (fake *fakeServer) handle(name string, handler func(context.Context, map[string]any) (any, *fakeError)) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.handlers[name] = handler
	fake.tools = append(fake.tools, Tool{
		Name:        name,
		Description: "Test tool " + name + ".",
		InputSchema: jsontext.Value(`{"type":"object","properties":{"text":{"type":"string"}}}`),
	})
}

func (fake *fakeServer) recorded() []fakeRequest {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]fakeRequest(nil), fake.requests...)
}

func (fake *fakeServer) recordedNotifications() []fakeRequest {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]fakeRequest(nil), fake.notifications...)
}

func (fake *fakeServer) deletedSessions() []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]string(nil), fake.deleted...)
}

func (fake *fakeServer) recordedAnswers() []fakeAnswer {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]fakeAnswer(nil), fake.answers...)
}

func (fake *fakeServer) serve(response http.ResponseWriter, request *http.Request) {
	sessionID := request.Header.Get("Mcp-Session-Id")
	if request.Method == http.MethodDelete {
		fake.mu.Lock()
		fake.deleted = append(fake.deleted, sessionID)
		fake.mu.Unlock()
		return
	}
	if request.Method != http.MethodPost {
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var message struct {
		ID     jsontext.Value `json:"id,omitzero"`
		Method string         `json:"method,omitzero"`
		Params jsontext.Value `json:"params,omitzero"`
		Result jsontext.Value `json:"result,omitzero"`
		Error  jsontext.Value `json:"error,omitzero"`
	}
	if err := json.UnmarshalRead(request.Body, &message); err != nil {
		http.Error(response, "bad JSON", http.StatusBadRequest)
		return
	}
	recorded := fakeRequest{Method: message.Method, Params: message.Params, Header: request.Header.Clone(), Session: sessionID}

	fake.mu.Lock()
	if message.Method == "" {
		// The client's answer to a request the server sent.
		fake.answers = append(fake.answers, fakeAnswer{ID: message.ID, Result: message.Result, Error: message.Error})
		fake.mu.Unlock()
		response.WriteHeader(http.StatusAccepted)
		return
	}
	if message.Method != "initialize" && !fake.noSession {
		if fake.expireSession || sessionID != fake.session {
			fake.expireSession = false
			fake.session = ""
			fake.mu.Unlock()
			http.Error(response, "session not found", http.StatusNotFound)
			return
		}
	}
	if len(message.ID) == 0 {
		fake.notifications = append(fake.notifications, recorded)
		fake.mu.Unlock()
		response.WriteHeader(http.StatusAccepted)
		return
	}
	fake.requests = append(fake.requests, recorded)
	fake.mu.Unlock()

	switch message.Method {
	case "initialize":
		fake.mu.Lock()
		fake.sessions++
		if !fake.noSession {
			fake.session = "session-" + strconv.Itoa(fake.sessions)
			response.Header().Set("Mcp-Session-Id", fake.session)
		}
		capabilities := map[string]any{"tools": map[string]any{}}
		if fake.noTools {
			capabilities = map[string]any{}
		}
		result := map[string]any{
			"protocolVersion": fake.protocolVersion,
			"capabilities":    capabilities,
			"serverInfo":      map[string]any{"name": "fake", "version": "1"},
		}
		fake.mu.Unlock()
		fake.reply(response, message.ID, result, nil, false)
	case "tools/list":
		var params struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(message.Params, &params)
		fake.mu.Lock()
		start, _ := strconv.Atoi(params.Cursor)
		size := fake.pageSize
		if size <= 0 {
			size = len(fake.tools)
		}
		end := min(start+size, len(fake.tools))
		result := map[string]any{"tools": fake.tools[start:end]}
		if end < len(fake.tools) {
			result["nextCursor"] = strconv.Itoa(end)
		}
		fake.mu.Unlock()
		fake.reply(response, message.ID, result, nil, false)
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(message.Params, &params); err != nil {
			fake.reply(response, message.ID, nil, &fakeError{Code: -32602, Message: err.Error()}, false)
			return
		}
		fake.mu.Lock()
		handler := fake.handlers[params.Name]
		stream := fake.stream
		fake.mu.Unlock()
		if handler == nil {
			fake.reply(response, message.ID, nil, &fakeError{Code: -32602, Message: "unknown tool " + params.Name}, stream)
			return
		}
		result, rpcErr := handler(request.Context(), params.Arguments)
		fake.reply(response, message.ID, result, rpcErr, stream)
	default:
		fake.reply(response, message.ID, nil, &fakeError{Code: -32601, Message: "Method not found"}, false)
	}
}

func (fake *fakeServer) reply(response http.ResponseWriter, id jsontext.Value, result any, rpcErr *fakeError, stream bool) {
	message := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		message["error"] = rpcErr
	} else {
		message["result"] = result
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		fake.t.Errorf("encode reply: %v", err)
		return
	}
	if !stream {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write(encoded)
		return
	}
	response.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(response, ": keep-alive comment\n\n")
	fmt.Fprint(response, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\n")
	fmt.Fprint(response, "data: \"params\":{\"progressToken\":1,\"progress\":1}}\n\n")
	fmt.Fprint(response, "data: {\"jsonrpc\":\"2.0\",\"id\":\"server-ping\",\"method\":\"ping\"}\n\n")
	fmt.Fprintf(response, "id: 7\r\ndata: %s\r\n\r\n", encoded)
}
