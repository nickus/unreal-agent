package llm

import "context"

type RequestOptions struct {
	CacheKey string
	// OnDelta, when set, receives the response text as the provider streams
	// it. Deltas are a preview only: the Response that Respond returns stays
	// authoritative and is identical whether or not OnDelta is set. It is
	// called from the goroutine reading the response, so it must not block.
	OnDelta func(Delta)
}

// DeltaChannel names the kind of streamed text.
type DeltaChannel string

const (
	// DeltaText is assistant message text.
	DeltaText DeltaChannel = "text"
	// DeltaReasoning is reasoning text or a reasoning summary.
	DeltaReasoning DeltaChannel = "reasoning"
)

// Delta is one piece of partially generated output.
type Delta struct {
	// Attempt counts the provider requests made for this response, from 1.
	// A retried request generates the response again from the start, so
	// text streamed by an earlier attempt is superseded.
	Attempt int
	// OutputIndex is the position of the output item the text belongs to.
	OutputIndex int
	Channel     DeltaChannel
	Text        string
}

type Adapter interface {
	Respond(context.Context, Request, RequestOptions) (Response, error)
}
