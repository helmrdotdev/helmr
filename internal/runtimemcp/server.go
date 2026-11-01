// Package runtimemcp exposes the model-facing collaboration catalog. Callers
// supply an already authenticated Session transport; arguments never identify
// the caller or grant management authority.
package runtimemcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/helmrdotdev/helmr/internal/content"
	"github.com/helmrdotdev/helmr/internal/mcppolicy"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Invoke func(context.Context, string, json.RawMessage) (json.RawMessage, error)

type listInput struct {
	Cursor string `json:"cursor,omitempty"`
}
type textPart struct {
	Type string `json:"type" jsonschema:"Content kind; must be text."`
	Text string `json:"text"`
}
type agentsInput struct {
	Operation string `json:"operation" jsonschema:"Creation operation: spawn uses the pinned Deployment; start uses the current Deployment."`
	Cursor    string `json:"cursor,omitempty"`
}
type createInput struct {
	AgentID        string `json:"agent"`
	Input          any    `json:"input" jsonschema:"Finite JSON matching the target agent input contract; use agents discovery for its pinned schema."`
	ComputerID     string `json:"computerId,omitempty"`
	IdempotencyKey string `json:"idempotencyKey" jsonschema:"Reuse this key with identical arguments when the outcome is uncertain."`
}
type enqueueInput struct {
	SessionID      string `json:"sessionId"`
	Input          any    `json:"input" jsonschema:"Finite JSON matching this Session agent input contract."`
	IdempotencyKey string `json:"idempotencyKey" jsonschema:"Reuse this key with identical arguments when the outcome is uncertain."`
}
type messageInput struct {
	SessionID      string     `json:"sessionId"`
	TurnID         string     `json:"turnId"`
	Message        []textPart `json:"message" jsonschema:"Ordered text parts delivered to this exact active Turn; never enqueued as fallback."`
	IdempotencyKey string     `json:"idempotencyKey"`
}
type turnControlInput struct {
	SessionID      string `json:"sessionId"`
	TurnID         string `json:"turnId"`
	IdempotencyKey string `json:"idempotencyKey"`
}
type controlInput struct {
	SessionID      string `json:"sessionId"`
	IdempotencyKey string `json:"idempotencyKey"`
}
type resumeInput struct {
	SessionID      string `json:"sessionId"`
	HoldID         string `json:"holdId"`
	IdempotencyKey string `json:"idempotencyKey"`
}
type sessionInput struct {
	SessionID string `json:"sessionId"`
}
type sessionsInput struct {
	Relation string   `json:"relation" jsonschema:"owned lists direct children; requested also includes independent work started by this Session."`
	Cursor   string   `json:"cursor,omitempty"`
	Status   []string `json:"status,omitempty" jsonschema:"Optional lifecycle filter: open, closing, closed. Omit to include all states."`
	Limit    *int     `json:"limit,omitempty" jsonschema:"Page size from 1 to 100; defaults to 50."`
}
type turnInput struct {
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId"`
}
type waitInput struct {
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId"`
	TimeoutMS int    `json:"timeoutMs" jsonschema:"Bounded observation timeout in milliseconds, from 1 to 30000. Timing out does not cancel the Turn."`
}

// The transport does not own Helmr Session lifetime. In particular HTTP DELETE,
// idle clients and MCP connection loss never close or cancel a Helmr Session.
func NewHandler(invoke Invoke) http.Handler {
	return NewScopedHandler(invoke, mcppolicy.Policy{Kind: "unrestricted"})
}
func NewScopedHandler(invoke Invoke, policy mcppolicy.Policy) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "helmr-runtime", Version: "1"}, &mcp.ServerOptions{
		Instructions: scopedInstructions(policy),
		// This catalog is fixed for the lifetime of the Session process. Advertising
		// changes makes clients retain notification streams for work that never occurs.
		//lint:ignore SA1019 Preserve the SDK default logging capability during its supported deprecation window.
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}, Logging: &mcp.LoggingCapabilities{}},
	})
	addScopedSkills(server, policy)
	addScoped[agentsInput](server, policy, invoke, "list_agents", "List Agent targets for spawn or start. Only root Sessions can create work.")
	addScoped[listInput](server, policy, invoke, "list_computers", "List Computers visible to this Session.")
	addScoped[createInput](server, policy, invoke, "spawn", "Create owned child work and return its Session and Turn admission receipt.")
	addScoped[createInput](server, policy, invoke, "start", "Create independent work and return its Session and Turn admission receipt.")
	addScoped[enqueueInput](server, policy, invoke, "enqueue", "Admit another Turn in an exact Session in this Environment. Existing holds remain in force.")
	addScoped[messageInput](server, policy, invoke, "send_turn", "Send text to an owned descendant's exact active Turn. A receipt confirms admission, not native delivery.")
	addScoped[turnControlInput](server, policy, invoke, "interrupt_turn", "Interrupt an owned descendant's exact running Turn without holding its Session or stopping its children. Acceptance does not prove cessation; a non-running Turn returns turn_not_interruptible with its status and reason.")
	addScoped[controlInput](server, policy, invoke, "interrupt_session", "Request interruption of an owned descendant and its subtree. A receipt is not proof that execution has stopped.")
	addScoped[resumeInput](server, policy, invoke, "resume_session", "Release an exact hold on an owned descendant, posted to the hold's session_id. Of interrupts, only those this Session placed can be released; storage pressure holds cannot. This does not replay input.")
	addScoped[controlInput](server, policy, invoke, "close_session", "Close admission to an owned descendant while draining work that can run; held queued work and pending questions are cancelled.")
	addScoped[controlInput](server, policy, invoke, "cancel_session", "Cancel pending work, request cessation of active work and close an owned descendant's subtree.")
	addScoped[sessionInput](server, policy, invoke, "inspect_session", "Inspect an accessible Session without expanding history access.")
	addScoped[sessionsInput](server, policy, invoke, "list_sessions", "Recover owned or requested Sessions and their initial Turn identities, including terminal work.")
	addScoped[turnInput](server, policy, invoke, "inspect_turn", "Inspect an exact accessible Turn.")
	addScoped[waitInput](server, policy, func(ctx context.Context, _ string, body json.RawMessage) (json.RawMessage, error) {
		var input waitInput
		if err := json.Unmarshal(body, &input); err != nil {
			return nil, err
		}
		return waitForTurn(ctx, invoke, input)
	}, "wait_turn", "Wait for an exact Turn outcome for a bounded time. This observation is repeatable.")
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 1024 * 1024, PropagateRequestCancellation: true})
}
func addScoped[I any](server *mcp.Server, policy mcppolicy.Policy, invoke Invoke, name, description string) {
	operation := name
	switch operation {
	case "wait_turn":
		operation = "inspect_turn"
	case "list_sessions":
		operation = "inspect_session"
	case "list_agents":
		if !policy.Has("start") && !policy.Has("spawn") {
			return
		}
		operation = ""
	}
	if operation != "" && !policy.Has(operation) {
		return
	}
	schema, err := jsonschema.For[I](nil)
	if err != nil {
		panic(err)
	}
	for _, field := range []string{"message"} {
		if property := schema.Properties[field]; property != nil {
			property.Type, property.Types = "array", nil
			property.MaxItems = new(content.ContentParts)
			property.Items.Properties["type"].Enum = []any{"text"}
		}
	}
	if operation := schema.Properties["operation"]; operation != nil {
		operation.Enum = []any{"spawn", "start"}
	}
	mcp.AddTool(server, &mcp.Tool{Name: name, Description: description, InputSchema: schema}, func(ctx context.Context, _ *mcp.CallToolRequest, input I) (*mcp.CallToolResult, any, error) {
		if err := validate(input); err != nil {
			return nil, nil, err
		}
		body, err := json.Marshal(input)
		if err != nil {
			return nil, nil, err
		}
		result, err := invoke(ctx, name, body)
		if err != nil {
			return nil, nil, err
		}
		var value any
		if err := json.Unmarshal(result, &value); err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(result)}}}, nil, nil
	})
}

func validate(input any) error {
	switch value := input.(type) {
	case agentsInput:
		if value.Operation != "spawn" && value.Operation != "start" {
			return errors.New("operation must be spawn or start")
		}
	case sessionsInput:
		if value.Relation != "owned" && value.Relation != "requested" {
			return errors.New("relation must be owned or requested")
		}
		if value.Limit != nil && (*value.Limit < 1 || *value.Limit > 100) {
			return errors.New("limit must be from 1 to 100")
		}
	case createInput:
		raw, err := json.Marshal(value.Input)
		if err != nil {
			return err
		}
		if _, err := content.Invocation(raw); err != nil {
			return err
		}
		if value.AgentID == "" || value.IdempotencyKey == "" {
			return errors.New("agent and idempotencyKey are required")
		}
	case enqueueInput:
		raw, err := json.Marshal(value.Input)
		if err != nil {
			return err
		}
		if _, err := content.Invocation(raw); err != nil {
			return err
		}
		if value.SessionID == "" || value.IdempotencyKey == "" {
			return errors.New("sessionId and idempotencyKey are required")
		}
	case messageInput:
		if value.SessionID == "" || value.TurnID == "" || value.IdempotencyKey == "" {
			return errors.New("sessionId, turnId and idempotencyKey are required")
		}
		raw, err := json.Marshal(value.Message)
		if err != nil {
			return err
		}
		if _, err := content.Input(raw); err != nil {
			return err
		}
	case controlInput:
		if value.SessionID == "" || value.IdempotencyKey == "" {
			return errors.New("sessionId and idempotencyKey are required")
		}
	case resumeInput:
		if value.SessionID == "" || value.HoldID == "" || value.IdempotencyKey == "" {
			return errors.New("sessionId, holdId and idempotencyKey are required")
		}
	case sessionInput:
		if value.SessionID == "" {
			return errors.New("sessionId is required")
		}
	case turnInput:
		if value.SessionID == "" || value.TurnID == "" {
			return errors.New("sessionId and turnId are required")
		}
	case waitInput:
		if value.SessionID == "" || value.TurnID == "" || value.TimeoutMS < 1 || value.TimeoutMS > 30000 {
			return errors.New("sessionId, turnId and timeoutMs from 1 to 30000 are required")
		}
	}
	return nil
}
