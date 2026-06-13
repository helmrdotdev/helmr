package waitpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/email"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	deliveryMaxAttempts = int32(5)
	deliveryClaimStale  = 5 * time.Minute
	deliverySignalGrace = 30 * time.Second
)

type resolvedPolicySnapshot struct {
	Name      string          `json:"name"`
	Label     string          `json:"label"`
	Config    json.RawMessage `json:"config"`
	IsDefault bool            `json:"is_default"`
}

func (n *Notifier) NotifyPending(ctx context.Context, pending Pending) {
	deliveries := n.queuePendingNotifications(ctx, pending)
	for _, delivery := range deliveries {
		if n.publisher == nil {
			continue
		}
		deliveryID := ids.MustFromPG(delivery.ID)
		if _, err := n.publisher.Publish(ctx, deliveryAsyncMessage(delivery)); err != nil {
			n.log.Warn("enqueue waitpoint notification failed", "delivery_id", deliveryID.String(), "error", err)
			continue
		}
		n.markDeliverySignaled(ctx, delivery, time.Now().UTC().Add(deliverySignalGrace))
	}
}

func (n *Notifier) queuePendingNotifications(ctx context.Context, pending Pending) []db.WaitpointDelivery {
	_, config, ok, err := policyFromSnapshot(pending)
	if err != nil {
		n.log.Warn("parse waitpoint policy failed", "run_id", ids.MustFromPG(pending.RunID).String(), "waitpoint_id", ids.MustFromPG(pending.ID).String(), "error", err)
		return nil
	}
	if !ok {
		return nil
	}
	recipients := emailRecipients(config)
	if len(recipients) == 0 {
		return nil
	}
	if !n.emailDeliveryConfigured() {
		n.createFailedEmailDeliveries(ctx, pending, recipients, "email delivery is not configured")
		return nil
	}
	if !n.responseTokensConfigured() {
		n.log.Warn("skip waitpoint email notification: response token API is not configured", "run_id", ids.MustFromPG(pending.RunID).String(), "waitpoint_id", ids.MustFromPG(pending.ID).String())
		n.createFailedEmailDeliveries(ctx, pending, recipients, "response token API is not configured")
		return nil
	}
	deliveries := make([]db.WaitpointDelivery, 0, len(recipients))
	for _, recipient := range recipients {
		delivery, err := n.createQueuedEmailDelivery(ctx, pending, recipient)
		if err != nil {
			n.log.Warn("create waitpoint delivery failed", "run_id", ids.MustFromPG(pending.RunID).String(), "waitpoint_id", ids.MustFromPG(pending.ID).String(), "recipient", recipient, "error", err)
			continue
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries
}

func (n *Notifier) emailDeliveryConfigured() bool {
	switch n.mailer.(type) {
	case nil, email.Unconfigured:
		return false
	default:
		return true
	}
}

func (n *Notifier) responseTokensConfigured() bool {
	return n.store != nil && auth.ValidateTokenSecret(n.authSecret) == nil
}

func (n *Notifier) createQueuedEmailDelivery(ctx context.Context, pending Pending, recipient string) (db.WaitpointDelivery, error) {
	deliveryID := ids.New()
	_, tokenHash, err := NewEmailResponseToken(n.authSecret, deliveryID)
	if err != nil {
		return db.WaitpointDelivery{}, err
	}
	if !KindExternallyCompletable(pending.Kind) {
		return db.WaitpointDelivery{}, errors.New("waitpoint kind cannot be responded to externally")
	}
	tokenMetadata, err := json.Marshal(map[string]any{
		"source":    "email",
		"recipient": recipient,
		"principal": recipient,
	})
	if err != nil {
		return db.WaitpointDelivery{}, err
	}
	deliveryMetadata, err := json.Marshal(map[string]any{
		"source": "policy",
	})
	if err != nil {
		return db.WaitpointDelivery{}, err
	}
	messageID := DeliveryMessageID(deliveryID, n.publicURL)
	delivery, err := n.store.CreateQueuedWaitpointEmailDelivery(ctx, db.CreateQueuedWaitpointEmailDeliveryParams{
		DeliveryID:       ids.ToPG(deliveryID),
		OrgID:            pending.OrgID,
		RunID:            pending.RunID,
		WaitpointID:      pending.ID,
		TokenHash:        tokenHash,
		ExpiresAt:        pgTimeToPG(time.Now().UTC().Add(DefaultResponseTokenTTL)),
		Recipient:        recipient,
		TokenMetadata:    tokenMetadata,
		MessageID:        pgText(messageID),
		DeliveryMetadata: deliveryMetadata,
	})
	if err != nil {
		return db.WaitpointDelivery{}, err
	}
	return deliveryFromQueuedRow(delivery), nil
}

func (n *Notifier) SendQueuedDelivery(ctx context.Context, deliveryID uuid.UUID) error {
	delivery, err := n.store.ClaimWaitpointDeliveryForSend(ctx, ids.ToPG(deliveryID))
	if isNoRows(err) {
		n.markObsoleteDeliveryFailed(ctx, deliveryID)
		return nil
	}
	if err != nil {
		return err
	}
	if err := n.sendClaimedDelivery(ctx, delivery); err != nil {
		n.markClaimedDeliveryError(ctx, delivery, err)
		return err
	}
	return nil
}

func (n *Notifier) markObsoleteDeliveryFailed(ctx context.Context, deliveryID uuid.UUID) {
	if _, err := n.store.MarkObsoleteWaitpointDeliveryFailed(ctx, ids.ToPG(deliveryID)); err != nil && !isNoRows(err) {
		n.log.Warn("mark obsolete waitpoint delivery failed", "delivery_id", deliveryID.String(), "error", err)
	}
}

func (n *Notifier) sendClaimedDelivery(ctx context.Context, delivery db.WaitpointDelivery) error {
	if delivery.Channel != "email" {
		return fmt.Errorf("unsupported waitpoint delivery channel %q", delivery.Channel)
	}
	pendingRow, err := n.store.GetWaitpointForDelivery(ctx, db.GetWaitpointForDeliveryParams{
		OrgID:      delivery.OrgID,
		DeliveryID: delivery.ID,
	})
	if err != nil {
		return err
	}
	pending := pendingFromDeliveryRow(pendingRow)
	run, err := n.store.GetRunSummary(ctx, db.GetRunSummaryParams{OrgID: pending.OrgID, ID: pending.RunID})
	if err != nil {
		return err
	}
	tokenID, err := ids.FromPG(delivery.ResponseTokenID)
	if err != nil {
		return fmt.Errorf("waitpoint delivery response token is not set: %w", err)
	}
	rawToken, _, err := NewEmailResponseToken(n.authSecret, tokenID)
	if err != nil {
		return err
	}
	link, err := ConfirmationURL(n.publicURL, tokenID.String(), rawToken)
	if err != nil {
		return err
	}
	message := notificationEmail(delivery.Recipient, runInfoFromSummary(run), pending, link)
	message.IdempotencyKey = "waitpoint-delivery/" + ids.MustFromPG(delivery.ID).String()
	if delivery.MessageID.Valid {
		message.MessageID = delivery.MessageID.String
	}
	if err := n.mailer.SendEmail(ctx, message); err != nil {
		return err
	}
	if _, err := n.store.MarkWaitpointDeliverySent(ctx, db.MarkWaitpointDeliverySentParams{
		OrgID:            delivery.OrgID,
		DeliveryID:       delivery.ID,
		AttemptCount:     delivery.AttemptCount,
		SendingStartedAt: delivery.SendingStartedAt,
		LastAttemptAt:    delivery.LastAttemptAt,
	}); isNoRows(err) {
		return fmt.Errorf("waitpoint delivery send claim was superseded")
	} else if err != nil {
		return err
	}
	return nil
}

func (n *Notifier) markClaimedDeliveryError(ctx context.Context, delivery db.WaitpointDelivery, cause error) {
	if delivery.AttemptCount >= deliveryMaxAttempts {
		n.markDeliveryFailed(ctx, delivery, cause.Error())
		return
	}
	delay := deliveryRetryDelay(delivery.AttemptCount)
	if _, err := n.store.MarkWaitpointDeliveryRetrying(ctx, db.MarkWaitpointDeliveryRetryingParams{
		LastError:        pgText(cause.Error()),
		NextAttemptAt:    pgTimeToPG(time.Now().UTC().Add(delay)),
		OrgID:            delivery.OrgID,
		DeliveryID:       delivery.ID,
		AttemptCount:     delivery.AttemptCount,
		SendingStartedAt: delivery.SendingStartedAt,
	}); isNoRows(err) {
		return
	} else if err != nil {
		n.log.Warn("mark waitpoint delivery retrying failed", "delivery_id", ids.MustFromPG(delivery.ID).String(), "error", err)
	}
}

func (n *Notifier) markDeliverySignaled(ctx context.Context, delivery db.WaitpointDelivery, nextAttemptAt time.Time) {
	_, err := n.store.MarkWaitpointDeliverySignaled(ctx, db.MarkWaitpointDeliverySignaledParams{
		NextAttemptAt: pgTimeToPG(nextAttemptAt),
		OrgID:         delivery.OrgID,
		DeliveryID:    delivery.ID,
	})
	if isNoRows(err) {
		return
	}
	if err != nil {
		n.log.Warn("mark waitpoint delivery signaled failed", "delivery_id", ids.MustFromPG(delivery.ID).String(), "error", err)
	}
}

func deliveryRetryDelay(attempt int32) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := time.Duration(1<<min(attempt-1, 5)) * time.Minute
	if delay > time.Hour {
		return time.Hour
	}
	return delay
}

func (n *Notifier) createFailedEmailDeliveries(ctx context.Context, pending Pending, recipients []string, reason string) {
	for _, recipient := range recipients {
		n.createFailedEmailDelivery(ctx, pending, pgtype.UUID{}, recipient, reason)
	}
}

func (n *Notifier) createFailedEmailDelivery(ctx context.Context, pending Pending, tokenID pgtype.UUID, recipient string, reason string) {
	if _, err := n.createEmailDelivery(ctx, pending, tokenID, recipient, db.WaitpointDeliveryStatusFailed, reason); err != nil {
		n.log.Warn("create failed waitpoint delivery failed", "run_id", ids.MustFromPG(pending.RunID).String(), "waitpoint_id", ids.MustFromPG(pending.ID).String(), "recipient", recipient, "error", err)
	}
}

func (n *Notifier) createEmailDelivery(ctx context.Context, pending Pending, tokenID pgtype.UUID, recipient string, status db.WaitpointDeliveryStatus, lastError string) (db.WaitpointDelivery, error) {
	metadata, err := json.Marshal(map[string]any{
		"source": "policy",
	})
	if err != nil {
		return db.WaitpointDelivery{}, err
	}
	deliveryID := ids.New()
	delivery, err := n.store.CreateWaitpointDelivery(ctx, db.CreateWaitpointDeliveryParams{
		DeliveryID:      ids.ToPG(deliveryID),
		OrgID:           pending.OrgID,
		RunID:           pending.RunID,
		RunWaitID:       pending.RunWaitID,
		WaitpointID:     pending.ID,
		ResponseTokenID: tokenID,
		Channel:         "email",
		RecipientKind:   "email",
		Recipient:       recipient,
		Status:          status,
		MessageID:       pgText(DeliveryMessageID(deliveryID, n.publicURL)),
		LastError:       pgText(lastError),
		Metadata:        metadata,
	})
	if err != nil {
		return db.WaitpointDelivery{}, err
	}
	return delivery, nil
}

func DeliveryMessageID(deliveryID uuid.UUID, publicURL *url.URL) string {
	host := "helmr.local"
	if publicURL != nil && strings.TrimSpace(publicURL.Hostname()) != "" {
		host = publicURL.Hostname()
	}
	return "<waitpoint-delivery-" + deliveryID.String() + "@" + host + ">"
}

func deliveryFromQueuedRow(row db.CreateQueuedWaitpointEmailDeliveryRow) db.WaitpointDelivery {
	return db.WaitpointDelivery(row)
}

func pendingFromDeliveryRow(row db.GetWaitpointForDeliveryRow) Pending {
	return Pending{
		ID:             row.ID,
		RunWaitID:      row.RunWaitID,
		OrgID:          row.OrgID,
		RunID:          row.RunID,
		Kind:           row.Kind,
		DisplayText:    row.DisplayText,
		PolicySnapshot: row.PolicySnapshot,
		RequestedAt:    row.RequestedAt,
	}
}

func (n *Notifier) markDeliveryFailed(ctx context.Context, delivery db.WaitpointDelivery, reason string) {
	if _, err := n.store.MarkWaitpointDeliveryFailed(ctx, db.MarkWaitpointDeliveryFailedParams{
		OrgID:            delivery.OrgID,
		DeliveryID:       delivery.ID,
		LastError:        pgText(reason),
		AttemptCount:     delivery.AttemptCount,
		SendingStartedAt: delivery.SendingStartedAt,
	}); isNoRows(err) {
		return
	} else if err != nil {
		n.log.Warn("mark waitpoint delivery failed failed", "delivery_id", ids.MustFromPG(delivery.ID).String(), "error", err)
	}
}

func policyFromSnapshot(pending Pending) (resolvedPolicySnapshot, api.WaitpointPolicyConfig, bool, error) {
	if len(pending.PolicySnapshot) == 0 {
		return resolvedPolicySnapshot{}, api.WaitpointPolicyConfig{}, false, nil
	}
	var policy resolvedPolicySnapshot
	if err := json.Unmarshal(pending.PolicySnapshot, &policy); err != nil {
		return resolvedPolicySnapshot{}, api.WaitpointPolicyConfig{}, false, err
	}
	var config api.WaitpointPolicyConfig
	if len(policy.Config) > 0 {
		if err := json.Unmarshal(policy.Config, &config); err != nil {
			return resolvedPolicySnapshot{}, api.WaitpointPolicyConfig{}, false, err
		}
	}
	return policy, config, true, nil
}

func notificationEmail(to string, run RunInfo, pending Pending, link string) email.Message {
	runID := ids.MustFromPG(run.ID).String()
	waitpointID := ids.MustFromPG(pending.ID).String()
	body := fmt.Sprintf(
		"A Helmr run is waiting for input.\n\nTask: %s\nRun: %s\nWaitpoint: %s\nType: %s\nRequested: %s\n\n%s\n\nReview and respond here:\n%s\n\nThis link opens a confirmation page before submitting a response.\n",
		run.TaskID,
		runID,
		waitpointID,
		pending.Kind,
		pgTime(pending.RequestedAt).Format(time.RFC3339),
		pending.DisplayText,
		link,
	)
	return email.Message{
		To:        to,
		Subject:   "Helmr waitpoint pending: " + run.TaskID,
		PlainText: body,
	}
}

func ConfirmationURL(publicURL *url.URL, tokenID string, token string) (string, error) {
	if publicURL == nil {
		return "", errors.New("public URL is not configured")
	}
	confirmation := publicURL.ResolveReference(&url.URL{Path: "/waitpoints/respond"})
	query := confirmation.Query()
	query.Set("id", tokenID)
	query.Set("token", token)
	confirmation.RawQuery = query.Encode()
	return confirmation.String(), nil
}

func ConfirmationPath(tokenID string, token string) string {
	confirmation := url.URL{Path: "/waitpoints/respond"}
	query := confirmation.Query()
	query.Set("id", tokenID)
	query.Set("token", token)
	confirmation.RawQuery = query.Encode()
	return confirmation.String()
}

func runInfoFromSummary(run db.GetRunSummaryRow) RunInfo {
	return RunInfo{ID: run.ID, TaskID: run.TaskID}
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
