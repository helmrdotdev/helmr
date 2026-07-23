package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/jackc/pgx/v5/pgtype"
)

type deploymentEventAppender interface {
	AppendDeploymentEvent(context.Context, db.AppendDeploymentEventParams) (db.AppendDeploymentEventRow, error)
}

func (s *Server) getDeploymentEvents(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("project storage is not configured")))
		return
	}
	store, ok := s.db.(deploymentStatusStore)
	if !ok {
		writeError(w, unavailable(errors.New("deployment storage is not configured")))
		return
	}
	deploymentID, err := parseUUIDParam(r, "deploymentID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	cursor, err := eventCursor(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	limit, err := eventLimit(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionTasksDeploy, scope) && !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	projectID, environmentID, err := runScopeIDs(scope)
	if err != nil {
		writeError(w, errors.New("get deployment events"))
		return
	}
	deployment, err := store.GetDeploymentForOrg(r.Context(), db.GetDeploymentForOrgParams{
		OrgID: pgvalue.UUID(actor.OrgID),
		ID:    pgvalue.UUID(deploymentID),
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("deployment not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("get deployment"))
		return
	}
	if deployment.ProjectID != projectID || deployment.EnvironmentID != environmentID {
		writeError(w, notFound(errors.New("deployment not found")))
		return
	}
	if r.URL.Query().Get("follow") == "1" || strings.Contains(r.Header.Get("accept"), "text/event-stream") {
		s.followDeploymentEvents(w, r, actor.OrgID, deploymentID, cursor)
		return
	}
	page, err := s.telemetryReader.ListEvents(r.Context(), telemetry.EventQuery{
		OrgID:       actor.OrgID,
		SubjectType: eventSubjectTypeDeployment,
		SubjectID:   pgvalue.MustUUIDValue(deployment.ID),
		AfterSeq:    cursor,
		Limit:       limit + 1,
	})
	if err != nil {
		writeError(w, errors.New("list deployment events"))
		return
	}
	rows := page.Events
	hasNext := len(rows) > int(limit)
	if hasNext {
		rows = rows[:limit]
	}
	var nextCursor *string
	if hasNext {
		value := rows[len(rows)-1].ID
		nextCursor = &value
	}
	writeJSON(w, http.StatusOK, api.RunEventPage{Events: rows, Cursor: telemetryCursor(cursor), NextCursor: nextCursor})
}

func (s *Server) followDeploymentEvents(w http.ResponseWriter, r *http.Request, orgID uuid.UUID, deploymentID uuid.UUID, cursor int64) {
	if s.eventStream == nil {
		writeError(w, unavailable(errors.New("event stream is not configured")))
		return
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	ctx, cancel := context.WithTimeout(r.Context(), runEventsFollowMaxDuration)
	defer cancel()
	err := s.eventStream.ReadSubject(ctx, orgID, eventSubjectTypeDeployment, deploymentID, cursor, func(event api.RunEvent) error {
		_, _ = fmt.Fprintf(w, "id: %s\n", event.ID)
		_, _ = fmt.Fprint(w, "event: deployment_event\n")
		_, _ = fmt.Fprint(w, "data: ")
		if err := encoder.Encode(event); err != nil {
			return err
		}
		_, _ = fmt.Fprint(w, "\n")
		if flusher != nil {
			flusher.Flush()
		}
		if deploymentEventKindIsTerminal(event.Kind) {
			cancel()
		}
		return nil
	}, func() error {
		_, _ = fmt.Fprint(w, ": keep-alive\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.log.Warn("follow deployment events failed", "error", err)
	}
}

func deploymentEventKindIsTerminal(kind string) bool {
	switch kind {
	case "deployment.deployed", "deployment.failed":
		return true
	default:
		return false
	}
}

func appendDeploymentLifecycleEvent(ctx context.Context, store deploymentEventAppender, orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, deploymentID pgtype.UUID, kind string, severity string, source string, status string, message string) error {
	payload, err := json.Marshal(map[string]string{"status": status})
	if err != nil {
		return err
	}
	_, err = store.AppendDeploymentEvent(ctx, db.AppendDeploymentEventParams{
		OrgID:          orgID,
		ProjectID:      projectID,
		EnvironmentID:  environmentID,
		DeploymentID:   deploymentID,
		Category:       "lifecycle",
		Severity:       severity,
		Source:         source,
		Kind:           kind,
		Message:        message,
		Payload:        payload,
		RedactionClass: "internal",
	})
	return err
}

func appendDeploymentBuildLogs(
	ctx context.Context,
	store deploymentEventAppender,
	orgID pgtype.UUID,
	projectID pgtype.UUID,
	environmentID pgtype.UUID,
	deploymentID pgtype.UUID,
	logs deployment.BuildLogs,
) error {
	const chunkBytes = 256 << 10
	metadata, err := json.Marshal(struct {
		ExitStatus int32 `json:"exitStatus"`
		Truncated  bool  `json:"truncated"`
	}{
		ExitStatus: logs.ExitStatus,
		Truncated:  logs.Truncated,
	})
	if err != nil {
		return err
	}
	if _, err := store.AppendDeploymentEvent(ctx, db.AppendDeploymentEventParams{
		OrgID:          orgID,
		ProjectID:      projectID,
		EnvironmentID:  environmentID,
		DeploymentID:   deploymentID,
		Category:       "build",
		Severity:       "info",
		Source:         "worker",
		Kind:           "deployment.build.exit",
		Message:        "Package manager exited",
		Payload:        metadata,
		RedactionClass: "sensitive",
	}); err != nil {
		return err
	}
	for _, stream := range []struct {
		name    string
		encoded string
	}{
		{name: "stdout", encoded: logs.StdoutBase64},
		{name: "stderr", encoded: logs.StderrBase64},
	} {
		content, err := base64.StdEncoding.DecodeString(stream.encoded)
		if err != nil {
			return fmt.Errorf("decode deployment build %s: %w", stream.name, err)
		}
		for offset := 0; offset < len(content); offset += chunkBytes {
			end := min(offset+chunkBytes, len(content))
			payload, err := json.Marshal(struct {
				ContentBase64 string `json:"contentBase64"`
				Offset        int    `json:"offset"`
				Stream        string `json:"stream"`
			}{
				ContentBase64: base64.StdEncoding.EncodeToString(content[offset:end]),
				Offset:        offset,
				Stream:        stream.name,
			})
			if err != nil {
				return err
			}
			if _, err := store.AppendDeploymentEvent(ctx, db.AppendDeploymentEventParams{
				OrgID:          orgID,
				ProjectID:      projectID,
				EnvironmentID:  environmentID,
				DeploymentID:   deploymentID,
				Category:       "build",
				Severity:       "info",
				Source:         "worker",
				Kind:           "deployment.build.log",
				Message:        stream.name,
				Payload:        payload,
				RedactionClass: "sensitive",
			}); err != nil {
				return err
			}
		}
	}
	return nil
}
