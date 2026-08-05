package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

type HTTPPolicy struct {
	AllowInsecureLoopback bool
	AllowPrivateNetworks  bool
	AllowRedirects        bool
	AllowedHosts          []string
	AllowedHeaders        []string
	MaxRedirects          int
	MaxResponseBytes      int64
	Resolver              *net.Resolver
}

type streamableHTTPConnector struct {
	client  *http.Client
	policy  HTTPPolicy
	secrets SecretProvider
}

func NewStreamableHTTPConnector(client *http.Client, policy HTTPPolicy, secrets SecretProvider) (Connector, error) {
	if client == nil {
		return nil, mcpError(ErrInvalidConfig, nil, "new HTTP connector", "", "", "HTTP client is required")
	}
	if policy.MaxRedirects <= 0 {
		policy.MaxRedirects = 5
	}
	if policy.MaxResponseBytes <= 0 {
		policy.MaxResponseBytes = defaultMaxResultBytes
	}
	policy.AllowedHosts = append([]string(nil), policy.AllowedHosts...)
	policy.AllowedHeaders = append([]string(nil), policy.AllowedHeaders...)
	if policy.Resolver == nil {
		policy.Resolver = net.DefaultResolver
	}
	return &streamableHTTPConnector{client: client, policy: policy, secrets: secrets}, nil
}

func (c *streamableHTTPConnector) Connect(ctx context.Context, request ConnectRequest) (Client, error) {
	if request.Config.Transport.Kind != TransportStreamableHTTP || request.Config.Transport.HTTP == nil {
		return nil, mcpError(ErrTransportDenied, nil, "connect http", request.Config.ID, "", "transport is not Streamable HTTP")
	}
	config := request.Config.Transport.HTTP
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil {
		return nil, mcpError(ErrTransportDenied, err, "connect http", request.Config.ID, "", "invalid endpoint")
	}
	if err := validateHTTPDestination(ctx, endpoint, c.policy); err != nil {
		return nil, mcpError(ErrTransportDenied, err, "connect http", request.Config.ID, "", "endpoint denied")
	}
	headers := make(http.Header, len(config.HeaderRefs)+2)
	if config.AuthRef != "" {
		if c.secrets == nil {
			return nil, mcpError(ErrTransportDenied, nil, "connect http", request.Config.ID, "", "secret provider is required")
		}
		value, err := c.secrets.Resolve(ctx, request.Scope.TenantKey, config.AuthRef)
		if err != nil || invalidHeaderValue(value) {
			return nil, mcpError(ErrTransportDenied, err, "connect http", request.Config.ID, "", "auth secret resolution failed")
		}
		headers.Set("Authorization", value)
	}
	for _, ref := range config.HeaderRefs {
		if !headerAllowed(ref.Name, c.policy.AllowedHeaders) {
			return nil, mcpError(ErrTransportDenied, nil, "connect http", request.Config.ID, "", "header name is not approved")
		}
		if c.secrets == nil {
			return nil, mcpError(ErrTransportDenied, nil, "connect http", request.Config.ID, "", "secret provider is required")
		}
		value, err := c.secrets.Resolve(ctx, request.Scope.TenantKey, ref.SecretRef)
		if err != nil || invalidHeaderValue(value) {
			return nil, mcpError(ErrTransportDenied, err, "connect http", request.Config.ID, "", "header secret resolution failed")
		}
		headers.Set(ref.Name, value)
	}
	transport := &streamableHTTPTransport{client: c.client, endpoint: endpoint, headers: headers, policy: c.policy}
	return NewProtocolClient(transport)
}

type streamableHTTPTransport struct {
	mu       sync.Mutex
	client   *http.Client
	endpoint *url.URL
	headers  http.Header
	policy   HTTPPolicy
	nextID   uint64
	closed   bool
}

func (t *streamableHTTPTransport) Send(ctx context.Context, request Request) (Response, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return Response{}, ErrClosed
	}
	t.nextID++
	id := t.nextID
	t.mu.Unlock()
	payload, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      uint64 `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{JSONRPC: "2.0", ID: id, Method: request.Method, Params: request.Params})
	if err != nil {
		return Response{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return Response{}, mcpError(ErrUpstream, err, "http request", "", "", "request construction failed")
	}
	httpRequest.Header = t.headers.Clone()
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json, text/event-stream")
	client := *t.client
	baseRedirect := client.CheckRedirect
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if !t.policy.AllowRedirects || len(via) > t.policy.MaxRedirects {
			return http.ErrUseLastResponse
		}
		if err := validateHTTPDestination(next.Context(), next.URL, t.policy); err != nil {
			return err
		}
		if len(via) > 0 && !sameOrigin(via[0].URL, next.URL) {
			next.Header.Del("Authorization")
			for name := range t.headers {
				if name != "Content-Type" && name != "Accept" {
					next.Header.Del(name)
				}
			}
		}
		if baseRedirect != nil {
			return baseRedirect(next, via)
		}
		return nil
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		return Response{}, mcpError(ErrUpstream, err, "http send", "", "", "request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Response{}, &UpstreamError{Code: response.StatusCode, Message: http.StatusText(response.StatusCode), Retryable: response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500}
	}
	limited := io.LimitReader(response.Body, t.policy.MaxResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return Response{}, mcpError(ErrUpstream, err, "http receive", "", "", "response read failed")
	}
	if int64(len(body)) > t.policy.MaxResponseBytes {
		return Response{}, ErrResultTooLarge
	}
	var envelope struct {
		ID     uint64         `json:"id"`
		Result any            `json:"result"`
		Error  *UpstreamError `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.ID != id {
		return Response{}, mcpError(ErrUpstream, err, "http receive", "", "", "invalid response envelope")
	}
	result, err := json.Marshal(envelope.Result)
	if err != nil {
		return Response{}, mcpError(ErrUpstream, err, "http receive", "", "", "invalid response result")
	}
	return Response{Result: result, Error: envelope.Error}, nil
}

func (t *streamableHTTPTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return nil
}

func validateHTTPDestination(ctx context.Context, endpoint *url.URL, policy HTTPPolicy) error {
	if endpoint == nil || endpoint.Hostname() == "" || endpoint.User != nil {
		return errors.New("invalid endpoint")
	}
	host := strings.ToLower(strings.TrimSuffix(endpoint.Hostname(), "."))
	if endpoint.Scheme != "https" {
		if endpoint.Scheme != "http" || !policy.AllowInsecureLoopback || !isLoopbackHost(host) {
			return errors.New("HTTPS is required")
		}
	}
	if len(policy.AllowedHosts) > 0 && !hostAllowed(host, endpoint.Port(), policy.AllowedHosts) {
		return errors.New("host is not allowed")
	}
	addresses, err := policy.Resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve host: %w", err)
	}
	if len(addresses) == 0 {
		return errors.New("host has no addresses")
	}
	for _, address := range addresses {
		insecureLoopback := endpoint.Scheme == "http" && policy.AllowInsecureLoopback && address.IP.IsLoopback()
		if !policy.AllowPrivateNetworks && !insecureLoopback && unsafeIP(address.IP) {
			return errors.New("host resolves to a non-public address")
		}
		if endpoint.Scheme == "http" && !address.IP.IsLoopback() {
			return errors.New("insecure endpoint did not resolve to loopback")
		}
	}
	return nil
}

func unsafeIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(net.IPv4bcast)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func hostAllowed(host, port string, allowed []string) bool {
	for _, entry := range allowed {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == host || port != "" && entry == net.JoinHostPort(host, port) {
			return true
		}
		if strings.HasPrefix(entry, "*.") && strings.HasSuffix(host, entry[1:]) && host != entry[2:] {
			return true
		}
	}
	return false
}

func headerAllowed(name string, allowed []string) bool {
	for _, entry := range allowed {
		if strings.EqualFold(strings.TrimSpace(entry), name) {
			return true
		}
	}
	return false
}

func sameOrigin(first, second *url.URL) bool {
	return strings.EqualFold(first.Scheme, second.Scheme) && strings.EqualFold(first.Hostname(), second.Hostname()) && effectivePort(first) == effectivePort(second)
}

func effectivePort(value *url.URL) string {
	if value.Port() != "" {
		return value.Port()
	}
	if value.Scheme == "https" {
		return "443"
	}
	if value.Scheme == "http" {
		return "80"
	}
	return strconv.Itoa(0)
}

func invalidHeaderValue(value string) bool {
	return value == "" || strings.ContainsAny(value, "\x00\r\n")
}

var _ Connector = (*streamableHTTPConnector)(nil)
var _ Transport = (*streamableHTTPTransport)(nil)
