package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

type Credentials interface {
	BotToken(context.Context, uuid.UUID, int64) (string, error)
}

type Disposition string

const (
	Acknowledged Disposition = "acknowledged"
	NotIssued    Disposition = "not_issued"
	RateLimited  Disposition = "rate_limited"
	Rejected     Disposition = "rejected"
	Uncertain    Disposition = "uncertain"
)

type DeliveryResult struct {
	Disposition Disposition
	ViewID      string
	Timestamp   string
	Code        string
	RetryAfter  time.Duration
}

// WebClient makes one exact request. It never retries content mutations, even
// when a transport failure appears transient. The durable delivery owner decides
// whether an acknowledged rejection permits another attempt.
type WebClient struct {
	credentials Credentials
	http        *http.Client
}

func NewWebClient(credentials Credentials, transport http.RoundTripper) *WebClient {
	return &WebClient{credentials: credentials, http: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

var timestampPattern = regexp.MustCompile(`^[0-9]{1,16}\.[0-9]{1,16}$`)

func (c *WebClient) Call(ctx context.Context, installation uuid.UUID, credentialRevision int64, method string, payload []byte) DeliveryResult {
	switch method {
	case "chat.postEphemeral", "chat.postMessage", "chat.update", "chat.startStream", "chat.appendStream", "chat.stopStream", "agents.sessions.setStatus", "views.open", "views.push", "views.update":
	default:
		return DeliveryResult{Disposition: Rejected, Code: "unsupported_method"}
	}
	if !json.Valid(payload) {
		return DeliveryResult{Disposition: Rejected, Code: "invalid_payload"}
	}
	token, err := c.credentials.BotToken(ctx, installation, credentialRevision)
	if err != nil || token == "" {
		return DeliveryResult{Disposition: NotIssued, Code: "credential_unavailable"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/"+method, bytes.NewReader(payload))
	if err != nil {
		return DeliveryResult{Disposition: Rejected, Code: "invalid_request"}
	}
	// Disable automatic request replay by net/http after a reused-connection error.
	request.GetBody = nil
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := c.http.Do(request)
	if err != nil {
		return DeliveryResult{Disposition: Uncertain, Code: "transport_outcome_unknown"}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		return rateLimitResult(response.Header)
	}
	if response.StatusCode != http.StatusOK {
		return DeliveryResult{Disposition: Uncertain, Code: "http_outcome_unknown"}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(body) > 1024*1024 {
		return DeliveryResult{Disposition: Uncertain, Code: "response_outcome_unknown"}
	}
	body, err = jsoncanon.Transform(body)
	if err != nil {
		return DeliveryResult{Disposition: Uncertain, Code: "response_outcome_unknown"}
	}
	var result struct {
		View struct {
			ID string `json:"id"`
		} `json:"view"`
		OK                 *bool  `json:"ok"`
		Error              string `json:"error"`
		Timestamp          string `json:"ts"`
		EphemeralTimestamp string `json:"message_ts"`
		Channel            string `json:"channel"`
		AgentStatus        string `json:"agent_status"`
		ResponseMetadata   struct {
			Warnings []string `json:"warnings"`
		} `json:"response_metadata"`
	}
	if json.Unmarshal(body, &result) != nil || result.OK == nil {
		return DeliveryResult{Disposition: Uncertain, Code: "response_outcome_unknown"}
	}
	if !*result.OK {
		switch result.Error {
		case "ratelimited", "rate_limited":
			return rateLimitResult(response.Header)
		case "message_not_in_streaming_state", "stopped_by_user", "streaming_mode_mismatch", "invalid_chunks", "expired_trigger_id", "invalid_trigger_id", "exchanged_trigger_id", "view_too_large", "invalid_auth", "not_authed", "token_revoked", "token_expired", "account_inactive", "missing_scope", "feature_disabled", "not_in_channel", "channel_not_found", "is_archived", "message_not_found", "invalid_arguments", "invalid_blocks", "msg_too_long", "invalid_thread_ts", "restricted_action", "ekm_access_denied", "method_not_supported_for_channel_type":
			return DeliveryResult{Disposition: Rejected, Code: result.Error}
		default:
			return DeliveryResult{Disposition: Uncertain, Code: "slack_outcome_unknown"}
		}
	}
	if method == "chat.postEphemeral" {
		result.Timestamp = result.EphemeralTimestamp
	}
	// The stop-event subscription warning does not invalidate a status update.
	// Other warnings may report truncated/dropped content and never authorize replay.
	for _, warning := range result.ResponseMetadata.Warnings {
		if method == "agents.sessions.setStatus" && warning == "missing_agent_session_stopped_event_subscription" {
			continue
		}
		return DeliveryResult{Disposition: Uncertain, Timestamp: result.Timestamp, Code: "slack_delivery_warning"}
	}
	var submitted struct {
		Channel string `json:"channel"`
		Status  string `json:"status"`
	}
	if json.Unmarshal(payload, &submitted) != nil {
		return DeliveryResult{Disposition: Uncertain, Code: "response_outcome_unknown"}
	}
	if result.Channel != "" && submitted.Channel != "" && result.Channel != submitted.Channel {
		return DeliveryResult{Disposition: Uncertain, Code: "destination_mismatch"}
	}
	if method == "agents.sessions.setStatus" && (submitted.Status == "" || result.AgentStatus != submitted.Status) {
		return DeliveryResult{Disposition: Uncertain, Code: "status_confirmation_unknown"}
	}
	if method == "chat.postEphemeral" || method == "chat.postMessage" || method == "chat.startStream" {
		if !timestampPattern.MatchString(result.Timestamp) {
			return DeliveryResult{Disposition: Uncertain, Code: "missing_message_identity"}
		}
	}
	if (method == "views.open" || method == "views.push" || method == "views.update") && result.View.ID == "" {
		return DeliveryResult{Disposition: Uncertain, Code: "missing_view_identity"}
	}
	return DeliveryResult{Disposition: Acknowledged, Timestamp: result.Timestamp, ViewID: result.View.ID}
}

func rateLimitResult(header http.Header) DeliveryResult {
	seconds, err := strconv.ParseInt(header.Get("Retry-After"), 10, 32)
	if err != nil || seconds < 1 {
		seconds = 60
	}
	return DeliveryResult{Disposition: RateLimited, Code: "rate_limited", RetryAfter: time.Duration(seconds) * time.Second}
}
