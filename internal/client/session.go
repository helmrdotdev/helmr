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

func (c *Client) StartActor(
	ctx context.Context,
	actorDeclaredID string,
	input api.StartActorRequest,
	opts EnvironmentScopeOptions,
) (api.StartActorResponse, error) {
	if err := api.ValidateActorDeclaredID(actorDeclaredID); err != nil {
		return api.StartActorResponse{}, err
	}
	if err := api.ValidateStartActorRequest(input); err != nil {
		return api.StartActorResponse{}, err
	}
	path, err := c.environmentScopedPath(
		opts.ProjectID,
		opts.EnvironmentID,
		"/actors/"+url.PathEscape(actorDeclaredID)+"/start",
	)
	if err != nil {
		return api.StartActorResponse{}, err
	}
	input.IdempotencyKey = invocationKey(input.IdempotencyKey)
	var response api.StartActorResponse
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.StartActorResponse{}, err
	}
	return response, nil
}

func (c *Client) RetrieveSession(
	ctx context.Context,
	sessionID string,
	opts EnvironmentScopeOptions,
) (api.Session, error) {
	if err := ids.Validate(sessionID); err != nil {
		return api.Session{}, err
	}
	path, err := c.environmentScopedPath(
		opts.ProjectID,
		opts.EnvironmentID,
		"/sessions/"+url.PathEscape(sessionID),
	)
	if err != nil {
		return api.Session{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.Session{}, err
	}
	var response api.Session
	if err := c.doJSON(req, &response); err != nil {
		return api.Session{}, err
	}
	return response, nil
}

type SessionListOptions struct {
	Statuses []string
	Cursor   string
	Limit    int32
	ActorID  string
	Key      string
	EnvironmentScopeOptions
}

func (c *Client) ListSessions(ctx context.Context, opts SessionListOptions) (api.ListSessionsResponse, error) {
	hasActorID := opts.ActorID != ""
	hasKey := opts.Key != ""
	if hasActorID != hasKey {
		return api.ListSessionsResponse{}, errors.New("actor ID and key must be provided together")
	}
	for _, status := range opts.Statuses {
		if err := api.ValidateSessionStatus(status); err != nil {
			return api.ListSessionsResponse{}, err
		}
	}
	if hasActorID {
		if opts.Cursor != "" || opts.Limit != 0 || len(opts.Statuses) != 0 {
			return api.ListSessionsResponse{}, errors.New("cursor, limit and status are not accepted with actor ID and key")
		}
		if err := api.ValidateActorDeclaredID(opts.ActorID); err != nil {
			return api.ListSessionsResponse{}, err
		}
		if err := api.ValidateActorKey(opts.Key); err != nil {
			return api.ListSessionsResponse{}, err
		}
	} else if opts.Limit < 0 || opts.Limit > 100 {
		return api.ListSessionsResponse{}, errors.New("session list limit must be in [1,100] when present")
	}
	path, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/sessions")
	if err != nil {
		return api.ListSessionsResponse{}, err
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
	if hasActorID {
		values.Set("actor_id", opts.ActorID)
		values.Set("key", opts.Key)
	}
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ListSessionsResponse{}, err
	}
	var response api.ListSessionsResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListSessionsResponse{}, err
	}
	if response.Sessions == nil {
		response.Sessions = []api.Session{}
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

func (c *Client) EnqueueSession(ctx context.Context, sessionID string, input api.SessionDataRequest, opts EnvironmentScopeOptions) (api.SessionAdmissionReceipt, error) {
	if err := api.ValidateSessionDataRequest(input); err != nil {
		return api.SessionAdmissionReceipt{}, err
	}
	path, err := c.sessionPath(sessionID, "/enqueue", opts)
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

func (c *Client) InterruptSessionTurn(ctx context.Context, sessionID string, turnID string, input api.InterruptTurnRequest, opts EnvironmentScopeOptions) (api.TurnInterruptReceipt, error) {
	if err := ids.Validate(turnID); err != nil {
		return api.TurnInterruptReceipt{}, err
	}
	path, err := c.sessionPath(sessionID, "/turns/"+url.PathEscape(turnID)+"/interrupt", opts)
	if err != nil {
		return api.TurnInterruptReceipt{}, err
	}
	input.IdempotencyKey = invocationKey(input.IdempotencyKey)
	var response api.TurnInterruptReceipt
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.TurnInterruptReceipt{}, err
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

func (c *Client) RetrieveSessionTurn(ctx context.Context, sessionID, turnID string, opts EnvironmentScopeOptions) (api.SessionTurn, error) {
	if err := ids.Validate(turnID); err != nil {
		return api.SessionTurn{}, err
	}
	path, err := c.sessionPath(sessionID, "/turns/"+url.PathEscape(turnID), opts)
	if err != nil {
		return api.SessionTurn{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.SessionTurn{}, err
	}
	var response api.SessionTurn
	if err := c.doJSON(req, &response); err != nil {
		return api.SessionTurn{}, err
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

func (c *Client) ReadSessionEvents(ctx context.Context, sessionID string, opts SessionEventReadOptions) (api.SessionEventPage, error) {
	if opts.After < 0 || opts.After > 1<<53-1 {
		return api.SessionEventPage{}, fmt.Errorf("session events after must be in [0,%d]", int64(1<<53-1))
	}
	if opts.Limit < 0 || opts.Limit > 1000 {
		return api.SessionEventPage{}, errors.New("session events limit must be in [1,1000] when present")
	}
	if opts.Limit == 0 {
		opts.Limit = 100
	}
	path, err := c.sessionPath(sessionID, "/events", opts.EnvironmentScopeOptions)
	if err != nil {
		return api.SessionEventPage{}, err
	}
	values := url.Values{}
	values.Set("after", strconv.FormatInt(opts.After, 10))
	values.Set("limit", strconv.FormatInt(int64(opts.Limit), 10))
	req, err := c.newRequest(ctx, http.MethodGet, path+"?"+values.Encode(), nil)
	if err != nil {
		return api.SessionEventPage{}, err
	}
	var response api.SessionEventPage
	if err := c.doJSON(req, &response); err != nil {
		return api.SessionEventPage{}, err
	}
	if response.Records == nil {
		response.Records = []api.SessionEvent{}
	}
	return response, nil
}

func (c *Client) sessionPath(sessionID, suffix string, opts EnvironmentScopeOptions) (string, error) {
	if err := ids.Validate(sessionID); err != nil {
		return "", err
	}
	return c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/sessions/"+url.PathEscape(sessionID)+suffix)
}
