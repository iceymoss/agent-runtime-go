package mcp

import "github.com/iceymoss/agent-runtime-go"

func cloneConfigSnapshot(value ConfigSnapshot) ConfigSnapshot {
	cloned := ConfigSnapshot{Generation: value.Generation, Servers: make([]ServerConfig, len(value.Servers))}
	for i := range value.Servers {
		cloned.Servers[i] = cloneServerConfig(value.Servers[i])
	}
	return cloned
}

func cloneServerConfig(value ServerConfig) ServerConfig {
	value.DisabledTools = append([]string(nil), value.DisabledTools...)
	if value.Transport.Stdio != nil {
		stdio := *value.Transport.Stdio
		stdio.Args = append([]string(nil), stdio.Args...)
		stdio.EnvRefs = append([]EnvRef(nil), stdio.EnvRefs...)
		value.Transport.Stdio = &stdio
	}
	if value.Transport.HTTP != nil {
		httpConfig := *value.Transport.HTTP
		httpConfig.HeaderRefs = append([]HeaderRef(nil), httpConfig.HeaderRefs...)
		value.Transport.HTTP = &httpConfig
	}
	return value
}

func cloneSnapshot(value Snapshot) Snapshot {
	servers := value.Servers
	value.Servers = make([]ServerSnapshot, len(servers))
	for i := range servers {
		value.Servers[i] = cloneServerSnapshot(servers[i])
	}
	value.Diagnostics = append([]Diagnostic(nil), value.Diagnostics...)
	return value
}

func cloneServerSnapshot(value ServerSnapshot) ServerSnapshot {
	tools := value.Tools
	value.Tools = make([]ToolDefinition, len(tools))
	for i := range tools {
		value.Tools[i] = cloneToolDefinition(tools[i])
	}
	return value
}

func cloneToolDefinition(value ToolDefinition) ToolDefinition {
	value.InputSchema = cloneMap(value.InputSchema)
	value.Annotations = cloneMap(value.Annotations)
	return value
}

func cloneToolResult(value ToolResult) ToolResult {
	value.StructuredContent = cloneValue(value.StructuredContent)
	contents := value.Content
	value.Content = make([]Content, len(contents))
	for i, content := range contents {
		content.Data = append([]byte(nil), content.Data...)
		content.Structured = cloneValue(content.Structured)
		content.Annotations = cloneMap(content.Annotations)
		value.Content[i] = content
	}
	return value
}

func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	cloned := make(map[string]any, len(value))
	for key, item := range value {
		cloned[key] = cloneValue(item)
	}
	return cloned
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		cloned := make([]any, len(typed))
		for i, item := range typed {
			cloned[i] = cloneValue(item)
		}
		return cloned
	case []byte:
		return append([]byte(nil), typed...)
	case []string:
		return append([]string(nil), typed...)
	case agent.TenantKey:
		return typed
	default:
		return typed
	}
}
