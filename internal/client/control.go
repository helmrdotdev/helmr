package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
)

func (c *Client) CreateRun(ctx context.Context, input api.CreateRunRequest) (api.RunResponse, error) {
	var response api.RunResponse
	if err := c.postJSON(ctx, "/api/runs", input, &response); err != nil {
		return api.RunResponse{}, err
	}
	return response, nil
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
	reader, pipeWriter := io.Pipe()
	multipartWriter := multipart.NewWriter(pipeWriter)
	go func() {
		err := writeDeploymentMultipart(multipartWriter, input, file)
		_ = pipeWriter.CloseWithError(err)
	}()
	req, err := c.newRequestWithBearer(ctx, http.MethodPost, "/api/deployments", reader, c.bearer)
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

func deploymentSourceDigest(source io.Reader) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, source); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
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

type SetSecretOptions struct {
	ProjectID     string
	EnvironmentID string
}

func (c *Client) SetSecret(ctx context.Context, name string, value string, opts ...SetSecretOptions) (api.SecretResponse, error) {
	var response api.SecretResponse
	request := api.SetSecretRequest{Value: value}
	if len(opts) > 0 {
		request.ProjectID = opts[0].ProjectID
		request.EnvironmentID = opts[0].EnvironmentID
	}
	if err := c.putJSON(ctx, "/api/secrets/"+url.PathEscape(name), request, &response); err != nil {
		return api.SecretResponse{}, err
	}
	return response, nil
}

func (c *Client) GetRun(ctx context.Context, id string) (api.RunResponse, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/runs/"+url.PathEscape(id), nil)
	if err != nil {
		return api.RunResponse{}, err
	}
	var response api.RunResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.RunResponse{}, err
	}
	return response, nil
}

type ListRunsOptions struct {
	Status        string
	Limit         int32
	ProjectID     string
	EnvironmentID string
}

type ListRunEventsOptions struct {
	Cursor int64
	Limit  int32
}

func (c *Client) ListRuns(ctx context.Context, opts ...ListRunsOptions) (api.ListRunsResponse, error) {
	path := "/api/runs"
	if len(opts) > 0 {
		values := url.Values{}
		if opts[0].Status != "" {
			values.Set("status", opts[0].Status)
		}
		if opts[0].Limit > 0 {
			values.Set("limit", strconv.FormatInt(int64(opts[0].Limit), 10))
		}
		if opts[0].ProjectID != "" {
			values.Set("project_id", opts[0].ProjectID)
		}
		if opts[0].EnvironmentID != "" {
			values.Set("environment_id", opts[0].EnvironmentID)
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

func (c *Client) GetRunLogs(ctx context.Context, id string) (api.LogSnapshotResponse, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/runs/"+url.PathEscape(id)+"/logs", nil)
	if err != nil {
		return api.LogSnapshotResponse{}, err
	}
	var response api.LogSnapshotResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.LogSnapshotResponse{}, err
	}
	return response, nil
}

func (c *Client) ListRunEvents(ctx context.Context, id string, opts ...ListRunEventsOptions) (api.RunEventPage, error) {
	path := "/api/runs/" + url.PathEscape(id) + "/events"
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

func (c *Client) ApproveWaitpoint(ctx context.Context, runID string, waitpointID string, request api.ResumeApprovalRequest) error {
	return c.postJSON(ctx, waitpointPath(runID, waitpointID, "approve"), request, nil)
}

func (c *Client) DenyWaitpoint(ctx context.Context, runID string, waitpointID string, request api.ResumeApprovalRequest) error {
	return c.postJSON(ctx, waitpointPath(runID, waitpointID, "deny"), request, nil)
}

func (c *Client) MessageWaitpoint(ctx context.Context, runID string, waitpointID string, request api.ResumeMessageRequest) error {
	return c.postJSON(ctx, waitpointPath(runID, waitpointID, "message"), request, nil)
}

func (c *Client) ListWaitpointPolicies(ctx context.Context) (api.ListWaitpointPoliciesResponse, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/waitpoint-policies", nil)
	if err != nil {
		return api.ListWaitpointPoliciesResponse{}, err
	}
	var response api.ListWaitpointPoliciesResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListWaitpointPoliciesResponse{}, err
	}
	return response, nil
}

func (c *Client) GetWaitpointPolicy(ctx context.Context, name string) (api.WaitpointPolicyResponse, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/waitpoint-policies/"+url.PathEscape(name), nil)
	if err != nil {
		return api.WaitpointPolicyResponse{}, err
	}
	var response api.WaitpointPolicyResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.WaitpointPolicyResponse{}, err
	}
	return response, nil
}

func (c *Client) CreateWaitpointPolicy(ctx context.Context, request api.CreateWaitpointPolicyRequest) (api.WaitpointPolicyResponse, error) {
	var response api.WaitpointPolicyResponse
	if err := c.postJSON(ctx, "/api/waitpoint-policies", request, &response); err != nil {
		return api.WaitpointPolicyResponse{}, err
	}
	return response, nil
}

func (c *Client) UpdateWaitpointPolicy(ctx context.Context, name string, request api.UpdateWaitpointPolicyRequest) (api.WaitpointPolicyResponse, error) {
	var response api.WaitpointPolicyResponse
	if err := c.patchJSON(ctx, "/api/waitpoint-policies/"+url.PathEscape(name), request, &response); err != nil {
		return api.WaitpointPolicyResponse{}, err
	}
	return response, nil
}

func (c *Client) ApplyWaitpointPolicy(ctx context.Context, name string, request api.UpdateWaitpointPolicyRequest) (api.WaitpointPolicyResponse, error) {
	policy, err := c.UpdateWaitpointPolicy(ctx, name, request)
	if err == nil {
		return policy, nil
	}
	if !IsStatus(err, http.StatusNotFound) {
		return api.WaitpointPolicyResponse{}, err
	}
	return c.CreateWaitpointPolicy(ctx, api.CreateWaitpointPolicyRequest{
		Name:   name,
		Label:  request.Label,
		Config: request.Config,
	})
}

func (c *Client) DisableWaitpointPolicy(ctx context.Context, name string) error {
	return c.postJSON(ctx, "/api/waitpoint-policies/"+url.PathEscape(name)+"/disable", map[string]any{}, nil)
}

func waitpointPath(runID string, waitpointID string, action string) string {
	return "/api/runs/" + url.PathEscape(runID) + "/waitpoints/" + url.PathEscape(waitpointID) + "/" + action
}
