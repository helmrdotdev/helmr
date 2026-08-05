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

const maxScheduleListLimit = int32(100)

type ListSchedulesOptions struct {
	Cursor string
	Limit  int32
	EnvironmentScopeOptions
}

func (c *Client) ListSchedules(
	ctx context.Context,
	opts ListSchedulesOptions,
) (api.ListSchedulesResponse, error) {
	if opts.Limit < 0 || opts.Limit > maxScheduleListLimit {
		return api.ListSchedulesResponse{}, fmt.Errorf(
			"schedule list limit must be in [1,%d] when present",
			maxScheduleListLimit,
		)
	}
	path, err := c.environmentScopedPath(
		opts.ProjectID,
		opts.EnvironmentID,
		"/schedules",
	)
	if err != nil {
		return api.ListSchedulesResponse{}, err
	}
	query := url.Values{}
	if cursor := strings.TrimSpace(opts.Cursor); cursor != "" {
		query.Set("cursor", cursor)
	}
	if opts.Limit > 0 {
		query.Set("limit", strconv.FormatInt(int64(opts.Limit), 10))
	}
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ListSchedulesResponse{}, err
	}
	var response api.ListSchedulesResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListSchedulesResponse{}, err
	}
	if response.Schedules == nil {
		response.Schedules = []api.ScheduleResponse{}
	}
	return response, nil
}

func (c *Client) GetSchedule(
	ctx context.Context,
	scheduleID string,
	opts EnvironmentScopeOptions,
) (api.ScheduleResponse, error) {
	if err := api.ValidateScheduleID(scheduleID); err != nil {
		return api.ScheduleResponse{}, err
	}
	path, err := c.environmentScopedPath(
		opts.ProjectID,
		opts.EnvironmentID,
		"/schedules/"+url.PathEscape(scheduleID),
	)
	if err != nil {
		return api.ScheduleResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ScheduleResponse{}, err
	}
	var response api.ScheduleResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ScheduleResponse{}, err
	}
	return response, nil
}
