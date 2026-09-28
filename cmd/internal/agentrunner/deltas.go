package agentrunner

import (
	"encoding/json/v2"
	"fmt"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

const (
	defaultModelDeltaInterval = 250 * time.Millisecond
	// A pending delta is written early once it grows this large, which bounds
	// the size of one event line during fast generation.
	maxModelDeltaBytes = 4 << 10
)

// modelDeltaEvent previews output of the turn that is generating. It is not a
// session item: it is never stored or replayed, and the turn's model_response
// item still carries the complete output. Consecutive deltas of one output item
// are merged into one event per interval.
type modelDeltaEvent struct {
	Type    string           `json:"type"`
	TurnID  session.TurnID   `json:"turn_id"`
	Attempt int              `json:"attempt"`
	Output  int              `json:"output_index"`
	Channel llm.DeltaChannel `json:"channel"`
	Text    string           `json:"text"`
}

// ModelDelta buffers streamed model output and writes it as model_delta
// events. Text is held for at most the configured interval; a change of turn,
// attempt, output item or channel, a session item, or the size limit writes it
// sooner.
func (observer *sessionObserver) ModelDelta(turnID session.TurnID, delta llm.Delta) {
	if delta.Text == "" {
		return
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.err != nil || observer.deltas == nil || observer.deltasClosed {
		return
	}
	pending := observer.pendingDelta
	if pending != nil && (pending.TurnID != turnID || pending.Attempt != delta.Attempt ||
		pending.Output != delta.OutputIndex || pending.Channel != delta.Channel) {
		if !observer.flushDeltaLocked() {
			return
		}
		pending = nil
	}
	if pending == nil {
		pending = &modelDeltaEvent{
			Type: "model_delta", TurnID: turnID, Attempt: delta.Attempt,
			Output: delta.OutputIndex, Channel: delta.Channel,
		}
		observer.pendingDelta = pending
		if observer.deltaInterval > 0 {
			observer.deltaGeneration++
			generation := observer.deltaGeneration
			observer.deltaTimer = time.AfterFunc(observer.deltaInterval, func() {
				observer.flushDelta(generation)
			})
		}
	}
	pending.Text += delta.Text
	if observer.deltaInterval <= 0 || len(pending.Text) >= maxModelDeltaBytes {
		observer.flushDeltaLocked()
	}
}

// CloseDeltas writes any buffered delta and drops later ones, so nothing is
// written after the run has finished.
func (observer *sessionObserver) CloseDeltas() {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	observer.flushDeltaLocked()
	observer.deltasClosed = true
}

func (observer *sessionObserver) flushDelta(generation uint64) {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if generation == observer.deltaGeneration {
		observer.flushDeltaLocked()
	}
}

// flushDeltaLocked writes the buffered delta, if any. It reports whether the
// observer can still write.
func (observer *sessionObserver) flushDeltaLocked() bool {
	if observer.err != nil {
		return false
	}
	pending := observer.pendingDelta
	if pending == nil {
		return true
	}
	observer.pendingDelta = nil
	if observer.deltaTimer != nil {
		observer.deltaTimer.Stop()
		observer.deltaTimer = nil
	}
	encoded, err := json.Marshal(pending)
	if err != nil {
		// Only a preview is lost; the turn's model_response still follows.
		return true
	}
	if _, err := fmt.Fprintf(observer.deltas, "%s\n", encoded); err != nil {
		observer.fail(fmt.Errorf("write model delta: %w", err))
		return false
	}
	return true
}
