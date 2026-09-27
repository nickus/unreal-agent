package openrouter

import (
	"encoding/json/jsontext"
	"errors"
	"maps"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

type Config struct {
	APIKey      string
	BaseURL     string
	MaxAttempts *int
	Trace       func(Exchange)
	// Extensions are extra top-level request fields. They are merged over the
	// client's own (cache_control), so a caller can also replace that default.
	Extensions map[string]jsontext.Value
}

type Exchange = responsesapi.Exchange

type Client struct {
	llm.Adapter
	remote *primitives.RemoteClient
}

var _ llm.Adapter = (*Client)(nil)

func NewClient(config Config) (*Client, error) {
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, errors.New("OpenRouter API key must be set")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if baseURL == "" {
		return nil, errors.New("OpenRouter base URL must be set")
	}

	extensions := map[string]jsontext.Value{
		// OpenRouter only caches prompts for upstreams that need explicit breakpoints
		// (Anthropic among them) when the request opts in; the top-level field places
		// the breakpoint on the last cacheable block and moves it forward each turn.
		// With the x-session-id header, every turn of a session lands on one warm upstream.
		// The one-hour TTL survives the gaps an agent session produces (a long tool
		// call, a many-minute reasoning turn, the tool heartbeat); it costs 2x instead
		// of 1.25x on the small per-turn cache writes and saves a full-prefix rewrite
		// per gap.
		"cache_control": jsontext.Value(`{"type":"ephemeral","ttl":"1h"}`),
	}
	maps.Copy(extensions, config.Extensions)

	remote := primitives.NewRemoteClient()
	adapter, err := responsesapi.NewAdapter(remote, responsesapi.Config{
		Endpoint: baseURL + "/responses",
		Headers: map[string][]string{
			"Authorization": {"Bearer " + config.APIKey},
			"Content-Type":  {"application/json"},
		},
		Trace:             config.Trace,
		MaxAttempts:       config.MaxAttempts,
		CacheKeyPlacement: responsesapi.CacheKeyPlacement{Header: "x-session-id"},
		Extensions:        extensions,
	})
	if err != nil {
		_ = remote.Close()
		return nil, err
	}
	return &Client{Adapter: adapter, remote: remote}, nil
}

func (client *Client) Close() error {
	return client.remote.Close()
}
