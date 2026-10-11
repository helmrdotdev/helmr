package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/jackc/pgx/v5"
)

// resolveMentionRoot reads exactly one requested root. Only a positive match or
// positive unrelated sender permits admission; missing evidence stays pending.
func (c *WebClient) resolveMentionRoot(ctx context.Context, pool db.TxBeginner, connection agent.SlackConnection, root string) (bool, error) {
	var reserved bool
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE slack_installations SET history_next_at=clock_timestamp()+$3*interval '1 second' WHERE id=$1 AND authorized_at=$2 AND disconnected_at IS NULL AND authorization_lost_at IS NULL AND history_next_at<=clock_timestamp()`, connection.InstallationID, connection.AuthorizedAt, historyReadInterval.Seconds())
		reserved = err == nil && tag.RowsAffected() == 1
		return err
	})
	if err != nil || !reserved {
		return false, err
	}
	token, err := c.credentials.BotToken(ctx, connection.InstallationID, connection.CredentialRevision)
	if err != nil || token == "" {
		return false, ErrChannelVerification
	}
	query := url.Values{"channel": {connection.ChannelID}, "latest": {root}, "inclusive": {"true"}, "limit": {"1"}, "include_all_metadata": {"true"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://slack.com/api/conversations.history?"+query.Encode(), nil)
	if err != nil {
		return false, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := c.http.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	rateLimited := func() (bool, error) {
		retry := rateLimitResult(response.Header).RetryAfter
		return false, db.RunTx(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE slack_installations SET history_next_at=GREATEST(history_next_at,clock_timestamp()+$2*interval '1 second') WHERE id=$1`, connection.InstallationID, retry.Seconds())
			return err
		})
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return rateLimited()
	}

	if response.StatusCode != http.StatusOK {
		return false, nil
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil {
		return false, err
	}
	if len(raw) > 1024*1024 {
		return false, nil
	}
	raw, err = jsoncanon.Transform(raw)
	if err != nil {
		return false, nil
	}
	var result struct {
		OK       bool              `json:"ok"`
		Error    string            `json:"error"`
		Messages []observedMessage `json:"messages"`
		Metadata struct {
			Warnings []string `json:"warnings"`
		} `json:"response_metadata"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return false, nil
	}
	if !result.OK && (result.Error == "ratelimited" || result.Error == "rate_limited") {
		return rateLimited()
	}
	if !result.OK || len(result.Messages) != 1 || len(result.Metadata.Warnings) > 0 {
		return false, nil
	}
	message := result.Messages[0]
	if message.Timestamp != root || message.User == "" || (message.Thread != "" && message.Thread != root) {
		return false, nil
	}
	message.Channel = connection.ChannelID
	matched, err := confirmOpeningMessage(ctx, pool, connection.OrganizationID, connection.TeamID, message)
	if err != nil || matched {
		return matched, err
	}
	var managed bool
	err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM slack_installations WHERE organization_id=$1 AND team_id=$2 AND (bot_user_id=$3 OR app_id=NULLIF($4,'')))`, connection.OrganizationID, connection.TeamID, message.User, message.AppID).Scan(&managed)
	})
	return !managed, err
}

// Opening recognition uses the immutable operation and authenticated actual
// sender, not a body digest that Slack can normalize. Historical/abandoned sends
// remain recognizable without reviving execution or delivery.
func confirmOpeningMessage(ctx context.Context, pool db.TxBeginner, org uuid.UUID, team string, message observedMessage) (bool, error) {
	var metadata struct {
		Type    string `json:"event_type"`
		Payload struct {
			Post     string `json:"post_id"`
			Revision int64  `json:"revision"`
		} `json:"event_payload"`
	}
	if json.Unmarshal(message.Metadata, &metadata) != nil || metadata.Type != "helmr_post" || metadata.Payload.Post == "" || metadata.Payload.Revision <= 0 {
		return false, nil
	}
	postID, err := uuid.Parse(metadata.Payload.Post)
	if err != nil {
		return false, nil
	}
	matched := false
	err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var thread, post uuid.UUID
		var currentRoot *string
		var revision *int64
		var state string
		var disposed bool
		err := tx.QueryRow(ctx, `SELECT t.id,p.id,t.thread_ts,p.inflight_revision,p.status,p.delivery_disposed_at IS NOT NULL
 FROM slack_posts p JOIN slack_threads t ON t.id=p.thread_id JOIN slack_threads sender ON sender.id=COALESCE(p.source_thread_id,p.thread_id)
 JOIN slack_channels c ON c.id=sender.channel_id JOIN slack_installations i ON i.id=c.installation_id
 WHERE p.id=$1 AND p.role='opening' AND t.front_session_id=p.session_id AND t.opening_publication_key=p.publication_key
 AND c.organization_id=$2 AND c.team_id=$3 AND c.slack_channel_id=$4 AND i.app_id=$5 AND i.bot_user_id=$6
 AND (p.inflight_method='chat.postMessage' AND p.inflight_attempt_id IS NOT NULL OR p.message_ts=$7)
 FOR NO KEY UPDATE OF t,p`, postID, org, team, message.Channel, message.AppID, message.User, message.Timestamp).Scan(&thread, &post, &currentRoot, &revision, &state, &disposed)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if currentRoot != nil {
			matched = *currentRoot == message.Timestamp
			return nil
		}
		if revision == nil || *revision != metadata.Payload.Revision || (state != "sending" && state != "uncertain" && !(state == "failed" && disposed)) {
			return nil
		}
		if _, err = tx.Exec(ctx, `UPDATE slack_posts SET status=CASE WHEN delivery_disposed_at IS NOT NULL THEN 'failed' WHEN desired_revision<=suppressed_revision THEN 'suppressed' ELSE 'posted' END,confirmed_revision=inflight_revision,message_ts=$2,posted_at=COALESCE(posted_at,clock_timestamp()),claimed_until=NULL,error=CASE WHEN delivery_disposed_at IS NOT NULL THEN 'delivery_abandoned' WHEN desired_revision<=suppressed_revision THEN 'publication_authority_unavailable' ELSE NULL END,inflight_method=NULL,inflight_payload=NULL,inflight_digest=NULL,inflight_revision=NULL,inflight_attempt_id=NULL WHERE id=$1::uuid`, post, message.Timestamp); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE slack_threads SET thread_ts=$2 WHERE id=$1::uuid AND thread_ts IS NULL`, thread, message.Timestamp); err != nil {
			return err
		}
		matched = true
		return nil
	})
	return matched, err
}
