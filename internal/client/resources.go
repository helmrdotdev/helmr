package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
)

const (
	maxActorListLimit       = int32(100)
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

func (c *Client) UpdateActor(
	ctx context.Context,
	actorDeclaredID string,
	input api.UpdateActorRequest,
	opts EnvironmentScopeOptions,
) (api.ActorStatus, error) {
	if err := api.ValidateActorDeclaredID(actorDeclaredID); err != nil {
		return api.ActorStatus{}, err
	}
	if err := api.ValidateUpdateActorRequest(input); err != nil {
		return api.ActorStatus{}, err
	}
	path, _, err := c.environmentScopedPath(
		opts.ProjectID,
		opts.EnvironmentID,
		"/actors/"+url.PathEscape(actorDeclaredID),
	)
	if err != nil {
		return api.ActorStatus{}, err
	}
	var response api.ActorStatus
	if err := c.patchJSON(ctx, path, input, &response); err != nil {
		return api.ActorStatus{}, err
	}
	return response, nil
}

type ListActorsOptions struct {
	Lifecycle api.ActorLifecycle
	Cursor    string
	Limit     int32
	EnvironmentScopeOptions
}

func (c *Client) ListActors(
	ctx context.Context,
	actorDeclaredID string,
	opts ListActorsOptions,
) (api.ListActorsResponse, error) {
	if err := api.ValidateActorDeclaredID(actorDeclaredID); err != nil {
		return api.ListActorsResponse{}, err
	}
	if opts.Lifecycle != "" {
		if err := api.ValidateActorLifecycle(string(opts.Lifecycle)); err != nil {
			return api.ListActorsResponse{}, err
		}
	}
	if opts.Limit < 0 || opts.Limit > maxActorListLimit {
		return api.ListActorsResponse{}, fmt.Errorf("Actor list limit must be in [1,%d] when present", maxActorListLimit)
	}
	path, _, err := c.environmentScopedPath(
		opts.ProjectID,
		opts.EnvironmentID,
		"/actors/"+url.PathEscape(actorDeclaredID),
	)
	if err != nil {
		return api.ListActorsResponse{}, err
	}
	values := url.Values{}
	if opts.Lifecycle != "" {
		values.Set("lifecycle", string(opts.Lifecycle))
	}
	if opts.Cursor != "" {
		values.Set("cursor", opts.Cursor)
	}
	if opts.Limit > 0 {
		values.Set("limit", strconv.FormatInt(int64(opts.Limit), 10))
	}
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ListActorsResponse{}, err
	}
	var response api.ListActorsResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListActorsResponse{}, err
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

func (c *Client) ListSandboxes(ctx context.Context, opts EnvironmentScopeOptions) (api.ListSandboxesResponse, error) {
	path, _, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/sandboxes")
	if err != nil {
		return api.ListSandboxesResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ListSandboxesResponse{}, err
	}
	var response api.ListSandboxesResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListSandboxesResponse{}, err
	}
	return response, nil
}

func (c *Client) GetSandbox(ctx context.Context, sandboxID string, opts EnvironmentScopeOptions) (api.SandboxResponse, error) {
	path, _, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/sandboxes/"+url.PathEscape(sandboxID))
	if err != nil {
		return api.SandboxResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.SandboxResponse{}, err
	}
	var response api.SandboxResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.SandboxResponse{}, err
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

func (c *Client) CreateWorkspace(ctx context.Context, input api.WorkspaceCreateRequest) (api.WorkspaceEnvelope, error) {
	path, scoped, err := c.environmentScopedPath(input.ProjectID, input.EnvironmentID, "/workspaces")
	if err != nil {
		return api.WorkspaceEnvelope{}, err
	}
	if scoped {
		input.ProjectID = ""
		input.EnvironmentID = ""
	}
	var response api.WorkspaceEnvelope
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.WorkspaceEnvelope{}, err
	}
	return response, nil
}

func (c *Client) ListWorkspaces(ctx context.Context, opts WorkspaceScopeOptions) (api.ListWorkspacesResponse, error) {
	path, err := c.workspaceCollectionPath(opts)
	if err != nil {
		return api.ListWorkspacesResponse{}, err
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

func (c *Client) GetWorkspace(ctx context.Context, workspaceID string, opts WorkspaceScopeOptions) (api.WorkspaceEnvelope, error) {
	path, err := c.workspaceItemPath(workspaceID, "", opts)
	if err != nil {
		return api.WorkspaceEnvelope{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.WorkspaceEnvelope{}, err
	}
	var response api.WorkspaceEnvelope
	if err := c.doJSON(req, &response); err != nil {
		return api.WorkspaceEnvelope{}, err
	}
	return response, nil
}

func (c *Client) UpdateWorkspace(ctx context.Context, workspaceID string, input api.WorkspacePatchRequest, opts WorkspaceScopeOptions) (api.WorkspaceEnvelope, error) {
	path, err := c.workspaceItemPath(workspaceID, "", opts)
	if err != nil {
		return api.WorkspaceEnvelope{}, err
	}
	var response api.WorkspaceEnvelope
	if err := c.patchJSON(ctx, path, input, &response); err != nil {
		return api.WorkspaceEnvelope{}, err
	}
	return response, nil
}

func (c *Client) DeleteWorkspace(ctx context.Context, workspaceID string, opts WorkspaceScopeOptions) error {
	path, err := c.workspaceItemPath(workspaceID, "", opts)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	return c.doJSON(req, nil)
}

func (c *Client) MaterializeWorkspace(ctx context.Context, workspaceID string, opts WorkspaceScopeOptions) (api.WorkspaceMountResponse, error) {
	path, err := c.workspaceItemPath(workspaceID, "/materialize", opts)
	if err != nil {
		return api.WorkspaceMountResponse{}, err
	}
	var response api.WorkspaceMountResponse
	if err := c.postJSON(ctx, path, api.WorkspaceMaterializeRequest{}, &response); err != nil {
		return api.WorkspaceMountResponse{}, err
	}
	return response, nil
}

func (c *Client) ConnectWorkspace(ctx context.Context, workspaceID string, opts WorkspaceScopeOptions) (api.WorkspaceMountResponse, error) {
	path, err := c.workspaceItemPath(workspaceID, "/connect", opts)
	if err != nil {
		return api.WorkspaceMountResponse{}, err
	}
	var response api.WorkspaceMountResponse
	if err := c.postJSON(ctx, path, api.WorkspaceMaterializeRequest{}, &response); err != nil {
		return api.WorkspaceMountResponse{}, err
	}
	return response, nil
}

func (c *Client) StopWorkspace(ctx context.Context, workspaceID string, input api.WorkspaceStopRequest, opts WorkspaceScopeOptions) (api.WorkspaceStopResponse, error) {
	path, err := c.workspaceItemPath(workspaceID, "/stop", opts)
	if err != nil {
		return api.WorkspaceStopResponse{}, err
	}
	var response api.WorkspaceStopResponse
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.WorkspaceStopResponse{}, err
	}
	return response, nil
}

func (c *Client) CreateWorkspaceExec(ctx context.Context, workspaceID string, input api.WorkspaceExecCreateRequest, opts WorkspaceScopeOptions) (api.WorkspaceExecEnvelope, error) {
	path, err := c.workspaceItemPath(workspaceID, "/execs", opts)
	if err != nil {
		return api.WorkspaceExecEnvelope{}, err
	}
	var response api.WorkspaceExecEnvelope
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.WorkspaceExecEnvelope{}, err
	}
	return response, nil
}

func (c *Client) ListWorkspaceExecs(ctx context.Context, workspaceID string, opts WorkspaceScopeOptions) (api.ListWorkspaceExecsResponse, error) {
	path, err := c.workspaceItemPath(workspaceID, "/execs", opts)
	if err != nil {
		return api.ListWorkspaceExecsResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ListWorkspaceExecsResponse{}, err
	}
	var response api.ListWorkspaceExecsResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListWorkspaceExecsResponse{}, err
	}
	return response, nil
}

func (c *Client) GetWorkspaceExec(ctx context.Context, workspaceID string, execID string, opts WorkspaceScopeOptions) (api.WorkspaceExecEnvelope, error) {
	path, err := c.workspaceItemPath(workspaceID, "/execs/"+url.PathEscape(execID), opts)
	if err != nil {
		return api.WorkspaceExecEnvelope{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.WorkspaceExecEnvelope{}, err
	}
	var response api.WorkspaceExecEnvelope
	if err := c.doJSON(req, &response); err != nil {
		return api.WorkspaceExecEnvelope{}, err
	}
	return response, nil
}

func (c *Client) WriteWorkspaceExecStdin(ctx context.Context, workspaceID string, execID string, input api.WorkspaceExecStdinWriteRequest, opts WorkspaceScopeOptions) (api.WorkspaceExecStreamChunkResponse, error) {
	path, err := c.workspaceItemPath(workspaceID, "/execs/"+url.PathEscape(execID)+"/stdin", opts)
	if err != nil {
		return api.WorkspaceExecStreamChunkResponse{}, err
	}
	var response api.WorkspaceExecStreamChunkResponse
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.WorkspaceExecStreamChunkResponse{}, err
	}
	return response, nil
}

func (c *Client) CloseWorkspaceExecStdin(ctx context.Context, workspaceID string, execID string, opts WorkspaceScopeOptions) (api.WorkspaceExecEnvelope, error) {
	path, err := c.workspaceItemPath(workspaceID, "/execs/"+url.PathEscape(execID)+"/stdin/close", opts)
	if err != nil {
		return api.WorkspaceExecEnvelope{}, err
	}
	var response api.WorkspaceExecEnvelope
	if err := c.postJSON(ctx, path, struct{}{}, &response); err != nil {
		return api.WorkspaceExecEnvelope{}, err
	}
	return response, nil
}

func (c *Client) ListWorkspaceExecStream(ctx context.Context, workspaceID string, execID string, stream string, cursor int64, opts WorkspaceScopeOptions) (api.ListWorkspaceExecStreamChunksResponse, error) {
	path, err := c.workspaceItemPath(workspaceID, "/execs/"+url.PathEscape(execID)+"/"+url.PathEscape(stream), opts)
	if err != nil {
		return api.ListWorkspaceExecStreamChunksResponse{}, err
	}
	values := url.Values{}
	if cursor > 0 {
		values.Set("cursor", strconv.FormatInt(cursor, 10))
	}
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ListWorkspaceExecStreamChunksResponse{}, err
	}
	var response api.ListWorkspaceExecStreamChunksResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListWorkspaceExecStreamChunksResponse{}, err
	}
	return response, nil
}

func (c *Client) FollowWorkspaceExecStream(ctx context.Context, workspaceID string, execID string, stream string, cursor int64, opts WorkspaceScopeOptions, handle func(WorkspaceStreamEvent) error) error {
	path, err := c.workspaceItemPath(workspaceID, "/execs/"+url.PathEscape(execID)+"/"+url.PathEscape(stream), opts)
	if err != nil {
		return err
	}
	return c.followWorkspaceStream(ctx, path, cursor, handle)
}

func (c *Client) CreateWorkspacePty(ctx context.Context, workspaceID string, input api.WorkspacePtyCreateRequest, opts WorkspaceScopeOptions) (api.WorkspacePtyEnvelope, error) {
	path, err := c.workspaceItemPath(workspaceID, "/pty", opts)
	if err != nil {
		return api.WorkspacePtyEnvelope{}, err
	}
	var response api.WorkspacePtyEnvelope
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.WorkspacePtyEnvelope{}, err
	}
	return response, nil
}

func (c *Client) ListWorkspacePtys(ctx context.Context, workspaceID string, opts WorkspaceScopeOptions) (api.ListWorkspacePtySessionsResponse, error) {
	path, err := c.workspaceItemPath(workspaceID, "/pty", opts)
	if err != nil {
		return api.ListWorkspacePtySessionsResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ListWorkspacePtySessionsResponse{}, err
	}
	var response api.ListWorkspacePtySessionsResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListWorkspacePtySessionsResponse{}, err
	}
	return response, nil
}

func (c *Client) GetWorkspacePty(ctx context.Context, workspaceID string, ptyID string, opts WorkspaceScopeOptions) (api.WorkspacePtyEnvelope, error) {
	path, err := c.workspaceItemPath(workspaceID, "/pty/"+url.PathEscape(ptyID), opts)
	if err != nil {
		return api.WorkspacePtyEnvelope{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.WorkspacePtyEnvelope{}, err
	}
	var response api.WorkspacePtyEnvelope
	if err := c.doJSON(req, &response); err != nil {
		return api.WorkspacePtyEnvelope{}, err
	}
	return response, nil
}

func (c *Client) WriteWorkspacePtyInput(ctx context.Context, workspaceID string, ptyID string, input api.WorkspacePtyInputWriteRequest, opts WorkspaceScopeOptions) (api.WorkspacePtyStreamChunkResponse, error) {
	path, err := c.workspaceItemPath(workspaceID, "/pty/"+url.PathEscape(ptyID)+"/input", opts)
	if err != nil {
		return api.WorkspacePtyStreamChunkResponse{}, err
	}
	var response api.WorkspacePtyStreamChunkResponse
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.WorkspacePtyStreamChunkResponse{}, err
	}
	return response, nil
}

func (c *Client) ResizeWorkspacePty(ctx context.Context, workspaceID string, ptyID string, input api.WorkspacePtyResizeRequest, opts WorkspaceScopeOptions) (api.WorkspacePtyEnvelope, error) {
	path, err := c.workspaceItemPath(workspaceID, "/pty/"+url.PathEscape(ptyID)+"/resize", opts)
	if err != nil {
		return api.WorkspacePtyEnvelope{}, err
	}
	var response api.WorkspacePtyEnvelope
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.WorkspacePtyEnvelope{}, err
	}
	return response, nil
}

func (c *Client) CloseWorkspacePty(ctx context.Context, workspaceID string, ptyID string, opts WorkspaceScopeOptions) (api.WorkspacePtyEnvelope, error) {
	path, err := c.workspaceItemPath(workspaceID, "/pty/"+url.PathEscape(ptyID)+"/close", opts)
	if err != nil {
		return api.WorkspacePtyEnvelope{}, err
	}
	var response api.WorkspacePtyEnvelope
	if err := c.postJSON(ctx, path, struct{}{}, &response); err != nil {
		return api.WorkspacePtyEnvelope{}, err
	}
	return response, nil
}

func (c *Client) ListWorkspacePtyOutput(ctx context.Context, workspaceID string, ptyID string, cursor int64, opts WorkspaceScopeOptions) (api.ListWorkspacePtyStreamChunksResponse, error) {
	path, err := c.workspaceItemPath(workspaceID, "/pty/"+url.PathEscape(ptyID)+"/output", opts)
	if err != nil {
		return api.ListWorkspacePtyStreamChunksResponse{}, err
	}
	values := url.Values{}
	if cursor > 0 {
		values.Set("cursor", strconv.FormatInt(cursor, 10))
	}
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ListWorkspacePtyStreamChunksResponse{}, err
	}
	var response api.ListWorkspacePtyStreamChunksResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListWorkspacePtyStreamChunksResponse{}, err
	}
	return response, nil
}

func (c *Client) FollowWorkspacePtyOutput(ctx context.Context, workspaceID string, ptyID string, cursor int64, opts WorkspaceScopeOptions, handle func(WorkspaceStreamEvent) error) error {
	path, err := c.workspaceItemPath(workspaceID, "/pty/"+url.PathEscape(ptyID)+"/output", opts)
	if err != nil {
		return err
	}
	return c.followWorkspaceStream(ctx, path, cursor, handle)
}

type WorkspaceStreamEvent struct {
	Event    string
	ID       string
	Chunk    json.RawMessage
	Terminal *api.WorkspaceStreamTerminalResponse
	Error    *api.WorkspaceStreamErrorResponse
}

func (c *Client) followWorkspaceStream(ctx context.Context, path string, cursor int64, handle func(WorkspaceStreamEvent) error) error {
	values := url.Values{}
	values.Set("follow", "1")
	if cursor > 0 {
		values.Set("cursor", strconv.FormatInt(cursor, 10))
	}
	if strings.Contains(path, "?") {
		path += "&" + values.Encode()
	} else {
		path += "?" + values.Encode()
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("accept", "text/event-stream")
	res, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return decodeError(res)
	}
	return readSSE(res.Body, func(eventName string, eventID string, data []byte) error {
		event := WorkspaceStreamEvent{Event: eventName, ID: eventID}
		switch eventName {
		case "workspace_stream_chunk":
			event.Chunk = append(json.RawMessage(nil), data...)
		case "workspace_stream_terminal", "workspace_stream_lost":
			var terminal api.WorkspaceStreamTerminalResponse
			if err := json.Unmarshal(data, &terminal); err != nil {
				return err
			}
			event.Terminal = &terminal
		case "workspace_stream_error":
			var streamErr api.WorkspaceStreamErrorResponse
			if err := json.Unmarshal(data, &streamErr); err != nil {
				return err
			}
			event.Error = &streamErr
		default:
			return nil
		}
		return handle(event)
	})
}

type SessionScopeOptions struct {
	ProjectID     string
	EnvironmentID string
	ExternalID    string
	Status        string
	TaskID        string
	Limit         int32
}

func (c *Client) sessionCollectionPath(opts SessionScopeOptions) (string, error) {
	path, _, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/sessions")
	return path, err
}

func (c *Client) sessionItemPath(sessionID string, suffix string, opts SessionScopeOptions) (string, error) {
	path, err := c.sessionCollectionPath(opts)
	if err != nil {
		return "", err
	}
	return environmentScopedResourcePath(path, sessionID, suffix), nil
}

func (c *Client) ListSessions(ctx context.Context, opts SessionScopeOptions) (api.ListSessionsResponse, error) {
	path, err := c.sessionCollectionPath(opts)
	if err != nil {
		return api.ListSessionsResponse{}, err
	}
	query := url.Values{}
	if strings.TrimSpace(opts.ExternalID) != "" {
		query.Set("external_id", strings.TrimSpace(opts.ExternalID))
	}
	if strings.TrimSpace(opts.Status) != "" {
		query.Set("status", strings.TrimSpace(opts.Status))
	}
	if strings.TrimSpace(opts.TaskID) != "" {
		query.Set("task_id", strings.TrimSpace(opts.TaskID))
	}
	if opts.Limit > 0 {
		query.Set("limit", strconv.FormatInt(int64(opts.Limit), 10))
	}
	if encoded := query.Encode(); encoded != "" {
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
	return response, nil
}

func (c *Client) GetSession(ctx context.Context, sessionID string, opts SessionScopeOptions) (api.SessionResponse, error) {
	path, err := c.sessionItemPath(sessionID, "", opts)
	if err != nil {
		return api.SessionResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.SessionResponse{}, err
	}
	var response api.SessionResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.SessionResponse{}, err
	}
	return response, nil
}

func (c *Client) CancelSession(ctx context.Context, sessionID string, input api.CancelSessionRequest, opts SessionScopeOptions) (api.SessionResponse, error) {
	path, err := c.sessionItemPath(sessionID, "/cancel", opts)
	if err != nil {
		return api.SessionResponse{}, err
	}
	var response api.SessionResponse
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.SessionResponse{}, err
	}
	return response, nil
}

func (c *Client) CloseSession(ctx context.Context, sessionID string, input api.CloseSessionRequest, opts SessionScopeOptions) (api.SessionResponse, error) {
	path, err := c.sessionItemPath(sessionID, "/close", opts)
	if err != nil {
		return api.SessionResponse{}, err
	}
	var response api.SessionResponse
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.SessionResponse{}, err
	}
	return response, nil
}

func (c *Client) AppendSessionInput(ctx context.Context, sessionID string, stream string, input api.AppendStreamRecordRequest, opts SessionScopeOptions) (api.AppendStreamRecordResponse, error) {
	path, err := c.sessionItemPath(sessionID, "/inputs/"+url.PathEscape(stream), opts)
	if err != nil {
		return api.AppendStreamRecordResponse{}, err
	}
	var response api.AppendStreamRecordResponse
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.AppendStreamRecordResponse{}, err
	}
	return response, nil
}

func (c *Client) ListSessionStreams(ctx context.Context, sessionID string, opts SessionScopeOptions) (api.ListSessionStreamsResponse, error) {
	path, err := c.sessionItemPath(sessionID, "/streams", opts)
	if err != nil {
		return api.ListSessionStreamsResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ListSessionStreamsResponse{}, err
	}
	var response api.ListSessionStreamsResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListSessionStreamsResponse{}, err
	}
	return response, nil
}

func (c *Client) ListSessionInputs(ctx context.Context, sessionID string, stream string, cursor int64, limit int32, opts SessionScopeOptions) (api.ListStreamRecordsResponse, error) {
	path, err := c.sessionItemPath(sessionID, "/inputs/"+url.PathEscape(stream), opts)
	if err != nil {
		return api.ListStreamRecordsResponse{}, err
	}
	values := url.Values{}
	if cursor > 0 {
		values.Set("after_sequence", strconv.FormatInt(cursor, 10))
	}
	if limit > 0 {
		values.Set("limit", strconv.FormatInt(int64(limit), 10))
	}
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ListStreamRecordsResponse{}, err
	}
	var response api.ListStreamRecordsResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListStreamRecordsResponse{}, err
	}
	return response, nil
}

func (c *Client) ListSessionOutputs(ctx context.Context, sessionID string, stream string, cursor int64, limit int32, opts SessionScopeOptions) (api.ListStreamRecordsResponse, error) {
	path, err := c.sessionItemPath(sessionID, "/outputs/"+url.PathEscape(stream), opts)
	if err != nil {
		return api.ListStreamRecordsResponse{}, err
	}
	values := url.Values{}
	if cursor > 0 {
		values.Set("after_sequence", strconv.FormatInt(cursor, 10))
	}
	if limit > 0 {
		values.Set("limit", strconv.FormatInt(int64(limit), 10))
	}
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ListStreamRecordsResponse{}, err
	}
	var response api.ListStreamRecordsResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListStreamRecordsResponse{}, err
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

func (c *Client) CreateToken(ctx context.Context, input api.CreateTokenRequest) (api.TokenResponse, error) {
	path, scoped, err := c.environmentScopedPath(input.ProjectID, input.EnvironmentID, "/tokens")
	if err != nil {
		return api.TokenResponse{}, err
	}
	if scoped {
		input.ProjectID = ""
		input.EnvironmentID = ""
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

func (c *Client) CancelToken(ctx context.Context, tokenID string, opts TokenScopeOptions) (api.TokenResponse, error) {
	path, err := c.tokenItemPath(tokenID, "/cancel", opts)
	if err != nil {
		return api.TokenResponse{}, err
	}
	var response api.TokenResponse
	if err := c.postJSON(ctx, path, struct{}{}, &response); err != nil {
		return api.TokenResponse{}, err
	}
	return response, nil
}

func readSSE(reader io.Reader, handle func(eventName string, eventID string, data []byte) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	eventName := ""
	eventID := ""
	var data bytes.Buffer
	flush := func() error {
		if data.Len() == 0 {
			eventName = ""
			eventID = ""
			return nil
		}
		payload := bytes.TrimSuffix(data.Bytes(), []byte{'\n'})
		if err := handle(eventName, eventID, payload); err != nil {
			return err
		}
		eventName = ""
		eventID = ""
		data.Reset()
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if value, ok := strings.CutPrefix(line, "event:"); ok {
			eventName = strings.TrimSpace(value)
			continue
		}
		if value, ok := strings.CutPrefix(line, "id:"); ok {
			eventID = strings.TrimSpace(value)
			continue
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(value, " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}
