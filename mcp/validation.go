package mcp

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

const (
	defaultConnectTimeout = 10 * time.Second
	defaultCallTimeout    = 30 * time.Second
	defaultMaxResultBytes = int64(4 << 20)
)

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	envNamePattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func validateScope(scope Scope) error {
	if !scope.TenantKey.Valid() {
		return mcpError(ErrInvalidConfig, nil, "validate scope", "", "", "tenant key is empty")
	}
	return nil
}

func normalizeConfigSnapshot(value ConfigSnapshot) (ConfigSnapshot, error) {
	value = cloneConfigSnapshot(value)
	seen := make(map[ServerID]struct{}, len(value.Servers))
	for i := range value.Servers {
		config := &value.Servers[i]
		if !identifierPattern.MatchString(string(config.ID)) {
			return ConfigSnapshot{}, mcpError(ErrInvalidConfig, nil, "validate config", config.ID, "", "invalid server id")
		}
		if _, exists := seen[config.ID]; exists {
			return ConfigSnapshot{}, mcpError(ErrInvalidConfig, nil, "validate config", config.ID, "", "duplicate server id")
		}
		seen[config.ID] = struct{}{}
		if config.ConnectTimeout <= 0 {
			config.ConnectTimeout = defaultConnectTimeout
		}
		if config.CallTimeout <= 0 {
			config.CallTimeout = defaultCallTimeout
		}
		if config.MaxResultBytes <= 0 {
			config.MaxResultBytes = defaultMaxResultBytes
		}
		if config.ConnectTimeout > 5*time.Minute || config.CallTimeout > 10*time.Minute {
			return ConfigSnapshot{}, mcpError(ErrInvalidConfig, nil, "validate config", config.ID, "", "timeout exceeds policy limit")
		}
		if err := validateTransport(config.ID, config.Transport); err != nil {
			return ConfigSnapshot{}, err
		}
		sort.Strings(config.DisabledTools)
	}
	sort.Slice(value.Servers, func(i, j int) bool { return value.Servers[i].ID < value.Servers[j].ID })
	return value, nil
}

func validateTransport(serverID ServerID, config TransportConfig) error {
	switch config.Kind {
	case TransportStdio:
		if config.Stdio == nil || config.HTTP != nil || !identifierPattern.MatchString(config.Stdio.ServerKey) {
			return mcpError(ErrInvalidConfig, nil, "validate stdio", serverID, "", "stdio requires one valid server key")
		}
		for _, arg := range config.Stdio.Args {
			if arg == "" || len(arg) > 4096 || strings.ContainsRune(arg, '\x00') || strings.ContainsAny(arg, "\r\n") {
				return mcpError(ErrInvalidConfig, nil, "validate stdio", serverID, "", "invalid argument")
			}
		}
		if len(config.Stdio.Args) > 128 {
			return mcpError(ErrInvalidConfig, nil, "validate stdio", serverID, "", "too many arguments")
		}
		seen := make(map[string]struct{}, len(config.Stdio.EnvRefs))
		for _, ref := range config.Stdio.EnvRefs {
			if !envNamePattern.MatchString(ref.Name) || !validSecretRef(ref.SecretRef) {
				return mcpError(ErrInvalidConfig, nil, "validate stdio", serverID, "", "invalid environment secret reference")
			}
			if _, exists := seen[ref.Name]; exists {
				return mcpError(ErrInvalidConfig, nil, "validate stdio", serverID, "", "duplicate environment name")
			}
			seen[ref.Name] = struct{}{}
		}
	case TransportStreamableHTTP:
		if config.HTTP == nil || config.Stdio != nil {
			return mcpError(ErrInvalidConfig, nil, "validate http", serverID, "", "http requires exactly one HTTP config")
		}
		parsed, err := url.Parse(config.HTTP.Endpoint)
		if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
			return mcpError(ErrInvalidConfig, err, "validate http", serverID, "", "invalid endpoint")
		}
		if config.HTTP.AuthRef != "" && !validSecretRef(config.HTTP.AuthRef) {
			return mcpError(ErrInvalidConfig, nil, "validate http", serverID, "", "invalid auth secret reference")
		}
		seen := make(map[string]struct{}, len(config.HTTP.HeaderRefs))
		for _, ref := range config.HTTP.HeaderRefs {
			name := strings.ToLower(strings.TrimSpace(ref.Name))
			if forbiddenHeader(name) || !validSecretRef(ref.SecretRef) {
				return mcpError(ErrTransportDenied, nil, "validate http", serverID, "", "forbidden header or invalid secret reference")
			}
			if _, exists := seen[name]; exists || (name == "authorization" && config.HTTP.AuthRef != "") {
				return mcpError(ErrInvalidConfig, nil, "validate http", serverID, "", "duplicate authorization or header")
			}
			seen[name] = struct{}{}
		}
	default:
		return mcpError(ErrTransportDenied, nil, "validate transport", serverID, "", "unsupported transport")
	}
	return nil
}

func validSecretRef(value string) bool {
	return value != "" && len(value) <= 512 && !strings.ContainsAny(value, "\x00\r\n")
}

func forbiddenHeader(name string) bool {
	switch name {
	case "", "host", "content-length", "connection", "proxy-connection", "keep-alive", "transfer-encoding", "te", "trailer", "upgrade":
		return true
	default:
		return strings.HasPrefix(name, "proxy-") || strings.HasPrefix(name, "sec-")
	}
}

func capabilityDefinitions(serverID ServerID, descriptors []ToolDescriptor, disabled []string) ([]ToolDefinition, error) {
	disabledSet := make(map[string]struct{}, len(disabled))
	for _, name := range disabled {
		disabledSet[name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(descriptors))
	definitions := make([]ToolDefinition, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if !identifierPattern.MatchString(descriptor.Name) {
			return nil, mcpError(ErrCapabilityInvalid, nil, "list tools", serverID, "", "invalid tool name")
		}
		if _, exists := seen[descriptor.Name]; exists {
			return nil, mcpError(ErrCapabilityInvalid, nil, "list tools", serverID, "", "duplicate tool name")
		}
		seen[descriptor.Name] = struct{}{}
		if _, excluded := disabledSet[descriptor.Name]; excluded {
			continue
		}
		if descriptor.InputSchema == nil {
			descriptor.InputSchema = map[string]any{"type": "object"}
		}
		if _, err := agent.CanonicalDigest(descriptor.InputSchema); err != nil {
			return nil, mcpError(ErrSchemaInvalid, err, "list tools", serverID, "", "input schema is not serializable")
		}
		canonical := fmt.Sprintf("mcp__%s__%s", sanitizeName(string(serverID)), sanitizeName(descriptor.Name))
		definitions = append(definitions, ToolDefinition{ServerID: serverID, Name: descriptor.Name, Canonical: canonical, Description: descriptor.Description, InputSchema: cloneMap(descriptor.InputSchema), Annotations: cloneMap(descriptor.Annotations)})
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Canonical < definitions[j].Canonical })
	return definitions, nil
}

func sanitizeName(value string) string {
	return strings.Map(func(char rune) rune {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' {
			return char
		}
		return '_'
	}, value)
}
