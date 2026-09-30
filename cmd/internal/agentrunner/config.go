package agentrunner

import (
	"context"
	"io"
	"slices"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type Config struct {
	Name         string
	Providers    []Provider
	ParseRequest func(io.Reader) (Request, ToolFactory, error)
}

type ToolConfig struct {
	Translators tool.StaticTranslators
	Names       []string
	SessionID   session.ID
	Getenv      func(string) string
	// MCPConfig is the path of an MCP server configuration file; empty
	// configures no servers.
	MCPConfig string
	// OperationDirectory is the session's absolute directory for files that
	// operations write.
	OperationDirectory string
}

type Tools struct {
	Registry   tool.Registry
	RemoteJobs []operation.RemoteJobHandler
	Close      func() error
	// Warnings describe tools that could not be offered; the run goes on
	// without them.
	Warnings []error
}

type ToolFactory func(context.Context, ToolConfig) (Tools, error)

func (parsed Request) EnabledTools(names ...string) []string {
	enabled := make([]string, 0, len(names))
	for _, name := range names {
		if !slices.Contains(parsed.DisallowedTools, name) {
			enabled = append(enabled, name)
		}
	}
	return enabled
}
