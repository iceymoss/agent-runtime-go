package mcp

import "context"

type routingConnector struct {
	connectors map[TransportKind]Connector
}

func NewRoutingConnector(stdio Connector, streamableHTTP Connector) (Connector, error) {
	if stdio == nil || streamableHTTP == nil {
		return nil, mcpError(ErrInvalidConfig, nil, "new routing connector", "", "", "both transport connectors are required")
	}
	return &routingConnector{connectors: map[TransportKind]Connector{
		TransportStdio:          stdio,
		TransportStreamableHTTP: streamableHTTP,
	}}, nil
}

func (c *routingConnector) Connect(ctx context.Context, request ConnectRequest) (Client, error) {
	connector := c.connectors[request.Config.Transport.Kind]
	if connector == nil {
		return nil, mcpError(ErrTransportDenied, nil, "connect", request.Config.ID, "", "unsupported transport")
	}
	return connector.Connect(ctx, request)
}

var _ Connector = (*routingConnector)(nil)
