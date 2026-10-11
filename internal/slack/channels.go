package slack

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"io"
	"net/http"
	"net/url"
	"uuid"
)

var ErrChannelVerification = errors.New("the Slack channel could not be verified; invite the app to an unshared workspace channel and try again")

// A failed read has not issued the content mutation. Preserve its retry and
// credential outcome separately from a confirmed channel access denial.
type channelVerificationFailure struct{ result DeliveryResult }

func (e *channelVerificationFailure) Error() string { return ErrChannelVerification.Error() }
func (e *channelVerificationFailure) Unwrap() error { return ErrChannelVerification }

func channelReadUnavailable() error {
	return &channelVerificationFailure{DeliveryResult{Disposition: NotIssued, Code: "channel_verification_unavailable"}}
}

// Verification is a read with the exact installation credential. Missing or
// ambiguous membership evidence fails closed; no implicit channel join occurs.
func (c *WebClient) verifyChannel(ctx context.Context, installation uuid.UUID, revision int64, team, channel string) (string, error) {
	token, err := c.credentials.BotToken(ctx, installation, revision)
	if err != nil || token == "" {
		return "", &channelVerificationFailure{DeliveryResult{Disposition: NotIssued, Code: "credential_unavailable"}}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://slack.com/api/conversations.info?"+url.Values{"channel": {channel}}.Encode(), nil)
	if err != nil {
		return "", channelReadUnavailable()
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := c.http.Do(request)
	if err != nil {
		return "", channelReadUnavailable()
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		return "", &channelVerificationFailure{rateLimitResult(response.Header)}
	}
	if response.StatusCode != http.StatusOK {
		return "", channelReadUnavailable()
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(body) > 1024*1024 {
		return "", channelReadUnavailable()
	}
	body, err = jsoncanon.Transform(body)
	if err != nil {
		return "", channelReadUnavailable()
	}
	var result struct {
		OK      *bool  `json:"ok"`
		Error   string `json:"error"`
		Channel struct {
			ID             string   `json:"id"`
			Name           string   `json:"name"`
			Team           string   `json:"context_team_id"`
			Channel        bool     `json:"is_channel"`
			Group          bool     `json:"is_group"`
			IM             bool     `json:"is_im"`
			MPIM           bool     `json:"is_mpim"`
			Member         bool     `json:"is_member"`
			Shared         bool     `json:"is_shared"`
			OrgShared      bool     `json:"is_org_shared"`
			ExtShared      bool     `json:"is_ext_shared"`
			PendingShared  bool     `json:"is_pending_ext_shared"`
			Archived       bool     `json:"is_archived"`
			Frozen         bool     `json:"is_frozen"`
			ReadOnly       bool     `json:"is_read_only"`
			NonThreadable  bool     `json:"is_non_threadable"`
			SharedTeams    []string `json:"shared_team_ids"`
			ConnectedTeams []string `json:"connected_team_ids"`
			PendingTeams   []string `json:"pending_connected_team_ids"`
			Pending        []string `json:"pending_shared"`
		} `json:"channel"`
		Metadata struct {
			Warnings []string `json:"warnings"`
		} `json:"response_metadata"`
	}
	if json.Unmarshal(body, &result) != nil || result.OK == nil || len(result.Metadata.Warnings) > 0 {
		return "", channelReadUnavailable()
	}
	if !*result.OK {
		switch result.Error {
		case "ratelimited", "rate_limited":
			return "", &channelVerificationFailure{rateLimitResult(response.Header)}
		case "invalid_auth", "not_authed", "token_revoked", "token_expired", "account_inactive", "missing_scope", "not_in_channel", "channel_not_found", "is_archived", "restricted_action", "ekm_access_denied", "method_not_supported_for_channel_type":
			return "", &channelVerificationFailure{DeliveryResult{Disposition: Rejected, Code: result.Error}}
		default:
			return "", channelReadUnavailable()
		}
	}
	v := result.Channel
	if v.ID != channel || v.Team != team || v.Name == "" || (!v.Channel && !v.Group) || v.IM || v.MPIM || !v.Member || v.Shared || v.OrgShared || v.ExtShared || v.PendingShared || v.Archived || v.Frozen || v.ReadOnly || v.NonThreadable || len(v.PendingTeams) > 0 || len(v.Pending) > 0 {
		return "", ErrChannelVerification
	}
	for _, other := range append(v.SharedTeams, v.ConnectedTeams...) {
		if other != team {
			return "", ErrChannelVerification
		}
	}
	return v.Name, nil
}

func (c *WebClient) VerifyChannel(ctx context.Context, installation uuid.UUID, revision int64, team, channel string) error {
	_, err := c.verifyChannel(ctx, installation, revision, team, channel)
	return err
}
