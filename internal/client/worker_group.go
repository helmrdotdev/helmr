package client

import (
	"context"
	"encoding/json"
	"net/url"

	"github.com/helmrdotdev/helmr/internal/api"
)

// IssueWorkerEnrollmentToken mints a single-use enrollment token for one
// machine to join the worker group.
func (c *Client) IssueWorkerEnrollmentToken(ctx context.Context, groupID string) (api.IssueEnrollmentTokenResponse, error) {
	var response api.IssueEnrollmentTokenResponse
	if err := c.postJSON(ctx, "/api/worker-groups/"+url.PathEscape(groupID)+"/enrollment-tokens", struct{}{}, &response); err != nil {
		return api.IssueEnrollmentTokenResponse{}, err
	}
	return response, nil
}

// RevokeWorkerHost revokes a worker host's machine key, so the machine can
// neither authenticate nor enroll with it again.
func (c *Client) RevokeWorkerHost(ctx context.Context, groupID, hostID string) error {
	var response json.RawMessage
	return c.postJSON(ctx, "/api/worker-groups/"+url.PathEscape(groupID)+"/hosts/"+url.PathEscape(hostID)+"/revoke", struct{}{}, &response)
}
