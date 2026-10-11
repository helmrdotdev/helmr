package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/ids"
)

func (c *Client) RetrieveSession(
	ctx context.Context,
	sessionID string,
	opts EnvironmentScopeOptions,
) (api.AgentSession, error) {
	if err := ids.Validate(sessionID); err != nil {
		return api.AgentSession{}, err
	}
	path, err := c.environmentScopedPath(
		opts.ProjectID,
		opts.EnvironmentID,
		"/sessions/"+url.PathEscape(sessionID),
	)
	if err != nil {
		return api.AgentSession{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.AgentSession{}, err
	}
	var response api.AgentSession
	if err := c.doJSON(req, &response); err != nil {
		return api.AgentSession{}, err
	}
	return response, nil
}

type SessionListOptions struct {
	Statuses           []string
	Cursor             string
	Limit              int32
	AgentID            string
	Key                *string
	ParentSessionID    string
	RequesterSessionID string
	EnvironmentScopeOptions
}

func (c *Client) ListSessions(ctx context.Context, opts SessionListOptions) (api.AgentSessionsPage, error) {
	if opts.Key != nil && (opts.AgentID == "" || *opts.Key == "") {
		return api.AgentSessionsPage{}, errors.New("a nonempty key requires an Agent ID")
	}
	for _, status := range opts.Statuses {
		if status != "open" && status != "closing" && status != "closed" && status != "cancelled" {
			return api.AgentSessionsPage{}, errors.New("invalid Session status")
		}
	}
	for _, id := range []string{opts.AgentID, opts.ParentSessionID, opts.RequesterSessionID, opts.Cursor} {
		if id != "" {
			if err := ids.Validate(id); err != nil {
				return api.AgentSessionsPage{}, err
			}
		}
	}
	if opts.Limit < 0 || opts.Limit > 100 {
		return api.AgentSessionsPage{}, errors.New("session list limit must be in [1,100] when present")
	}
	path, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/sessions")
	if err != nil {
		return api.AgentSessionsPage{}, err
	}
	values := url.Values{}
	for _, status := range opts.Statuses {
		values.Add("status", status)
	}
	if opts.Cursor != "" {
		values.Set("cursor", opts.Cursor)
	}
	if opts.Limit > 0 {
		values.Set("limit", strconv.FormatInt(int64(opts.Limit), 10))
	}
	for name, value := range map[string]string{"agent_id": opts.AgentID, "parent_session_id": opts.ParentSessionID, "requester_session_id": opts.RequesterSessionID} {
		if value != "" {
			values.Set(name, value)
		}
	}
	if opts.Key != nil {
		values.Set("key", *opts.Key)
	}
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.AgentSessionsPage{}, err
	}
	var response api.AgentSessionsPage
	if err := c.doJSON(req, &response); err != nil {
		return api.AgentSessionsPage{}, err
	}
	if response.Sessions == nil {
		response.Sessions = []api.AgentSession{}
	}
	return response, nil
}

func (c *Client) SendSession(ctx context.Context, sessionID string, input api.SessionDataRequest, opts EnvironmentScopeOptions) (api.SessionAdmissionReceipt, error) {
	if err := api.ValidateSessionDataRequest(input); err != nil {
		return api.SessionAdmissionReceipt{}, err
	}
	path, err := c.sessionPath(sessionID, "/send", opts)
	if err != nil {
		return api.SessionAdmissionReceipt{}, err
	}
	input.IdempotencyKey = invocationKey(input.IdempotencyKey)
	var response api.SessionAdmissionReceipt
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.SessionAdmissionReceipt{}, err
	}
	return response, nil
}

func (c *Client) EnqueueSession(ctx context.Context, sessionID string, input api.EnqueueSessionRequest, opts EnvironmentScopeOptions) (api.TurnAdmission, error) {
	path, err := c.sessionPath(sessionID, "/enqueue", opts)
	if err != nil {
		return api.TurnAdmission{}, err
	}
	input.IdempotencyKey = invocationKey(input.IdempotencyKey)
	var response api.TurnAdmission
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.TurnAdmission{}, err
	}
	return response, nil
}

func (c *Client) SendSessionMessage(ctx context.Context, sessionID string, turnID string, input api.SessionDataRequest, opts EnvironmentScopeOptions) (api.SessionMessageReceipt, error) {
	if err := api.ValidateSessionDataRequest(input); err != nil {
		return api.SessionMessageReceipt{}, err
	}
	if err := ids.Validate(turnID); err != nil {
		return api.SessionMessageReceipt{}, err
	}
	path, err := c.sessionPath(sessionID, "/turns/"+url.PathEscape(turnID)+"/messages", opts)
	if err != nil {
		return api.SessionMessageReceipt{}, err
	}
	input.IdempotencyKey = invocationKey(input.IdempotencyKey)
	var response api.SessionMessageReceipt
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.SessionMessageReceipt{}, err
	}
	return response, nil
}

func (c *Client) CloseSession(ctx context.Context, sessionID string, input api.CloseSessionRequest, opts EnvironmentScopeOptions) (api.SessionCloseReceipt, error) {
	path, err := c.sessionPath(sessionID, "/close", opts)
	if err != nil {
		return api.SessionCloseReceipt{}, err
	}
	input.IdempotencyKey = invocationKey(input.IdempotencyKey)
	var response api.SessionCloseReceipt
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.SessionCloseReceipt{}, err
	}
	return response, nil
}

func (c *Client) CancelSession(ctx context.Context, sessionID string, input api.CancelSessionRequest, opts EnvironmentScopeOptions) (api.SessionCancelReceipt, error) {
	path, err := c.sessionPath(sessionID, "/cancel", opts)
	if err != nil {
		return api.SessionCancelReceipt{}, err
	}
	input.IdempotencyKey = invocationKey(input.IdempotencyKey)
	var response api.SessionCancelReceipt
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.SessionCancelReceipt{}, err
	}
	return response, nil
}

func (c *Client) ResumeSession(ctx context.Context, sessionID string, input api.ResumeSessionRequest, opts EnvironmentScopeOptions) (api.SessionResumeReceipt, error) {
	if err := ids.Validate(input.HoldID); err != nil {
		return api.SessionResumeReceipt{}, err
	}
	path, err := c.sessionPath(sessionID, "/resume", opts)
	if err != nil {
		return api.SessionResumeReceipt{}, err
	}
	input.IdempotencyKey = invocationKey(input.IdempotencyKey)
	var response api.SessionResumeReceipt
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.SessionResumeReceipt{}, err
	}
	return response, nil
}

func (c *Client) RetrieveSessionTurn(ctx context.Context, sessionID, turnID string, opts EnvironmentScopeOptions) (api.AgentTurn, error) {
	if err := ids.Validate(turnID); err != nil {
		return api.AgentTurn{}, err
	}
	path, err := c.sessionPath(sessionID, "/turns/"+url.PathEscape(turnID), opts)
	if err != nil {
		return api.AgentTurn{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.AgentTurn{}, err
	}
	var response api.AgentTurn
	if err := c.doJSON(req, &response); err != nil {
		return api.AgentTurn{}, err
	}
	return response, nil
}

// SessionEventReadOptions reads the retained timeline strictly after After.
// Zero values select after=0 and the default page size of 100.
type SessionEventReadOptions struct {
	After int64
	Limit int32
	EnvironmentScopeOptions
}

func (c *Client) ReadSessionEvents(ctx context.Context, sessionID string, opts SessionEventReadOptions) (api.AgentSessionEventPage, error) {
	if opts.After < 0 || opts.After > 1<<53-1 {
		return api.AgentSessionEventPage{}, fmt.Errorf("session events after must be in [0,%d]", int64(1<<53-1))
	}
	if opts.Limit < 0 || opts.Limit > 1000 {
		return api.AgentSessionEventPage{}, errors.New("session events limit must be in [1,1000] when present")
	}
	if opts.Limit == 0 {
		opts.Limit = 100
	}
	path, err := c.sessionPath(sessionID, "/events", opts.EnvironmentScopeOptions)
	if err != nil {
		return api.AgentSessionEventPage{}, err
	}
	values := url.Values{}
	values.Set("after", strconv.FormatInt(opts.After, 10))
	values.Set("limit", strconv.FormatInt(int64(opts.Limit), 10))
	req, err := c.newRequest(ctx, http.MethodGet, path+"?"+values.Encode(), nil)
	if err != nil {
		return api.AgentSessionEventPage{}, err
	}
	var response api.AgentSessionEventPage
	if err := c.doJSON(req, &response); err != nil {
		return api.AgentSessionEventPage{}, err
	}
	if response.Records == nil {
		response.Records = []api.AgentSessionEvent{}
	}
	return response, nil
}

func (c *Client) sessionPath(sessionID, suffix string, opts EnvironmentScopeOptions) (string, error) {
	if err := ids.Validate(sessionID); err != nil {
		return "", err
	}
	return c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/sessions/"+url.PathEscape(sessionID)+suffix)
}

// SessionTurnListOptions reads one finite page in Turn admission order.
type SessionTurnListOptions struct {
	Cursor string
	Limit  int32
	EnvironmentScopeOptions
}

func (c *Client) ListSessionTurns(ctx context.Context, sessionID string, opts SessionTurnListOptions) (api.AgentTurnsPage, error) {
	result := api.AgentTurnsPage{Turns: []api.AgentTurn{}}
	if opts.Limit < 0 || opts.Limit > 100 {
		return result, errors.New("turn list limit must be in [1,100] when present")
	}
	if opts.Cursor != "" {
		n, err := strconv.ParseInt(opts.Cursor, 10, 64)
		if err != nil || n < 0 {
			return result, errors.New("turn cursor must be a nonnegative sequence")
		}
	}
	path, err := c.sessionPath(sessionID, "/turns", opts.EnvironmentScopeOptions)
	if err != nil {
		return result, err
	}
	query := url.Values{}
	if opts.Cursor != "" {
		query.Set("cursor", opts.Cursor)
	}
	if opts.Limit > 0 {
		query.Set("limit", strconv.Itoa(int(opts.Limit)))
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return result, err
	}
	err = c.doJSON(req, &result)
	return result, err
}

func (c *Client) InterruptSession(ctx context.Context, sessionID string, input api.InterruptSessionRequest, opts EnvironmentScopeOptions) (api.SessionInterruptReceipt, error) {
	var result api.SessionInterruptReceipt
	path, err := c.sessionPath(sessionID, "/interrupt", opts)
	if err != nil {
		return result, err
	}
	input.IdempotencyKey = invocationKey(input.IdempotencyKey)
	err = c.postJSON(ctx, path, input, &result)
	return result, err
}
