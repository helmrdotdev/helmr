package slack

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"
)

// RunDelivery owns a bounded set of workers so a slow external mutation does not
// block independent threads. Durable claims and installation pacing remain the
// send authority across workers and dispatcher processes. Cancellation joins all
// in-flight workers before returning to the process supervisor.
func RunDelivery(ctx context.Context, pool db.TxBeginner, client *WebClient, log *slog.Logger) error {
	if pool == nil || client == nil || log == nil {
		return errors.New("the Slack delivery dependencies are required")
	}
	var workers errgroup.Group
	for range 8 {
		workers.Go(func() error {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				_, err := ReconcileDelivery(ctx, pool, client, 4)
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if err != nil {
					log.ErrorContext(ctx, "Slack delivery reconciliation failed", "error", err)
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-ticker.C:
				}
			}
		})
	}
	return workers.Wait()
}

func (s *CredentialStore) refreshCandidates(ctx context.Context) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := db.RunTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM slack_installations WHERE disconnected_at IS NULL AND authorization_lost_at IS NULL AND
 ((refresh_attempt_id IS NOT NULL AND refresh_deadline<=clock_timestamp()) OR
 (refresh_attempt_id IS NULL AND credential_expires_at<=clock_timestamp()+interval '5 minutes' AND refresh_next_at<=clock_timestamp()))
 ORDER BY COALESCE(refresh_deadline,credential_expires_at),id LIMIT 32`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, err
}

// RunCredentials resolves due exchanges independently of publication work. Each
// bounded attempt owns its replacement until persistence or expiry; this loop
// waits for all owners before another scan or shutdown.
func (s *CredentialStore) RunCredentials(ctx context.Context, transport http.RoundTripper, log *slog.Logger) error {
	if log == nil {
		return errors.New("the Slack refresh dependencies are required")
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		ids, err := s.refreshCandidates(ctx)
		if err == nil {
			failures := make([]error, len(ids))
			var workers errgroup.Group
			workers.SetLimit(8)
			for i, id := range ids {
				workers.Go(func() error {
					_, oauth, err := s.InstallationOAuth(ctx, id, transport)
					if err != nil {
						failures[i] = err
						return nil
					}
					_, failures[i] = s.Refresh(ctx, id, oauth)
					return nil
				})
			}
			_ = workers.Wait()
			err = errors.Join(failures...)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.ErrorContext(ctx, "Slack credential reconciliation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
