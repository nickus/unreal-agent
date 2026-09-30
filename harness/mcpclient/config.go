// Package mcpclient is a Model Context Protocol client for servers that use
// the streamable HTTP transport, and the remote-job handler that runs model
// tool calls through it.
package mcpclient

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	// DefaultTimeout bounds one tools/call request when a server sets no timeout.
	DefaultTimeout = 5 * time.Minute
	// MaxTimeout is the largest per-server timeout a configuration may set.
	MaxTimeout = time.Hour
	// StartupTimeout bounds initialize and tools/list for one server; a server
	// with a smaller Timeout uses that instead.
	StartupTimeout = time.Minute
)

// ServerConfig describes one MCP server reached over streamable HTTP.
type ServerConfig struct {
	Name    string
	URL     string
	Headers map[string]string
	// Timeout bounds each tools/call request to this server.
	Timeout time.Duration
}

// callTimeout is Timeout, or DefaultTimeout when Timeout is not set.
func (config ServerConfig) callTimeout() time.Duration {
	if config.Timeout <= 0 {
		return DefaultTimeout
	}
	return config.Timeout
}

// configFile is the "mcpServers" layout that other MCP clients read as well.
type configFile struct {
	MCPServers map[string]serverEntry `json:"mcpServers"`
}

type serverEntry struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	// Timeout is in milliseconds.
	Timeout *int64 `json:"timeout"`
}

// Headers that the client sets itself on every request.
var reservedHeaders = []string{
	"accept",
	"content-length",
	"content-type",
	"host",
	"last-event-id",
	"mcp-protocol-version",
	"mcp-session-id",
}

var (
	headerNamePattern = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	// ${NAME} references an environment variable.
	environmentReference = regexp.MustCompile(`\$\{([^}]*)\}`)
	environmentName      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// LoadConfig reads an MCP server configuration file. See ParseConfig.
func LoadConfig(path string, getenv func(string) string) ([]ServerConfig, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read MCP configuration: %w", err)
	}
	servers, err := ParseConfig(encoded, getenv)
	if err != nil {
		return nil, fmt.Errorf("MCP configuration %s: %w", path, err)
	}
	return servers, nil
}

// ParseConfig decodes {"mcpServers": {"name": {"type": "http", "url": ...,
// "headers": {...}, "timeout": milliseconds}}}. A URL or header value may
// reference environment variables as ${NAME}, so that credentials can stay out
// of the file. Servers are returned sorted by name.
func ParseConfig(encoded []byte, getenv func(string) string) ([]ServerConfig, error) {
	var file configFile
	if err := json.Unmarshal(encoded, &file, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if file.MCPServers == nil {
		return nil, errors.New(`"mcpServers" must be an object`)
	}
	servers := make([]ServerConfig, 0, len(file.MCPServers))
	for name, entry := range file.MCPServers {
		server, err := parseServer(name, entry, getenv)
		if err != nil {
			return nil, fmt.Errorf("server %q: %w", name, err)
		}
		servers = append(servers, server)
	}
	slices.SortFunc(servers, func(a, b ServerConfig) int { return strings.Compare(a.Name, b.Name) })
	return servers, nil
}

func parseServer(name string, entry serverEntry, getenv func(string) string) (ServerConfig, error) {
	if strings.TrimSpace(name) == "" {
		return ServerConfig{}, errors.New("name must not be empty")
	}
	switch entry.Type {
	case "", "http":
	default:
		return ServerConfig{}, fmt.Errorf("type %q is not supported; only streamable HTTP servers (type \"http\") are", entry.Type)
	}
	// Values are expanded before validation, and errors never quote them:
	// they may hold credentials.
	endpoint, err := expandEnvironment(entry.URL, getenv)
	if err != nil {
		return ServerConfig{}, fmt.Errorf("url: %w", err)
	}
	if err := validateURL(endpoint); err != nil {
		return ServerConfig{}, fmt.Errorf("url: %w", err)
	}
	headers := make(map[string]string, len(entry.Headers))
	for header, value := range entry.Headers {
		if !headerNamePattern.MatchString(header) {
			return ServerConfig{}, fmt.Errorf("header name %q is invalid", header)
		}
		if slices.Contains(reservedHeaders, strings.ToLower(header)) {
			return ServerConfig{}, fmt.Errorf("header %q is set by the client and cannot be configured", header)
		}
		expanded, err := expandEnvironment(value, getenv)
		if err != nil {
			return ServerConfig{}, fmt.Errorf("header %q: %w", header, err)
		}
		if strings.ContainsAny(expanded, "\r\n\x00") {
			return ServerConfig{}, fmt.Errorf("header %q value contains a line break or NUL", header)
		}
		headers[header] = expanded
	}
	timeout := DefaultTimeout
	if entry.Timeout != nil {
		if *entry.Timeout <= 0 {
			return ServerConfig{}, errors.New("timeout must be a positive number of milliseconds")
		}
		if *entry.Timeout > MaxTimeout.Milliseconds() {
			return ServerConfig{}, fmt.Errorf("timeout must not exceed %d milliseconds", MaxTimeout.Milliseconds())
		}
		timeout = time.Duration(*entry.Timeout) * time.Millisecond
	}
	return ServerConfig{Name: name, URL: endpoint, Headers: headers, Timeout: timeout}, nil
}

func validateURL(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("must be set")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return errors.New("is not a valid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("must use http or https")
	}
	if parsed.Host == "" {
		return errors.New("must include a host")
	}
	return nil
}

// expandEnvironment replaces each ${NAME} with the variable's value. A
// variable that is unset or empty is an error, so that a missing credential is
// reported instead of sent as an empty header.
func expandEnvironment(value string, getenv func(string) string) (string, error) {
	var expandErr error
	expanded := environmentReference.ReplaceAllStringFunc(value, func(reference string) string {
		name := reference[2 : len(reference)-1]
		if !environmentName.MatchString(name) {
			expandErr = errors.Join(expandErr, fmt.Errorf("%q is not a valid environment variable reference", reference))
			return ""
		}
		resolved := ""
		if getenv != nil {
			resolved = getenv(name)
		}
		if resolved == "" {
			expandErr = errors.Join(expandErr, fmt.Errorf("environment variable %s is not set", name))
		}
		return resolved
	})
	if expandErr != nil {
		return "", expandErr
	}
	return expanded, nil
}
