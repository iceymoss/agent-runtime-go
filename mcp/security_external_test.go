package mcp_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/mcp"
)

func TestManagerRejectsUnvalidatedConfigBeforeConnect(t *testing.T) {
	tests := []struct {
		name      string
		transport mcp.TransportConfig
	}{
		{name: "unknown stdio key shape", transport: mcp.TransportConfig{Kind: mcp.TransportStdio, Stdio: &mcp.StdioConfig{ServerKey: "/bin/sh"}}},
		{name: "shell argument newline", transport: mcp.TransportConfig{Kind: mcp.TransportStdio, Stdio: &mcp.StdioConfig{ServerKey: "approved", Args: []string{"ok\nnext"}}}},
		{name: "forbidden header", transport: mcp.TransportConfig{Kind: mcp.TransportStreamableHTTP, HTTP: &mcp.HTTPConfig{Endpoint: "https://example.com/mcp", HeaderRefs: []mcp.HeaderRef{{Name: "Host", SecretRef: "secret/auth"}}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &fakeSource{snapshots: map[agent.TenantKey]mcp.ConfigSnapshot{
				"tenant/security": {Servers: []mcp.ServerConfig{{ID: "server", Enabled: true, Required: true, Transport: test.transport}}},
			}}
			connector := &countConnector{}
			manager, err := mcp.NewManager(source, connector)
			if err != nil {
				t.Fatal(err)
			}
			err = manager.StartScope(context.Background(), mcp.Scope{TenantKey: "tenant/security"})
			if err == nil || connector.calls != 0 {
				t.Fatalf("unsafe config reached connector: err=%v calls=%d", err, connector.calls)
			}
		})
	}
}

func TestHTTPConnectorRejectsInsecureAndPrivateDestinations(t *testing.T) {
	connector, err := mcp.NewStreamableHTTPConnector(http.DefaultClient, mcp.HTTPPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"http://localhost/mcp", "https://127.0.0.1/mcp"} {
		_, err := connector.Connect(context.Background(), mcp.ConnectRequest{
			Scope:  mcp.Scope{TenantKey: "tenant/http"},
			Config: mcp.ServerConfig{ID: "server", Transport: mcp.TransportConfig{Kind: mcp.TransportStreamableHTTP, HTTP: &mcp.HTTPConfig{Endpoint: endpoint}}},
		})
		if !errors.Is(err, mcp.ErrTransportDenied) {
			t.Fatalf("endpoint %q: %v", endpoint, err)
		}
	}
}

type countConnector struct{ calls int }

func (c *countConnector) Connect(context.Context, mcp.ConnectRequest) (mcp.Client, error) {
	c.calls++
	return nil, errors.New("unexpected connect")
}
