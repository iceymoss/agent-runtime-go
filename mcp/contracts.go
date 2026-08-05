// Package mcp provides a tenant-scoped MCP capability runtime. It owns live
// transport generations but no durable generation records.
package mcp

import (
	"context"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

type ServerID string
type Generation string
type TransportKind string
type State string

const (
	TransportStdio          TransportKind = "stdio"
	TransportStreamableHTTP TransportKind = "streamable_http"

	StateNew      State = "new"
	StateStarting State = "starting"
	StateReady    State = "ready"
	StateDegraded State = "degraded"
	StateClosing  State = "closing"
	StateClosed   State = "closed"
)

type Scope struct {
	TenantKey agent.TenantKey
}

type ServerConfig struct {
	ID             ServerID
	Version        string
	Enabled        bool
	Transport      TransportConfig
	DisabledTools  []string
	Required       bool
	ConnectTimeout time.Duration
	CallTimeout    time.Duration
	MaxResultBytes int64
}

type TransportConfig struct {
	Kind  TransportKind
	Stdio *StdioConfig
	HTTP  *HTTPConfig
}

type StdioConfig struct {
	ServerKey string
	Args      []string
	EnvRefs   []EnvRef
}

type EnvRef struct {
	Name      string
	SecretRef string
}

type HTTPConfig struct {
	Endpoint   string
	AuthRef    string
	HeaderRefs []HeaderRef
}

type HeaderRef struct {
	Name      string
	SecretRef string
}

type ConfigSnapshot struct {
	Generation string
	Servers    []ServerConfig
}

type ConfigSource interface {
	Snapshot(context.Context, Scope) (ConfigSnapshot, error)
}

type SecretProvider interface {
	Resolve(context.Context, agent.TenantKey, string) (string, error)
}

// Transport is the minimal request/response port used by protocol clients.
// Implementations must be safe for concurrent Send calls unless documented by
// their connector as serialized.
type Transport interface {
	Send(context.Context, Request) (Response, error)
	Close() error
}

type Request struct {
	Method string
	Params any
}

type Response struct {
	Result []byte
	Error  *UpstreamError
}

// Connector establishes and initializes one tenant/server client.
type Connector interface {
	Connect(context.Context, ConnectRequest) (Client, error)
}

// TransportFactory is retained as the descriptive S27 port name.
type TransportFactory = Connector

type ConnectRequest struct {
	Scope  Scope
	Config ServerConfig
}

type InitializeResult struct {
	ProtocolVersion string
	ServerName      string
	ServerVersion   string
	Instructions    string
	Capabilities    Capabilities
}

type Capabilities struct {
	Tools     bool
	Prompts   bool
	Resources bool
}

type Client interface {
	Initialize(context.Context) (InitializeResult, error)
	ListTools(context.Context) ([]ToolDescriptor, error)
	CallTool(context.Context, ClientToolCall) (ToolResult, error)
	Ping(context.Context) error
	Close() error
}

type ToolDescriptor struct {
	Name        string
	Description string
	InputSchema map[string]any
	Annotations map[string]any
}

type ClientToolCall struct {
	Name      string
	Arguments map[string]any
}

type ContentKind string

const (
	ContentText         ContentKind = "text"
	ContentImage        ContentKind = "image"
	ContentAudio        ContentKind = "audio"
	ContentResource     ContentKind = "resource"
	ContentResourceLink ContentKind = "resource_link"
	ContentStructured   ContentKind = "structured"
)

type Content struct {
	Kind        ContentKind
	Text        string
	Data        []byte
	MIMEType    string
	URI         string
	Name        string
	Structured  any
	Annotations map[string]any
}

type ToolResult struct {
	Content           []Content
	StructuredContent any
	IsError           bool
}

type ToolCall struct {
	Scope      Scope
	Generation Generation
	ServerID   ServerID
	CallID     string
	Name       string
	Arguments  map[string]any
}

type ToolDefinition struct {
	ServerID    ServerID
	Name        string
	Canonical   string
	Description string
	InputSchema map[string]any
	Annotations map[string]any
}

func (d ToolDefinition) AgentDefinition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: d.Canonical, Description: d.Description, Parameters: cloneMap(d.InputSchema)}
}

type ServerSnapshot struct {
	ID                   ServerID
	ConnectionGeneration Generation
	Initialize           InitializeResult
	Tools                []ToolDefinition
}

type Diagnostic struct {
	ServerID ServerID
	Message  string
}

type Snapshot struct {
	TenantKey        agent.TenantKey
	Generation       Generation
	ConfigGeneration string
	ConfigDigest     string
	Servers          []ServerSnapshot
	Diagnostics      []Diagnostic
}

type Status struct {
	State       State
	Generation  Generation
	Ready       bool
	Degraded    bool
	Diagnostics []Diagnostic
}

type CloseReport struct {
	Closed     []ServerID
	Incomplete []ServerID
}

type GenerationLease interface {
	Generation() Generation
	Snapshot() Snapshot
	CallTool(context.Context, ToolCall) (ToolResult, error)
	Close() error
}

type Manager interface {
	StartScope(context.Context, Scope) error
	CloseScope(context.Context, Scope) (CloseReport, error)
	WaitReady(context.Context, Scope) (Status, error)
	Snapshot(Scope) (Snapshot, bool)
	AcquireGeneration(context.Context, Scope, Generation) (GenerationLease, error)
	Reload(context.Context, Scope) (Snapshot, error)
	Refresh(context.Context, Scope) (Snapshot, error)
	CallTool(context.Context, ToolCall) (ToolResult, error)
	Status(Scope) Status
	Close(context.Context) (CloseReport, error)
}
