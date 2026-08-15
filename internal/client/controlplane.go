package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/ids"
)

var ErrDeploymentObjectUploadNotAttempted = errors.New("deployment object upload was not attempted")

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

func (c *Client) environmentScopedPath(projectID string, environmentID string, suffix string) (string, error) {
	if projectID == "" && environmentID == "" {
		if c.sessionScopedRoutes {
			return "", fmt.Errorf("project and environment are required for session-scoped API routes")
		}
		return "/v1" + suffix, nil
	}
	if !c.sessionScopedRoutes {
		return "", errors.New("project and environment scope is only accepted on session-scoped API routes")
	}
	if projectID == "" || environmentID == "" {
		return "", fmt.Errorf("project and environment are required for session-scoped API routes")
	}
	return projectEnvironmentPath(projectID, environmentID) + suffix, nil
}

func environmentScopedResourcePath(base string, id string, suffix string) string {
	return base + "/" + url.PathEscape(id) + suffix
}

func (c *Client) PlanDeploymentBundleUploads(
	ctx context.Context,
	bundleJSON []byte,
	scope EnvironmentScopeOptions,
) (api.DeploymentBundleUploadPlanResponse, error) {
	path, err := c.environmentScopedPath(scope.ProjectID, scope.EnvironmentID, "/deployment-bundles/upload-plan")
	if err != nil {
		return api.DeploymentBundleUploadPlanResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, bytes.NewReader(bundleJSON))
	if err != nil {
		return api.DeploymentBundleUploadPlanResponse{}, err
	}
	req.Header.Set("content-type", "application/vnd.helmr.deployment-bundle.v0+json")
	var response api.DeploymentBundleUploadPlanResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.DeploymentBundleUploadPlanResponse{}, err
	}
	return response, nil
}

func (c *Client) UploadDeploymentBundleObject(
	ctx context.Context,
	upload api.DeploymentBundleUpload,
	objectPath string,
) error {
	if upload.Method != http.MethodPut || strings.TrimSpace(upload.URL) == "" {
		return fmt.Errorf("%w: deployment object upload plan is invalid", ErrDeploymentObjectUploadNotAttempted)
	}
	file, err := os.Open(objectPath)
	if err != nil {
		return fmt.Errorf("%w: open deployment object: %w", ErrDeploymentObjectUploadNotAttempted, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("%w: stat deployment object: %w", ErrDeploymentObjectUploadNotAttempted, err)
	}
	req, err := http.NewRequestWithContext(ctx, upload.Method, upload.URL, file)
	if err != nil {
		return fmt.Errorf("%w: construct deployment object upload", ErrDeploymentObjectUploadNotAttempted)
	}
	contentLengthSet := false
	for name, value := range upload.Headers {
		if strings.EqualFold(name, "authorization") {
			return fmt.Errorf("%w: deployment object upload plan contains a credential header", ErrDeploymentObjectUploadNotAttempted)
		}
		if strings.EqualFold(name, "content-length") {
			if contentLengthSet {
				return fmt.Errorf("%w: deployment object upload plan contains duplicate content length headers", ErrDeploymentObjectUploadNotAttempted)
			}
			contentLength, parseErr := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if parseErr != nil || contentLength < 0 {
				return fmt.Errorf("%w: deployment object upload plan contains an invalid content length", ErrDeploymentObjectUploadNotAttempted)
			}
			if contentLength != info.Size() {
				return fmt.Errorf("%w: deployment object size differs from the upload plan", ErrDeploymentObjectUploadNotAttempted)
			}
			req.ContentLength = contentLength
			contentLengthSet = true
			continue
		}
		req.Header.Set(name, value)
	}
	if !contentLengthSet {
		return fmt.Errorf("%w: deployment object upload plan is missing content length", ErrDeploymentObjectUploadNotAttempted)
	}
	response, err := c.transport.Do(req)
	if err != nil {
		var statusError interface{ HTTPStatusCode() int }
		if errors.As(err, &statusError) && statusError.HTTPStatusCode() != 0 {
			return err
		}
		return errors.New("deployment object upload transport failed")
	}
	defer response.Body.Close()
	_, copyErr := io.Copy(io.Discard, response.Body)
	if copyErr != nil {
		return errors.New("read deployment object upload response")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("deployment object upload returned %s", response.Status)
	}
	return nil
}

func (c *Client) FinalizeDeploymentBundle(
	ctx context.Context,
	input api.FinalizeDeploymentBundleRequest,
	scope EnvironmentScopeOptions,
	progress func(api.DeploymentBundleFinalizeObject) error,
) (api.DeploymentResponse, error) {
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	input.BundleDigest = strings.TrimSpace(input.BundleDigest)
	if input.IdempotencyKey == "" || input.BundleDigest == "" {
		return api.DeploymentResponse{}, errors.New("deployment idempotency key and bundle digest are required")
	}
	path, err := c.environmentScopedPath(scope.ProjectID, scope.EnvironmentID, "/deployment-bundles/finalize")
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(input); err != nil {
		return api.DeploymentResponse{}, fmt.Errorf("encode deployment finalization request: %w", err)
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, bytes.NewReader(body.Bytes()))
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "text/event-stream")
	response, err := c.transport.Do(req)
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	defer response.Body.Close()
	mediaType, _, parseErr := mime.ParseMediaType(response.Header.Get("content-type"))
	if parseErr != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		return api.DeploymentResponse{}, errors.New("deployment finalization response is not an event stream")
	}
	created, err := consumeDeploymentFinalizeStream(response.Body, input.BundleDigest, progress)
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	if created.BundleDigest != input.BundleDigest {
		return api.DeploymentResponse{}, errors.New("deployment finalization completed with another bundle digest")
	}
	return created, nil
}

type DeploymentFinalizeError struct {
	Code    string
	Message string
}

func (e *DeploymentFinalizeError) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Message
}

type deploymentFinalizeObserverError struct{ err error }

func (e deploymentFinalizeObserverError) Error() string { return e.err.Error() }
func (e deploymentFinalizeObserverError) Unwrap() error { return e.err }

func consumeDeploymentFinalizeStream(
	reader io.Reader,
	expectedBundleDigest string,
	progress func(api.DeploymentBundleFinalizeObject) error,
) (api.DeploymentResponse, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	var event string
	var data []byte
	started := false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if event == "" && len(data) == 0 {
				continue
			}
			if event == "" || len(data) == 0 {
				return api.DeploymentResponse{}, errors.New("deployment finalization event is incomplete")
			}
			switch event {
			case api.DeploymentBundleFinalizeEventStarted:
				if started {
					return api.DeploymentResponse{}, errors.New("deployment finalization stream started more than once")
				}
				var value api.DeploymentBundleFinalizeStarted
				if err := decodeDeploymentFinalizeEvent(data, &value); err != nil || value.BundleDigest != expectedBundleDigest {
					return api.DeploymentResponse{}, errors.New("deployment finalization started event is invalid")
				}
				started = true
			case api.DeploymentBundleFinalizeEventPing:
				if !started || string(data) != "{}" {
					return api.DeploymentResponse{}, errors.New("deployment finalization ping event is invalid")
				}
			case api.DeploymentBundleFinalizeEventObjectVerified:
				if !started {
					return api.DeploymentResponse{}, errors.New("deployment finalization progress preceded the started event")
				}
				var value api.DeploymentBundleFinalizeObject
				if err := decodeDeploymentFinalizeEvent(data, &value); err != nil || strings.TrimSpace(value.Digest) == "" {
					return api.DeploymentResponse{}, errors.New("deployment finalization object event is invalid")
				}
				if progress != nil {
					if err := progress(value); err != nil {
						return api.DeploymentResponse{}, deploymentFinalizeObserverError{err: err}
					}
				}
			case api.DeploymentBundleFinalizeEventComplete:
				if !started {
					return api.DeploymentResponse{}, errors.New("deployment finalization completed before it started")
				}
				var value api.DeploymentResponse
				if err := decodeDeploymentFinalizeEvent(data, &value); err != nil || value.ID == "" || value.BundleDigest == "" {
					return api.DeploymentResponse{}, errors.New("deployment finalization completion event is invalid")
				}
				return value, nil
			case api.DeploymentBundleFinalizeEventError:
				if !started {
					return api.DeploymentResponse{}, errors.New("deployment finalization failed before it started")
				}
				var value api.DeploymentBundleFinalizeError
				if err := decodeDeploymentFinalizeEvent(data, &value); err != nil || value.Code == "" || value.Message == "" {
					return api.DeploymentResponse{}, errors.New("deployment finalization error event is invalid")
				}
				return api.DeploymentResponse{}, &DeploymentFinalizeError{Code: value.Code, Message: value.Message}
			default:
				return api.DeploymentResponse{}, errors.New("deployment finalization stream contains an unknown event")
			}
			event = ""
			data = nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			return api.DeploymentResponse{}, errors.New("deployment finalization stream field is invalid")
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			if event != "" {
				return api.DeploymentResponse{}, errors.New("deployment finalization event name is duplicated")
			}
			event = value
		case "data":
			if data != nil {
				return api.DeploymentResponse{}, errors.New("deployment finalization event data is duplicated")
			}
			data = []byte(value)
		default:
			return api.DeploymentResponse{}, errors.New("deployment finalization stream field is unsupported")
		}
	}
	if err := scanner.Err(); err != nil {
		return api.DeploymentResponse{}, fmt.Errorf("read deployment finalization stream: %w", err)
	}
	return api.DeploymentResponse{}, errors.New("deployment finalization stream ended without a terminal event")
}

func decodeDeploymentFinalizeEvent(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("deployment finalization event contains trailing data")
		}
		return err
	}
	return nil
}

func (c *Client) GetDeployment(ctx context.Context, deploymentID string, scope EnvironmentScopeOptions) (api.DeploymentResponse, error) {
	if err := ids.Validate(deploymentID); err != nil {
		return api.DeploymentResponse{}, err
	}
	basePath, err := c.environmentScopedPath(scope.ProjectID, scope.EnvironmentID, "/deployments")
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

func (c *Client) PromoteDeployment(ctx context.Context, deployment string, input api.PromoteDeploymentRequest, scope EnvironmentScopeOptions) (api.DeploymentResponse, error) {
	if err := ids.Validate(deployment); err != nil {
		return api.DeploymentResponse{}, err
	}
	basePath, err := c.environmentScopedPath(scope.ProjectID, scope.EnvironmentID, "/deployments")
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	path := environmentScopedResourcePath(basePath, deployment, "/promote")
	var response api.DeploymentResponse
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.DeploymentResponse{}, err
	}
	return response, nil
}

type SecretOptions struct {
	ProjectID     string
	EnvironmentID string
	Cursor        string
	Limit         int32
	Name          string
}

func (c *Client) ListSecrets(ctx context.Context, opts ...SecretOptions) (api.ListSecretsResponse, error) {
	path, err := c.secretCollectionPath(opts...)
	if err != nil {
		return api.ListSecretsResponse{}, err
	}
	if len(opts) > 0 {
		values := url.Values{}
		if opts[0].Name != "" {
			if opts[0].Cursor != "" || opts[0].Limit != 0 {
				return api.ListSecretsResponse{}, errors.New("cursor and limit are not accepted with Secret name")
			}
			values.Set("name", opts[0].Name)
		} else {
			if opts[0].Cursor != "" {
				values.Set("cursor", opts[0].Cursor)
			}
			if opts[0].Limit > 0 {
				values.Set("limit", strconv.FormatInt(int64(opts[0].Limit), 10))
			}
		}
		if encoded := values.Encode(); encoded != "" {
			path += "?" + encoded
		}
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

func (c *Client) RetrieveSecret(ctx context.Context, id string, opts ...SecretOptions) (api.SecretResponse, error) {
	path, err := c.secretItemPath(id, opts...)
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

func (c *Client) CreateSecret(ctx context.Context, name string, value string, idempotencyKey string, opts ...SecretOptions) (api.SecretResponse, error) {
	var response api.SecretResponse
	path, err := c.secretCollectionPath(opts...)
	if err != nil {
		return api.SecretResponse{}, err
	}
	if err := c.postJSON(ctx, path, api.CreateSecretRequest{
		Name: name, Value: value, IdempotencyKey: idempotencyKey,
	}, &response); err != nil {
		return api.SecretResponse{}, err
	}
	return response, nil
}

func (c *Client) RotateSecret(ctx context.Context, id string, value string, idempotencyKey string, opts ...SecretOptions) (api.SecretResponse, error) {
	var response api.SecretResponse
	path, err := c.secretItemPath(id, opts...)
	if err != nil {
		return api.SecretResponse{}, err
	}
	if err := c.postJSON(ctx, path+"/rotate", api.RotateSecretRequest{
		Value: value, IdempotencyKey: idempotencyKey,
	}, &response); err != nil {
		return api.SecretResponse{}, err
	}
	return response, nil
}

func (c *Client) RevokeSecret(ctx context.Context, id string, idempotencyKey string, opts ...SecretOptions) (api.SecretResponse, error) {
	var response api.SecretResponse
	path, err := c.secretItemPath(id, opts...)
	if err != nil {
		return api.SecretResponse{}, err
	}
	if err := c.postJSON(ctx, path+"/revoke", api.RevokeSecretRequest{
		IdempotencyKey: idempotencyKey,
	}, &response); err != nil {
		return api.SecretResponse{}, err
	}
	return response, nil
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
	return "/v1/secrets", nil
}

func (c *Client) secretCollectionPathWithScope(opts SecretOptions) (string, error) {
	path, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/secrets")
	return path, err
}

func (c *Client) secretItemPath(id string, opts ...SecretOptions) (string, error) {
	if err := ids.Validate(id); err != nil {
		return "", err
	}
	hasScope := len(opts) > 0 && (strings.TrimSpace(opts[0].ProjectID) != "" || strings.TrimSpace(opts[0].EnvironmentID) != "")
	if c.sessionScopedRoutes {
		scope := SecretOptions{}
		if len(opts) > 0 {
			scope = opts[0]
		}
		basePath, err := c.environmentScopedPath(scope.ProjectID, scope.EnvironmentID, "/secrets")
		if err != nil {
			return "", err
		}
		return environmentScopedResourcePath(basePath, id, ""), nil
	}
	if hasScope {
		return "", errors.New("project and environment scope is only accepted on session-scoped API routes")
	}
	return "/v1/secrets/" + url.PathEscape(id), nil
}

type RunScopeOptions struct {
	ProjectID     string
	EnvironmentID string
}

func (c *Client) GetRun(ctx context.Context, id string, opts ...RunScopeOptions) (api.RunSnapshotResponse, error) {
	path, err := c.runItemPath(id, "", opts...)
	if err != nil {
		return api.RunSnapshotResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.RunSnapshotResponse{}, err
	}
	var response api.RunSnapshotResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.RunSnapshotResponse{}, err
	}
	return response, nil
}

func (c *Client) CancelRun(ctx context.Context, id string, opts ...RunScopeOptions) (api.RunSnapshotResponse, error) {
	path, err := c.runItemPath(id, "/cancel", opts...)
	if err != nil {
		return api.RunSnapshotResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, nil)
	if err != nil {
		return api.RunSnapshotResponse{}, err
	}
	var response api.RunSnapshotResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.RunSnapshotResponse{}, err
	}
	return response, nil
}

type ListRunsOptions struct {
	Statuses      []string
	Cursor        string
	Limit         int32
	ProjectID     string
	EnvironmentID string
}

type ListRunEventsOptions struct {
	Cursor     string
	Limit      int32
	Severities []string
	RunScopeOptions
}

type ListRunLogsOptions struct {
	Cursor string
	Limit  int32
	Levels []string
	RunScopeOptions
}

func (c *Client) runItemPath(id string, suffix string, opts ...RunScopeOptions) (string, error) {
	if err := ids.Validate(id); err != nil {
		return "", err
	}
	scope := RunScopeOptions{}
	if len(opts) > 0 {
		scope = opts[0]
	}
	basePath, err := c.environmentScopedPath(scope.ProjectID, scope.EnvironmentID, "/runs")
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
	path, err := c.environmentScopedPath(scope.ProjectID, scope.EnvironmentID, "/runs")
	if err != nil {
		return api.ListRunsResponse{}, err
	}
	if len(opts) > 0 {
		values := url.Values{}
		for _, status := range opts[0].Statuses {
			if status = strings.TrimSpace(status); status != "" {
				values.Add("status", status)
			}
		}
		if cursor := strings.TrimSpace(opts[0].Cursor); cursor != "" {
			values.Set("cursor", cursor)
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
		return api.ListRunsResponse{}, err
	}
	var response api.ListRunsResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListRunsResponse{}, err
	}
	return response, nil
}

func (c *Client) ListRunLogs(ctx context.Context, id string, opts ...ListRunLogsOptions) (api.RunLogPage, error) {
	scope := RunScopeOptions{}
	if len(opts) > 0 {
		scope = opts[0].RunScopeOptions
	}
	path, err := c.runItemPath(id, "/logs", scope)
	if err != nil {
		return api.RunLogPage{}, err
	}
	if len(opts) > 0 {
		values := url.Values{}
		if opts[0].Cursor != "" {
			values.Set("cursor", opts[0].Cursor)
		}
		if opts[0].Limit > 0 {
			values.Set("limit", strconv.FormatInt(int64(opts[0].Limit), 10))
		}
		for _, level := range opts[0].Levels {
			values.Add("level", level)
		}
		if encoded := values.Encode(); encoded != "" {
			path += "?" + encoded
		}
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.RunLogPage{}, err
	}
	var response api.RunLogPage
	if err := c.doJSON(req, &response); err != nil {
		return api.RunLogPage{}, err
	}
	return response, nil
}

func (c *Client) ListRunEvents(ctx context.Context, id string, opts ...ListRunEventsOptions) (api.RunEventRecordPage, error) {
	scope := RunScopeOptions{}
	if len(opts) > 0 {
		scope = opts[0].RunScopeOptions
	}
	path, err := c.runItemPath(id, "/events", scope)
	if err != nil {
		return api.RunEventRecordPage{}, err
	}
	if len(opts) > 0 {
		values := url.Values{}
		if opts[0].Cursor != "" {
			values.Set("cursor", opts[0].Cursor)
		}
		if opts[0].Limit > 0 {
			values.Set("limit", strconv.FormatInt(int64(opts[0].Limit), 10))
		}
		for _, severity := range opts[0].Severities {
			values.Add("severity", severity)
		}
		if encoded := values.Encode(); encoded != "" {
			path += "?" + encoded
		}
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.RunEventRecordPage{}, err
	}
	var response api.RunEventRecordPage
	if err := c.doJSON(req, &response); err != nil {
		return api.RunEventRecordPage{}, err
	}
	return response, nil
}
