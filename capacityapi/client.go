package capacityapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	RoutePrefix                  = "/capacity/v0"
	WorkerGroupsPath             = "/worker-groups"
	WorkerInstancesPath          = "/worker-instances"
	ErrorCodeQueuedDemandPresent = "queued_demand_present"
	maximumResponseBytes         = int64(4 << 20)
	capacityTokenDecodedBytes    = 32
)

type HTTPError struct {
	StatusCode int
	Status     string
	Code       string
	Message    string
	Details    map[string]json.RawMessage
}

func (e *HTTPError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("capacity control plane returned %s", e.Status)
	}
	return fmt.Sprintf("capacity control plane returned %s: %s", e.Status, e.Message)
}

type Client struct {
	baseURL *url.URL
	token   string
	http    *http.Client
}

type Option func(*Client)

func WithHTTPClient(client *http.Client) Option {
	return func(target *Client) {
		if client != nil {
			target.http = client
		}
	}
}

func NewClient(rawBaseURL, token string, options ...Option) (*Client, error) {
	baseURL, err := url.Parse(strings.TrimSpace(rawBaseURL))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, errors.New("capacity control plane URL must be an absolute URL")
	}
	localHTTP := baseURL.Scheme == "http" && (baseURL.Hostname() == "127.0.0.1" || baseURL.Hostname() == "localhost")
	if baseURL.Scheme != "https" && !localHTTP {
		return nil, errors.New("capacity control plane URL must use HTTPS outside localhost")
	}
	token = strings.TrimSpace(token)
	decodedToken, decodeErr := base64.RawURLEncoding.DecodeString(token)
	if decodeErr != nil || len(decodedToken) != capacityTokenDecodedBytes || base64.RawURLEncoding.EncodeToString(decodedToken) != token {
		return nil, errors.New("capacity token must be a canonical base64url-no-pad encoding of exactly 32 bytes")
	}
	client := &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 20 * time.Second},
	}
	for _, option := range options {
		option(client)
	}
	return client, nil
}

func (c *Client) Plan(ctx context.Context, workerGroupID string, request CapacityPlanRequest) (CapacityPlanResponse, error) {
	var response CapacityPlanResponse
	path := "/api" + RoutePrefix + WorkerGroupsPath + "/" + url.PathEscape(workerGroupID) + "/plan"
	err := c.do(ctx, http.MethodPost, path, request, &response)
	return response, err
}

func (c *Client) ReconcileWorkerGroupPrimaryPools(
	ctx context.Context,
	workerGroupID string,
	request ReconcileWorkerGroupPrimaryPoolsRequest,
) (ReconcileWorkerGroupPrimaryPoolsResponse, error) {
	var response ReconcileWorkerGroupPrimaryPoolsResponse
	path := "/api" + RoutePrefix + WorkerGroupsPath + "/" + url.PathEscape(workerGroupID) + "/primary-pools"
	err := c.do(ctx, http.MethodPut, path, request, &response)
	return response, err
}

func (c *Client) ResolveWorkerGroup(ctx context.Context, regionID, name string) (CapacityWorkerGroup, error) {
	query := url.Values{"region_id": []string{regionID}, "name": []string{name}}
	path := "/api" + RoutePrefix + WorkerGroupsPath + "/resolve?" + query.Encode()
	var response CapacityWorkerGroup
	err := c.do(ctx, http.MethodGet, path, nil, &response)
	return response, err
}

func (c *Client) ResolveWorkerPool(ctx context.Context, workerGroupID, name string) (CapacityWorkerPool, error) {
	query := url.Values{"name": []string{name}}
	path := "/api" + RoutePrefix + WorkerGroupsPath + "/" + url.PathEscape(workerGroupID) + "/pools/resolve?" + query.Encode()
	var response CapacityWorkerPool
	err := c.do(ctx, http.MethodGet, path, nil, &response)
	return response, err
}

func (c *Client) WorkerInstances(ctx context.Context, workerGroupID string, resourceIDs []string, statuses []WorkerInstanceStatus, hasUnreclaimedRuntime bool, limit int32) (WorkerInstancesResponse, error) {
	query := url.Values{}
	if workerGroupID = strings.TrimSpace(workerGroupID); workerGroupID != "" {
		query.Set("worker_group_id", workerGroupID)
	}
	for _, status := range statuses {
		query.Add("status", string(status))
	}
	for _, resourceID := range resourceIDs {
		query.Add("resource_id", resourceID)
	}
	if hasUnreclaimedRuntime {
		query.Set("has_unreclaimed_runtime", "true")
	}
	if limit > 0 {
		query.Set("limit", strconv.FormatInt(int64(limit), 10))
	}
	path := "/api" + RoutePrefix + WorkerInstancesPath
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var response WorkerInstancesResponse
	err := c.do(ctx, http.MethodGet, path, nil, &response)
	return response, err
}

func (c *Client) ConfirmWorkerInstanceProviderAbsent(ctx context.Context, workerInstanceID string) (WorkerInstance, error) {
	var response WorkerInstance
	path := "/api" + RoutePrefix + WorkerInstancesPath + "/" + url.PathEscape(workerInstanceID) + "/lost"
	err := c.do(ctx, http.MethodPost, path, nil, &response)
	return response, err
}

func (c *Client) WorkerInstance(ctx context.Context, workerInstanceID string) (WorkerInstance, error) {
	var response WorkerInstance
	path := "/api" + RoutePrefix + WorkerInstancesPath + "/" + url.PathEscape(workerInstanceID)
	err := c.do(ctx, http.MethodGet, path, nil, &response)
	return response, err
}

func (c *Client) DrainWorkerInstance(ctx context.Context, workerInstanceID string, request DrainWorkerInstanceRequest) (WorkerInstance, error) {
	var response WorkerInstance
	path := "/api" + RoutePrefix + WorkerInstancesPath + "/" + url.PathEscape(workerInstanceID) + "/drain"
	err := c.do(ctx, http.MethodPost, path, request, &response)
	return response, err
}

func (c *Client) do(ctx context.Context, method, path string, requestBody, responseBody any) error {
	reference, err := url.Parse(path)
	if err != nil {
		return err
	}
	target := c.baseURL.ResolveReference(reference)
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes+1))
	if readErr != nil {
		return fmt.Errorf("read capacity control plane response: %w", readErr)
	}
	if int64(len(payload)) > maximumResponseBytes {
		return errors.New("capacity control plane response exceeds the maximum size")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		var envelope struct {
			Error struct {
				Code    string                     `json:"code"`
				Message string                     `json:"message"`
				Details map[string]json.RawMessage `json:"details"`
			} `json:"error"`
		}
		if json.Unmarshal(payload, &envelope) != nil {
			return &HTTPError{StatusCode: response.StatusCode, Status: response.Status}
		}
		return &HTTPError{
			StatusCode: response.StatusCode, Status: response.Status,
			Code: envelope.Error.Code, Message: envelope.Error.Message, Details: envelope.Error.Details,
		}
	}
	if responseBody == nil {
		return nil
	}
	if err := json.Unmarshal(payload, responseBody); err != nil {
		return fmt.Errorf("decode capacity control plane response: %w", err)
	}
	return nil
}
