package coordinator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type turnDelta struct {
	turnID session.TurnID
	delta  llm.Delta
}

func TestCoordinatorForwardsModelDeltasForTheGeneratingTurn(t *testing.T) {
	store := emptyFakeStore()
	inputs := newTestInbox(t)
	deltas := make(chan turnDelta, 4)
	streamed := make(chan func(llm.Delta), 1)
	canceled := make(chan struct{})
	adapter := &fakeAdapter{}
	adapter.respond = func(ctx context.Context, _ llm.Request) (llm.Response, error) {
		options := adapter.requestOptionsSnapshot()
		onDelta := options[len(options)-1].OnDelta
		if onDelta == nil {
			return llm.Response{}, errors.New("request has no delta callback")
		}
		onDelta(llm.Delta{Attempt: 1, Channel: llm.DeltaText, Text: "partial"})
		streamed <- onDelta
		<-ctx.Done()
		<-canceled
		// A stream still draining after cancellation must not reach the caller.
		onDelta(llm.Delta{Attempt: 1, Channel: llm.DeltaText, Text: "late"})
		return llm.Response{}, ctx.Err()
	}
	current := New(Dependencies{
		SessionID:      "session-1",
		Inbox:          inputs,
		Restored:       store.resume,
		Sessions:       store,
		ContextBuilder: contextbuilder.NewBuilder(),
		LLM:            adapter,
		Tools:          tool.NewRegistry(tool.StaticTranslators{}),
		Operations:     newFakeOperationManager(),
		OnModelDelta: func(turnID session.TurnID, delta llm.Delta) {
			deltas <- turnDelta{turnID, delta}
		},
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- current.Run(ctx) }()

	submitTestInput(t, inputs, externalEvent(t, 1, "input-1", "hello"))
	receiveTestValue(t, streamed)
	got := receiveTestValue(t, deltas)
	if len(store.appendedTurns) != 1 {
		t.Fatalf("appended turns = %#v", store.appendedTurns)
	}
	want := turnDelta{store.appendedTurns[0].ID, llm.Delta{Attempt: 1, Channel: llm.DeltaText, Text: "partial"}}
	if got != want {
		t.Fatalf("delta = %#v, want %#v", got, want)
	}

	cancel()
	close(canceled)
	if err := receiveTestValue(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
	select {
	case late := <-deltas:
		t.Fatalf("delta after cancellation was forwarded: %#v", late)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestCoordinatorWithoutDeltaHookRequestsNoDeltas(t *testing.T) {
	store := emptyFakeStore()
	inputs := newTestInbox(t)
	started := make(chan llm.RequestOptions, 1)
	adapter := &fakeAdapter{}
	adapter.respond = func(ctx context.Context, _ llm.Request) (llm.Response, error) {
		options := adapter.requestOptionsSnapshot()
		started <- options[len(options)-1]
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}
	current := newTestCoordinatorWithAdapter(store, inputs, newFakeOperationManager(), contextbuilder.NewBuilder(), tool.NewRegistry(tool.StaticTranslators{}), adapter)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- current.Run(ctx) }()
	submitTestInput(t, inputs, externalEvent(t, 1, "input-1", "hello"))
	options := receiveTestValue(t, started)
	cancel()
	receiveTestValue(t, done)
	if options.OnDelta != nil || options.CacheKey != "session-1" {
		t.Fatalf("request options = %#v", options)
	}
}
