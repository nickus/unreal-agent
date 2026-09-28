package agentrunner

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// lockedBuffer can be read while a delta timer writes to it.
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (output *lockedBuffer) Write(data []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.buffer.Write(data)
}

func (output *lockedBuffer) String() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.buffer.String()
}

// outputLine is either a model_delta event or a session item.
type outputLine struct {
	Type    string `json:"type"`
	TurnID  string `json:"turn_id"`
	Attempt int    `json:"attempt"`
	Output  int    `json:"output_index"`
	Channel string `json:"channel"`
	Text    string `json:"text"`
	Kind    string `json:"Kind"`
}

func decodeOutputLines(t *testing.T, output string) []outputLine {
	t.Helper()
	var lines []outputLine
	for line := range strings.Lines(output) {
		var decoded outputLine
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		lines = append(lines, decoded)
	}
	return lines
}

func newDeltaObserver(output io.Writer, interval time.Duration, cancel context.CancelFunc) *sessionObserver {
	return &sessionObserver{
		sessionID: "session", output: output, cancel: cancel,
		deltas: output, deltaInterval: interval,
	}
}

func textDelta(text string) llm.Delta {
	return llm.Delta{Attempt: 1, OutputIndex: 0, Channel: llm.DeltaText, Text: text}
}

func TestModelDeltasCoalesceUntilTheNextSessionItem(t *testing.T) {
	var output lockedBuffer
	observer := newDeltaObserver(&output, time.Hour, func() {})
	for _, text := range []string{"Hel", "lo", " world"} {
		observer.ModelDelta("turn-1", textDelta(text))
	}
	if output.String() != "" {
		t.Fatalf("delta written before its interval: %q", output.String())
	}
	observer.Observe("session", sessionstore.Item{
		Sequence: 3, Kind: sessionstore.ItemModelResponse,
		Data: sessionstore.ModelResponse{TurnID: "turn-1", Response: llm.Response{Output: []llm.Item{{
			Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "Hello world"},
		}}}},
	})
	got := decodeOutputLines(t, output.String())
	want := []outputLine{
		{Type: "model_delta", TurnID: "turn-1", Attempt: 1, Channel: "text", Text: "Hello world"},
		{Kind: "model_response"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("output = %#v, want %#v", got, want)
	}
	if err := observer.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestModelDeltasSplitWhenTheStreamChanges(t *testing.T) {
	var output lockedBuffer
	observer := newDeltaObserver(&output, time.Hour, func() {})
	sends := []struct {
		turn  session.TurnID
		delta llm.Delta
	}{
		{"turn-1", llm.Delta{Attempt: 1, OutputIndex: 0, Channel: llm.DeltaReasoning, Text: "think "}},
		{"turn-1", llm.Delta{Attempt: 1, OutputIndex: 0, Channel: llm.DeltaReasoning, Text: "more"}},
		{"turn-1", llm.Delta{Attempt: 1, OutputIndex: 1, Channel: llm.DeltaText, Text: "Hel"}},
		{"turn-1", llm.Delta{Attempt: 2, OutputIndex: 1, Channel: llm.DeltaText, Text: "Hello"}},
		{"turn-1", llm.Delta{Attempt: 2, OutputIndex: 2, Channel: llm.DeltaText, Text: "Second"}},
		{"turn-2", llm.Delta{Attempt: 1, OutputIndex: 0, Channel: llm.DeltaText, Text: "Next"}},
		{"turn-2", llm.Delta{Attempt: 1, OutputIndex: 0, Channel: llm.DeltaText, Text: ""}},
	}
	for _, send := range sends {
		observer.ModelDelta(send.turn, send.delta)
	}
	observer.CloseDeltas()
	observer.ModelDelta("turn-2", textDelta("after close"))
	got := decodeOutputLines(t, output.String())
	want := []outputLine{
		{Type: "model_delta", TurnID: "turn-1", Attempt: 1, Output: 0, Channel: "reasoning", Text: "think more"},
		{Type: "model_delta", TurnID: "turn-1", Attempt: 1, Output: 1, Channel: "text", Text: "Hel"},
		{Type: "model_delta", TurnID: "turn-1", Attempt: 2, Output: 1, Channel: "text", Text: "Hello"},
		{Type: "model_delta", TurnID: "turn-1", Attempt: 2, Output: 2, Channel: "text", Text: "Second"},
		{Type: "model_delta", TurnID: "turn-2", Attempt: 1, Output: 0, Channel: "text", Text: "Next"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("output = %#v\nwant %#v", got, want)
	}
}

func TestModelDeltasAreWrittenAfterTheInterval(t *testing.T) {
	var output lockedBuffer
	observer := newDeltaObserver(&output, 10*time.Millisecond, func() {})
	defer observer.CloseDeltas()
	observer.ModelDelta("turn-1", textDelta("slow"))
	deadline := time.Now().Add(5 * time.Second)
	for output.String() == "" {
		if time.Now().After(deadline) {
			t.Fatal("buffered delta was not written after its interval")
		}
		time.Sleep(time.Millisecond)
	}
	observer.ModelDelta("turn-1", textDelta(" tokens"))
	for strings.Count(output.String(), "\n") < 2 {
		if time.Now().After(deadline) {
			t.Fatal("second delta was not written after its interval")
		}
		time.Sleep(time.Millisecond)
	}
	got := decodeOutputLines(t, output.String())
	if len(got) != 2 || got[0].Text != "slow" || got[1].Text != " tokens" {
		t.Fatalf("output = %#v", got)
	}
}

func TestModelDeltasAreWrittenEarlyWhenLarge(t *testing.T) {
	var output lockedBuffer
	observer := newDeltaObserver(&output, time.Hour, func() {})
	large := strings.Repeat("x", maxModelDeltaBytes-1)
	observer.ModelDelta("turn-1", textDelta(large))
	if output.String() != "" {
		t.Fatal("delta below the size limit was written early")
	}
	observer.ModelDelta("turn-1", textDelta("yz"))
	got := decodeOutputLines(t, output.String())
	if len(got) != 1 || got[0].Text != large+"yz" {
		t.Fatalf("output has %d lines", len(got))
	}
}

func TestModelDeltasWithoutIntervalAreWrittenAsTheyArrive(t *testing.T) {
	var output lockedBuffer
	observer := newDeltaObserver(&output, 0, func() {})
	observer.ModelDelta("turn-1", textDelta("a"))
	observer.ModelDelta("turn-1", textDelta("b"))
	got := decodeOutputLines(t, output.String())
	if len(got) != 2 || got[0].Text != "a" || got[1].Text != "b" {
		t.Fatalf("output = %#v", got)
	}
}

func TestModelDeltaWriteFailureStopsTheRun(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	want := errors.New("stdout closed")
	output := &failingWriter{err: want}
	observer := newDeltaObserver(output, 0, cancel)
	observer.ModelDelta("turn-1", textDelta("a"))
	if !errors.Is(observer.Err(), want) || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("observer error = %v, context error = %v", observer.Err(), ctx.Err())
	}
	observer.ModelDelta("turn-1", textDelta("b"))
	observer.Observe("session", sessionstore.Item{Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "turn-2", Type: session.TurnRegular}})
	if output.writes != 1 {
		t.Fatalf("writes after failure = %d", output.writes)
	}
}

type failingWriter struct {
	err    error
	writes int
}

func (output *failingWriter) Write([]byte) (int, error) {
	output.writes++
	return 0, output.err
}

func deltaRunEnvironment(name string) string {
	if name == llmAPIKeyEnvironment {
		return "secret"
	}
	return ""
}

func streamingClient() *fakeClient {
	return &fakeClient{
		stream: []llm.Delta{
			{Attempt: 1, OutputIndex: 0, Channel: llm.DeltaReasoning, Text: "Check "},
			{Attempt: 1, OutputIndex: 0, Channel: llm.DeltaReasoning, Text: "first."},
			{Attempt: 1, OutputIndex: 1, Channel: llm.DeltaText, Text: "All "},
			{Attempt: 1, OutputIndex: 1, Channel: llm.DeltaText, Text: "done"},
		},
		respond: func(context.Context, llm.Request) (llm.Response, error) {
			return llm.Response{Stop: llm.StopComplete, Output: []llm.Item{{
				Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "All done"},
			}}}, nil
		},
	}
}

func TestRunMainStreamsModelDeltasToStdout(t *testing.T) {
	workspace := t.TempDir()
	logDirectory := filepath.Join(t.TempDir(), "logs")
	var stdout, stderr bytes.Buffer
	code := RunMain(t.Context(),
		[]string{"-workspace", workspace, "-session-directory", t.TempDir(), "-log-directory", logDirectory, "-model-delta-interval", "1h", "-p", "hello"},
		deltaRunEnvironment, func() []string { return nil }, strings.NewReader(""), &stdout, &stderr, testConfig(streamingClient()))
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	lines := decodeOutputLines(t, stdout.String())
	var turnID string
	var kinds []string
	var deltas []outputLine
	for _, line := range lines {
		if line.Type == "model_delta" {
			kinds = append(kinds, "model_delta")
			deltas = append(deltas, line)
			continue
		}
		kinds = append(kinds, line.Kind)
		if line.Kind == "turn" {
			var item sessionstore.Item
			if err := json.Unmarshal(stdoutLine(t, stdout.String(), len(kinds)-1), &item); err != nil {
				t.Fatal(err)
			}
			turnID = string(item.Data.(session.Turn).ID)
		}
	}
	joined := strings.Join(kinds, " ")
	if !strings.Contains(joined, "turn model_delta model_delta model_response") {
		t.Fatalf("output sequence = %q", joined)
	}
	want := []outputLine{
		{Type: "model_delta", TurnID: turnID, Attempt: 1, Output: 0, Channel: "reasoning", Text: "Check first."},
		{Type: "model_delta", TurnID: turnID, Attempt: 1, Output: 1, Channel: "text", Text: "All done"},
	}
	if turnID == "" || !reflect.DeepEqual(deltas, want) {
		t.Fatalf("deltas = %#v, want %#v", deltas, want)
	}
	logs, err := filepath.Glob(filepath.Join(logDirectory, "*.jsonl"))
	if err != nil || len(logs) != 1 {
		t.Fatalf("logs = %v, %v", logs, err)
	}
	logged, err := os.ReadFile(logs[0])
	if err != nil {
		t.Fatal(err)
	}
	var itemsOnly strings.Builder
	for line := range strings.Lines(stdout.String()) {
		if !strings.Contains(line, `"type":"model_delta"`) {
			itemsOnly.WriteString(line)
		}
	}
	if string(logged) != itemsOnly.String() {
		t.Fatalf("session log = %q, want the session items of stdout %q", logged, itemsOnly.String())
	}
}

func stdoutLine(t *testing.T, output string, index int) []byte {
	t.Helper()
	for i, line := range slices.Collect(strings.Lines(output)) {
		if i == index {
			return []byte(line)
		}
	}
	t.Fatalf("stdout has no line %d", index)
	return nil
}

func TestRunMainOmitsModelDeltasWhenPartialMessagesAreOff(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunMain(t.Context(),
		[]string{"-workspace", t.TempDir(), "-session-directory", t.TempDir(), `{"prompt":"hello","include_partial_messages":false}`},
		deltaRunEnvironment, func() []string { return nil }, strings.NewReader(""), &stdout, &stderr, testConfig(streamingClient()))
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "model_delta") {
		t.Fatalf("stdout has deltas: %s", stdout.String())
	}
	assertItemSequence(t, stdout.String(),
		"input.control input.external input.control turn model_response",
		"input.control input.external turn input.control model_response",
		"input.control input.external turn model_response input.control",
	)
}

func TestRunMainRejectsNegativeModelDeltaInterval(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunMain(t.Context(),
		[]string{"-workspace", t.TempDir(), "-model-delta-interval", "-1s", "-p", "hello"},
		deltaRunEnvironment, func() []string { return nil }, strings.NewReader(""), &stdout, &stderr, testConfig(streamingClient()))
	if code != 1 || !strings.Contains(stderr.String(), "model delta interval must not be negative") {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
}

// TestRunMainStreamsDeltasFromAResponsesServer drives the OpenAI-compatible
// provider against a local SSE server: text deltas reach stdout as they
// stream, and the model_response item is the same as without streaming.
func TestRunMainStreamsDeltasFromAResponsesServer(t *testing.T) {
	const response = `{"id":"r","status":"completed","output":[` +
		`{"id":"rs-1","type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"Plan it."}]},` +
		`{"id":"m-1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello there"}]}],` +
		`"usage":{"input_tokens":7,"output_tokens":3}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range []string{
			`{"type":"response.created","response":{"id":"r","status":"in_progress"}}`,
			`{"type":"response.reasoning_text.delta","output_index":0,"content_index":0,"delta":"Plan "}`,
			`{"type":"response.reasoning_text.delta","output_index":0,"content_index":0,"delta":"it."}`,
			`{"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"Hello"}`,
			`{"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":" there"}`,
			`{"type":"response.completed","response":` + response + `}`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
			w.(http.Flusher).Flush()
		}
	}))
	defer server.Close()
	run := func(request string) (string, llm.Response) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := RunMain(t.Context(),
			[]string{"-workspace", t.TempDir(), "-session-directory", t.TempDir(), "-model-delta-interval", "0"},
			func(name string) string {
				return map[string]string{
					llmBaseURLEnvironment: server.URL,
					llmAPIKeyEnvironment:  "test-key",
				}[name]
			}, func() []string { return nil }, strings.NewReader(request), &stdout, &stderr,
			Config{Name: "unreal-agent-runner", ParseRequest: parseTestRequest, Providers: DefaultProviders()})
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
		}
		var final llm.Response
		for line := range strings.Lines(stdout.String()) {
			if strings.Contains(line, `"type":"model_delta"`) {
				continue
			}
			var item sessionstore.Item
			if err := json.Unmarshal([]byte(line), &item); err != nil {
				t.Fatal(err)
			}
			if item.Kind == sessionstore.ItemModelResponse {
				final = item.Data.(sessionstore.ModelResponse).Response
			}
		}
		return stdout.String(), final
	}
	streamed, streamedResponse := run(`{"prompt":"hello","model":"test","max_attempts":1}`)
	plain, plainResponse := run(`{"prompt":"hello","model":"test","max_attempts":1,"include_partial_messages":false}`)
	if strings.Contains(plain, "model_delta") {
		t.Fatal("deltas written with partial messages off")
	}
	if !reflect.DeepEqual(streamedResponse, plainResponse) || len(streamedResponse.Output) != 2 {
		t.Fatalf("streamed response = %#v, plain = %#v", streamedResponse, plainResponse)
	}
	var kinds, texts []string
	for _, line := range decodeOutputLines(t, streamed) {
		if line.Type == "model_delta" {
			kinds = append(kinds, "model_delta."+line.Channel)
			texts = append(texts, line.Text)
		} else {
			kinds = append(kinds, line.Kind)
		}
	}
	joined := strings.Join(kinds, " ")
	if !strings.Contains(joined, "turn model_delta.reasoning model_delta.reasoning model_delta.text model_delta.text model_response") {
		t.Fatalf("output sequence = %q", joined)
	}
	if !slices.Equal(texts, []string{"Plan ", "it.", "Hello", " there"}) {
		t.Fatalf("delta texts = %q", texts)
	}
}
