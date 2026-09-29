package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/idempotency"
)

// deploymentFinalizePingEvery keeps an idle finalization stream open while
// objects are verified.
const deploymentFinalizePingEvery = 10 * time.Second

type deploymentFinalizeResult struct {
	response api.DeploymentResponse
	err      error
}

func (s *Server) finalizeDeploymentBundle(w http.ResponseWriter, r *http.Request) {
	var request api.FinalizeDeploymentBundleRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid deployment bundle finalization request: %w", err))
		return
	}
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	request.BundleDigest = strings.TrimSpace(request.BundleDigest)
	if request.IdempotencyKey == "" {
		writeError(w, badRequest(errors.New("deployment idempotency key is required")))
		return
	}
	if _, err := artifact.RuntimeDigestBytes(request.BundleDigest); err != nil {
		writeError(w, badRequest(errors.New("deployment bundle digest is invalid")))
		return
	}
	principal := principalFromContext(r.Context())
	scope, _, _, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	finalization, err := s.deploymentFinalizer.Prepare(
		r.Context(), principal, scope, request.BundleDigest, request.IdempotencyKey,
	)
	if err != nil {
		s.writeDeploymentError(w, err)
		return
	}
	streamDeploymentFinalization(w, r, s.log, deploymentFinalizePingEvery, finalization.BundleDigest(), func(
		ctx context.Context,
		progress func(objectDigest string) error,
	) (api.DeploymentResponse, error) {
		record, err := s.deploymentFinalizer.Finalize(ctx, s.db, s.tx, finalization, progress)
		if err != nil {
			return api.DeploymentResponse{}, err
		}
		return deploymentResponse(record), nil
	})
}

type deploymentFinalizer func(
	context.Context,
	func(objectDigest string) error,
) (api.DeploymentResponse, error)

// streamDeploymentFinalization runs finish while streaming its progress as
// server-sent events: started, object_verified per verified object, ping while
// idle, then one complete or error event. A client disconnect cancels finish.
func streamDeploymentFinalization(
	w http.ResponseWriter,
	r *http.Request,
	log *slog.Logger,
	pingEvery time.Duration,
	bundleDigest string,
	finish deploymentFinalizer,
) {
	if _, ok := w.(http.Flusher); !ok {
		writeError(w, unavailable(errors.New("deployment finalization streaming is unavailable")))
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	if err := writeDeploymentFinalizeEvent(w, api.DeploymentBundleFinalizeEventStarted, api.DeploymentBundleFinalizeStarted{
		BundleDigest: bundleDigest,
	}); err != nil {
		return
	}
	if err := http.NewResponseController(w).Flush(); err != nil {
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	progress := make(chan string)
	result := make(chan deploymentFinalizeResult, 1)
	go func() {
		var completed deploymentFinalizeResult
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(ctx, "deployment finalizer panic", "panic", recovered, "stack", string(debug.Stack()))
				completed = deploymentFinalizeResult{err: errors.New("deployment finalizer panicked")}
			}
			result <- completed
		}()
		completed.response, completed.err = finish(ctx, func(objectDigest string) error {
			select {
			case progress <- objectDigest:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()

	ticker := time.NewTicker(pingEvery)
	defer ticker.Stop()
	for {
		var event string
		var payload any
		terminal := false
		select {
		case <-r.Context().Done():
			log.Info("deployment finalization stream disconnected", "bundle_digest", bundleDigest)
			return
		case <-ticker.C:
			event = api.DeploymentBundleFinalizeEventPing
			payload = struct{}{}
		case objectDigest := <-progress:
			event = api.DeploymentBundleFinalizeEventObjectVerified
			payload = api.DeploymentBundleFinalizeObject{Digest: objectDigest}
		case completed := <-result:
			terminal = true
			if completed.err == nil {
				event = api.DeploymentBundleFinalizeEventComplete
				payload = completed.response
			} else {
				log.Error("deployment bundle finalization failed", "error", completed.err)
				event = api.DeploymentBundleFinalizeEventError
				payload = publicDeploymentFinalizeError(completed.err)
			}
		}
		// Completion and cancellation can both be ready in the select. Do not
		// emit a terminal event after observing that the client disconnected.
		if r.Context().Err() != nil {
			return
		}
		if err := writeDeploymentFinalizeEvent(w, event, payload); err != nil {
			cancel()
			return
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			cancel()
			return
		}
		if terminal {
			return
		}
	}
}

func writeDeploymentFinalizeEvent(w io.Writer, event string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if bytes.ContainsAny(encoded, "\r\n") {
		return errors.New("deployment finalization event is not a single line")
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encoded)
	return err
}

// publicDeploymentFinalizeError describes a finalization failure in the
// stream with a closed code and message.
func publicDeploymentFinalizeError(err error) api.DeploymentBundleFinalizeError {
	var expired idempotency.ExpiredError
	if errors.As(err, &expired) {
		return api.DeploymentBundleFinalizeError{Code: expired.ErrorCode(), Message: expired.Error()}
	}
	var idempotencyConflict idempotency.ConflictError
	if errors.As(err, &idempotencyConflict) {
		return api.DeploymentBundleFinalizeError{
			Code: "idempotency_conflict", Message: "idempotency key conflicts with another deployment bundle",
		}
	}
	var invalidObject deployment.InvalidObjectError
	if errors.As(err, &invalidObject) {
		return api.DeploymentBundleFinalizeError{
			Code: "invalid_deployment_object", Message: "deployment object failed verification",
		}
	}
	return api.DeploymentBundleFinalizeError{
		Code: "deployment_finalization_unavailable", Message: "deployment finalization is unavailable",
	}
}
