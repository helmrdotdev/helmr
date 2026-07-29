package control

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) workerCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.WorkerCreateWorkspaceRequest
	if err := decodeWorkerActorRequest(r, &request, "Workspace create"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := validateWorkerWorkspaceCorrelation(request.CorrelationID); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := api.ValidateWorkspaceDeclaredID(request.WorkspaceDeclaredID); err != nil {
		writeError(w, badRequest(err))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	source, err := s.workerRunSource(r.Context(), worker, request.Lease)
	if err != nil {
		s.writeWorkerWorkspaceSourceError(w, "create", request.Lease.RunID, err)
		return
	}
	result, err := s.createWorkspace(r.Context(), workspaceCreateRequest{
		OrgID:         pgvalue.MustUUIDValue(source.OrgID),
		ProjectID:     pgvalue.MustUUIDValue(source.ProjectID),
		EnvironmentID: pgvalue.MustUUIDValue(source.EnvironmentID),
		Declaration: workspaceDeclarationSelector{
			Kind:  workspaceDeclarationRunPinned,
			RunID: pgvalue.MustUUIDValue(source.RunID),
		},
		DeclaredID: request.WorkspaceDeclaredID,
		Key:        request.Key, Secrets: request.Secrets, IdempotencyKey: idempotencyKey,
		Authorize: func(ctx context.Context, q db.Querier) error {
			_, err := authorizeWorkerRunSource(ctx, q, worker, request.Lease)
			return err
		},
	})
	if err != nil {
		if errors.Is(err, errStaleWorkerRunSource) {
			s.writeWorkerWorkspaceSourceError(w, "create", request.Lease.RunID, err)
			return
		}
		if failure, ok := workerWorkspaceCreateFailure(err); ok {
			writeJSON(w, http.StatusOK, api.WorkerCreateWorkspaceResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		s.log.Error("create run-sourced Workspace", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("create run-sourced Workspace"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerCreateWorkspaceResponse{
		CorrelationID: request.CorrelationID,
		Completed:     &api.CreateWorkspaceResponse{WorkspaceID: result.WorkspaceID.String()},
	})
}

func (s *Server) workerRetrieveWorkspace(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.WorkerRetrieveWorkspaceRequest
	if err := decodeWorkerActorRequest(r, &request, "Workspace retrieve"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := validateWorkerWorkspaceRequest(request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	var snapshot api.WorkspaceSnapshot
	err := s.inTx(r.Context(), func(work *txWork) error {
		source, err := authorizeWorkerRunSource(r.Context(), work.q, worker, request.Lease)
		if err != nil {
			return err
		}
		record, err := resolveWorkerWorkspace(r.Context(), work.q, source, request.Workspace)
		if err != nil {
			return err
		}
		snapshot, err = s.workspaceSnapshot(r.Context(), work.q, record)
		return err
	})
	if s.writeWorkerWorkspaceReadResult(w, request.CorrelationID, request.Lease.RunID, "retrieve", err) {
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerRetrieveWorkspaceResponse{
		CorrelationID: request.CorrelationID, Completed: &snapshot,
	})
}

func (s *Server) workerReadWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerReadWorkspaceFileRequest
	if !s.decodeWorkerWorkspaceFileRequest(w, r, &request, "Workspace file read") {
		return
	}
	worker := workerFromContext(r.Context())
	var content api.WorkspaceFileContent
	var source workspaceFileSource
	err := s.inTx(r.Context(), func(work *txWork) error {
		runSource, err := authorizeWorkerRunSource(r.Context(), work.q, worker, request.Lease)
		if err != nil {
			return err
		}
		record, err := resolveWorkerWorkspace(r.Context(), work.q, runSource, request.Workspace)
		if err != nil {
			return err
		}
		source, err = s.resolveCurrentWorkspaceFileSource(r.Context(), work.q, record)
		return err
	})
	if err == nil {
		content, err = s.readWorkspaceFileSource(r.Context(), source, request.Path)
	}
	if failure, handled := s.workerWorkspaceFileFailure(w, request.Lease.RunID, "read", err); handled {
		if failure != nil {
			writeJSON(w, http.StatusOK, api.WorkerReadWorkspaceFileResponse{
				CorrelationID: request.CorrelationID, Failed: failure,
			})
		}
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerReadWorkspaceFileResponse{
		CorrelationID: request.CorrelationID, Completed: &content,
	})
}

func (s *Server) workerStatWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerReadWorkspaceFileRequest
	if !s.decodeWorkerWorkspaceFileRequest(w, r, &request, "Workspace file stat") {
		return
	}
	worker := workerFromContext(r.Context())
	var entry api.WorkspaceFileEntry
	var source workspaceFileSource
	err := s.inTx(r.Context(), func(work *txWork) error {
		runSource, err := authorizeWorkerRunSource(r.Context(), work.q, worker, request.Lease)
		if err != nil {
			return err
		}
		record, err := resolveWorkerWorkspace(r.Context(), work.q, runSource, request.Workspace)
		if err != nil {
			return err
		}
		source, err = s.resolveCurrentWorkspaceFileSource(r.Context(), work.q, record)
		return err
	})
	if err == nil {
		entry, err = s.statWorkspaceFileSource(r.Context(), source, request.Path)
	}
	if failure, handled := s.workerWorkspaceFileFailure(w, request.Lease.RunID, "stat", err); handled {
		if failure != nil {
			writeJSON(w, http.StatusOK, api.WorkerStatWorkspaceFileResponse{
				CorrelationID: request.CorrelationID, Failed: failure,
			})
		}
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerStatWorkspaceFileResponse{
		CorrelationID: request.CorrelationID, Completed: &entry,
	})
}

func (s *Server) workerListWorkspaceFiles(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.WorkerListWorkspaceFilesRequest
	if err := decodeWorkerActorRequest(r, &request, "Workspace file list"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := validateWorkerWorkspaceRequest(request.WorkerRetrieveWorkspaceRequest); err != nil {
		writeError(w, badRequest(err))
		return
	}
	target, err := validateWorkspaceFilePath(request.Path)
	if err != nil || request.Limit < 1 || request.Limit > workspaceFileListMaxLimit {
		writeError(w, badRequest(errors.New("Workspace file list request is invalid")))
		return
	}
	request.Path = target
	worker := workerFromContext(r.Context())
	var page api.WorkspaceFilePage
	var record db.Workspace
	var source workspaceFileSource
	var after string
	var now time.Time
	err = s.inTx(r.Context(), func(work *txWork) error {
		runSource, err := authorizeWorkerRunSource(r.Context(), work.q, worker, request.Lease)
		if err != nil {
			return err
		}
		record, err = resolveWorkerWorkspace(r.Context(), work.q, runSource, request.Workspace)
		if err != nil {
			return err
		}
		now = time.Now()
		source, after, err = s.resolveWorkspaceFileListSource(
			r.Context(), work.q, record, request.Path, request.Cursor, now,
		)
		return err
	})
	if err == nil {
		page, err = s.listWorkspaceFileSource(
			r.Context(), pgvalue.UUIDString(record.ID), source, request.Path, after, request.Limit, now,
		)
	}
	if failure, handled := s.workerWorkspaceFileFailure(w, request.Lease.RunID, "list", err); handled {
		if failure != nil {
			writeJSON(w, http.StatusOK, api.WorkerListWorkspaceFilesResponse{
				CorrelationID: request.CorrelationID, Failed: failure,
			})
		}
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerListWorkspaceFilesResponse{
		CorrelationID: request.CorrelationID, Completed: &page,
	})
}

func (s *Server) workerExecuteWorkspace(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.WorkerExecuteWorkspaceRequest
	if err := decodeWorkerActorRequest(r, &request, "Workspace exec"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := validateWorkerWorkspaceRequest(request.WorkerRetrieveWorkspaceRequest); err != nil {
		writeError(w, badRequest(err))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil || idempotencyKey == "" {
		writeError(w, badRequest(errors.New("Workspace exec idempotency_key is invalid")))
		return
	}
	timeout := time.Duration(0)
	if request.TimeoutMS != nil {
		timeout = time.Duration(*request.TimeoutMS) * time.Millisecond
	}
	worker := workerFromContext(r.Context())
	source, record, err := s.workerWorkspaceSourceAndRecord(r.Context(), worker, request.WorkerRetrieveWorkspaceRequest)
	if err != nil {
		if failure, ok := workerWorkspaceReferenceFailure(err); ok {
			writeJSON(w, http.StatusOK, api.WorkerExecuteWorkspaceResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		s.writeWorkerWorkspaceSourceError(w, "exec", request.Lease.RunID, err)
		return
	}
	admission, err := s.admitWorkspaceExec(r.Context(), workspaceExecRequest{
		OrgID: pgvalue.MustUUIDValue(source.OrgID), ProjectID: pgvalue.MustUUIDValue(source.ProjectID),
		EnvironmentID: pgvalue.MustUUIDValue(source.EnvironmentID), Workspace: record,
		Creator: workspaceExecCreator{
			SubjectType: "run", SubjectID: pgvalue.MustUUIDValue(source.RunID).String(),
		},
		Command: request.Command, Cwd: request.Cwd, Env: request.Env, Stdin: request.Stdin,
		Timeout: timeout, IdempotencyKey: idempotencyKey,
		Authorize: func(ctx context.Context, q db.Querier) error {
			_, err := authorizeWorkerRunSource(ctx, q, worker, request.Lease)
			return err
		},
	})
	if err != nil {
		if errors.Is(err, errStaleWorkerRunSource) {
			s.writeWorkerWorkspaceSourceError(w, "exec", request.Lease.RunID, err)
			return
		}
		if failure, ok := workerWorkspaceExecFailure(err); ok {
			writeJSON(w, http.StatusOK, api.WorkerExecuteWorkspaceResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		writeError(w, errors.New("admit run-sourced Workspace exec"))
		return
	}
	if workspaceExecTerminal(admission.Process.State) {
		result, resultErr := workspaceExecResult(admission.Process)
		if resultErr != nil {
			if failure, ok := workerWorkspaceExecFailure(resultErr); ok {
				writeJSON(w, http.StatusOK, api.WorkerExecuteWorkspaceResponse{
					CorrelationID: request.CorrelationID, Failed: &failure,
				})
				return
			}
			s.log.Error("project replayed run-sourced Workspace exec", "run_id", request.Lease.RunID, "error", resultErr)
			writeError(w, errors.New("project run-sourced Workspace exec"))
			return
		}
		writeJSON(w, http.StatusOK, api.WorkerExecuteWorkspaceResponse{
			CorrelationID: request.CorrelationID, Completed: &result,
		})
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerExecuteWorkspaceResponse{
		CorrelationID: request.CorrelationID,
		Pending:       &api.WorkerWorkspaceExecPending{ProcessID: pgvalue.MustUUIDValue(admission.Process.ID).String()},
	})
}

func (s *Server) workerPollWorkspaceExec(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.WorkerPollWorkspaceExecRequest
	if err := decodeWorkerActorRequest(r, &request, "Workspace exec poll"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := validateWorkerWorkspaceRequest(request.WorkerRetrieveWorkspaceRequest); err != nil {
		writeError(w, badRequest(err))
		return
	}
	processID, err := parseCanonicalUUID("process_id", request.ProcessID)
	if err != nil || processID == uuid.Nil {
		writeError(w, badRequest(errors.New("Workspace exec process_id is invalid")))
		return
	}
	worker := workerFromContext(r.Context())
	var process db.WorkspaceProcess
	err = s.inTx(r.Context(), func(work *txWork) error {
		source, err := authorizeWorkerRunSource(r.Context(), work.q, worker, request.Lease)
		if err != nil {
			return err
		}
		record, err := resolveWorkerWorkspace(r.Context(), work.q, source, request.Workspace)
		if err != nil {
			return err
		}
		process, err = work.q.GetWorkspaceExec(r.Context(), db.GetWorkspaceExecParams{
			OrgID: source.OrgID, ProjectID: source.ProjectID, EnvironmentID: source.EnvironmentID,
			WorkspaceID: record.ID, ID: pgvalue.UUID(processID),
		})
		if err != nil {
			return err
		}
		if process.CreatedBySubjectType != "run" ||
			process.CreatedBySubjectID != pgvalue.MustUUIDValue(source.RunID).String() {
			return pgx.ErrNoRows
		}
		return nil
	})
	if err != nil {
		if failure, ok := workerWorkspaceReferenceFailure(err); ok {
			writeJSON(w, http.StatusOK, api.WorkerExecuteWorkspaceResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		s.writeWorkerWorkspaceSourceError(w, "exec poll", request.Lease.RunID, err)
		return
	}
	if !workspaceExecTerminal(process.State) {
		writeJSON(w, http.StatusOK, api.WorkerExecuteWorkspaceResponse{
			CorrelationID: request.CorrelationID,
			Pending:       &api.WorkerWorkspaceExecPending{ProcessID: request.ProcessID},
		})
		return
	}
	result, err := workspaceExecResult(process)
	if err != nil {
		if failure, ok := workerWorkspaceExecFailure(err); ok {
			writeJSON(w, http.StatusOK, api.WorkerExecuteWorkspaceResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		s.log.Error("project run-sourced Workspace exec", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("project run-sourced Workspace exec"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerExecuteWorkspaceResponse{
		CorrelationID: request.CorrelationID, Completed: &result,
	})
}

func (s *Server) workerDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.WorkerDeleteWorkspaceRequest
	if err := decodeWorkerActorRequest(r, &request, "Workspace delete"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := validateWorkerWorkspaceRequest(request.WorkerRetrieveWorkspaceRequest); err != nil {
		writeError(w, badRequest(err))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	source, record, err := s.workerWorkspaceSourceAndRecord(r.Context(), worker, request.WorkerRetrieveWorkspaceRequest)
	if err != nil {
		if failure, ok := workerWorkspaceReferenceFailure(err); ok {
			writeJSON(w, http.StatusOK, api.WorkerDeleteWorkspaceResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		s.writeWorkerWorkspaceSourceError(w, "delete", request.Lease.RunID, err)
		return
	}
	result, err := s.deleteWorkspace(r.Context(), workspaceDeleteRequest{
		OrgID: pgvalue.MustUUIDValue(source.OrgID), ProjectID: pgvalue.MustUUIDValue(source.ProjectID),
		EnvironmentID: pgvalue.MustUUIDValue(source.EnvironmentID), WorkspaceID: pgvalue.MustUUIDValue(record.ID),
		IdempotencyKey: idempotencyKey,
		Authorize: func(ctx context.Context, q db.Querier) error {
			_, err := authorizeWorkerRunSource(ctx, q, worker, request.Lease)
			return err
		},
	})
	if err != nil {
		if errors.Is(err, errStaleWorkerRunSource) {
			s.writeWorkerWorkspaceSourceError(w, "delete", request.Lease.RunID, err)
			return
		}
		if failure, ok := workerWorkspaceDeleteFailure(err); ok {
			writeJSON(w, http.StatusOK, api.WorkerDeleteWorkspaceResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		writeError(w, errors.New("delete run-sourced Workspace"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerDeleteWorkspaceResponse{
		CorrelationID: request.CorrelationID,
		Completed:     &api.DeleteWorkspaceReceipt{WorkspaceID: result.WorkspaceID.String()},
	})
}

func (s *Server) workerWorkspaceSourceAndRecord(
	ctx context.Context,
	worker workerActor,
	request api.WorkerRetrieveWorkspaceRequest,
) (workerRunSourceAuthority, db.Workspace, error) {
	var source workerRunSourceAuthority
	var record db.Workspace
	err := s.inTx(ctx, func(work *txWork) error {
		var err error
		source, err = authorizeWorkerRunSource(ctx, work.q, worker, request.Lease)
		if err != nil {
			return err
		}
		record, err = resolveWorkerWorkspace(ctx, work.q, source, request.Workspace)
		return err
	})
	return source, record, err
}

func resolveWorkerWorkspace(
	ctx context.Context,
	q db.Querier,
	source workerRunSourceAuthority,
	address api.WorkerWorkspaceAddress,
) (db.Workspace, error) {
	workspaceID := pgtype.UUID{}
	key := pgtype.Text{}
	if address.WorkspaceID != "" {
		id, err := ids.Parse(address.WorkspaceID)
		if err != nil {
			return db.Workspace{}, err
		}
		workspaceID = pgvalue.UUID(id)
	} else {
		key = pgtype.Text{String: address.WorkspaceKey, Valid: true}
	}
	id, err := q.ResolveWorkspaceTarget(ctx, db.ResolveWorkspaceTargetParams{
		OrgID: source.OrgID, ProjectID: source.ProjectID, EnvironmentID: source.EnvironmentID,
		ID: workspaceID, Key: key,
	})
	if err != nil {
		return db.Workspace{}, err
	}
	return q.GetWorkspace(ctx, db.GetWorkspaceParams{
		OrgID: source.OrgID, ProjectID: source.ProjectID, EnvironmentID: source.EnvironmentID, ID: id,
	})
}

func validateWorkerWorkspaceRequest(request api.WorkerRetrieveWorkspaceRequest) error {
	if err := validateWorkerWorkspaceCorrelation(request.CorrelationID); err != nil {
		return err
	}
	hasID := request.Workspace.WorkspaceID != ""
	hasKey := request.Workspace.WorkspaceKey != ""
	if hasID == hasKey {
		return errors.New("Workspace address requires exactly one of workspace_id or workspace_key")
	}
	if hasID {
		if err := ids.Validate(request.Workspace.WorkspaceID); err != nil {
			return errors.New("Workspace ID is invalid")
		}
	} else if err := validateWorkspaceKey(&request.Workspace.WorkspaceKey); err != nil {
		return err
	}
	return nil
}

func validateWorkerWorkspaceCorrelation(value string) error {
	if err := ids.Validate(value); err != nil {
		return errors.New("Workspace runtime correlation ID is invalid")
	}
	return nil
}

func (s *Server) decodeWorkerWorkspaceFileRequest(
	w http.ResponseWriter,
	r *http.Request,
	request *api.WorkerReadWorkspaceFileRequest,
	label string,
) bool {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return false
	}
	if err := decodeWorkerActorRequest(r, request, label); err != nil {
		writeError(w, badRequest(err))
		return false
	}
	if err := validateWorkerWorkspaceRequest(request.WorkerRetrieveWorkspaceRequest); err != nil {
		writeError(w, badRequest(err))
		return false
	}
	target, err := validateWorkspaceFilePath(request.Path)
	if err != nil {
		writeError(w, badRequest(err))
		return false
	}
	request.Path = target
	return true
}

func (s *Server) writeWorkerWorkspaceReadResult(
	w http.ResponseWriter,
	correlationID string,
	runID string,
	operation string,
	err error,
) bool {
	if err == nil {
		return false
	}
	if failure, ok := workerWorkspaceReferenceFailure(err); ok {
		writeJSON(w, http.StatusOK, api.WorkerRetrieveWorkspaceResponse{
			CorrelationID: correlationID, Failed: &failure,
		})
		return true
	}
	s.writeWorkerWorkspaceSourceError(w, operation, runID, err)
	return true
}

func (s *Server) workerWorkspaceFileFailure(
	w http.ResponseWriter,
	runID string,
	operation string,
	err error,
) (*api.WorkerRuntimeOperationFailure, bool) {
	if err == nil {
		return nil, false
	}
	if errors.Is(err, errStaleWorkerRunSource) {
		s.writeWorkerWorkspaceSourceError(w, operation, runID, err)
		return nil, true
	}
	if failure, ok := workerWorkspaceReferenceFailure(err); ok {
		return &failure, true
	}
	switch {
	case errors.Is(err, archive.ErrTarEntryNotFound):
		return &api.WorkerRuntimeOperationFailure{Code: "workspace_file_not_found", Message: "Workspace file was not found"}, true
	case errors.Is(err, errWorkspaceFileCursorExpired):
		return &api.WorkerRuntimeOperationFailure{Code: "workspace_file_cursor_expired", Message: "Workspace file cursor expired"}, true
	case errors.Is(err, errWorkspaceFileCursorInvalid):
		return &api.WorkerRuntimeOperationFailure{Code: "invalid_workspace_file_cursor", Message: "Workspace file cursor is invalid"}, true
	default:
		s.log.Error("read run-sourced Workspace file", "operation", operation, "run_id", runID, "error", err)
		writeError(w, errors.New("read run-sourced Workspace file"))
		return nil, true
	}
}

func workerWorkspaceReferenceFailure(err error) (api.WorkerRuntimeOperationFailure, bool) {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return api.WorkerRuntimeOperationFailure{Code: "workspace_not_found", Message: "Workspace was not found"}, true
	case errors.Is(err, errStaleWorkerRunSource):
		return api.WorkerRuntimeOperationFailure{}, false
	default:
		return api.WorkerRuntimeOperationFailure{}, false
	}
}

func workerWorkspaceCreateFailure(err error) (api.WorkerRuntimeOperationFailure, bool) {
	var keyConflict WorkspaceKeyConflictError
	var idempotencyConflict idempotency.ConflictError
	switch {
	case errors.Is(err, errWorkspaceCreateInvalid):
		return api.WorkerRuntimeOperationFailure{Code: "invalid_workspace_create", Message: err.Error()}, true
	case errors.Is(err, errWorkspaceNotDeployed):
		return api.WorkerRuntimeOperationFailure{Code: "workspace_not_deployed", Message: err.Error()}, true
	case errors.Is(err, errWorkspaceSecretUnavailable):
		return api.WorkerRuntimeOperationFailure{Code: "secret_unavailable", Message: err.Error()}, true
	case errors.As(err, &keyConflict):
		return api.WorkerRuntimeOperationFailure{Code: "workspace_key_conflict", Message: err.Error()}, true
	case errors.As(err, &idempotencyConflict):
		return api.WorkerRuntimeOperationFailure{Code: "idempotency_conflict", Message: err.Error()}, true
	default:
		return api.WorkerRuntimeOperationFailure{}, false
	}
}

func workerWorkspaceExecFailure(err error) (api.WorkerRuntimeOperationFailure, bool) {
	var idempotencyConflict idempotency.ConflictError
	var coder errorCoder
	if errors.As(err, &coder) && coder.ErrorCode() != "" {
		retryable := false
		var retryer errorRetryer
		if errors.As(err, &retryer) {
			retryable = retryer.ErrorRetryable()
		}
		return api.WorkerRuntimeOperationFailure{Code: coder.ErrorCode(), Message: err.Error(), Retryable: retryable}, true
	}
	switch {
	case errors.Is(err, errWorkspaceExecStdinTooLarge):
		return api.WorkerRuntimeOperationFailure{Code: "workspace_stdin_too_large", Message: err.Error()}, true
	case errors.Is(err, errWorkspaceExecTooLarge):
		return api.WorkerRuntimeOperationFailure{Code: "workspace_exec_request_too_large", Message: err.Error()}, true
	case errors.Is(err, errWorkspaceExecInvalid):
		return api.WorkerRuntimeOperationFailure{Code: "invalid_workspace_exec", Message: err.Error()}, true
	case errors.Is(err, errWorkspaceSecretUnavailable):
		return api.WorkerRuntimeOperationFailure{Code: "secret_unavailable", Message: err.Error()}, true
	case errors.Is(err, errWorkspaceNotFound), errors.Is(err, pgx.ErrNoRows):
		return api.WorkerRuntimeOperationFailure{Code: "workspace_not_found", Message: "Workspace was not found"}, true
	case errors.Is(err, errWorkspaceBusy):
		return api.WorkerRuntimeOperationFailure{Code: "workspace_busy", Message: err.Error(), Retryable: true}, true
	case errors.As(err, &idempotencyConflict):
		return api.WorkerRuntimeOperationFailure{Code: "idempotency_conflict", Message: err.Error()}, true
	default:
		return api.WorkerRuntimeOperationFailure{}, false
	}
}

func workerWorkspaceDeleteFailure(err error) (api.WorkerRuntimeOperationFailure, bool) {
	var idempotencyConflict idempotency.ConflictError
	switch {
	case errors.Is(err, errWorkspaceNotFound):
		return api.WorkerRuntimeOperationFailure{Code: "workspace_not_found", Message: err.Error()}, true
	case errors.Is(err, errWorkspaceBusy):
		return api.WorkerRuntimeOperationFailure{Code: "workspace_busy", Message: err.Error(), Retryable: true}, true
	case errors.As(err, &idempotencyConflict):
		return api.WorkerRuntimeOperationFailure{Code: "idempotency_conflict", Message: err.Error()}, true
	default:
		return api.WorkerRuntimeOperationFailure{}, false
	}
}

func (s *Server) writeWorkerWorkspaceSourceError(
	w http.ResponseWriter,
	operation string,
	runID string,
	err error,
) {
	if errors.Is(err, errStaleWorkerRunSource) {
		writeError(w, conflict(errStaleWorkerRunSource))
		return
	}
	s.log.Error("authorize worker Workspace operation source",
		"operation", operation, "run_id", runID, "error", err)
	writeError(w, errors.New("authorize worker Workspace operation source"))
}
