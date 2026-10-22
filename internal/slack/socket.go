package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/coder/websocket"
	"github.com/helmrdotdev/helmr/internal/platformhttp"
)

// SocketConfig has the same application collaborators as HTTP delivery. Only
// the connection and its durable ownership are specific to Socket Mode.
type SocketConfig struct {
	Credentials *CredentialStore
	Client      *WebClient
	AppOrigin   *url.URL
	ControlKey  []byte
	Transport   http.RoundTripper
	Log         *slog.Logger
}

type SocketSupervisor struct {
	cfg  SocketConfig
	http *http.Client
}

func NewSocketSupervisor(cfg SocketConfig) (*SocketSupervisor, error) {
	if cfg.Credentials == nil || cfg.Credentials.mode != TransportSocket || cfg.Client == nil || cfg.AppOrigin == nil || len(cfg.ControlKey) != 32 {
		return nil, errors.New("complete Socket Mode configuration is required")
	}
	if cfg.Transport == nil {
		cfg.Transport = platformhttp.Direct()
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &SocketSupervisor{cfg: cfg, http: &http.Client{Transport: cfg.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Each replica owns a bounded set of apps. PostgreSQL leases let more replicas
// share the remaining apps without opening a socket on every replica for each app.
func (s *SocketSupervisor) Run(ctx context.Context) error {
	const maxConnections = 32
	replica := uuid.NewV7()
	active := 0
	finished := make(chan struct{}, maxConnections)
	var wg sync.WaitGroup
	defer wg.Wait()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-finished:
			active--
		case <-timer.C:
			for active < maxConnections && ctx.Err() == nil {
				acquireCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				claim, err := s.cfg.Credentials.acquireSocket(acquireCtx, replica)
				cancel()
				if err != nil {
					s.cfg.Log.Warn("Slack socket discovery unavailable")
					break
				}
				if claim == nil {
					break
				}
				active++
				wg.Go(func() { defer func() { finished <- struct{}{} }(); s.runClaim(ctx, *claim) })
			}
			timer.Reset(5 * time.Second)
		}
	}
}

type socketFailure struct {
	reason         string
	actionRequired bool
}

func (e socketFailure) Error() string { return e.reason }

func (s *SocketSupervisor) runClaim(ctx context.Context, claim socketClaim) {
	err := s.runConnection(ctx, claim)
	state, reason, retry := "reconnecting", "connection_failed", time.Duration(1000+rand.IntN(4000))*time.Millisecond
	var failure socketFailure
	if errors.As(err, &failure) {
		reason = failure.reason
		if failure.actionRequired {
			state = "action_required"
			retry = 5 * time.Minute
		}
	}
	if ctx.Err() != nil {
		state, reason, retry = "disconnected", "", 0
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.cfg.Credentials.releaseSocket(cleanup, claim, state, reason, retry); err != nil {
		s.cfg.Log.Warn("Slack socket release unavailable", "registration_id", claim.Registration)
	}
	// Never log network errors or close reasons: they can include signed socket URLs.
	if ctx.Err() == nil {
		s.cfg.Log.Info("Slack socket disconnected", "registration_id", claim.Registration, "state", state, "reason", reason)
	}
}

func socketURLAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "wss" || u.User != nil || u.Fragment != "" || u.Opaque != "" || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	switch u.Hostname() {
	case "wss.slack.com", "wss-primary.slack.com", "wss-backup.slack.com":
		return true
	default:
		return false
	}
}

func (s *SocketSupervisor) openSocket(ctx context.Context, claim socketClaim) (*websocket.Conn, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/apps.connections.open", strings.NewReader(""))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+claim.Token)
	response, err := s.http.Do(req)
	if err != nil {
		return nil, socketFailure{reason: "connection_failed"}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16*1024+1))
	if err != nil || len(body) > 16*1024 {
		return nil, socketFailure{reason: "protocol_error"}
	}
	var opened struct {
		OK    bool   `json:"ok"`
		URL   string `json:"url"`
		Error string `json:"error"`
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &opened) != nil {
		return nil, socketFailure{reason: "connection_failed"}
	}
	if !opened.OK {
		permanent := false
		switch opened.Error {
		case "invalid_auth", "not_authed", "token_revoked", "missing_scope", "not_allowed_token_type", "app_level_token_required":
			permanent = true
		}
		return nil, socketFailure{reason: "api_rejected", actionRequired: permanent}
	}
	if !socketURLAllowed(opened.URL) {
		return nil, socketFailure{reason: "endpoint_not_allowed", actionRequired: true}
	}
	conn, _, err := websocket.Dial(ctx, opened.URL, &websocket.DialOptions{HTTPClient: s.http})
	if err != nil {
		return nil, socketFailure{reason: "connection_failed"}
	}
	conn.SetReadLimit(1024 * 1024)
	_, raw, err := conn.Read(ctx)
	if err != nil {
		_ = conn.CloseNow()
		return nil, socketFailure{reason: "connection_failed"}
	}
	var hello struct {
		Type           string `json:"type"`
		ConnectionInfo struct {
			AppID string `json:"app_id"`
		} `json:"connection_info"`
	}
	if json.Unmarshal(raw, &hello) != nil || hello.Type != "hello" {
		_ = conn.CloseNow()
		return nil, socketFailure{reason: "protocol_error"}
	}
	if hello.ConnectionInfo.AppID != claim.AppID {
		_ = conn.CloseNow()
		return nil, socketFailure{reason: "app_mismatch", actionRequired: true}
	}
	return conn, nil
}

func (s *SocketSupervisor) runConnection(ctx context.Context, claim socketClaim) error {
	opening, cancelOpen := context.WithTimeout(ctx, 10*time.Second)
	conn, err := s.openSocket(opening, claim)
	cancelOpen()
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	observed, cancelObserve := context.WithTimeout(ctx, 5*time.Second)
	err = s.cfg.Credentials.observeSocket(observed, claim, false)
	cancelObserve()
	if err != nil {
		return err
	}
	run, cancel := context.WithCancelCause(ctx)
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		tick := time.NewTicker(socketLeaseDuration / 3)
		defer tick.Stop()
		for {
			select {
			case <-run.Done():
				return
			case <-tick.C:
				renewal, stop := context.WithTimeout(run, 5*time.Second)
				err := s.cfg.Credentials.renewSocket(renewal, claim)
				stop()
				if err != nil {
					cancel(err)
					return
				}
				ping, stop := context.WithTimeout(run, 5*time.Second)
				err = conn.Ping(ping)
				stop()
				if err != nil {
					cancel(socketFailure{reason: "connection_failed"})
					return
				}
			}
		}
	}()
	defer func() { cancel(context.Canceled); <-renewed }()
	for {
		_, raw, err := conn.Read(run)
		if err != nil {
			if run.Err() != nil {
				return context.Cause(run)
			}
			return socketFailure{reason: "connection_failed"}
		}
		if err = s.receiveSocketEnvelope(run, conn, claim, raw); err != nil {
			return err
		}
	}
}

type socketEnvelope struct {
	Type            string          `json:"type"`
	EnvelopeID      string          `json:"envelope_id"`
	AcceptsResponse bool            `json:"accepts_response_payload"`
	Payload         json.RawMessage `json:"payload"`
	Reason          string          `json:"reason"`
}

func (s *SocketSupervisor) receiveSocketEnvelope(ctx context.Context, conn *websocket.Conn, claim socketClaim, raw []byte) error {
	var envelope socketEnvelope
	if json.Unmarshal(raw, &envelope) != nil {
		return socketFailure{reason: "protocol_error"}
	}
	if envelope.Type == "disconnect" {
		if envelope.Reason == "link_disabled" {
			return socketFailure{reason: "link_disabled", actionRequired: true}
		}
		return socketFailure{reason: "connection_failed"}
	}
	if envelope.EnvelopeID == "" || len(envelope.EnvelopeID) > 256 {
		return socketFailure{reason: "protocol_error"}
	}
	receiptCtx, cancel := context.WithTimeout(ctx, receiptTimeout)
	defer cancel()
	db := socketReceiptDB{pool: s.cfg.Credentials.pool, claim: claim}
	result := receiptResult{disposition: receiptInvalid}
	// Bind payload workspace as well as app. The dedicated registration may only
	// deliver to the installation selected under its claim.
	switch envelope.Type {
	case "events_api":
		var identity struct {
			Team string `json:"team_id"`
		}
		if json.Unmarshal(envelope.Payload, &identity) == nil && identity.Team == claim.TeamID {
			h := EventHandler{Database: db, AppID: claim.AppID, Client: s.cfg.Client, Log: s.cfg.Log}
			result = h.receiveEventPayload(receiptCtx, envelope.Payload)
		}
	case "interactive":
		var identity struct {
			Team struct {
				ID string `json:"id"`
			} `json:"team"`
		}
		if json.Unmarshal(envelope.Payload, &identity) == nil && identity.Team.ID == claim.TeamID {
			h := InteractionHandler{Database: db, AppID: claim.AppID, Client: s.cfg.Client, ControlKey: s.cfg.ControlKey, AppOrigin: s.cfg.AppOrigin}
			result = h.receiveInteractionPayload(receiptCtx, envelope.Payload)
		}
	}
	if errors.Is(result.err, errSocketClaimLost) {
		return errSocketClaimLost
	}
	if result.disposition == receiptRetryable {
		return nil
	}
	// This also fences ignored or malformed events, whose handler did not start a
	// receipt transaction. A lost owner must not keep consuming Slack deliveries.
	if err := s.cfg.Credentials.observeSocket(receiptCtx, claim, true); err != nil {
		return err
	}
	ack := struct {
		EnvelopeID string          `json:"envelope_id"`
		Payload    json.RawMessage `json:"payload,omitempty"`
	}{EnvelopeID: envelope.EnvelopeID}
	if envelope.AcceptsResponse {
		ack.Payload = result.payload
	}
	body, _ := json.Marshal(ack)
	if err := conn.Write(receiptCtx, websocket.MessageText, body); err != nil {
		return socketFailure{reason: "connection_failed"}
	}
	return nil
}
