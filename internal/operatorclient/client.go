package operatorclient

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

	"github.com/helmrdotdev/helmr/internal/api"
)

const maximumResponseBytes = int64(4 << 20)
const operatorTokenDecodedBytes = 32

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

func New(rawBaseURL, token string, options ...Option) (*Client, error) {
	baseURL, err := url.Parse(strings.TrimSpace(rawBaseURL))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, errors.New("operator Control URL must be an absolute URL")
	}
	if baseURL.Scheme != "https" && baseURL.Hostname() != "127.0.0.1" && baseURL.Hostname() != "localhost" {
		return nil, errors.New("operator Control URL must use HTTPS outside localhost")
	}
	token = strings.TrimSpace(token)
	decodedToken, decodeErr := base64.RawURLEncoding.DecodeString(token)
	if decodeErr != nil || len(decodedToken) != operatorTokenDecodedBytes || base64.RawURLEncoding.EncodeToString(decodedToken) != token {
		return nil, errors.New("operator token must be a canonical base64url-no-pad encoding of exactly 32 bytes")
	}
	client := &Client{
		baseURL: baseURL,
		token:   token,
		http: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
	for _, option := range options {
		option(client)
	}
	return client, nil
}

func (c *Client) CapacityObservations(ctx context.Context) (api.OperatorCapacityObservationsResponse, error) {
	var response api.OperatorCapacityObservationsResponse
	if err := c.do(ctx, http.MethodGet, "/api/operator/capacity/observations", nil, &response); err != nil {
		return response, err
	}
	return response, nil
}

func (c *Client) WorkerInstances(ctx context.Context, workerGroupID string, resourceIDs, states []string, limit int32) (api.OperatorWorkerInstancesResponse, error) {
	query := url.Values{}
	if workerGroupID = strings.TrimSpace(workerGroupID); workerGroupID != "" {
		query.Set("worker_group_id", workerGroupID)
	}
	for _, state := range states {
		query.Add("state", state)
	}
	for _, resourceID := range resourceIDs {
		query.Add("resource_id", resourceID)
	}
	if limit > 0 {
		query.Set("limit", strconv.FormatInt(int64(limit), 10))
	}
	path := "/api/operator/worker-instances"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var response api.OperatorWorkerInstancesResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &response); err != nil {
		return response, err
	}
	return response, nil
}

func (c *Client) WorkerInstance(ctx context.Context, workerInstanceID string) (api.OperatorWorkerInstance, error) {
	var response api.OperatorWorkerInstance
	path := "/api/operator/worker-instances/" + url.PathEscape(workerInstanceID)
	if err := c.do(ctx, http.MethodGet, path, nil, &response); err != nil {
		return response, err
	}
	return response, nil
}

func (c *Client) DrainWorkerInstance(ctx context.Context, workerInstanceID string, request api.OperatorDrainWorkerInstanceRequest) (api.OperatorWorkerInstance, error) {
	var response api.OperatorWorkerInstance
	path := "/api/operator/worker-instances/" + url.PathEscape(workerInstanceID) + "/drain"
	if err := c.do(ctx, http.MethodPost, path, request, &response); err != nil {
		return response, err
	}
	return response, nil
}

func (c *Client) do(ctx context.Context, method, path string, requestBody any, responseBody any) error {
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
		return fmt.Errorf("read operator Control response: %w", readErr)
	}
	if int64(len(payload)) > maximumResponseBytes {
		return errors.New("operator Control response exceeds the maximum size")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("operator Control returned %s: %s", response.Status, strings.TrimSpace(string(payload)))
	}
	if responseBody == nil {
		return nil
	}
	if err := json.Unmarshal(payload, responseBody); err != nil {
		return fmt.Errorf("decode operator Control response: %w", err)
	}
	return nil
}
