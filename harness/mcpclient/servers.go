package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"sync"
)

// Server is a server whose session is open and whose tools were listed.
type Server struct {
	Config ServerConfig
	Client *Client
	Tools  []Tool
}

// ClientInfo identifies this client in initialize requests.
func ClientInfo() Implementation {
	version := "devel"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
	return Implementation{Name: "unreal-agent", Version: version}
}

// Connect initializes each server and lists its tools, all servers at once.
// A server that fails is left out and its error returned, so that one
// unreachable server does not take the others down.
func Connect(ctx context.Context, configs []ServerConfig, httpClient *http.Client) ([]*Server, []error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	servers := make([]*Server, len(configs))
	failures := make([]error, len(configs))
	var workers sync.WaitGroup
	for index, config := range configs {
		workers.Go(func() {
			server, err := connect(ctx, config, httpClient)
			if err != nil {
				failures[index] = fmt.Errorf("MCP server %q: %w", config.Name, err)
				return
			}
			servers[index] = server
		})
	}
	workers.Wait()
	connected := make([]*Server, 0, len(servers))
	for _, server := range servers {
		if server != nil {
			connected = append(connected, server)
		}
	}
	var errs []error
	for _, err := range failures {
		if err != nil {
			errs = append(errs, err)
		}
	}
	return connected, errs
}

func connect(ctx context.Context, config ServerConfig, httpClient *http.Client) (*Server, error) {
	timeout := min(config.callTimeout(), StartupTimeout)
	startup, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := NewClient(config, httpClient, ClientInfo())
	tools, err := func() ([]Tool, error) {
		if err := client.Initialize(startup); err != nil {
			return nil, err
		}
		return client.ListTools(startup)
	}()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			err = fmt.Errorf("no answer within %s: %w", timeout, err)
		}
		closing, cancelClose := context.WithTimeout(context.WithoutCancel(ctx), notificationTimeout)
		defer cancelClose()
		client.Close(closing)
		return nil, err
	}
	return &Server{Config: config, Client: client, Tools: tools}, nil
}
