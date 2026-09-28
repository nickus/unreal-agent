package responsesapi

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"slices"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

func (adapter *adapter) exchange(ctx context.Context, body []byte, cacheKey string, onDelta func(llm.Delta)) (int, []byte, error) {
	events := make(chan primitives.PrimitiveEvent)
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		request := adapter.remoteRequest(body, cacheKey)
		result := adapter.exchangeAttempt(ctx, request, events, attemptDeltas(onDelta, attempt))
		if !result.retry || attempt >= adapter.maxAttempts {
			return result.status, result.body, result.err
		}
		if err := ctx.Err(); err != nil {
			return result.status, nil, err
		}
		now := time.Now()
		delay := responseRetryDelay(request.RetryPolicy, attempt, result.apiError, result.headers, now, rand.Float64())
		primitives.ScheduleTimer(ctx, primitives.TimerRequest{
			Source: request.Source, CorrelationID: request.CorrelationID + ":backoff",
			Deadline: now.Add(delay),
		}, events)
		event := <-events
		switch event.Type {
		case primitives.PrimitiveEventTimerFired:
		case primitives.PrimitiveEventCanceled:
			return result.status, nil, canceledError(ctx)
		case primitives.PrimitiveEventFailed:
			return result.status, nil, remoteFailureError(ctx, event)
		default:
			return result.status, nil, fmt.Errorf("unexpected retry timer event %q", event.Type)
		}
	}
}

type responseAttempt struct {
	status   int
	headers  http.Header
	body     []byte
	err      error
	apiError *APIError
	retry    bool
}

func (adapter *adapter) exchangeAttempt(ctx context.Context, request primitives.RemoteRequest, events chan primitives.PrimitiveEvent, onDelta func(llm.Delta)) responseAttempt {
	adapter.remote.SendRequest(ctx, request, events)
	var result responseAttempt
	state := responseState{onDelta: onDelta}
	var streaming bool
	var parseErr error
	for {
		event := <-events
		var transportErr error
		switch event.Type {
		case primitives.PrimitiveEventRemoteResponseStarted:
			started := event.Result.(primitives.RemoteResponseStartedResult)
			result.status, result.headers = started.StatusCode, http.Header(started.Headers)
			streaming = result.status >= http.StatusOK && result.status < http.StatusMultipleChoices
			continue
		case primitives.PrimitiveEventRemoteOutput:
			if parseErr != nil || state.terminal || state.err != nil {
				continue
			}
			output := event.Result.(primitives.RemoteOutputResult)
			if streaming {
				payload := bytes.TrimSpace(primitives.SSEData(output.Data))
				if len(payload) != 0 && !bytes.Equal(payload, []byte("[DONE]")) {
					parseErr = state.observe(payload)
				}
			} else if len(result.body)+len(output.Data) > 1<<20 {
				parseErr = errors.New("responses API error response exceeds 1 MiB")
			} else {
				result.body = append(result.body, output.Data...)
			}
			continue
		case primitives.PrimitiveEventRemoteCompleted:
		case primitives.PrimitiveEventFailed:
			transportErr = remoteFailureError(ctx, event)
		case primitives.PrimitiveEventCanceled:
			transportErr = canceledError(ctx)
		default:
			continue
		}

		if parseErr != nil {
			result.err = parseErr
			return result
		}
		switch {
		case state.terminal:
			result.body, result.err = state.unwrap()
			if result.err != nil {
				return result
			}
			result.apiError = state.failure
		case ctx.Err() != nil:
			result.err = ctx.Err()
			return result
		case state.err != nil:
			result.err = state.err
			if !errors.As(state.err, &result.apiError) {
				return result
			}
		case result.status != 0 && !streaming:
			result.apiError = providerError(result.status, result.body)
		case transportErr != nil:
			result.err = transportErr
			result.retry = event.Type == primitives.PrimitiveEventFailed
			return result
		default:
			result.body, result.err = state.unwrap()
			result.retry = errors.Is(result.err, io.ErrUnexpectedEOF)
			return result
		}
		result.retry = retryableResponseError(result.apiError, request.RetryPolicy.RetryableStatusCodes)
		return result
	}
}

type responseState struct {
	terminal bool
	response jsontext.Value
	items    map[int]jsontext.Value
	failure  *APIError
	err      error
	// onDelta receives streamed text as it arrives. It only observes the
	// stream: the fields above never depend on the deltas.
	onDelta func(llm.Delta)
	// summaryParts holds the last reasoning summary part streamed per output
	// item, to separate the parts as the assembled summary list does.
	summaryParts map[int]int
}

// deltaChannels maps the Responses API text delta events to the kind of
// text they carry. Other delta events (tool call arguments, audio) are not
// streamed.
var deltaChannels = map[string]llm.DeltaChannel{
	"response.output_text.delta":            llm.DeltaText,
	"response.refusal.delta":                llm.DeltaText,
	"response.reasoning_text.delta":         llm.DeltaReasoning,
	"response.reasoning_summary_text.delta": llm.DeltaReasoning,
}

// attemptDeltas tags the deltas of one request attempt with its number.
func attemptDeltas(onDelta func(llm.Delta), attempt int) func(llm.Delta) {
	if onDelta == nil {
		return nil
	}
	return func(delta llm.Delta) {
		delta.Attempt = attempt
		onDelta(delta)
	}
}

func (state *responseState) observe(data []byte) error {
	if state.terminal || state.err != nil {
		return state.err
	}
	var event struct {
		Type        string         `json:"type"`
		Response    jsontext.Value `json:"response"`
		Item        jsontext.Value `json:"item"`
		OutputIndex *int           `json:"output_index"`
		// Raw so that a provider's non-string delta (audio, custom events)
		// cannot fail the decoding of an otherwise valid stream.
		Delta        jsontext.Value `json:"delta"`
		SummaryIndex *int           `json:"summary_index"`
		Code         string         `json:"code"`
		Message      string         `json:"message"`
		Param        string         `json:"param"`
		Error        *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Param   string `json:"param"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("invalid Responses stream event JSON: %w", err)
	}
	if event.Type == "response.completed" || event.Type == "response.failed" || event.Type == "response.incomplete" {
		state.terminal = true
		state.response = event.Response
		if event.Type == "response.failed" {
			state.failure = providerError(http.StatusOK, event.Response)
		}
	}
	if channel, ok := deltaChannels[event.Type]; ok && state.onDelta != nil {
		state.observeDelta(channel, event.OutputIndex, event.SummaryIndex, event.Delta)
	}
	if event.Type == "response.output_item.done" {
		item := bytes.TrimSpace(event.Item)
		if event.OutputIndex == nil || *event.OutputIndex < 0 || len(item) == 0 || item[0] != '{' {
			return nil
		}
		if state.items == nil {
			state.items = make(map[int]jsontext.Value)
		}
		state.items[*event.OutputIndex] = event.Item
	}
	if event.Type == "error" {
		// The provider nests code/message under an "error" object as often as it
		// puts them at the top level (e.g. an in-band credit_balance_exhausted
		// failure). Read both so the surfaced error is diagnosable and classifiable
		// instead of an opaque empty "status 200:".
		code, message, param, kind := event.Code, event.Message, event.Param, ""
		if event.Error != nil {
			if code == "" {
				code = event.Error.Code
			}
			if message == "" {
				message = event.Error.Message
			}
			if param == "" {
				param = event.Error.Param
			}
			kind = event.Error.Type
		}
		if kind == "" {
			kind = "error"
		}
		state.err = &APIError{StatusCode: http.StatusOK, Code: code, Message: message, Param: param, Type: kind}
	}
	return nil
}

// observeDelta forwards a streamed text fragment. A malformed delta is
// skipped rather than reported: the terminal response carries the text.
func (state *responseState) observeDelta(channel llm.DeltaChannel, outputIndex, summaryIndex *int, raw jsontext.Value) {
	var text string
	if len(raw) == 0 || json.Unmarshal(raw, &text) != nil || text == "" {
		return
	}
	index := 0
	if outputIndex != nil {
		index = *outputIndex
	}
	if summaryIndex != nil {
		// A reasoning summary arrives as separate parts; start each new one
		// on its own paragraph.
		last, seen := state.summaryParts[index]
		if seen && last != *summaryIndex {
			text = "\n\n" + text
		}
		if state.summaryParts == nil {
			state.summaryParts = make(map[int]int)
		}
		state.summaryParts[index] = *summaryIndex
	}
	state.onDelta(llm.Delta{OutputIndex: index, Channel: channel, Text: text})
}

func (state *responseState) unwrap() ([]byte, error) {
	if state.err != nil {
		return nil, state.err
	}
	body := bytes.TrimSpace(state.response)
	if len(body) == 0 || body[0] != '{' {
		return nil, fmt.Errorf("responses stream ended without a terminal response: %w", io.ErrUnexpectedEOF)
	}
	if len(state.items) == 0 {
		return body, nil
	}
	// GPT subscription endpoint *does not* return outputs on the final response
	// But it does return everything besides the outputs
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	var output []jsontext.Value
	if raw := fields["output"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &output); err != nil {
			return nil, err
		}
	}
	if len(output) != 0 {
		// captured response already has outputs, they are authoritative
		return body, nil
	}
	// defensively allow gaps in output items
	for _, index := range slices.Sorted(maps.Keys(state.items)) {
		output = append(output, state.items[index])
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, err
	}
	fields["output"] = encoded
	return json.Marshal(fields, json.Deterministic(true))
}
