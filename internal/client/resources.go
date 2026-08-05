package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/ids"
)

const (
	maxActorOutputReadLimit = int32(100)
	maxActorOutputSequence  = int64(1<<53 - 1)
)

func (c *Client) BaseURL() string {
	return c.transport.BaseURL()
}

func (c *Client) GetMe(ctx context.Context) (api.MeResponse, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/me", nil)
	if err != nil {
		return api.MeResponse{}, err
	}
	var response api.MeResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.MeResponse{}, err
	}
	return response, nil
}

func (c *Client) SendSessionInput(
	ctx context.Context,
	sessionID string,
	input api.SendSessionInputRequest,
	opts EnvironmentScopeOptions,
) (api.SessionInput, error) {
	if err := ids.Validate(sessionID); err != nil {
		return api.SessionInput{}, err
	}
	if err := api.ValidateSendSessionInputRequest(input); err != nil {
		return api.SessionInput{}, err
	}
	path, _, err := c.environmentScopedPath(
		opts.ProjectID,
		opts.EnvironmentID,
		"/sessions/"+url.PathEscape(sessionID)+"/inputs",
	)
	if err != nil {
		return api.SessionInput{}, err
	}
	var response api.SessionInput
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.SessionInput{}, err
	}
	return response, nil
}

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
	path, _, err := c.environmentScopedPath(
		opts.ProjectID,
		opts.EnvironmentID,
		"/actors/"+url.PathEscape(actorDeclaredID)+"/start",
	)
	if err != nil {
		return api.StartActorResponse{}, err
	}
	var response api.StartActorResponse
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.StartActorResponse{}, err
	}
	return response, nil
}

func (c *Client) CloseSession(
	ctx context.Context,
	sessionID string,
	input api.CloseSessionRequest,
	opts EnvironmentScopeOptions,
) (api.SessionCloseReceipt, error) {
	if err := ids.Validate(sessionID); err != nil {
		return api.SessionCloseReceipt{}, err
	}
	if err := api.ValidateCloseSessionRequest(input); err != nil {
		return api.SessionCloseReceipt{}, err
	}
	path, _, err := c.environmentScopedPath(
		opts.ProjectID,
		opts.EnvironmentID,
		"/sessions/"+url.PathEscape(sessionID)+"/close",
	)
	if err != nil {
		return api.SessionCloseReceipt{}, err
	}
	var response api.SessionCloseReceipt
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.SessionCloseReceipt{}, err
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
	path, _, err := c.environmentScopedPath(
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
	Cursor  string
	Limit   int32
	ActorID string
	Key     string
	EnvironmentScopeOptions
}

func (c *Client) ListSessions(ctx context.Context, opts SessionListOptions) (api.ListSessionsResponse, error) {
	hasActorID := opts.ActorID != ""
	hasKey := opts.Key != ""
	if hasActorID != hasKey {
		return api.ListSessionsResponse{}, errors.New("actor ID and key must be provided together")
	}
	if hasActorID {
		if opts.Cursor != "" || opts.Limit != 0 {
			return api.ListSessionsResponse{}, errors.New("cursor and limit are not accepted with actor ID and key")
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
	path, _, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/sessions")
	if err != nil {
		return api.ListSessionsResponse{}, err
	}
	values := url.Values{}
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

type ActorOutputReadOptions struct {
	After *int64
	Limit int32
	EnvironmentScopeOptions
}

func (c *Client) ReadSessionOutputs(
	ctx context.Context,
	sessionID string,
	opts ActorOutputReadOptions,
) (api.SessionOutputPage, error) {
	if err := ids.Validate(sessionID); err != nil {
		return api.SessionOutputPage{}, err
	}
	if opts.After != nil && (*opts.After < 0 || *opts.After > maxActorOutputSequence) {
		return api.SessionOutputPage{}, fmt.Errorf(
			"actor output after must be in [0,%d] when present",
			maxActorOutputSequence,
		)
	}
	if opts.Limit < 0 || opts.Limit > maxActorOutputReadLimit {
		return api.SessionOutputPage{}, fmt.Errorf(
			"actor output limit must be in [1,%d] when present",
			maxActorOutputReadLimit,
		)
	}
	path, _, err := c.environmentScopedPath(
		opts.ProjectID,
		opts.EnvironmentID,
		"/sessions/"+url.PathEscape(sessionID)+"/outputs",
	)
	if err != nil {
		return api.SessionOutputPage{}, err
	}
	values := url.Values{}
	if opts.After != nil {
		values.Set("after", strconv.FormatInt(*opts.After, 10))
	}
	if opts.Limit > 0 {
		values.Set("limit", strconv.FormatInt(int64(opts.Limit), 10))
	}
	path += "?" + values.Encode()
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.SessionOutputPage{}, err
	}
	var response api.SessionOutputPage
	if err := c.doJSON(req, &response); err != nil {
		return api.SessionOutputPage{}, err
	}
	if response.Records == nil {
		response.Records = []api.SessionOutput{}
	}
	return response, nil
}

type EnvironmentScopeOptions struct {
	ProjectID     string
	EnvironmentID string
}

func (c *Client) ListDeployments(ctx context.Context, opts EnvironmentScopeOptions) (api.ListDeploymentsResponse, error) {
	path, _, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/deployments")
	if err != nil {
		return api.ListDeploymentsResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ListDeploymentsResponse{}, err
	}
	var response api.ListDeploymentsResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListDeploymentsResponse{}, err
	}
	return response, nil
}

func (c *Client) ListTasks(ctx context.Context, opts EnvironmentScopeOptions) (api.ListTasksResponse, error) {
	path, _, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/tasks")
	if err != nil {
		return api.ListTasksResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ListTasksResponse{}, err
	}
	var response api.ListTasksResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListTasksResponse{}, err
	}
	return response, nil
}

func (c *Client) GetTask(ctx context.Context, taskID string, opts EnvironmentScopeOptions) (api.Task, error) {
	path, _, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/tasks/"+url.PathEscape(taskID))
	if err != nil {
		return api.Task{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.Task{}, err
	}
	var response api.Task
	if err := c.doJSON(req, &response); err != nil {
		return api.Task{}, err
	}
	return response, nil
}

func (c *Client) StartTask(
	ctx context.Context,
	taskID string,
	request api.StartTaskRequest,
	opts EnvironmentScopeOptions,
) (api.StartTaskResponse, error) {
	taskID = strings.TrimSpace(taskID)
	if err := api.ValidateDefinitionID(taskID); err != nil {
		return api.StartTaskResponse{}, err
	}
	path, _, err := c.environmentScopedPath(
		opts.ProjectID,
		opts.EnvironmentID,
		"/tasks/"+url.PathEscape(taskID)+"/start",
	)
	if err != nil {
		return api.StartTaskResponse{}, err
	}
	var response api.StartTaskResponse
	if err := c.postJSON(ctx, path, request, &response); err != nil {
		return api.StartTaskResponse{}, err
	}
	return response, nil
}

type WorkspaceScopeOptions struct {
	ProjectID     string
	EnvironmentID string
}

func (c *Client) workspaceCollectionPath(opts WorkspaceScopeOptions) (string, error) {
	path, _, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/workspaces")
	return path, err
}

func (c *Client) workspaceItemPath(workspaceID string, suffix string, opts WorkspaceScopeOptions) (string, error) {
	path, err := c.workspaceCollectionPath(opts)
	if err != nil {
		return "", err
	}
	return environmentScopedResourcePath(path, workspaceID, suffix), nil
}

func (c *Client) workspaceResourcePath(workspaceID string, suffix string, opts WorkspaceScopeOptions) (string, error) {
	if err := ids.Validate(workspaceID); err != nil {
		return "", err
	}
	return c.workspaceItemPath(workspaceID, suffix, opts)
}

func (c *Client) CreateWorkspace(
	ctx context.Context,
	declaredID string,
	input api.CreateWorkspaceRequest,
	opts WorkspaceScopeOptions,
) (api.WorkspaceSnapshot, error) {
	path, _, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/sandboxes/"+url.PathEscape(declaredID)+"/workspaces")
	if err != nil {
		return api.WorkspaceSnapshot{}, err
	}
	var response api.WorkspaceSnapshot
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.WorkspaceSnapshot{}, err
	}
	return response, nil
}

func (c *Client) GetWorkspace(ctx context.Context, workspaceID string, opts WorkspaceScopeOptions) (api.WorkspaceSnapshot, error) {
	path, err := c.workspaceResourcePath(workspaceID, "", opts)
	if err != nil {
		return api.WorkspaceSnapshot{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.WorkspaceSnapshot{}, err
	}
	var response api.WorkspaceSnapshot
	if err := c.doJSON(req, &response); err != nil {
		return api.WorkspaceSnapshot{}, err
	}
	return response, nil
}

func (c *Client) ListWorkspaces(
	ctx context.Context,
	key *string,
	opts WorkspaceScopeOptions,
) (api.ListWorkspacesResponse, error) {
	path, err := c.workspaceCollectionPath(opts)
	if err != nil {
		return api.ListWorkspacesResponse{}, err
	}
	if key != nil {
		path += "?" + url.Values{"key": []string{*key}}.Encode()
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ListWorkspacesResponse{}, err
	}
	var response api.ListWorkspacesResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListWorkspacesResponse{}, err
	}
	return response, nil
}

func (c *Client) DeleteWorkspace(
	ctx context.Context,
	workspaceID string,
	input api.DeleteWorkspaceRequest,
	opts WorkspaceScopeOptions,
) (api.DeleteWorkspaceReceipt, error) {
	path, err := c.workspaceResourcePath(workspaceID, "", opts)
	if err != nil {
		return api.DeleteWorkspaceReceipt{}, err
	}
	var response api.DeleteWorkspaceReceipt
	if err := c.deleteJSON(ctx, path, input, &response); err != nil {
		return api.DeleteWorkspaceReceipt{}, err
	}
	return response, nil
}

func (c *Client) ReadWorkspaceFile(
	ctx context.Context,
	workspaceID string,
	pathname string,
	opts WorkspaceScopeOptions,
) (api.WorkspaceFileContent, error) {
	path, err := c.workspaceResourcePath(workspaceID, "/files/content", opts)
	if err != nil {
		return api.WorkspaceFileContent{}, err
	}
	path += "?" + url.Values{"path": []string{pathname}}.Encode()
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.WorkspaceFileContent{}, err
	}
	var response api.WorkspaceFileContent
	if err := c.doJSON(req, &response); err != nil {
		return api.WorkspaceFileContent{}, err
	}
	return response, nil
}

type WorkspaceFileListOptions struct {
	Path   string
	Cursor string
	Limit  int32
}

func (c *Client) ListWorkspaceFiles(
	ctx context.Context,
	workspaceID string,
	input WorkspaceFileListOptions,
	opts WorkspaceScopeOptions,
) (api.WorkspaceFilePage, error) {
	path, err := c.workspaceResourcePath(workspaceID, "/files", opts)
	if err != nil {
		return api.WorkspaceFilePage{}, err
	}
	query := url.Values{}
	if input.Path != "" {
		query.Set("path", input.Path)
	}
	if input.Cursor != "" {
		query.Set("cursor", input.Cursor)
	}
	if input.Limit > 0 {
		query.Set("limit", strconv.FormatInt(int64(input.Limit), 10))
	}
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.WorkspaceFilePage{}, err
	}
	var response api.WorkspaceFilePage
	if err := c.doJSON(req, &response); err != nil {
		return api.WorkspaceFilePage{}, err
	}
	return response, nil
}

func (c *Client) StatWorkspaceFile(
	ctx context.Context,
	workspaceID string,
	pathname string,
	opts WorkspaceScopeOptions,
) (api.WorkspaceFileEntry, error) {
	path, err := c.workspaceResourcePath(workspaceID, "/files/stat", opts)
	if err != nil {
		return api.WorkspaceFileEntry{}, err
	}
	path += "?" + url.Values{"path": []string{pathname}}.Encode()
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.WorkspaceFileEntry{}, err
	}
	var response api.WorkspaceFileEntry
	if err := c.doJSON(req, &response); err != nil {
		return api.WorkspaceFileEntry{}, err
	}
	return response, nil
}

func (c *Client) ExecuteWorkspace(
	ctx context.Context,
	workspaceID string,
	input api.ExecuteWorkspaceRequest,
	opts WorkspaceScopeOptions,
) (api.ExecuteWorkspaceResult, error) {
	path, err := c.workspaceResourcePath(workspaceID, "/exec", opts)
	if err != nil {
		return api.ExecuteWorkspaceResult{}, err
	}
	var response api.ExecuteWorkspaceResult
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.ExecuteWorkspaceResult{}, err
	}
	return response, nil
}

type TokenScopeOptions struct {
	ProjectID     string
	EnvironmentID string
}

func (c *Client) tokenCollectionPath(opts TokenScopeOptions) (string, error) {
	path, _, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/tokens")
	return path, err
}

func (c *Client) tokenItemPath(tokenID string, suffix string, opts TokenScopeOptions) (string, error) {
	if err := ids.Validate(tokenID); err != nil {
		return "", err
	}
	path, err := c.tokenCollectionPath(opts)
	if err != nil {
		return "", err
	}
	return environmentScopedResourcePath(path, tokenID, suffix), nil
}

func (c *Client) CreateToken(ctx context.Context, input api.CreateTokenRequest, opts TokenScopeOptions) (api.TokenResponse, error) {
	path, err := c.tokenCollectionPath(opts)
	if err != nil {
		return api.TokenResponse{}, err
	}
	var response api.TokenResponse
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.TokenResponse{}, err
	}
	return response, nil
}

func (c *Client) GetToken(ctx context.Context, tokenID string, opts TokenScopeOptions) (api.TokenResponse, error) {
	path, err := c.tokenItemPath(tokenID, "", opts)
	if err != nil {
		return api.TokenResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.TokenResponse{}, err
	}
	var response api.TokenResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.TokenResponse{}, err
	}
	return response, nil
}

func (c *Client) CompleteToken(ctx context.Context, tokenID string, input api.CompleteTokenRequest, opts TokenScopeOptions) (api.CompleteTokenResponse, error) {
	path, err := c.tokenItemPath(tokenID, "/complete", opts)
	if err != nil {
		return api.CompleteTokenResponse{}, err
	}
	var response api.CompleteTokenResponse
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.CompleteTokenResponse{}, err
	}
	return response, nil
}

func (c *Client) CancelToken(ctx context.Context, tokenID string, input api.CancelTokenRequest, opts TokenScopeOptions) (api.TokenResponse, error) {
	path, err := c.tokenItemPath(tokenID, "/cancel", opts)
	if err != nil {
		return api.TokenResponse{}, err
	}
	var response api.TokenResponse
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.TokenResponse{}, err
	}
	return response, nil
}
