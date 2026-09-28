package responsesapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

const deltaResponse = `{"id":"resp-d","status":"completed","output":[{"id":"rs-1","type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"Look first."}]},{"id":"msg-1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello world"}]}],"usage":{"input_tokens":3,"output_tokens":5}}`

// deltaStream is a Responses stream whose text also arrives as deltas,
// including events that must not be forwarded.
var deltaStream = []string{
	`{"type":"response.created","response":{"id":"resp-d","status":"in_progress"}}`,
	`{"type":"response.reasoning_text.delta","output_index":0,"content_index":0,"delta":"Look "}`,
	`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"first."}`,
	`{"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"Hello"}`,
	// Not text: tool arguments and audio are not previewed.
	`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"command\""}`,
	`{"type":"response.audio.delta","delta":{"format":"pcm","data":"AAAA"}}`,
	// Malformed or empty text deltas are skipped without failing the stream.
	`{"type":"response.output_text.delta","output_index":1,"delta":{"text":"wrong shape"}}`,
	`{"type":"response.output_text.delta","output_index":1,"delta":""}`,
	`{"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":" world"}`,
	`{"type":"response.completed","response":` + deltaResponse + `}`,
	// Frames after the terminal response are ignored.
	`{"type":"response.output_text.delta","output_index":1,"delta":" late"}`,
}

func writeDeltaStream(w http.ResponseWriter, frames []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, frame := range frames {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
		w.(http.Flusher).Flush()
	}
}

type deltaRecorder struct {
	mu     sync.Mutex
	deltas []llm.Delta
}

func (recorder *deltaRecorder) record(delta llm.Delta) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.deltas = append(recorder.deltas, delta)
}

func (recorder *deltaRecorder) snapshot() []llm.Delta {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return slices.Clone(recorder.deltas)
}

func TestResponsesStreamForwardsTextDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeDeltaStream(w, deltaStream)
	}))
	defer server.Close()
	var traces []Exchange
	adapter := newTestAdapterWithConfig(t, Config{Endpoint: server.URL, Trace: func(e Exchange) { traces = append(traces, e) }})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var recorder deltaRecorder
	streamed, err := adapter.Respond(ctx, validRequest(), llm.RequestOptions{OnDelta: recorder.record})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := adapter.Respond(ctx, validRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want, err := decodeResponse([]byte(deltaResponse))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(streamed, want) || !reflect.DeepEqual(plain, want) {
		t.Fatalf("streamed = %#v\nplain = %#v\nwant %#v", streamed, plain, want)
	}
	if len(traces) != 2 || string(traces[0].ResponseBody) != deltaResponse || string(traces[1].ResponseBody) != deltaResponse {
		t.Fatal("deltas changed the traced terminal response")
	}
	wantDeltas := []llm.Delta{
		{Attempt: 1, OutputIndex: 0, Channel: llm.DeltaReasoning, Text: "Look "},
		{Attempt: 1, OutputIndex: 0, Channel: llm.DeltaReasoning, Text: "first."},
		{Attempt: 1, OutputIndex: 1, Channel: llm.DeltaText, Text: "Hello"},
		{Attempt: 1, OutputIndex: 1, Channel: llm.DeltaText, Text: " world"},
	}
	if got := recorder.snapshot(); !reflect.DeepEqual(got, wantDeltas) {
		t.Fatalf("deltas = %#v, want %#v", got, wantDeltas)
	}
}

func TestResponsesStreamDeltasArriveBeforeTheResponseCompletes(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeDeltaStream(w, []string{`{"type":"response.output_text.delta","output_index":0,"delta":"early"}`})
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		writeDeltaStream(w, []string{`{"type":"response.completed","response":` + completedResponse + `}`})
	}))
	defer server.Close()
	adapter := newTestAdapterWithConfig(t, Config{Endpoint: server.URL})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	first := make(chan llm.Delta, 1)
	var returned atomic.Bool
	done := make(chan error, 1)
	go func() {
		_, err := adapter.Respond(ctx, validRequest(), llm.RequestOptions{OnDelta: func(delta llm.Delta) {
			if returned.Load() {
				t.Error("delta delivered after Respond returned")
			}
			first <- delta
		}})
		returned.Store(true)
		done <- err
	}()
	select {
	case delta := <-first:
		if delta.Text != "early" || delta.Channel != llm.DeltaText || delta.Attempt != 1 {
			t.Fatalf("delta = %#v", delta)
		}
	case err := <-done:
		t.Fatalf("Respond returned before streaming a delta: %v", err)
	case <-ctx.Done():
		t.Fatal("delta was not streamed while the response was open")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestResponsesStreamDeltasNameTheRetriedAttempt(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			// The stream ends without a terminal response, which is retried.
			writeDeltaStream(w, []string{`{"type":"response.output_text.delta","output_index":0,"delta":"Hel"}`})
			return
		}
		writeDeltaStream(w, []string{
			`{"type":"response.output_text.delta","output_index":0,"delta":"fallback"}`,
			`{"type":"response.completed","response":{"id":"new","status":"completed","output":[` + fallbackMessage + `]}}`,
		})
	}))
	defer server.Close()
	adapter := newTestAdapterWithConfig(t, Config{Endpoint: server.URL, MaxAttempts: new(2)})
	var recorder deltaRecorder
	got, err := adapter.Respond(t.Context(), validRequest(), llm.RequestOptions{OnDelta: recorder.record})
	if err != nil || attempts.Load() != 2 || got.ID != "new" {
		t.Fatalf("attempts=%d response=%#v error=%v", attempts.Load(), got, err)
	}
	if text := got.Output[0].Data.(llm.Message).Text; text != "fallback" {
		t.Fatalf("message = %q: partial text of the failed attempt leaked into the response", text)
	}
	want := []llm.Delta{
		{Attempt: 1, Channel: llm.DeltaText, Text: "Hel"},
		{Attempt: 2, Channel: llm.DeltaText, Text: "fallback"},
	}
	if deltas := recorder.snapshot(); !reflect.DeepEqual(deltas, want) {
		t.Fatalf("deltas = %#v, want %#v", deltas, want)
	}
}

func TestResponseStateDeltasDoNotAffectTheAssembledResponse(t *testing.T) {
	var withDeltas, without responseState
	var recorder deltaRecorder
	withDeltas.onDelta = recorder.record
	frames := append([]string{
		`{"type":"response.output_item.done","output_index":1,"item":` + fallbackMessage + `}`,
	}, deltaStream...)
	for _, frame := range frames {
		if err := withDeltas.observe([]byte(frame)); err != nil {
			t.Fatal(err)
		}
		if err := without.observe([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	a, errA := withDeltas.unwrap()
	b, errB := without.unwrap()
	if errA != nil || errB != nil || string(a) != string(b) || string(a) != deltaResponse {
		t.Fatalf("with deltas = %s, %v; without = %s, %v", a, errA, b, errB)
	}
	if len(recorder.snapshot()) != 4 {
		t.Fatalf("deltas = %#v", recorder.snapshot())
	}
}

func TestResponsesErrorResponseForwardsNoDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"response.output_text.delta","delta":"not a stream"}`)
	}))
	defer server.Close()
	adapter := newTestAdapterWithConfig(t, Config{Endpoint: server.URL})
	var recorder deltaRecorder
	if _, err := adapter.Respond(t.Context(), validRequest(), llm.RequestOptions{OnDelta: recorder.record}); err == nil {
		t.Fatal("error response succeeded")
	}
	if deltas := recorder.snapshot(); len(deltas) != 0 {
		t.Fatalf("error body was streamed as deltas: %#v", deltas)
	}
}

func TestResponseStateSeparatesReasoningSummaryParts(t *testing.T) {
	var state responseState
	var recorder deltaRecorder
	state.onDelta = recorder.record
	for _, frame := range []string{
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"**Plan**"}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":" first."}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":1,"delta":"**Check**"}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":2,"summary_index":0,"delta":"Other item."}`,
	} {
		if err := state.observe([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	var texts []string
	for _, delta := range recorder.snapshot() {
		texts = append(texts, delta.Text)
	}
	if want := []string{"**Plan**", " first.", "\n\n**Check**", "Other item."}; !slices.Equal(texts, want) {
		t.Fatalf("texts = %q, want %q", texts, want)
	}
}
