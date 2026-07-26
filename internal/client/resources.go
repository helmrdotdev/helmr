package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
)

const (
	maxActorOutputReadLimit = int32(100)
	maxActorOutputSequence  = int64(1<<53 - 1)
)

func (c *Client) BaseURL() string {
	return c.baseURL.String()
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

func (c *Client) SendActorInput(
	ctx context.Context,
	actorDeclaredID string,
	input api.SendActorInputRequest,
	opts EnvironmentScopeOptions,
) (api.SendActorInputResponse, error) {
	if err := api.ValidateActorDeclaredID(actorDeclaredID); err != nil {
		return api.SendActorInputResponse{}, err
	}
	if err := api.ValidateSendActorInputRequest(input); err != nil {
		return api.SendActorInputResponse{}, err
	}
	path, _, err := c.environmentScopedPath(
		opts.ProjectID,
		opts.EnvironmentID,
		"/actors/"+url.PathEscape(actorDeclaredID)+"/input",
	)
	if err != nil {
		return api.SendActorInputResponse{}, err
	}
	var response api.SendActorInputResponse
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.SendActorInputResponse{}, err
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

func (c *Client) CloseActor(
	ctx context.Context,
	actorDeclaredID string,
	input api.ActorOperationRequest,
	opts EnvironmentScopeOptions,
) (api.ActorOperationReceipt, error) {
	if err := api.ValidateActorDeclaredID(actorDeclaredID); err != nil {
		return api.ActorOperationReceipt{}, err
	}
	if err := api.ValidateActorOperationRequest(input); err != nil {
		return api.ActorOperationReceipt{}, err
	}
	path, _, err := c.environmentScopedPath(
		opts.ProjectID,
		opts.EnvironmentID,
		"/actors/"+url.PathEscape(actorDeclaredID)+"/close",
	)
	if err != nil {
		return api.ActorOperationReceipt{}, err
	}
	var response api.ActorOperationReceipt
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.ActorOperationReceipt{}, err
	}
	return response, nil
}

func (c *Client) GetActorStatus(
	ctx context.Context,
	actorDeclaredID string,
	reference api.ActorReference,
	opts EnvironmentScopeOptions,
) (api.ActorStatus, error) {
	if err := api.ValidateActorDeclaredID(actorDeclaredID); err != nil {
		return api.ActorStatus{}, err
	}
	if err := api.ValidateActorReference(reference); err != nil {
		return api.ActorStatus{}, err
	}
	path, _, err := c.environmentScopedPath(
		opts.ProjectID,
		opts.EnvironmentID,
		"/actors/"+url.PathEscape(actorDeclaredID)+"/status",
	)
	if err != nil {
		return api.ActorStatus{}, err
	}
	values := url.Values{}
	if reference.ActorID != "" {
		values.Set("actor_id", reference.ActorID)
	} else {
		values.Set("actor_key", reference.ActorKey)
	}
	path += "?" + values.Encode()
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ActorStatus{}, err
	}
	var response api.ActorStatus
	if err := c.doJSON(req, &response); err != nil {
		return api.ActorStatus{}, err
	}
	return response, nil
}

type ActorOutputReadOptions struct {
	After *int64
	Limit int32
	EnvironmentScopeOptions
}

func (c *Client) ReadActorOutput(
	ctx context.Context,
	actorDeclaredID string,
	reference api.ActorReference,
	opts ActorOutputReadOptions,
) (api.ActorOutputPage, error) {
	if err := api.ValidateActorDeclaredID(actorDeclaredID); err != nil {
		return api.ActorOutputPage{}, err
	}
	if err := api.ValidateActorReference(reference); err != nil {
		return api.ActorOutputPage{}, err
	}
	if opts.After != nil && (*opts.After < 0 || *opts.After > maxActorOutputSequence) {
		return api.ActorOutputPage{}, fmt.Errorf(
			"actor output after must be in [0,%d] when present",
			maxActorOutputSequence,
		)
	}
	if opts.Limit < 0 || opts.Limit > maxActorOutputReadLimit {
		return api.ActorOutputPage{}, fmt.Errorf(
			"actor output limit must be in [1,%d] when present",
			maxActorOutputReadLimit,
		)
	}
	path, _, err := c.environmentScopedPath(
		opts.ProjectID,
		opts.EnvironmentID,
		"/actors/"+url.PathEscape(actorDeclaredID)+"/output",
	)
	if err != nil {
		return api.ActorOutputPage{}, err
	}
	values := url.Values{}
	if reference.ActorID != "" {
		values.Set("actor_id", reference.ActorID)
	} else {
		values.Set("actor_key", reference.ActorKey)
	}
	if opts.After != nil {
		values.Set("after", strconv.FormatInt(*opts.After, 10))
	}
	if opts.Limit > 0 {
		values.Set("limit", strconv.FormatInt(int64(opts.Limit), 10))
	}
	path += "?" + values.Encode()
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ActorOutputPage{}, err
	}
	var response api.ActorOutputPage
	if err := c.doJSON(req, &response); err != nil {
		return api.ActorOutputPage{}, err
	}
	if response.Records == nil {
		response.Records = []api.ActorOutputRecord{}
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

func (c *Client) GetTask(ctx context.Context, taskID string, opts EnvironmentScopeOptions) (api.DeploymentTaskResponse, error) {
	path, _, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/tasks/"+url.PathEscape(taskID))
	if err != nil {
		return api.DeploymentTaskResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.DeploymentTaskResponse{}, err
	}
	var response api.DeploymentTaskResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.DeploymentTaskResponse{}, err
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
	if err := api.ValidateTaskID(taskID); err != nil {
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

func (c *Client) CreateWorkspace(
	ctx context.Context,
	declaredID string,
	input api.CreateWorkspaceRequest,
	opts WorkspaceScopeOptions,
) (api.CreateWorkspaceResponse, error) {
	path, err := c.workspaceItemPath(declaredID, "/create", opts)
	if err != nil {
		return api.CreateWorkspaceResponse{}, err
	}
	var response api.CreateWorkspaceResponse
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.CreateWorkspaceResponse{}, err
	}
	return response, nil
}

func (c *Client) GetWorkspace(ctx context.Context, workspaceID string, opts WorkspaceScopeOptions) (api.WorkspaceSnapshot, error) {
	path, err := c.workspaceItemPath(workspaceID, "", opts)
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

func (c *Client) GetWorkspaceByKey(
	ctx context.Context,
	declaredID string,
	key string,
	opts WorkspaceScopeOptions,
) (api.WorkspaceSnapshot, error) {
	path, err := c.workspaceItemPath("by-key", "/"+url.PathEscape(declaredID), opts)
	if err != nil {
		return api.WorkspaceSnapshot{}, err
	}
	path += "?" + url.Values{"key": []string{key}}.Encode()
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

func (c *Client) DeleteWorkspace(
	ctx context.Context,
	workspaceID string,
	input api.DeleteWorkspaceRequest,
	opts WorkspaceScopeOptions,
) (api.DeleteWorkspaceReceipt, error) {
	path, err := c.workspaceItemPath(workspaceID, "/delete", opts)
	if err != nil {
		return api.DeleteWorkspaceReceipt{}, err
	}
	var response api.DeleteWorkspaceReceipt
	if err := c.postJSON(ctx, path, input, &response); err != nil {
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
	path, err := c.workspaceItemPath(workspaceID, "/files/content", opts)
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
	path, err := c.workspaceItemPath(workspaceID, "/files", opts)
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
	path, err := c.workspaceItemPath(workspaceID, "/files/stat", opts)
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
	path, err := c.workspaceItemPath(workspaceID, "/exec", opts)
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
