package mcp

import (
	"context"
	"fmt"

	jsoncodec "github.com/iceymoss/agent-runtime-go/internal/jsoncodec"
)

type protocolClient struct {
	transport Transport
}

func NewProtocolClient(transport Transport) (Client, error) {
	if transport == nil {
		return nil, mcpError(ErrInvalidConfig, nil, "new protocol client", "", "", "transport is required")
	}
	return &protocolClient{transport: transport}, nil
}

func (c *protocolClient) Initialize(ctx context.Context) (InitializeResult, error) {
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Instructions string `json:"instructions"`
		Capabilities struct {
			Tools     *struct{} `json:"tools"`
			Prompts   *struct{} `json:"prompts"`
			Resources *struct{} `json:"resources"`
		} `json:"capabilities"`
	}
	err := c.invoke(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "agent-runtime-go", "version": "1"},
	}, &result)
	if err != nil {
		return InitializeResult{}, err
	}
	return InitializeResult{
		ProtocolVersion: result.ProtocolVersion,
		ServerName:      result.ServerInfo.Name,
		ServerVersion:   result.ServerInfo.Version,
		Instructions:    result.Instructions,
		Capabilities: Capabilities{
			Tools:     result.Capabilities.Tools != nil,
			Prompts:   result.Capabilities.Prompts != nil,
			Resources: result.Capabilities.Resources != nil,
		},
	}, nil
}

func (c *protocolClient) ListTools(ctx context.Context) ([]ToolDescriptor, error) {
	var result struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
			Annotations map[string]any `json:"annotations"`
		} `json:"tools"`
	}
	if err := c.invoke(ctx, "tools/list", map[string]any{}, &result); err != nil {
		return nil, err
	}
	tools := make([]ToolDescriptor, len(result.Tools))
	for i, item := range result.Tools {
		tools[i] = ToolDescriptor{Name: item.Name, Description: item.Description, InputSchema: cloneMap(item.InputSchema), Annotations: cloneMap(item.Annotations)}
	}
	return tools, nil
}

func (c *protocolClient) CallTool(ctx context.Context, call ClientToolCall) (ToolResult, error) {
	var result struct {
		Content []struct {
			Type        ContentKind    `json:"type"`
			Text        string         `json:"text"`
			Data        []byte         `json:"data"`
			MIMEType    string         `json:"mimeType"`
			URI         string         `json:"uri"`
			Name        string         `json:"name"`
			Structured  any            `json:"structuredContent"`
			Annotations map[string]any `json:"annotations"`
		} `json:"content"`
		StructuredContent any  `json:"structuredContent"`
		IsError           bool `json:"isError"`
	}
	if err := c.invoke(ctx, "tools/call", map[string]any{"name": call.Name, "arguments": cloneMap(call.Arguments)}, &result); err != nil {
		return ToolResult{}, err
	}
	converted := ToolResult{StructuredContent: cloneValue(result.StructuredContent), IsError: result.IsError, Content: make([]Content, len(result.Content))}
	for i, item := range result.Content {
		converted.Content[i] = Content{Kind: item.Type, Text: item.Text, Data: append([]byte(nil), item.Data...), MIMEType: item.MIMEType, URI: item.URI, Name: item.Name, Structured: cloneValue(item.Structured), Annotations: cloneMap(item.Annotations)}
	}
	return converted, nil
}

func (c *protocolClient) Ping(ctx context.Context) error {
	return c.invoke(ctx, "ping", map[string]any{}, &struct{}{})
}

func (c *protocolClient) Close() error { return c.transport.Close() }

func (c *protocolClient) invoke(ctx context.Context, method string, params any, target any) error {
	response, err := c.transport.Send(ctx, Request{Method: method, Params: params})
	if err != nil {
		return err
	}
	if response.Error != nil {
		return response.Error
	}
	if !jsoncodec.Valid(response.Result) {
		return fmt.Errorf("%w: invalid JSON result", ErrUpstream)
	}
	if err := jsoncodec.Unmarshal(response.Result, target); err != nil {
		return fmt.Errorf("%w: decode result: %w", ErrUpstream, err)
	}
	return nil
}

var _ Client = (*protocolClient)(nil)
