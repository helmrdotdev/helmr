package controlplane

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/email"
	"github.com/helmrdotdev/helmr/internal/identity"
	"log/slog"
	"time"
)

// RunSecurityNotifications delivers committed account notices. Provider errors
// never undo account changes; the durable row is retried with its stable key.
func RunSecurityNotifications(ctx context.Context, q db.DBTX, sender email.Sender, log *slog.Logger) error {
	if _, disabled := sender.(email.Unconfigured); disabled {
		<-ctx.Done()
		return ctx.Err()
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		n, found, err := identity.ClaimSecurityNotification(ctx, q)
		if err != nil {
			return err
		}
		if !found {
			timer.Reset(time.Second)
			continue
		}
		deliveryCtx, cancel := context.WithTimeout(ctx, identity.SecurityNotificationDeliveryTimeout)
		err = sender.SendEmail(deliveryCtx, email.Message{To: n.Recipient, Subject: "Helmr account security change", PlainText: n.Text(), IdempotencyKey: "account-security-" + n.ID.String(), MessageID: "<account-security-" + n.ID.String() + "@helmr>"})
		cancel()
		if err != nil {
			log.Warn("account security notification delivery failed", "notification_id", n.ID)
		}
		if completeErr := identity.CompleteSecurityNotification(ctx, q, n, err == nil); completeErr != nil {
			return completeErr
		}
		timer.Reset(0)
	}
}
