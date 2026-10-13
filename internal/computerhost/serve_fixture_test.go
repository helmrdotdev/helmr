package computerhost

import (
	"context"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
)

// Test entry points that serve without an activation channel.

func serveAgentSessionForTest(ctx context.Context, connection *AgentSessionConnection, environmentID string, client AgentRuntimeClient, observe func(context.Context, *agentv1.GuestSessionMessage) error) error {
	return ServeAgentSession(ctx, connection, environmentID, client, observe, nil)
}

func serveControlledAgentSessionForTest(ctx context.Context, connection *AgentSessionConnection, attached *agentv1.SessionAttached, environment string, client AgentControlClient, observe func(context.Context, *agentv1.GuestSessionMessage) error) error {
	return ServeControlledAgentSession(ctx, connection, attached, environment, client, observe, nil)
}
