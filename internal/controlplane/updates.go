package controlplane

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// Updates carry invalidation hints only. Consumers read authoritative snapshots
// on connect and on each hint, so no cursor or replay buffer can lose a result.
func (s *Server) streamSessionUpdates(w http.ResponseWriter, r *http.Request) {
	id, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		s.writeAgentHTTPError(w, session.ErrInvalidInput)
		return
	}
	authorize := func() error {
		principal, _, err := s.sessionPrincipal(r)
		if err != nil {
			return err
		}
		current := r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal))
		caller, env, err := s.agentRequestAuthority(current, auth.PermissionSessionsRead)
		if err != nil {
			return err
		}
		_, err = session.GetSession(current.Context(), s.tx, caller, env, id)
		return err
	}
	wake, unsubscribe := s.changes.Subscribe("session:" + id.String())
	defer unsubscribe()
	if err := authorize(); err != nil {
		s.writeAgentHTTPError(w, err)
		return
	}
	streamUpdates(w, r, wake, authorize)
}

func (s *Server) streamWorkerUpdates(w http.ResponseWriter, r *http.Request) {
	host := workerFromContext(r.Context())
	token, _ := bearerToken(r.Header.Get("authorization"))
	authorize := func() error {
		_, err := workergroup.AuthenticateHost(r.Context(), s.db, s.hostAuth, token, time.Now())
		return err
	}
	wake, unsubscribe := s.changes.Subscribe("worker:" + host.HostID.String())
	defer unsubscribe()
	streamUpdates(w, r, wake, authorize)
}

func streamUpdates(w http.ResponseWriter, r *http.Request, wake <-chan struct{}, authorize func() error) {
	controller := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	changed := true
	for {
		if err := authorize(); err != nil {
			return
		}
		// Bound slow readers without holding a database transaction or connection.
		if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
			return
		}
		frame := ": heartbeat\n\n"
		if changed {
			frame = "event: change\ndata: {}\n\n"
		}
		if _, err := fmt.Fprint(w, frame); err != nil {
			return
		}
		if err := controller.Flush(); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-wake:
			changed = true
		case <-heartbeat.C:
			changed = false
		}
	}
}

func (s *Server) streamCapacityUpdates(w http.ResponseWriter, r *http.Request) {
	token, _ := bearerToken(r.Header.Get("Authorization"))
	authorize := func() error {
		if len(s.capacityTokenHash) == 0 || !hmac.Equal(auth.HashCredential(token), s.capacityTokenHash) {
			return errors.New("capacity authentication is required")
		}
		return nil
	}
	wake, unsubscribe := s.changes.Subscribe("capacity")
	defer unsubscribe()
	streamUpdates(w, r, wake, authorize)
}
