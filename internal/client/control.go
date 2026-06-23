package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

const (
	taskStartPendingMaxWait      = 10 * time.Second
	taskStartPendingDefaultDelay = 250 * time.Millisecond
)

func (c *Client) StartTask(ctx context.Context, taskID string, input api.TaskStartRequest) (api.TaskStartResponse, error) {
	taskID = strings.TrimSpace(taskID)
	if err := api.ValidateTaskID(taskID); err != nil {
		return api.TaskStartResponse{}, err
	}
	path, scoped, err := c.environmentScopedPath(input.ProjectID, input.EnvironmentID, "/tasks/"+url.PathEscape(taskID)+"/start")
	if err != nil {
		return api.TaskStartResponse{}, err
	}
	if scoped {
		input.ProjectID = ""
		input.EnvironmentID = ""
	}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(input); err != nil {
		return api.TaskStartResponse{}, fmt.Errorf("encode request: %w", err)
	}
	bodyBytes := body.Bytes()
	startedAt := time.Now()
	for {
		req, err := c.newRequestWithBearer(ctx, http.MethodPost, path, bytes.NewReader(bodyBytes), c.bearer)
		if err != nil {
			return api.TaskStartResponse{}, err
		}
		req.Header.Set("content-type", "application/json")
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return api.TaskStartResponse{}, err
		}
		if resp.StatusCode == http.StatusAccepted {
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				return api.TaskStartResponse{}, fmt.Errorf("read task start pending response: %w", readErr)
			}
			if !taskStartPendingResponse(body) {
				return api.TaskStartResponse{}, decodeErrorBody(resp.StatusCode, resp.Status, body)
			}
			delay := taskStartPendingRetryDelay(resp.Header.Get("Retry-After"))
			if time.Since(startedAt)+delay > taskStartPendingMaxWait {
				return api.TaskStartResponse{}, decodeErrorBody(resp.StatusCode, resp.Status, body)
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return api.TaskStartResponse{}, ctx.Err()
			case <-timer.C:
			}
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return api.TaskStartResponse{}, decodeError(resp)
		}
		var response api.TaskStartResponse
		if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
			return api.TaskStartResponse{}, fmt.Errorf("decode response: %w", err)
		}
		return response, nil
	}
}

func taskStartPendingResponse(body []byte) bool {
	var payload struct {
		Code string `json:"code"`
	}
	return json.Unmarshal(body, &payload) == nil && payload.Code == "idempotency_pending"
}

func taskStartPendingRetryDelay(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return taskStartPendingDefaultDelay
	}
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil && seconds > 0 {
		delay := time.Duration(seconds * float64(time.Second))
		if delay > taskStartPendingMaxWait {
			return taskStartPendingMaxWait
		}
		return delay
	}
	if retryAt, err := http.ParseTime(raw); err == nil {
		delay := time.Until(retryAt)
		if delay <= 0 {
			return taskStartPendingDefaultDelay
		}
		if delay > taskStartPendingMaxWait {
			return taskStartPendingMaxWait
		}
		return delay
	}
	return taskStartPendingDefaultDelay
}

func (c *Client) ListProjects(ctx context.Context) (api.ListProjectsResponse, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/projects", nil)
	if err != nil {
		return api.ListProjectsResponse{}, err
	}
	var response api.ListProjectsResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListProjectsResponse{}, err
	}
	return response, nil
}

func (c *Client) GetProject(ctx context.Context, projectID string) (api.ProjectSummary, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/projects/"+url.PathEscape(projectID), nil)
	if err != nil {
		return api.ProjectSummary{}, err
	}
	var response api.ProjectSummary
	if err := c.doJSON(req, &response); err != nil {
		return api.ProjectSummary{}, err
	}
	return response, nil
}

func (c *Client) CreateProject(ctx context.Context, request api.CreateProjectRequest) (api.ProjectSummary, error) {
	var response api.ProjectSummary
	if err := c.postJSON(ctx, "/api/projects", request, &response); err != nil {
		return api.ProjectSummary{}, err
	}
	return response, nil
}

func (c *Client) UpdateProject(ctx context.Context, projectID string, request api.UpdateProjectRequest) (api.ProjectSummary, error) {
	var response api.ProjectSummary
	if err := c.patchJSON(ctx, "/api/projects/"+url.PathEscape(projectID), request, &response); err != nil {
		return api.ProjectSummary{}, err
	}
	return response, nil
}

func (c *Client) DeleteProject(ctx context.Context, projectID string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, "/api/projects/"+url.PathEscape(projectID), nil)
	if err != nil {
		return err
	}
	return c.doJSON(req, nil)
}

func (c *Client) GetEnvironment(ctx context.Context, projectID string, environmentID string) (api.EnvironmentSummary, error) {
	req, err := c.newRequest(ctx, http.MethodGet, projectEnvironmentPath(projectID, environmentID), nil)
	if err != nil {
		return api.EnvironmentSummary{}, err
	}
	var response api.EnvironmentSummary
	if err := c.doJSON(req, &response); err != nil {
		return api.EnvironmentSummary{}, err
	}
	return response, nil
}

func (c *Client) CreateEnvironment(ctx context.Context, projectID string, request api.CreateEnvironmentRequest) (api.EnvironmentSummary, error) {
	var response api.EnvironmentSummary
	if err := c.postJSON(ctx, "/api/projects/"+url.PathEscape(projectID)+"/environments", request, &response); err != nil {
		return api.EnvironmentSummary{}, err
	}
	return response, nil
}

func (c *Client) UpdateEnvironment(ctx context.Context, projectID string, environmentID string, request api.UpdateEnvironmentRequest) (api.EnvironmentSummary, error) {
	var response api.EnvironmentSummary
	if err := c.patchJSON(ctx, projectEnvironmentPath(projectID, environmentID), request, &response); err != nil {
		return api.EnvironmentSummary{}, err
	}
	return response, nil
}

func (c *Client) DeleteEnvironment(ctx context.Context, projectID string, environmentID string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, projectEnvironmentPath(projectID, environmentID), nil)
	if err != nil {
		return err
	}
	return c.doJSON(req, nil)
}

func projectEnvironmentPath(projectID string, environmentID string) string {
	return "/api/projects/" + url.PathEscape(projectID) + "/environments/" + url.PathEscape(environmentID)
}

func (c *Client) environmentScopedPath(projectID string, environmentID string, suffix string) (string, bool, error) {
	projectID = strings.TrimSpace(projectID)
	environmentID = strings.TrimSpace(environmentID)
	if projectID == "" && environmentID == "" {
		if c.sessionScopedRoutes {
			return "", false, fmt.Errorf("project and environment are required for session-scoped API routes")
		}
		return "/api" + suffix, false, nil
	}
	if !c.sessionScopedRoutes {
		return "", false, errors.New("project and environment scope is only accepted on session-scoped API routes")
	}
	if projectID == "" || environmentID == "" {
		return "", false, fmt.Errorf("project and environment are required for session-scoped API routes")
	}
	return projectEnvironmentPath(projectID, environmentID) + suffix, true, nil
}

func environmentScopedResourcePath(base string, id string, suffix string) string {
	return base + "/" + url.PathEscape(id) + suffix
}

func (c *Client) CreateDeployment(ctx context.Context, input api.CreateDeploymentRequest, sourceTarPath string) (api.DeploymentResponse, error) {
	file, err := os.Open(sourceTarPath)
	if err != nil {
		return api.DeploymentResponse{}, fmt.Errorf("open deployment source archive: %w", err)
	}
	defer file.Close()
	digest, err := deploymentSourceDigest(file)
	if err != nil {
		return api.DeploymentResponse{}, fmt.Errorf("hash deployment source archive: %w", err)
	}
	if input.ContentHash = strings.TrimSpace(input.ContentHash); input.ContentHash == "" {
		input.ContentHash = digest
	} else if input.ContentHash != digest {
		return api.DeploymentResponse{}, fmt.Errorf("deployment source archive digest %s does not match metadata content_hash %s", digest, input.ContentHash)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return api.DeploymentResponse{}, fmt.Errorf("rewind deployment source archive: %w", err)
	}
	path, scoped, err := c.environmentScopedPath(input.ProjectID, input.EnvironmentID, "/deployments")
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	if scoped {
		input.ProjectID = ""
		input.EnvironmentID = ""
	}
	reader, pipeWriter := io.Pipe()
	multipartWriter := multipart.NewWriter(pipeWriter)
	go func() {
		err := writeDeploymentMultipart(multipartWriter, input, file)
		_ = pipeWriter.CloseWithError(err)
	}()
	req, err := c.newRequestWithBearer(ctx, http.MethodPost, path, reader, c.bearer)
	if err != nil {
		_ = reader.Close()
		return api.DeploymentResponse{}, err
	}
	req.Header.Set("content-type", multipartWriter.FormDataContentType())
	var response api.DeploymentResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.DeploymentResponse{}, err
	}
	return response, nil
}

func (c *Client) GetDeployment(ctx context.Context, deploymentID string, input api.GetDeploymentRequest) (api.DeploymentResponse, error) {
	basePath, _, err := c.environmentScopedPath(input.ProjectID, input.EnvironmentID, "/deployments")
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	path := environmentScopedResourcePath(basePath, deploymentID, "")
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	var response api.DeploymentResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.DeploymentResponse{}, err
	}
	return response, nil
}

func (c *Client) FollowDeploymentEvents(ctx context.Context, deploymentID string, input api.GetDeploymentRequest, cursor int64, handle func(api.RunEvent) error) error {
	basePath, _, err := c.environmentScopedPath(input.ProjectID, input.EnvironmentID, "/deployments")
	if err != nil {
		return err
	}
	values := url.Values{}
	values.Set("follow", "1")
	path := environmentScopedResourcePath(basePath, deploymentID, "/events") + "?" + values.Encode()
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("accept", "text/event-stream")
	if cursor > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(cursor, 10))
	}
	res, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return decodeError(res)
	}
	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var event api.RunEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return err
		}
		if err := handle(event); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (c *Client) PromoteDeployment(ctx context.Context, deployment string, input api.PromoteDeploymentRequest) (api.DeploymentResponse, error) {
	basePath, scoped, err := c.environmentScopedPath(input.ProjectID, input.EnvironmentID, "/deployments")
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	if scoped {
		input.ProjectID = ""
		input.EnvironmentID = ""
	}
	path := environmentScopedResourcePath(basePath, deployment, "/promote")
	var response api.DeploymentResponse
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.DeploymentResponse{}, err
	}
	return response, nil
}

func deploymentSourceDigest(source io.Reader) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, source); err != nil {
		return "", err
	}
	return sha256sum.DigestHash(hash), nil
}

func writeDeploymentMultipart(writer *multipart.Writer, input api.CreateDeploymentRequest, source io.Reader) error {
	metadata, err := json.Marshal(input)
	if err != nil {
		_ = writer.Close()
		return fmt.Errorf("encode deployment metadata: %w", err)
	}
	if err := writer.WriteField("metadata", string(metadata)); err != nil {
		_ = writer.Close()
		return err
	}
	part, err := writer.CreateFormFile("deployment_source", "deployment-source.tar")
	if err != nil {
		_ = writer.Close()
		return err
	}
	if _, err := io.Copy(part, source); err != nil {
		_ = writer.Close()
		return err
	}
	return writer.Close()
}

type SecretOptions struct {
	ProjectID     string
	EnvironmentID string
}

type WaitpointTokenOptions struct {
	ProjectID     string
	EnvironmentID string
	Status        string
}

func (c *Client) ListSecrets(ctx context.Context, opts ...SecretOptions) (api.ListSecretsResponse, error) {
	path, err := c.secretCollectionPath(opts...)
	if err != nil {
		return api.ListSecretsResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ListSecretsResponse{}, err
	}
	var response api.ListSecretsResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListSecretsResponse{}, err
	}
	return response, nil
}

func (c *Client) GetSecret(ctx context.Context, name string, opts ...SecretOptions) (api.SecretResponse, error) {
	path, err := c.secretItemPath(name, opts...)
	if err != nil {
		return api.SecretResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.SecretResponse{}, err
	}
	var response api.SecretResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.SecretResponse{}, err
	}
	return response, nil
}

func (c *Client) SetSecret(ctx context.Context, name string, value string, opts ...SecretOptions) (api.SecretResponse, error) {
	var response api.SecretResponse
	request := api.SetSecretRequest{Value: value}
	path, scoped, err := c.secretItemPathWithScope(name, opts...)
	if err != nil {
		return api.SecretResponse{}, err
	}
	if len(opts) > 0 {
		request.ProjectID = opts[0].ProjectID
		request.EnvironmentID = opts[0].EnvironmentID
	}
	if scoped {
		request.ProjectID = ""
		request.EnvironmentID = ""
	}
	if err := c.putJSON(ctx, path, request, &response); err != nil {
		return api.SecretResponse{}, err
	}
	return response, nil
}

func (c *Client) DeleteSecret(ctx context.Context, name string, opts ...SecretOptions) error {
	path, err := c.secretItemPath(name, opts...)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	return c.doJSON(req, nil)
}

func (c *Client) CreateWaitpointToken(ctx context.Context, request api.CreateWaitpointTokenRequest, opts ...WaitpointTokenOptions) (api.WaitpointTokenResponse, error) {
	path, err := c.waitpointTokenCollectionPath(opts...)
	if err != nil {
		return api.WaitpointTokenResponse{}, err
	}
	var response api.WaitpointTokenResponse
	if err := c.postJSON(ctx, path, request, &response); err != nil {
		return api.WaitpointTokenResponse{}, err
	}
	return response, nil
}

func (c *Client) ListWaitpointTokens(ctx context.Context, opts ...WaitpointTokenOptions) (api.ListWaitpointTokensResponse, error) {
	path, err := c.waitpointTokenCollectionPath(opts...)
	if err != nil {
		return api.ListWaitpointTokensResponse{}, err
	}
	if len(opts) > 0 && strings.TrimSpace(opts[0].Status) != "" {
		values := url.Values{}
		values.Set("status", strings.TrimSpace(opts[0].Status))
		path += "?" + values.Encode()
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ListWaitpointTokensResponse{}, err
	}
	var response api.ListWaitpointTokensResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListWaitpointTokensResponse{}, err
	}
	return response, nil
}

func (c *Client) GetWaitpointToken(ctx context.Context, tokenID string, opts ...WaitpointTokenOptions) (api.WaitpointTokenResponse, error) {
	path, err := c.waitpointTokenCollectionPath(opts...)
	if err != nil {
		return api.WaitpointTokenResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path+"/"+url.PathEscape(tokenID), nil)
	if err != nil {
		return api.WaitpointTokenResponse{}, err
	}
	var response api.WaitpointTokenResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.WaitpointTokenResponse{}, err
	}
	return response, nil
}

func (c *Client) CompleteWaitpointToken(ctx context.Context, tokenID string, request api.CompleteWaitpointTokenRequest) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(request); err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/api/waitpoints/tokens/"+url.PathEscape(tokenID)+"/complete", &body)
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	return c.doJSON(req, nil)
}

func (c *Client) secretCollectionPath(opts ...SecretOptions) (string, error) {
	hasScope := len(opts) > 0 && (strings.TrimSpace(opts[0].ProjectID) != "" || strings.TrimSpace(opts[0].EnvironmentID) != "")
	if hasScope && c.sessionScopedRoutes {
		return c.secretCollectionPathWithScope(opts[0])
	}
	if hasScope {
		return "", errors.New("project and environment scope is only accepted on session-scoped API routes")
	}
	if c.sessionScopedRoutes {
		return c.secretCollectionPathWithScope(SecretOptions{})
	}
	return "/api/secrets", nil
}

func (c *Client) waitpointTokenCollectionPath(opts ...WaitpointTokenOptions) (string, error) {
	hasScope := len(opts) > 0 && (strings.TrimSpace(opts[0].ProjectID) != "" || strings.TrimSpace(opts[0].EnvironmentID) != "")
	if hasScope && c.sessionScopedRoutes {
		path, _, err := c.environmentScopedPath(opts[0].ProjectID, opts[0].EnvironmentID, "/waitpoints/tokens")
		return path, err
	}
	if hasScope {
		return "", errors.New("project and environment scope is only accepted on session-scoped API routes")
	}
	if c.sessionScopedRoutes {
		path, _, err := c.environmentScopedPath("", "", "/waitpoints/tokens")
		return path, err
	}
	return "/api/waitpoints/tokens", nil
}

func (c *Client) secretCollectionPathWithScope(opts SecretOptions) (string, error) {
	path, _, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/secrets")
	return path, err
}

func (c *Client) secretItemPath(name string, opts ...SecretOptions) (string, error) {
	path, _, err := c.secretItemPathWithScope(name, opts...)
	return path, err
}

func (c *Client) secretItemPathWithScope(name string, opts ...SecretOptions) (string, bool, error) {
	hasScope := len(opts) > 0 && (strings.TrimSpace(opts[0].ProjectID) != "" || strings.TrimSpace(opts[0].EnvironmentID) != "")
	if c.sessionScopedRoutes {
		scope := SecretOptions{}
		if len(opts) > 0 {
			scope = opts[0]
		}
		basePath, scoped, err := c.environmentScopedPath(scope.ProjectID, scope.EnvironmentID, "/secrets")
		if err != nil {
			return "", false, err
		}
		return environmentScopedResourcePath(basePath, name, ""), scoped, nil
	}
	if hasScope {
		return "", false, errors.New("project and environment scope is only accepted on session-scoped API routes")
	}
	return "/api/secrets/" + url.PathEscape(name), false, nil
}

type RunScopeOptions struct {
	ProjectID     string
	EnvironmentID string
}

func (c *Client) GetRun(ctx context.Context, id string, opts ...RunScopeOptions) (api.RunResponse, error) {
	path, err := c.runItemPath(id, "", opts...)
	if err != nil {
		return api.RunResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.RunResponse{}, err
	}
	var response api.RunResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.RunResponse{}, err
	}
	return response, nil
}

func (c *Client) CancelRun(ctx context.Context, id string, input api.CancelRunRequest, opts ...RunScopeOptions) (api.CancelRunResponse, error) {
	var response api.CancelRunResponse
	path, err := c.runItemPath(id, "/cancel", opts...)
	if err != nil {
		return api.CancelRunResponse{}, err
	}
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.CancelRunResponse{}, err
	}
	return response, nil
}

type ListRunsOptions struct {
	Status        string
	Limit         int32
	SessionID     string
	ProjectID     string
	EnvironmentID string
}

type ListRunEventsOptions struct {
	Cursor int64
	Limit  int32
	RunScopeOptions
}

func (c *Client) runItemPath(id string, suffix string, opts ...RunScopeOptions) (string, error) {
	scope := RunScopeOptions{}
	if len(opts) > 0 {
		scope = opts[0]
	}
	basePath, _, err := c.environmentScopedPath(scope.ProjectID, scope.EnvironmentID, "/runs")
	if err != nil {
		return "", err
	}
	return environmentScopedResourcePath(basePath, id, suffix), nil
}

func (c *Client) ListRuns(ctx context.Context, opts ...ListRunsOptions) (api.ListRunsResponse, error) {
	scope := RunScopeOptions{}
	if len(opts) > 0 {
		scope.ProjectID = opts[0].ProjectID
		scope.EnvironmentID = opts[0].EnvironmentID
	}
	path, _, err := c.environmentScopedPath(scope.ProjectID, scope.EnvironmentID, "/runs")
	if err != nil {
		return api.ListRunsResponse{}, err
	}
	if len(opts) > 0 {
		values := url.Values{}
		if opts[0].Status != "" {
			values.Set("status", opts[0].Status)
		}
		if opts[0].Limit > 0 {
			values.Set("limit", strconv.FormatInt(int64(opts[0].Limit), 10))
		}
		if strings.TrimSpace(opts[0].SessionID) != "" {
			values.Set("session_id", strings.TrimSpace(opts[0].SessionID))
		}
		if encoded := values.Encode(); encoded != "" {
			path += "?" + encoded
		}
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ListRunsResponse{}, err
	}
	var response api.ListRunsResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListRunsResponse{}, err
	}
	return response, nil
}

func (c *Client) GetRunLogs(ctx context.Context, id string, opts ...RunScopeOptions) (api.LogSnapshotResponse, error) {
	path, err := c.runItemPath(id, "/logs", opts...)
	if err != nil {
		return api.LogSnapshotResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.LogSnapshotResponse{}, err
	}
	var response api.LogSnapshotResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.LogSnapshotResponse{}, err
	}
	return response, nil
}

func (c *Client) FollowRunLogs(ctx context.Context, id string, cursor int64, handle func(api.RunLogChunk) error, opts ...RunScopeOptions) error {
	values := url.Values{}
	values.Set("follow", "1")
	path, err := c.runItemPath(id, "/logs", opts...)
	if err != nil {
		return err
	}
	path += "?" + values.Encode()
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("accept", "text/event-stream")
	if cursor > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(cursor, 10))
	}
	res, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return decodeError(res)
	}
	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var chunk api.RunLogChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return err
		}
		if err := handle(chunk); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (c *Client) ListRunEvents(ctx context.Context, id string, opts ...ListRunEventsOptions) (api.RunEventPage, error) {
	scope := RunScopeOptions{}
	if len(opts) > 0 {
		scope = opts[0].RunScopeOptions
	}
	path, err := c.runItemPath(id, "/events", scope)
	if err != nil {
		return api.RunEventPage{}, err
	}
	if len(opts) > 0 {
		values := url.Values{}
		if opts[0].Cursor > 0 {
			values.Set("cursor", strconv.FormatInt(opts[0].Cursor, 10))
		}
		if opts[0].Limit > 0 {
			values.Set("limit", strconv.FormatInt(int64(opts[0].Limit), 10))
		}
		if encoded := values.Encode(); encoded != "" {
			path += "?" + encoded
		}
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.RunEventPage{}, err
	}
	var response api.RunEventPage
	if err := c.doJSON(req, &response); err != nil {
		return api.RunEventPage{}, err
	}
	return response, nil
}

func (c *Client) FollowRunEvents(ctx context.Context, id string, cursor int64, handle func(api.RunEvent) error, opts ...RunScopeOptions) error {
	values := url.Values{}
	values.Set("follow", "1")
	path, err := c.runItemPath(id, "/events", opts...)
	if err != nil {
		return err
	}
	path += "?" + values.Encode()
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("accept", "text/event-stream")
	if cursor > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(cursor, 10))
	}
	res, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return decodeError(res)
	}
	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var event api.RunEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return err
		}
		if err := handle(event); err != nil {
			return err
		}
	}
	return scanner.Err()
}
