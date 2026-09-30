package mcpclient

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseConfigReadsServersAndExpandsEnvironment(t *testing.T) {
	environment := map[string]string{"TOKEN_B": "secret-b", "HOST": "tools.example.test"}
	servers, err := ParseConfig([]byte(`{"mcpServers":{
		"b":{"type":"http","url":"https://${HOST}/mcp","headers":{"Authorization":"Bearer ${TOKEN_B}","X-Literal":"$HOME and ${ not a reference"},"timeout":330000},
		"a":{"url":"http://127.0.0.1:9/mcp"}
	}}`), func(name string) string { return environment[name] })
	if err != nil {
		t.Fatal(err)
	}
	want := []ServerConfig{
		{Name: "a", URL: "http://127.0.0.1:9/mcp", Headers: map[string]string{}, Timeout: DefaultTimeout},
		{Name: "b", URL: "https://tools.example.test/mcp", Headers: map[string]string{
			"Authorization": "Bearer secret-b",
			"X-Literal":     "$HOME and ${ not a reference",
		}, Timeout: 330 * time.Second},
	}
	if !reflect.DeepEqual(servers, want) {
		t.Fatalf("servers = %#v", servers)
	}
}

func TestParseConfigRejectsInvalidServers(t *testing.T) {
	for _, test := range []struct {
		name, config, want string
	}{
		{"not an object", `[]`, "invalid JSON"},
		{"missing servers", `{}`, `"mcpServers" must be an object`},
		{"unknown field", `{"mcpServers":{"s":{"url":"https://x.test","command":"run"}}}`, "invalid JSON"},
		{"stdio server", `{"mcpServers":{"s":{"type":"stdio"}}}`, `server "s": type "stdio" is not supported`},
		{"legacy SSE server", `{"mcpServers":{"s":{"type":"sse","url":"https://x.test"}}}`, `type "sse" is not supported`},
		{"blank name", `{"mcpServers":{" ":{"url":"https://x.test"}}}`, "name must not be empty"},
		{"missing URL", `{"mcpServers":{"s":{"type":"http"}}}`, "url: must be set"},
		{"other scheme", `{"mcpServers":{"s":{"url":"ftp://x.test"}}}`, "url: must use http or https"},
		{"no host", `{"mcpServers":{"s":{"url":"https:///path"}}}`, "url: must include a host"},
		{"reserved header", `{"mcpServers":{"s":{"url":"https://x.test","headers":{"mcp-session-id":"x"}}}}`, `header "mcp-session-id" is set by the client`},
		{"invalid header name", `{"mcpServers":{"s":{"url":"https://x.test","headers":{"Bad Header":"x"}}}}`, `header name "Bad Header" is invalid`},
		{"header line break", `{"mcpServers":{"s":{"url":"https://x.test","headers":{"X-A":"a\r\nX-B: b"}}}}`, `header "X-A" value contains a line break`},
		{"zero timeout", `{"mcpServers":{"s":{"url":"https://x.test","timeout":0}}}`, "timeout must be a positive number"},
		{"long timeout", `{"mcpServers":{"s":{"url":"https://x.test","timeout":3600001}}}`, "timeout must not exceed 3600000"},
		{"unset variable", `{"mcpServers":{"s":{"url":"https://x.test","headers":{"Authorization":"Bearer ${MISSING_TOKEN}"}}}}`, "environment variable MISSING_TOKEN is not set"},
		{"invalid reference", `{"mcpServers":{"s":{"url":"https://x.test/${1BAD}"}}}`, `"${1BAD}" is not a valid environment variable reference`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(test.config), func(string) string { return "" })
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ParseConfig() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestParseConfigErrorsDoNotQuoteExpandedValues(t *testing.T) {
	getenv := func(name string) string {
		if name == "TOKEN" {
			return "super-secret\nvalue"
		}
		return ""
	}
	for _, config := range []string{
		`{"mcpServers":{"s":{"url":"https://x.test","headers":{"Authorization":"Bearer ${TOKEN}"}}}}`,
		`{"mcpServers":{"s":{"url":"ftp://x.test/${TOKEN}"}}}`,
	} {
		_, err := ParseConfig([]byte(config), getenv)
		if err == nil || strings.Contains(err.Error(), "super-secret") {
			t.Fatalf("ParseConfig() error = %v", err)
		}
	}
}

func TestLoadConfigNamesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"s":{"type":"stdio"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path, nil); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"), nil); err == nil || !strings.Contains(err.Error(), "read MCP configuration") {
		t.Fatalf("LoadConfig() error = %v", err)
	}
}
