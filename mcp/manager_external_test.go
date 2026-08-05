package mcp_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/mcp"
)

type fakeSource struct {
	mu        sync.Mutex
	snapshots map[agent.TenantKey]mcp.ConfigSnapshot
	err       error
	calls     []agent.TenantKey
}

func (s *fakeSource) Snapshot(_ context.Context, scope mcp.Scope) (mcp.ConfigSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, scope.TenantKey)
	if s.err != nil {
		return mcp.ConfigSnapshot{}, s.err
	}
	return s.snapshots[scope.TenantKey], nil
}

type fakeConnector struct {
	mu      sync.Mutex
	clients map[agent.TenantKey][]*fakeClient
}

func (c *fakeConnector) Connect(_ context.Context, request mcp.ConnectRequest) (mcp.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	client := &fakeClient{tenant: request.Scope.TenantKey}
	c.clients[request.Scope.TenantKey] = append(c.clients[request.Scope.TenantKey], client)
	return client, nil
}

type fakeClient struct {
	mu     sync.Mutex
	tenant agent.TenantKey
	closed int
}

func (c *fakeClient) Initialize(context.Context) (mcp.InitializeResult, error) {
	return mcp.InitializeResult{ProtocolVersion: "test", Capabilities: mcp.Capabilities{Tools: true}}, nil
}

func (c *fakeClient) ListTools(context.Context) ([]mcp.ToolDescriptor, error) {
	return []mcp.ToolDescriptor{{Name: "lookup", InputSchema: map[string]any{"type": "object"}}}, nil
}

func (c *fakeClient) CallTool(ctx context.Context, _ mcp.ClientToolCall) (mcp.ToolResult, error) {
	select {
	case <-ctx.Done():
		return mcp.ToolResult{}, ctx.Err()
	default:
		return mcp.ToolResult{Content: []mcp.Content{{Kind: mcp.ContentText, Text: string(c.tenant)}}}, nil
	}
}

func (c *fakeClient) Ping(context.Context) error { return nil }

func (c *fakeClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed++
	return nil
}

func (c *fakeClient) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func TestManagerLazyTenantIsolationAndDeadline(t *testing.T) {
	first := agent.TenantKey("tenant/first")
	second := agent.TenantKey("tenant/second")
	source := &fakeSource{snapshots: map[agent.TenantKey]mcp.ConfigSnapshot{
		first:  testConfig("1"),
		second: testConfig("1"),
	}}
	connector := &fakeConnector{clients: make(map[agent.TenantKey][]*fakeClient)}
	manager, err := mcp.NewManager(source, connector)
	if err != nil {
		t.Fatal(err)
	}
	if status := manager.Status(mcp.Scope{TenantKey: first}); status.State != mcp.StateNew {
		t.Fatalf("unexpected initial status: %+v", status)
	}
	if len(source.calls) != 0 {
		t.Fatal("constructor enumerated tenants")
	}
	if err := manager.StartScope(context.Background(), mcp.Scope{TenantKey: first}); err != nil {
		t.Fatal(err)
	}
	if len(source.calls) != 1 || source.calls[0] != first {
		t.Fatalf("unexpected source calls: %v", source.calls)
	}
	snapshot, ok := manager.Snapshot(mcp.Scope{TenantKey: first})
	if !ok {
		t.Fatal("missing first tenant snapshot")
	}
	result, err := manager.CallTool(context.Background(), mcp.ToolCall{Scope: mcp.Scope{TenantKey: first}, Generation: snapshot.Generation, ServerID: "server", Name: "lookup"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content[0].Text != string(first) {
		t.Fatalf("cross-tenant result: %+v", result)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.CallTool(cancelled, mcp.ToolCall{Scope: mcp.Scope{TenantKey: first}, Generation: snapshot.Generation, ServerID: "server", Name: "lookup"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("deadline not propagated: %v", err)
	}
}

func TestManagerGenerationLeaseDrainAndFailedReload(t *testing.T) {
	tenant := agent.TenantKey("tenant/lease")
	source := &fakeSource{snapshots: map[agent.TenantKey]mcp.ConfigSnapshot{tenant: testConfig("1")}}
	connector := &fakeConnector{clients: make(map[agent.TenantKey][]*fakeClient)}
	manager, err := mcp.NewManager(source, connector)
	if err != nil {
		t.Fatal(err)
	}
	scope := mcp.Scope{TenantKey: tenant}
	if err := manager.StartScope(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	first, _ := manager.Snapshot(scope)
	lease, err := manager.AcquireGeneration(context.Background(), scope, first.Generation)
	if err != nil {
		t.Fatal(err)
	}
	source.snapshots[tenant] = testConfig("2")
	second, err := manager.Reload(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if first.Generation == second.Generation {
		t.Fatal("reload did not publish a new generation")
	}
	oldClient := connector.clients[tenant][0]
	if oldClient.closeCount() != 0 {
		t.Fatal("leased old generation closed early")
	}
	if _, err := lease.CallTool(context.Background(), mcp.ToolCall{ServerID: "server", Name: "lookup"}); err != nil {
		t.Fatalf("leased old generation unavailable: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if oldClient.closeCount() != 1 {
		t.Fatalf("old generation close count = %d", oldClient.closeCount())
	}
	if err := lease.Close(); err != nil || oldClient.closeCount() != 1 {
		t.Fatal("lease close is not idempotent")
	}
	source.mu.Lock()
	source.err = errors.New("source unavailable")
	source.mu.Unlock()
	if _, err := manager.Reload(context.Background(), scope); err == nil {
		t.Fatal("failed reload succeeded")
	}
	retained, ok := manager.Snapshot(scope)
	if !ok || retained.Generation != second.Generation {
		t.Fatal("failed reload replaced current generation")
	}
	if status := manager.Status(scope); !status.Ready || !status.Degraded {
		t.Fatalf("unexpected degraded status: %+v", status)
	}
	report, err := manager.Close(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Incomplete) != 0 || len(report.Closed) != 1 {
		t.Fatalf("unexpected close report: %+v", report)
	}
	if _, err := manager.Reload(context.Background(), scope); !errors.Is(err, mcp.ErrClosed) {
		t.Fatalf("reload after close: %v", err)
	}
}

func testConfig(version string) mcp.ConfigSnapshot {
	return mcp.ConfigSnapshot{Generation: version, Servers: []mcp.ServerConfig{{
		ID: "server", Version: version, Enabled: true, Required: true,
		ConnectTimeout: time.Second, CallTimeout: time.Second,
		Transport: mcp.TransportConfig{Kind: mcp.TransportStdio, Stdio: &mcp.StdioConfig{ServerKey: "approved"}},
	}}}
}
