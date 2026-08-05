package mcp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/iceymoss/agent-runtime-go"
	jsoncodec "github.com/iceymoss/agent-runtime-go/internal/jsoncodec"
)

type manager struct {
	mu        sync.RWMutex
	source    ConfigSource
	connector Connector
	scopes    map[agent.TenantKey]*scopeState
	closed    bool
	sequence  atomic.Uint64
}

type scopeState struct {
	opMu sync.Mutex
	mu   sync.RWMutex

	scope       Scope
	state       State
	snapshot    Snapshot
	generations map[Generation]*generationState
	diagnostics []Diagnostic
	changed     chan struct{}
}

type generationState struct {
	snapshot Snapshot
	servers  map[ServerID]*liveServer
	refs     int
	retiring bool
	closed   bool
}

type liveServer struct {
	config ServerConfig
	client Client
	info   ServerSnapshot
}

type lease struct {
	once       sync.Once
	owner      *scopeState
	generation *generationState
	value      Generation
	closeErr   error
}

func NewManager(source ConfigSource, connector Connector) (Manager, error) {
	if source == nil || connector == nil {
		return nil, mcpError(ErrInvalidConfig, nil, "new manager", "", "", "source and connector are required")
	}
	return &manager{source: source, connector: connector, scopes: make(map[agent.TenantKey]*scopeState)}, nil
}

func (m *manager) StartScope(ctx context.Context, scope Scope) error {
	if err := validateScope(scope); err != nil {
		return err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	state, exists := m.scopes[scope.TenantKey]
	if !exists {
		state = &scopeState{scope: scope, state: StateNew, generations: make(map[Generation]*generationState), changed: make(chan struct{})}
		m.scopes[scope.TenantKey] = state
	}
	m.mu.Unlock()

	state.opMu.Lock()
	defer state.opMu.Unlock()
	state.mu.RLock()
	alreadyStarted := state.state != StateNew
	state.mu.RUnlock()
	if alreadyStarted {
		return nil
	}
	state.transition(StateStarting, nil)
	if _, err := m.reloadLocked(ctx, state); err != nil {
		state.transition(StateDegraded, []Diagnostic{{Message: "initial load failed"}})
		return err
	}
	return nil
}

func (m *manager) Reload(ctx context.Context, scope Scope) (Snapshot, error) {
	state, err := m.scope(scope)
	if err != nil {
		return Snapshot{}, err
	}
	state.opMu.Lock()
	defer state.opMu.Unlock()
	return m.reloadLocked(ctx, state)
}

func (m *manager) Refresh(ctx context.Context, scope Scope) (Snapshot, error) {
	return m.Reload(ctx, scope)
}

func (m *manager) reloadLocked(ctx context.Context, state *scopeState) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	state.mu.RLock()
	closed := state.state == StateClosing || state.state == StateClosed
	state.mu.RUnlock()
	if closed || m.isClosed() {
		return Snapshot{}, ErrClosed
	}
	configSnapshot, err := m.source.Snapshot(ctx, state.scope)
	if err != nil {
		state.degrade("config source failed")
		return Snapshot{}, mcpError(ErrNotReady, err, "reload", "", "", "config source failed")
	}
	configSnapshot, err = normalizeConfigSnapshot(configSnapshot)
	if err != nil {
		state.degrade("configuration rejected")
		return Snapshot{}, err
	}
	built, err := m.buildGeneration(ctx, state.scope, configSnapshot)
	if err != nil {
		state.degrade("generation refresh failed")
		return Snapshot{}, err
	}

	state.mu.Lock()
	if state.state == StateClosing || state.state == StateClosed || m.isClosed() {
		state.mu.Unlock()
		closeGeneration(built)
		return Snapshot{}, ErrClosed
	}
	if existing := state.generations[built.snapshot.Generation]; existing != nil {
		state.snapshot = existing.snapshot
		state.state = statusState(existing.snapshot)
		state.diagnostics = append([]Diagnostic(nil), existing.snapshot.Diagnostics...)
		state.notifyLocked()
		state.mu.Unlock()
		closeGeneration(built)
		return cloneSnapshot(existing.snapshot), nil
	}
	old := state.generations[state.snapshot.Generation]
	state.generations[built.snapshot.Generation] = built
	state.snapshot = built.snapshot
	state.state = statusState(built.snapshot)
	state.diagnostics = append([]Diagnostic(nil), built.snapshot.Diagnostics...)
	if old != nil {
		old.retiring = true
	}
	state.notifyLocked()
	toClose := collectRetiredLocked(state)
	result := cloneSnapshot(built.snapshot)
	state.mu.Unlock()
	closeServers(toClose)
	return result, nil
}

func (m *manager) buildGeneration(ctx context.Context, scope Scope, config ConfigSnapshot) (*generationState, error) {
	built := &generationState{servers: make(map[ServerID]*liveServer)}
	diagnostics := make([]Diagnostic, 0)
	for _, serverConfig := range config.Servers {
		if !serverConfig.Enabled {
			continue
		}
		server, err := m.connectServer(ctx, scope, serverConfig)
		if err != nil {
			if serverConfig.Required {
				closeGeneration(built)
				return nil, err
			}
			diagnostics = append(diagnostics, Diagnostic{ServerID: serverConfig.ID, Message: "optional server unavailable"})
			continue
		}
		built.servers[serverConfig.ID] = server
	}
	serverSnapshots := make([]ServerSnapshot, 0, len(built.servers))
	for _, server := range built.servers {
		serverSnapshots = append(serverSnapshots, cloneServerSnapshot(server.info))
	}
	sort.Slice(serverSnapshots, func(i, j int) bool { return serverSnapshots[i].ID < serverSnapshots[j].ID })
	configDigest, err := agent.CanonicalDigest(config)
	if err != nil {
		closeGeneration(built)
		return nil, mcpError(ErrInvalidConfig, err, "digest config", "", "", "configuration digest failed")
	}
	digestValue := struct {
		ConfigGeneration string
		ConfigDigest     string
		ConnectionSerial uint64
		Servers          []ServerSnapshot
	}{ConfigGeneration: config.Generation, ConfigDigest: configDigest, ConnectionSerial: m.sequence.Add(1), Servers: serverSnapshots}
	digest, err := agent.CanonicalDigest(digestValue)
	if err != nil {
		closeGeneration(built)
		return nil, mcpError(ErrCapabilityInvalid, err, "digest generation", "", "", "capability digest failed")
	}
	built.snapshot = Snapshot{TenantKey: scope.TenantKey, Generation: Generation(digest), ConfigGeneration: config.Generation, ConfigDigest: configDigest, Servers: serverSnapshots, Diagnostics: diagnostics}
	for i := range built.snapshot.Servers {
		built.snapshot.Servers[i].ConnectionGeneration = built.snapshot.Generation
	}
	for _, server := range built.servers {
		server.info.ConnectionGeneration = built.snapshot.Generation
	}
	return built, nil
}

func (m *manager) connectServer(ctx context.Context, scope Scope, config ServerConfig) (*liveServer, error) {
	connectCtx, cancel := context.WithTimeout(ctx, config.ConnectTimeout)
	defer cancel()
	client, err := m.connector.Connect(connectCtx, ConnectRequest{Scope: scope, Config: cloneServerConfig(config)})
	if err != nil {
		return nil, mcpError(ErrReconnectFailed, err, "connect", config.ID, "", "connection failed")
	}
	if client == nil {
		return nil, mcpError(ErrReconnectFailed, nil, "connect", config.ID, "", "connector returned nil client")
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = client.Close()
		}
	}()
	initialize, err := client.Initialize(connectCtx)
	if err != nil {
		return nil, mcpError(ErrUpstream, err, "initialize", config.ID, "", "initialize failed")
	}
	if !initialize.Capabilities.Tools {
		return nil, mcpError(ErrCapabilityInvalid, nil, "initialize", config.ID, "", "tools capability is required")
	}
	descriptors, err := client.ListTools(connectCtx)
	if err != nil {
		return nil, mcpError(ErrUpstream, err, "list tools", config.ID, "", "tool listing failed")
	}
	definitions, err := capabilityDefinitions(config.ID, descriptors, config.DisabledTools)
	if err != nil {
		return nil, err
	}
	succeeded = true
	return &liveServer{config: cloneServerConfig(config), client: client, info: ServerSnapshot{ID: config.ID, Initialize: initialize, Tools: definitions}}, nil
}

func (m *manager) Snapshot(scope Scope) (Snapshot, bool) {
	state, err := m.scope(scope)
	if err != nil {
		return Snapshot{}, false
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	if state.snapshot.Generation == "" {
		return Snapshot{}, false
	}
	return cloneSnapshot(state.snapshot), true
}

func (m *manager) AcquireGeneration(ctx context.Context, scope Scope, generation Generation) (GenerationLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state, err := m.scope(scope)
	if err != nil {
		return nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.state == StateClosing || state.state == StateClosed {
		return nil, ErrClosed
	}
	selected := state.generations[generation]
	if selected == nil || selected.closed {
		return nil, mcpError(ErrGenerationUnavailable, nil, "acquire generation", "", generation, "exact generation is unavailable")
	}
	selected.refs++
	return &lease{owner: state, generation: selected, value: generation}, nil
}

func (m *manager) CallTool(ctx context.Context, call ToolCall) (ToolResult, error) {
	leaseValue, err := m.AcquireGeneration(ctx, call.Scope, call.Generation)
	if err != nil {
		return ToolResult{}, err
	}
	defer func() { _ = leaseValue.Close() }()
	return leaseValue.CallTool(ctx, call)
}

func (l *lease) Generation() Generation { return l.value }

func (l *lease) Snapshot() Snapshot {
	if l == nil || l.generation == nil {
		return Snapshot{}
	}
	return cloneSnapshot(l.generation.snapshot)
}

func (l *lease) CallTool(ctx context.Context, call ToolCall) (ToolResult, error) {
	if l == nil || l.generation == nil {
		return ToolResult{}, ErrGenerationUnavailable
	}
	if call.Generation != "" && call.Generation != l.value {
		return ToolResult{}, mcpError(ErrGenerationUnavailable, nil, "call tool", call.ServerID, call.Generation, "lease generation mismatch")
	}
	if call.Scope.TenantKey != "" && call.Scope.TenantKey != l.generation.snapshot.TenantKey {
		return ToolResult{}, mcpError(ErrGenerationUnavailable, nil, "call tool", call.ServerID, l.value, "lease tenant mismatch")
	}
	server := l.generation.servers[call.ServerID]
	if server == nil {
		return ToolResult{}, mcpError(ErrServerNotFound, nil, "call tool", call.ServerID, l.value, "server absent from generation")
	}
	if !serverHasTool(server, call.Name) {
		return ToolResult{}, mcpError(ErrCapabilityInvalid, nil, "call tool", call.ServerID, l.value, "tool absent from generation")
	}
	callCtx, cancel := context.WithTimeout(ctx, server.config.CallTimeout)
	defer cancel()
	result, err := server.client.CallTool(callCtx, ClientToolCall{Name: call.Name, Arguments: cloneMap(call.Arguments)})
	if err != nil {
		return ToolResult{}, mcpError(ErrUpstream, err, "call tool", call.ServerID, l.value, "upstream call failed")
	}
	result = cloneToolResult(result)
	if resultSize(result) > server.config.MaxResultBytes {
		return ToolResult{}, mcpError(ErrResultTooLarge, nil, "call tool", call.ServerID, l.value, "result exceeds configured limit")
	}
	return result, nil
}

func (l *lease) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		l.owner.mu.Lock()
		if l.generation.refs > 0 {
			l.generation.refs--
		}
		servers := collectRetiredLocked(l.owner)
		l.owner.notifyLocked()
		l.owner.mu.Unlock()
		l.closeErr = closeServers(servers)
	})
	return l.closeErr
}

func (m *manager) WaitReady(ctx context.Context, scope Scope) (Status, error) {
	state, err := m.scope(scope)
	if err != nil {
		return Status{}, err
	}
	for {
		state.mu.RLock()
		status := state.statusLocked()
		changed := state.changed
		state.mu.RUnlock()
		switch status.State {
		case StateNew:
			return status, ErrNotStarted
		case StateReady, StateDegraded:
			if status.Ready {
				return status, nil
			}
			return status, ErrNotReady
		case StateClosing, StateClosed:
			return status, ErrClosed
		}
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-changed:
		}
	}
}

func (m *manager) Status(scope Scope) Status {
	state, err := m.scope(scope)
	if err != nil {
		if errors.Is(err, ErrNotStarted) {
			return Status{State: StateNew}
		}
		return Status{State: StateClosed}
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.statusLocked()
}

func (m *manager) CloseScope(ctx context.Context, scope Scope) (CloseReport, error) {
	state, err := m.scope(scope)
	if err != nil {
		if errors.Is(err, ErrNotStarted) {
			return CloseReport{}, nil
		}
		return CloseReport{}, err
	}
	state.opMu.Lock()
	defer state.opMu.Unlock()
	return closeScopeState(ctx, state)
}

func (m *manager) Close(ctx context.Context) (CloseReport, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return CloseReport{}, nil
	}
	m.closed = true
	states := make([]*scopeState, 0, len(m.scopes))
	for _, state := range m.scopes {
		states = append(states, state)
	}
	m.mu.Unlock()
	var report CloseReport
	var closeErr error
	for _, state := range states {
		state.opMu.Lock()
		part, err := closeScopeState(ctx, state)
		state.opMu.Unlock()
		report.Closed = append(report.Closed, part.Closed...)
		report.Incomplete = append(report.Incomplete, part.Incomplete...)
		closeErr = errors.Join(closeErr, err)
	}
	sort.Slice(report.Closed, func(i, j int) bool { return report.Closed[i] < report.Closed[j] })
	sort.Slice(report.Incomplete, func(i, j int) bool { return report.Incomplete[i] < report.Incomplete[j] })
	return report, closeErr
}

func closeScopeState(ctx context.Context, state *scopeState) (CloseReport, error) {
	state.mu.Lock()
	if state.state == StateClosed {
		state.mu.Unlock()
		return CloseReport{}, nil
	}
	state.state = StateClosing
	for _, generation := range state.generations {
		generation.retiring = true
	}
	state.notifyLocked()
	servers := collectRetiredLocked(state)
	var incomplete []ServerID
	for _, generation := range state.generations {
		if generation.refs > 0 {
			for id := range generation.servers {
				incomplete = append(incomplete, id)
			}
		}
	}
	if len(incomplete) == 0 {
		state.state = StateClosed
		state.notifyLocked()
	}
	state.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return CloseReport{Incomplete: uniqueServerIDs(incomplete)}, err
	}
	err := closeServers(servers)
	closed := make([]ServerID, 0, len(servers))
	for _, server := range servers {
		closed = append(closed, server.info.ID)
	}
	return CloseReport{Closed: uniqueServerIDs(closed), Incomplete: uniqueServerIDs(incomplete)}, err
}

func (m *manager) scope(scope Scope) (*scopeState, error) {
	if err := validateScope(scope); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, ErrClosed
	}
	state := m.scopes[scope.TenantKey]
	if state == nil {
		return nil, ErrNotStarted
	}
	return state, nil
}

func (m *manager) isClosed() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.closed
}

func (state *scopeState) transition(next State, diagnostics []Diagnostic) {
	state.mu.Lock()
	state.state = next
	state.diagnostics = append([]Diagnostic(nil), diagnostics...)
	state.notifyLocked()
	state.mu.Unlock()
}

func (state *scopeState) degrade(message string) {
	state.mu.Lock()
	state.state = StateDegraded
	state.diagnostics = []Diagnostic{{Message: message}}
	state.notifyLocked()
	state.mu.Unlock()
}

func (state *scopeState) notifyLocked() {
	close(state.changed)
	state.changed = make(chan struct{})
}

func (state *scopeState) statusLocked() Status {
	return Status{State: state.state, Generation: state.snapshot.Generation, Ready: state.snapshot.Generation != "", Degraded: state.state == StateDegraded, Diagnostics: append([]Diagnostic(nil), state.diagnostics...)}
}

func statusState(snapshot Snapshot) State {
	if len(snapshot.Diagnostics) > 0 || len(snapshot.Servers) == 0 {
		return StateDegraded
	}
	return StateReady
}

func collectRetiredLocked(state *scopeState) []*liveServer {
	var servers []*liveServer
	for generationID, generation := range state.generations {
		if !generation.retiring || generation.refs != 0 || generation.closed {
			continue
		}
		generation.closed = true
		for _, server := range generation.servers {
			servers = append(servers, server)
		}
		delete(state.generations, generationID)
	}
	if state.state == StateClosing && len(state.generations) == 0 {
		state.state = StateClosed
	}
	return servers
}

func closeGeneration(generation *generationState) error {
	if generation == nil {
		return nil
	}
	servers := make([]*liveServer, 0, len(generation.servers))
	for _, server := range generation.servers {
		servers = append(servers, server)
	}
	return closeServers(servers)
}

func closeServers(servers []*liveServer) error {
	var result error
	for _, server := range servers {
		if server != nil && server.client != nil {
			result = errors.Join(result, server.client.Close())
		}
	}
	return result
}

func serverHasTool(server *liveServer, name string) bool {
	for _, definition := range server.info.Tools {
		if definition.Name == name || definition.Canonical == name {
			return true
		}
	}
	return false
}

func resultSize(result ToolResult) int64 {
	var size int64
	for _, content := range result.Content {
		size += int64(len(content.Text) + len(content.Data) + len(content.URI) + len(content.Name) + len(content.MIMEType))
	}
	if result.StructuredContent != nil {
		serialized, err := jsoncodec.Marshal(result.StructuredContent)
		if err == nil {
			size += int64(len(serialized))
		}
	}
	for _, content := range result.Content {
		if content.Structured == nil && content.Annotations == nil {
			continue
		}
		serialized, err := jsoncodec.Marshal(struct {
			Structured  any            `json:"structured,omitempty"`
			Annotations map[string]any `json:"annotations,omitempty"`
		}{Structured: content.Structured, Annotations: content.Annotations})
		if err == nil {
			size += int64(len(serialized))
		}
	}
	return size
}

func uniqueServerIDs(values []ServerID) []ServerID {
	seen := make(map[ServerID]struct{}, len(values))
	result := make([]ServerID, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; !exists {
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

var _ Manager = (*manager)(nil)
var _ GenerationLease = (*lease)(nil)

func (m *manager) String() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return fmt.Sprintf("mcp.Manager{scopes:%d, closed:%t}", len(m.scopes), m.closed)
}
