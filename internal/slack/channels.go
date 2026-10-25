package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/slackroute"
	"github.com/jackc/pgx/v5"
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
	return c.verifyConversation(ctx, installation, revision, team, channel, "")
}

// A DM requires the exact peer established by the signed gesture or retained
// thread. Channel fields such as name and membership are not present on DMs.
func (c *WebClient) verifyConversation(ctx context.Context, installation uuid.UUID, revision int64, team, channel, peer string) (string, error) {
	direct := slackroute.IsDirectMessage(channel)
	if (!direct && !definition.ValidSlackChannelID(channel)) || (direct && peer == "") || team == "" {
		return "", ErrChannelVerification
	}

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
		return "", &channelVerificationFailure{channelRateLimit(response.Header)}
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
			User           string   `json:"user"`
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
			return "", &channelVerificationFailure{channelRateLimit(response.Header)}
		case "invalid_auth", "not_authed", "token_revoked", "token_expired", "account_inactive", "missing_scope", "not_in_channel", "channel_not_found", "is_archived", "restricted_action", "ekm_access_denied", "method_not_supported_for_channel_type":
			return "", &channelVerificationFailure{DeliveryResult{Disposition: Rejected, Code: result.Error}}
		default:
			return "", channelReadUnavailable()
		}
	}
	v := result.Channel
	if v.ID != channel || v.MPIM || v.Shared || v.OrgShared || v.ExtShared || v.PendingShared || v.Archived || v.Frozen || v.ReadOnly || v.NonThreadable || len(v.PendingTeams) > 0 || len(v.Pending) > 0 {
		return "", ErrChannelVerification
	}
	for _, other := range append(v.SharedTeams, v.ConnectedTeams...) {
		if other != team {
			return "", ErrChannelVerification
		}
	}
	if direct {
		// The exact installation token identifies the workspace. Slack may omit
		// context_team_id on an IM; a conflicting supplied team is never accepted.
		if !v.IM || v.Channel || v.Group || v.User != peer || (v.Team != "" && v.Team != team) {
			return "", ErrChannelVerification
		}
		return "", nil
	}
	if v.Team != team || v.Name == "" || (!v.Channel && !v.Group) || v.IM || !v.Member {
		return "", ErrChannelVerification
	}
	if peer != "" {
		if err := c.verifyChannelMember(ctx, token, channel, peer); err != nil {
			return "", err
		}
	}
	return v.Name, nil
}

// rememberChannelName keeps the name Slack reported for a channel in
// slack_channels so the Console can show it; a DM has no name and is left out.
func rememberChannelName(ctx context.Context, pool db.TxBeginner, installation uuid.UUID, channel, name string) error {
	if name == "" {
		return nil
	}
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO slack_channels(installation_id,slack_channel_id,name) VALUES($1,$2,left($3,255))
 ON CONFLICT(installation_id,slack_channel_id) DO UPDATE SET name=excluded.name,observed_at=clock_timestamp() WHERE slack_channels.name IS DISTINCT FROM excluded.name`, installation, channel, name)
		return err
	})
}

func (c *WebClient) VerifyConversation(ctx context.Context, installation uuid.UUID, revision int64, team, channel, peer string) error {
	_, err := c.verifyConversation(ctx, installation, revision, team, channel, peer)
	return err
}

func channelRateLimit(header http.Header) DeliveryResult {
	result := rateLimitResult(header)
	result.RateLimitMethod = "conversations.info"
	return result
}

// Channel presence is checked against Slack, not inferred from a historical post
// or possession of a channel-visible control token.
func (c *WebClient) verifyChannelMember(ctx context.Context, token, channel, actor string) error {
	cursor := ""
	for {
		values := url.Values{"channel": {channel}, "limit": {"200"}}
		if cursor != "" {
			values.Set("cursor", cursor)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://slack.com/api/conversations.members?"+values.Encode(), nil)
		if err != nil {
			return channelReadUnavailable()
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := c.http.Do(request)
		if err != nil {
			return channelReadUnavailable()
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
		response.Body.Close()
		if response.StatusCode == http.StatusTooManyRequests {
			result := rateLimitResult(response.Header)
			result.RateLimitMethod = "conversations.members"
			return &channelVerificationFailure{result}
		}
		if response.StatusCode != http.StatusOK || readErr != nil || len(body) > 1024*1024 {
			return channelReadUnavailable()
		}
		var result struct {
			OK       *bool    `json:"ok"`
			Members  []string `json:"members"`
			Metadata struct {
				Cursor   string   `json:"next_cursor"`
				Warnings []string `json:"warnings"`
			} `json:"response_metadata"`
		}
		if json.Unmarshal(body, &result) != nil || result.OK == nil || !*result.OK || len(result.Metadata.Warnings) > 0 {
			return channelReadUnavailable()
		}
		for _, member := range result.Members {
			if member == actor {
				return nil
			}
		}
		if result.Metadata.Cursor == "" {
			return ErrChannelVerification
		}
		if result.Metadata.Cursor == cursor {
			return channelReadUnavailable()
		}
		cursor = result.Metadata.Cursor
	}
}

// VerifyInputConversation is used immediately before dispatching queued Slack
// input. Confirmed loss of participation rejects work; unavailable reads retry.
func (c *WebClient) VerifyInputConversation(ctx context.Context, installation uuid.UUID, revision int64, team, channel, actor string) (bool, error) {
	err := c.VerifyConversation(ctx, installation, revision, team, channel, actor)
	if err == nil {
		return true, nil
	}
	var failure *channelVerificationFailure
	if errors.As(err, &failure) {
		if failure.result.Disposition == Rejected {
			return false, nil
		}
		return false, err
	}
	if errors.Is(err, ErrChannelVerification) {
		return false, nil
	}
	return false, err
}
