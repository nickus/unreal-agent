package main

import (
	"context"
	"io"
	"net/http"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/mcpclient"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/mcptool"
)

func parseRequest(input io.Reader) (agentrunner.Request, agentrunner.ToolFactory, error) {
	var parsed agentrunner.Request
	if err := agentrunner.DecodeRequest(input, &parsed); err != nil {
		return agentrunner.Request{}, nil, err
	}
	return parsed, func(ctx context.Context, config agentrunner.ToolConfig) (agentrunner.Tools, error) {
		registry := tool.NewRegistry(config.Translators, parsed.EnabledTools(config.Names...)...)
		return withMCPTools(ctx, config, registry, parsed.DisallowedTools)
	}, nil
}

// withMCPTools adds the tools of the configured MCP servers. The MCP call
// handler is registered even without servers, so that a resumed session can
// settle calls it recorded in an earlier run.
func withMCPTools(
	ctx context.Context,
	config agentrunner.ToolConfig,
	registry tool.Registry,
	disallowed []string,
) (agentrunner.Tools, error) {
	var servers []mcpclient.ServerConfig
	if config.MCPConfig != "" {
		loaded, err := mcpclient.LoadConfig(config.MCPConfig, config.Getenv)
		if err != nil {
			return agentrunner.Tools{}, err
		}
		servers = loaded
	}
	// An own transport, so that Close releases only these connections.
	transport := http.DefaultTransport
	if base, ok := transport.(*http.Transport); ok {
		transport = base.Clone()
	}
	httpClient := &http.Client{Transport: transport}
	connected, warnings := mcpclient.Connect(ctx, servers, httpClient)
	var tools []mcptool.Tool
	for _, server := range connected {
		for _, listed := range server.Tools {
			tools = append(tools, mcptool.Tool{Server: server.Config.Name, Tool: listed})
		}
	}
	registry, problems := mcptool.NewRegistry(registry, tools, disallowed)
	jobs := mcpclient.NewJobs(ctx, connected, config.OperationDirectory, httpClient)
	return agentrunner.Tools{
		Registry:   registry,
		RemoteJobs: []operation.RemoteJobHandler{jobs},
		Close:      jobs.Close,
		Warnings:   append(warnings, problems...),
	}, nil
}
