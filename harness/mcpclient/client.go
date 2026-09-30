package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// LatestProtocolVersion is the MCP revision the client asks for.
const LatestProtocolVersion = "2025-06-18"

const (
	// maxResponseBytes bounds one HTTP response body, JSON or event stream.
	maxResponseBytes = 32 << 20
	// maxErrorBodyBytes bounds how much of a failed response is read.
	maxErrorBodyBytes = 64 << 10
	// errorExcerptLength bounds response text quoted in errors.
	errorExcerptLength = 500
	// maxToolPages bounds tools/list pagination.
	maxToolPages = 100
	// notificationTimeout bounds best-effort messages sent after a request
	// ended, such as a cancellation or a session DELETE.
	notificationTimeout = 5 * time.Second
)

// Revisions whose tools/list and tools/call the client understands. A server
// answers initialize with the requested revision or one it prefers.
var supportedProtocolVersions = []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25"}

// Implementation identifies the client to servers.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Tool is one entry of a tools/list result.
type Tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitzero"`
	Description string         `json:"description,omitzero"`
	InputSchema jsontext.Value `json:"inputSchema,omitzero"`
}

// CallToolResult is a tools/call result. Content holds the raw content blocks.
type CallToolResult struct {
	Content           []jsontext.Value `json:"content"`
	StructuredContent jsontext.Value   `json:"structuredContent,omitzero"`
	IsError           bool             `json:"isError,omitzero"`
}

// RPCError is a JSON-RPC error returned by a server.
type RPCError struct {
	Code    int64
	Message string
	Data    jsontext.Value
}

func (rpcErr *RPCError) Error() string {
	message := fmt.Sprintf("JSON-RPC error %d: %s", rpcErr.Code, excerpt(rpcErr.Message))
	if data := compactJSON(rpcErr.Data); data != "" && data != "null" {
		message += " (data: " + excerpt(data) + ")"
	}
	return message
}

// HTTPStatusError is a non-success HTTP answer that carried no JSON-RPC error.
type HTTPStatusError struct {
	StatusCode int
	Body       string
}

func (statusErr *HTTPStatusError) Error() string {
	message := fmt.Sprintf("HTTP %d %s", statusErr.StatusCode, http.StatusText(statusErr.StatusCode))
	if statusErr.Body != "" {
		message += ": " + statusErr.Body
	}
	return message
}

// errSessionExpired reports a 404 for a request that carried a session ID:
// the server ended the session and the client must initialize again.
var errSessionExpired = errors.New("MCP session expired")

// Client talks to one MCP server over the streamable HTTP transport. It is
// safe for concurrent use once Initialize has returned.
type Client struct {
	endpoint string
	headers  map[string]string
	http     *http.Client
	info     Implementation
	nextID   atomic.Int64
	// background tracks best-effort notifications sent after a request ended.
	background sync.WaitGroup

	// initializing serializes session setup; mu guards the session fields.
	initializing sync.Mutex
	mu           sync.Mutex
	session      session
}

type session struct {
	id              string
	protocolVersion string
	generation      uint64
	tools           bool
}

// NewClient returns a client for config. It does not contact the server.
func NewClient(config ServerConfig, httpClient *http.Client, info Implementation) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		endpoint: config.URL,
		headers:  config.Headers,
		http:     httpClient,
		info:     info,
	}
}

type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitzero"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitzero"`
}

type jsonrpcMessage struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      jsontext.Value `json:"id,omitzero"`
	Method  string         `json:"method,omitzero"`
	Result  jsontext.Value `json:"result,omitzero"`
	Error   *struct {
		Code    int64          `json:"code"`
		Message string         `json:"message"`
		Data    jsontext.Value `json:"data,omitzero"`
	} `json:"error,omitzero"`
}

type jsonrpcResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Result  any    `json:"result,omitzero"`
	Error   any    `json:"error,omitzero"`
}

// Initialize opens a session: it negotiates the protocol revision and sends
// notifications/initialized.
func (client *Client) Initialize(ctx context.Context) error {
	client.initializing.Lock()
	defer client.initializing.Unlock()
	return client.initialize(ctx)
}

func (client *Client) initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": LatestProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      client.info,
	}
	raw, header, err := client.exchange(ctx, session{}, "initialize", params)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools jsontext.Value `json:"tools,omitzero"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("initialize: decode result: %w", err)
	}
	if !slices.Contains(supportedProtocolVersions, result.ProtocolVersion) {
		return fmt.Errorf("initialize: server chose protocol version %q; supported: %s",
			excerpt(result.ProtocolVersion), strings.Join(supportedProtocolVersions, ", "))
	}
	sessionID := header.Get("Mcp-Session-Id")
	for _, character := range []byte(sessionID) {
		if character < 0x21 || character > 0x7e {
			return errors.New("initialize: server sent a session ID with characters outside visible ASCII")
		}
	}
	client.mu.Lock()
	client.session = session{
		id:              sessionID,
		protocolVersion: result.ProtocolVersion,
		generation:      client.session.generation + 1,
		tools:           len(result.Capabilities.Tools) != 0 && string(result.Capabilities.Tools) != "null",
	}
	current := client.session
	client.mu.Unlock()
	if err := client.notify(ctx, current, "notifications/initialized", nil); err != nil {
		return fmt.Errorf("notifications/initialized: %w", err)
	}
	return nil
}

func (client *Client) currentSession() session {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.session
}

// renew opens a new session after the server ended the one of generation;
// a concurrent caller that already renewed it wins.
func (client *Client) renew(ctx context.Context, generation uint64) error {
	client.initializing.Lock()
	defer client.initializing.Unlock()
	if client.currentSession().generation != generation {
		return nil
	}
	return client.initialize(ctx)
}

// ListTools returns every tool the server lists, following pagination. A
// server that declares no tools capability has none.
func (client *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if !client.currentSession().tools {
		return nil, nil
	}
	var tools []Tool
	cursor := ""
	seen := make(map[string]struct{})
	for range maxToolPages {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := client.request(ctx, "tools/list", params)
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		var page struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor,omitzero"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("tools/list: decode result: %w", err)
		}
		tools = append(tools, page.Tools...)
		if page.NextCursor == "" {
			return tools, nil
		}
		if _, repeated := seen[page.NextCursor]; repeated {
			return nil, errors.New("tools/list: server repeated a pagination cursor")
		}
		seen[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
	return nil, fmt.Errorf("tools/list: more than %d pages", maxToolPages)
}

// CallTool calls one tool. A result with IsError set is returned without an
// error: the tool ran and reported a failure. When ctx ends first, the server
// is told the request was cancelled.
func (client *Client) CallTool(ctx context.Context, name string, arguments jsontext.Value) (CallToolResult, error) {
	if len(arguments) == 0 {
		arguments = jsontext.Value("{}")
	}
	params := struct {
		Name      string         `json:"name"`
		Arguments jsontext.Value `json:"arguments"`
	}{Name: name, Arguments: arguments}
	raw, err := client.request(ctx, "tools/call", params)
	if err != nil {
		return CallToolResult{}, err
	}
	var result CallToolResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return CallToolResult{}, fmt.Errorf("decode tools/call result: %w", err)
	}
	return result, nil
}

// Close waits for pending notifications and ends the session on the server.
// Failures are ignored: the server expires abandoned sessions itself.
func (client *Client) Close(ctx context.Context) {
	client.background.Wait()
	current := client.currentSession()
	if current.id == "" {
		return
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, client.endpoint, nil)
	if err != nil {
		return
	}
	client.setHeaders(request, current)
	response, err := client.do(request)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxErrorBodyBytes))
	_ = response.Body.Close()
}

// request sends a request in the current session, and opens a new session
// once when the server reports that the current one has ended.
func (client *Client) request(ctx context.Context, method string, params any) (jsontext.Value, error) {
	current := client.currentSession()
	result, _, err := client.exchange(ctx, current, method, params)
	if !errors.Is(err, errSessionExpired) {
		return result, err
	}
	if err := client.renew(ctx, current.generation); err != nil {
		return nil, fmt.Errorf("renew expired session: %w", err)
	}
	result, _, err = client.exchange(ctx, client.currentSession(), method, params)
	return result, err
}

// exchange posts one request and waits for its response, which arrives either
// as a JSON body or on an event stream.
func (client *Client) exchange(
	ctx context.Context,
	current session,
	method string,
	params any,
) (jsontext.Value, http.Header, error) {
	id := client.nextID.Add(1)
	body, err := json.Marshal(jsonrpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return nil, nil, fmt.Errorf("encode request: %w", err)
	}
	result, header, err := client.post(ctx, current, id, body)
	if err != nil && ctx.Err() != nil && method != "initialize" {
		// The request may still be running on the server.
		client.notifyCancelled(current, id, context.Cause(ctx))
	}
	return result, header, err
}

func (client *Client) post(ctx context.Context, current session, id int64, body []byte) (jsontext.Value, http.Header, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	client.setHeaders(request, current)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err := client.do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("send request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		answered, err := statusError(response, id)
		// A 404 for a request in a session means the session is gone, unless
		// the body answers this very request.
		if response.StatusCode == http.StatusNotFound && current.id != "" && !answered {
			return nil, nil, errSessionExpired
		}
		return nil, nil, err
	}
	if response.StatusCode == http.StatusAccepted || response.StatusCode == http.StatusNoContent {
		return nil, nil, fmt.Errorf("server answered HTTP %d without a response", response.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil {
		return nil, nil, fmt.Errorf("response content type %q is invalid", excerpt(response.Header.Get("Content-Type")))
	}
	limited := &limitedReader{reader: response.Body, remaining: maxResponseBytes}
	var result jsontext.Value
	switch strings.ToLower(mediaType) {
	case "application/json":
		encoded, readErr := io.ReadAll(limited)
		if readErr != nil {
			return nil, nil, fmt.Errorf("read response: %w", readErr)
		}
		var found bool
		result, found, err = client.handleMessages(ctx, current, id, encoded)
		if err == nil && !found {
			err = errors.New("response did not answer the request")
		}
	case "text/event-stream":
		result, err = client.readEventStream(ctx, current, id, limited)
	default:
		err = fmt.Errorf("unexpected response content type %q", excerpt(mediaType))
	}
	if err != nil {
		return nil, nil, err
	}
	return result, response.Header, nil
}

// do sends request. Its errors leave the URL out: a configured URL may carry
// a credential, and errors end up in logs.
func (client *Client) do(request *http.Request) (*http.Response, error) {
	response, err := client.http.Do(request)
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = fmt.Errorf("%s: %w", urlErr.Op, urlErr.Err)
	}
	return response, err
}

func (client *Client) setHeaders(request *http.Request, current session) {
	for name, value := range client.headers {
		request.Header.Set(name, value)
	}
	if current.protocolVersion != "" {
		request.Header.Set("MCP-Protocol-Version", current.protocolVersion)
	}
	if current.id != "" {
		request.Header.Set("Mcp-Session-Id", current.id)
	}
}

// readEventStream reads server-sent events until the response for id.
func (client *Client) readEventStream(ctx context.Context, current session, id int64, body io.Reader) (jsontext.Value, error) {
	reader := bufio.NewReader(body)
	var data []byte
	hasData := false
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, fmt.Errorf("read event stream: %w", readErr)
		}
		ended := readErr == io.EOF
		line = bytes.TrimRight(line, "\r\n")
		// Lines starting with ':' are comments; only data fields matter here.
		if len(line) != 0 && line[0] != ':' {
			field, value, _ := bytes.Cut(line, []byte(":"))
			if string(field) == "data" {
				if hasData {
					data = append(data, '\n')
				}
				data = append(data, bytes.TrimPrefix(value, []byte(" "))...)
				hasData = true
			}
		}
		// A blank line ends an event; so does the end of the stream, for
		// servers that close it without a final blank line.
		if (len(line) == 0 || ended) && hasData {
			result, found, err := client.handleMessages(ctx, current, id, data)
			if err != nil || found {
				return result, err
			}
			data, hasData = data[:0], false
		}
		if ended {
			return nil, errors.New("event stream ended before the response")
		}
	}
}

// handleMessages processes one JSON-RPC message or batch from the server and
// returns the result for id when it is among them. Server requests are
// answered; notifications are ignored.
func (client *Client) handleMessages(ctx context.Context, current session, id int64, encoded []byte) (jsontext.Value, bool, error) {
	value := jsontext.Value(bytes.TrimSpace(encoded))
	if !value.IsValid() {
		return nil, false, fmt.Errorf("server sent invalid JSON: %s", excerpt(string(value)))
	}
	messages := []jsontext.Value{value}
	if value.Kind() == '[' {
		messages = nil
		if err := json.Unmarshal(value, &messages); err != nil {
			return nil, false, fmt.Errorf("decode message batch: %w", err)
		}
	}
	for _, raw := range messages {
		var message jsonrpcMessage
		if err := json.Unmarshal(raw, &message); err != nil {
			return nil, false, fmt.Errorf("decode message: %w", err)
		}
		if message.Method != "" {
			if len(message.ID) != 0 {
				client.answerServerRequest(ctx, current, message)
			}
			continue
		}
		if !matchesID(message.ID, id) {
			continue
		}
		if message.Error != nil {
			return nil, true, &RPCError{Code: message.Error.Code, Message: message.Error.Message, Data: message.Error.Data}
		}
		if len(message.Result) == 0 {
			return nil, true, errors.New("response has neither a result nor an error")
		}
		return message.Result, true, nil
	}
	return nil, false, nil
}

func matchesID(raw jsontext.Value, id int64) bool {
	var decoded any
	if len(raw) == 0 || json.Unmarshal(raw, &decoded) != nil {
		return false
	}
	switch value := decoded.(type) {
	case float64:
		return value == float64(id)
	case string:
		return value == strconv.FormatInt(id, 10)
	default:
		return false
	}
}

// answerServerRequest replies to a request the server sent on a response
// stream. The client offers no capabilities, so only ping succeeds.
func (client *Client) answerServerRequest(ctx context.Context, current session, message jsonrpcMessage) {
	response := jsonrpcResponse{JSONRPC: "2.0", ID: message.ID}
	if message.Method == "ping" {
		response.Result = map[string]any{}
	} else {
		response.Error = map[string]any{"code": -32601, "message": "Method not found"}
	}
	body, err := json.Marshal(response)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, notificationTimeout)
	defer cancel()
	_ = client.send(ctx, current, body)
}

func (client *Client) notify(ctx context.Context, current session, method string, params any) error {
	body, err := json.Marshal(jsonrpcRequest{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("encode notification: %w", err)
	}
	return client.send(ctx, current, body)
}

func (client *Client) notifyCancelled(current session, id int64, cause error) {
	reason := "request cancelled"
	if errors.Is(cause, context.DeadlineExceeded) {
		reason = "request timed out"
	}
	client.background.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), notificationTimeout)
		defer cancel()
		_ = client.notify(ctx, current, "notifications/cancelled", map[string]any{"requestId": id, "reason": reason})
	})
}

// send posts a notification or a response, for which the server owes no
// JSON-RPC answer.
func (client *Client) send(ctx context.Context, current session, body []byte) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	client.setHeaders(request, current)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err := client.do(request)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		_, err := statusError(response, 0)
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxErrorBodyBytes))
	return nil
}

// statusError describes a non-success answer, preferring the JSON-RPC error
// that some servers put in the body. answered reports whether that error
// answers the request with id.
func statusError(response *http.Response, id int64) (answered bool, err error) {
	body, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBodyBytes))
	var message jsonrpcMessage
	if json.Unmarshal(body, &message) == nil && message.Error != nil {
		rpcErr := &RPCError{Code: message.Error.Code, Message: message.Error.Message, Data: message.Error.Data}
		return matchesID(message.ID, id), fmt.Errorf("HTTP %d: %w", response.StatusCode, rpcErr)
	}
	return false, &HTTPStatusError{StatusCode: response.StatusCode, Body: excerpt(string(body))}
}

// limitedReader fails, instead of truncating, once more than remaining bytes
// are read.
type limitedReader struct {
	reader    io.Reader
	remaining int64
}

func (limited *limitedReader) Read(buffer []byte) (int, error) {
	if limited.remaining < 0 {
		return 0, fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
	}
	if int64(len(buffer)) > limited.remaining+1 {
		buffer = buffer[:limited.remaining+1]
	}
	count, err := limited.reader.Read(buffer)
	limited.remaining -= int64(count)
	if limited.remaining < 0 {
		return 0, fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
	}
	return count, err
}

// excerpt shortens text quoted from a server to one bounded line.
func excerpt(text string) string {
	text = strings.ToValidUTF8(text, "�")
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= errorExcerptLength {
		return text
	}
	runes := []rune(text)
	return string(runes[:errorExcerptLength]) + "…"
}

func compactJSON(value jsontext.Value) string {
	if len(value) == 0 {
		return ""
	}
	compacted := value.Clone()
	if err := compacted.Compact(); err != nil {
		return string(value)
	}
	return string(compacted)
}
