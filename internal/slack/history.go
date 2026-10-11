package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/jackc/pgx/v5"
)

// Each explicit check reads at most 120 messages across eight paced pages.
const historyPageBudget = 8

// Dedicated Apps are created by each customer for their own workspace. Slack
// gives these internal Apps Tier 3 history reads; pace below 50/minute and obey
// any stricter Retry-After returned by Slack.
const historyReadInterval = 2 * time.Second

type messagePage struct {
	Messages   []observedMessage
	NextCursor string
	More       bool
	Code       string
	RetryAfter time.Duration
}

// readMessages performs one authenticated, bounded page read. The caller owns
// installation pacing and continuation. Empty pages, missing scope and exhausted
// pagination never prove that an uncertain content mutation did not happen.
func (c *WebClient) readMessages(ctx context.Context, installation uuid.UUID, credential int64, channel, thread, cursor string) messagePage {
	if channel == "" || (thread != "" && !timestampPattern.MatchString(thread)) || len(cursor) > 4096 {
		return messagePage{Code: "invalid_history_request"}
	}
	token, err := c.credentials.BotToken(ctx, installation, credential)
	if err != nil || token == "" {
		return messagePage{Code: "credential_unavailable"}
	}
	query := url.Values{"channel": {channel}, "include_all_metadata": {"true"}, "limit": {"15"}}
	method := "conversations.history"
	if thread != "" {
		method = "conversations.replies"
		query.Set("ts", thread)
	}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://slack.com/api/"+method+"?"+query.Encode(), nil)
	if err != nil {
		return messagePage{Code: "invalid_history_request"}
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := c.http.Do(request)
	if err != nil {
		return messagePage{Code: "history_unavailable"}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		result := rateLimitResult(response.Header)
		return messagePage{Code: result.Code, RetryAfter: result.RetryAfter}
	}
	if response.StatusCode != http.StatusOK {
		return messagePage{Code: "history_unavailable"}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(body) > 1024*1024 {
		return messagePage{Code: "invalid_history_response"}
	}
	body, err = jsoncanon.Transform(body)
	if err != nil {
		return messagePage{Code: "invalid_history_response"}
	}
	var result struct {
		OK       *bool             `json:"ok"`
		Error    string            `json:"error"`
		Messages []observedMessage `json:"messages"`
		HasMore  bool              `json:"has_more"`
		Metadata struct {
			NextCursor string   `json:"next_cursor"`
			Warnings   []string `json:"warnings"`
		} `json:"response_metadata"`
	}
	if json.Unmarshal(body, &result) != nil || result.OK == nil || len(result.Metadata.NextCursor) > 4096 {
		return messagePage{Code: "invalid_history_response"}
	}
	if !*result.OK {
		if result.Error == "ratelimited" || result.Error == "rate_limited" {
			limited := rateLimitResult(response.Header)
			return messagePage{Code: limited.Code, RetryAfter: limited.RetryAfter}
		}
		if result.Error == "invalid_cursor" {
			return messagePage{Code: "history_cursor_expired"}
		}
		// Missing read scope does not revoke the separate publication capability.
		return messagePage{Code: "history_unavailable"}
	}
	if len(result.Metadata.Warnings) > 0 {
		return messagePage{Code: "history_incomplete"}
	}
	for i := range result.Messages {
		// The authenticated API query establishes the channel, not a message-supplied
		// field or its publicly writable metadata.
		result.Messages[i].Channel = channel
	}
	return messagePage{Messages: result.Messages, NextCursor: result.Metadata.NextCursor, More: result.HasMore || result.Metadata.NextCursor != ""}
}

// ReconcileUnknownPost reserves a paced read, then acquires authenticated evidence
// outside the transaction. Pagination survives restarts without retaining a copied
// transcript. No negative observation changes the uncertain delivery disposition.
func ReconcileUnknownPost(ctx context.Context, pool db.TxBeginner, client *WebClient, post uuid.UUID) (bool, error) {
	var installation, sender, organization uuid.UUID
	var team string
	var credential int64
	var channel, cursor string
	var pages int
	var root *string
	var digest []byte
	reserved := false
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var env, sourceChannel, targetChannel uuid.UUID
		err := tx.QueryRow(ctx, `SELECT p.environment_id,t.channel_id,source.channel_id,c.installation_id,c.organization_id,c.team_id
 FROM slack_posts p JOIN slack_threads t ON t.id=p.thread_id JOIN slack_threads source ON source.id=COALESCE(p.source_thread_id,p.thread_id) JOIN slack_channels c ON c.id=source.channel_id WHERE p.id=$1`, post).Scan(&env, &targetChannel, &sourceChannel, &sender, &organization, &team)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		// Lock both exact generations before selecting a currently authorized
		// reader. A retired sender does not prevent the target proving its post.
		if _, err = agent.LockSlackChannelsForDelivery(ctx, tx, env, []uuid.UUID{sourceChannel, targetChannel}); err != nil {
			return err
		}
		var ready bool
		err = tx.QueryRow(ctx, `SELECT i.id,i.credential_revision,i.history_next_at<=clock_timestamp() FROM slack_channels c
 JOIN agent_publications p ON p.id=c.publication_id JOIN slack_installations i ON i.id=c.installation_id JOIN slack_app_registrations r ON r.id=p.slack_app_registration_id
 WHERE c.id=ANY($1::uuid[]) AND p.revoked_at IS NULL AND r.retired_at IS NULL AND i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL AND i.credential_ciphertext IS NOT NULL
 ORDER BY (i.history_next_at<=clock_timestamp()) DESC,(i.id=$2) DESC,i.id LIMIT 1`, []uuid.UUID{sourceChannel, targetChannel}, sender).Scan(&installation, &credential, &ready)
		if errors.Is(err, pgx.ErrNoRows) {
			_, err = tx.Exec(ctx, `UPDATE slack_posts SET reconciliation_paused_at=clock_timestamp(),error='delivery_unverifiable' WHERE id=$1 AND status IN ('uncertain','failed') AND inflight_attempt_id IS NOT NULL`, post)
			return err
		}
		if err != nil || !ready {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT t.slack_channel_id,t.thread_ts,p.reconciliation_cursor,p.inflight_digest,p.reconciliation_pages FROM slack_posts p JOIN slack_threads t ON t.id=p.thread_id
 WHERE p.id=$1 AND (p.status='uncertain' OR (p.status='failed' AND p.delivery_disposed_at IS NOT NULL)) AND p.reconciliation_paused_at IS NULL AND p.next_attempt_at<=clock_timestamp() AND p.inflight_method IN ('chat.postMessage','chat.update','chat.startStream','chat.appendStream') FOR UPDATE OF p`, post).Scan(&channel, &root, &cursor, &digest, &pages)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE slack_installations SET history_next_at=clock_timestamp()+$2*interval '1 second' WHERE id=$1`, installation, historyReadInterval.Seconds()); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE slack_posts SET next_attempt_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, post); err != nil {
			return err
		}
		reserved = true
		return nil
	})
	if err != nil || !reserved {
		return false, err
	}
	thread := ""
	if root != nil {
		thread = *root
	}
	page := client.readMessages(ctx, installation, credential, channel, thread, cursor)
	for _, message := range page.Messages {
		if matched, e := confirmOpeningMessage(ctx, pool, organization, team, message); e != nil {
			return true, e
		} else if matched {
			continue
		}
		if _, err = confirmMessage(ctx, pool, sender, message); err != nil {
			return true, err
		}
	}
	err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM slack_installations WHERE id=$1 FOR NO KEY UPDATE`, installation).Scan(&id); err != nil {
			return err
		}
		next, code := cursor, page.Code
		paused := page.RetryAfter == 0
		// A local credential refresh can temporarily prevent any HTTP read. Give
		// it the existing paced window to finish, without consuming an unbounded
		// series of attempts or turning one normal rotation into operator work.
		if code == "credential_unavailable" {
			pages++
			paused = pages >= historyPageBudget
		}
		if code == "history_cursor_expired" {
			next = ""
		}
		if code == "" {
			pages++
			next = page.NextCursor
			switch {
			case !page.More:
				code = "history_no_match"
			case next == "" || next == cursor:
				code = "history_pagination_unavailable"
			case pages >= historyPageBudget:
				code = "history_page_limit"
			default:
				paused = false
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE slack_posts SET reconciliation_cursor=$3,reconciliation_pages=$7,
 reconciliation_paused_at=CASE WHEN $8 THEN clock_timestamp() ELSE NULL END,error=COALESCE(NULLIF($4,''),error),
 next_attempt_at=GREATEST(next_attempt_at,clock_timestamp()+$5*interval '1 second')
 WHERE id=$1 AND (status='uncertain' OR (status='failed' AND delivery_disposed_at IS NOT NULL))
 AND inflight_digest=$2 AND reconciliation_cursor=$6 AND reconciliation_paused_at IS NULL`, post, digest, next, code, page.RetryAfter.Seconds(), cursor, pages, paused); err != nil {
			return err
		}
		if page.RetryAfter > 0 {
			_, err := tx.Exec(ctx, `UPDATE slack_installations SET history_next_at=GREATEST(history_next_at,clock_timestamp()+$2*interval '1 second') WHERE id=$1`, installation, page.RetryAfter.Seconds())
			return err
		}
		return nil
	})
	return true, err
}
