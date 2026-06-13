// Package waitpoint delivers waitpoint notifications and response tokens.
package waitpoint

import (
	"context"
	"errors"
	"log/slog"
	"net/mail"
	"net/url"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/email"
	"github.com/helmrdotdev/helmr/internal/sqs"
	"github.com/jackc/pgx/v5/pgtype"
)

type Config struct {
	Log        *slog.Logger
	Store      Store
	Mailer     email.Sender
	Publisher  Publisher
	PublicURL  *url.URL
	AuthSecret []byte
}

type Store interface {
	CreateQueuedWaitpointEmailDelivery(context.Context, db.CreateQueuedWaitpointEmailDeliveryParams) (db.CreateQueuedWaitpointEmailDeliveryRow, error)
	ClaimWaitpointDeliveryForSend(context.Context, pgtype.UUID) (db.WaitpointDelivery, error)
	MarkObsoleteWaitpointDeliveryFailed(context.Context, pgtype.UUID) (db.WaitpointDelivery, error)
	GetWaitpointForDelivery(context.Context, db.GetWaitpointForDeliveryParams) (db.GetWaitpointForDeliveryRow, error)
	GetRunSummary(context.Context, db.GetRunSummaryParams) (db.GetRunSummaryRow, error)
	MarkWaitpointDeliverySent(context.Context, db.MarkWaitpointDeliverySentParams) (db.WaitpointDelivery, error)
	MarkWaitpointDeliveryRetrying(context.Context, db.MarkWaitpointDeliveryRetryingParams) (db.WaitpointDelivery, error)
	MarkWaitpointDeliverySignaled(context.Context, db.MarkWaitpointDeliverySignaledParams) (db.WaitpointDelivery, error)
	CreateWaitpointDelivery(context.Context, db.CreateWaitpointDeliveryParams) (db.WaitpointDelivery, error)
	MarkWaitpointDeliveryFailed(context.Context, db.MarkWaitpointDeliveryFailedParams) (db.WaitpointDelivery, error)
	RequeueStaleSendingWaitpointDeliveries(context.Context, db.RequeueStaleSendingWaitpointDeliveriesParams) error
	ListDueWaitpointDeliveries(context.Context, int32) ([]db.WaitpointDelivery, error)
}

type Publisher interface {
	Publish(context.Context, sqs.Message) (string, error)
}

type Subscriber interface {
	Receive(context.Context) ([]sqs.ReceivedMessage, error)
	Delete(context.Context, sqs.ReceivedMessage) error
}

type Notifier struct {
	log        *slog.Logger
	store      Store
	mailer     email.Sender
	publisher  Publisher
	publicURL  *url.URL
	authSecret []byte
}

type Pending struct {
	ID             pgtype.UUID
	RunWaitID      pgtype.UUID
	OrgID          pgtype.UUID
	RunID          pgtype.UUID
	Kind           db.WaitpointKind
	DisplayText    string
	PolicySnapshot []byte
	RequestedAt    pgtype.Timestamptz
}

type RunInfo struct {
	ID     pgtype.UUID
	TaskID string
}

func NewNotifier(cfg Config) (*Notifier, error) {
	if cfg.Store == nil {
		return nil, errors.New("waitpoint store is required")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	mailer := cfg.Mailer
	if mailer == nil {
		mailer = email.Unconfigured{}
	}
	return &Notifier{
		log:        log,
		store:      cfg.Store,
		mailer:     mailer,
		publisher:  cfg.Publisher,
		publicURL:  cfg.PublicURL,
		authSecret: cfg.AuthSecret,
	}, nil
}

func KindExternallyCompletable(kind db.WaitpointKind) bool {
	switch kind {
	case db.WaitpointKindHuman:
		return true
	default:
		return false
	}
}

func emailRecipients(config api.WaitpointPolicyConfig) []string {
	seen := map[string]struct{}{}
	recipients := []string{}
	for _, delivery := range config.Deliveries {
		if delivery.Type != "email" {
			continue
		}
		for _, raw := range delivery.To {
			recipient := normalizeEmailRecipient(raw)
			if recipient == "" {
				continue
			}
			if _, ok := seen[recipient]; ok {
				continue
			}
			seen[recipient] = struct{}{}
			recipients = append(recipients, recipient)
		}
	}
	return recipients
}

func normalizeEmailRecipient(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func ValidateEmailRecipient(value string) error {
	normalized := normalizeEmailRecipient(value)
	if normalized == "" {
		return errors.New("email is required")
	}
	address, err := mail.ParseAddress(normalized)
	if err != nil || address.Address != normalized {
		return errors.New("email is invalid")
	}
	return nil
}
