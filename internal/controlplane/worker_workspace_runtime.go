package controlplane

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
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func (s *Server) workerCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.CreateWorkspaceRequest
	if err := decodeWorkerActorRequest(r, &request, "workspace create"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := validateWorkerWorkspaceCorrelation(request.CorrelationID); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := api.ValidateSandboxDeclaredID(request.SandboxDeclaredID); err != nil {
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
		s.writeWorkerWorkspaceSourceError(w, "create", request.Lease.ID, err)
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
		DeclaredID: request.SandboxDeclaredID,
		Key:        request.Key, Secrets: request.Secrets, IdempotencyKey: idempotencyKey,
		Authorize: func(ctx context.Context, q db.Querier) error {
			_, err := authorizeWorkerRunSource(ctx, q, worker, request.Lease)
			return err
		},
	})
	if err != nil {
		if errors.Is(err, errStaleWorkerRunSource) {
			s.writeWorkerWorkspaceSourceError(w, "create", request.Lease.ID, err)
			return
		}
		if failure, ok := workerWorkspaceCreateFailure(err); ok {
			writeJSON(w, http.StatusOK, workerapi.CreateWorkspaceResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		s.log.Error("create run-sourced Workspace", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("create run-sourced workspace"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.CreateWorkspaceResponse{
		CorrelationID: request.CorrelationID,
		Completed:     &workerapi.CreateWorkspaceResult{WorkspaceID: result.WorkspaceID.String()},
	})
}

func (s *Server) workerRetrieveWorkspace(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.RetrieveWorkspaceRequest
	if err := decodeWorkerActorRequest(r, &request, "workspace retrieve"); err != nil {
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
	if s.writeWorkerWorkspaceReadResult(w, request.CorrelationID, request.Lease.ID, "retrieve", err) {
		return
	}
	writeJSON(w, http.StatusOK, workerapi.RetrieveWorkspaceResponse{
		CorrelationID: request.CorrelationID, Completed: &snapshot,
	})
}

func (s *Server) workerReadWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ReadWorkspaceFileRequest
	if !s.decodeWorkerWorkspaceFileRequest(w, r, &request, "workspace file read") {
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
	if failure, handled := s.workerWorkspaceFileFailure(w, request.Lease.ID, "read", err); handled {
		if failure != nil {
			writeJSON(w, http.StatusOK, workerapi.ReadWorkspaceFileResponse{
				CorrelationID: request.CorrelationID, Failed: failure,
			})
		}
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ReadWorkspaceFileResponse{
		CorrelationID: request.CorrelationID, Completed: &content,
	})
}

func (s *Server) workerStatWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ReadWorkspaceFileRequest
	if !s.decodeWorkerWorkspaceFileRequest(w, r, &request, "workspace file stat") {
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
	if failure, handled := s.workerWorkspaceFileFailure(w, request.Lease.ID, "stat", err); handled {
		if failure != nil {
			writeJSON(w, http.StatusOK, workerapi.StatWorkspaceFileResponse{
				CorrelationID: request.CorrelationID, Failed: failure,
			})
		}
		return
	}
	writeJSON(w, http.StatusOK, workerapi.StatWorkspaceFileResponse{
		CorrelationID: request.CorrelationID, Completed: &entry,
	})
}

func (s *Server) workerListWorkspaceFiles(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.ListWorkspaceFilesRequest
	if err := decodeWorkerActorRequest(r, &request, "workspace file list"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := validateWorkerWorkspaceRequest(request.RetrieveWorkspaceRequest); err != nil {
		writeError(w, badRequest(err))
		return
	}
	target, err := validateWorkspaceFilePath(request.Path)
	if err != nil || request.Limit < 1 || request.Limit > workspaceFileListMaxLimit {
		writeError(w, badRequest(errors.New("workspace file list request is invalid")))
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
	if failure, handled := s.workerWorkspaceFileFailure(w, request.Lease.ID, "list", err); handled {
		if failure != nil {
			writeJSON(w, http.StatusOK, workerapi.ListWorkspaceFilesResponse{
				CorrelationID: request.CorrelationID, Failed: failure,
			})
		}
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ListWorkspaceFilesResponse{
		CorrelationID: request.CorrelationID, Completed: &page,
	})
}

func (s *Server) workerExecuteWorkspace(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.ExecuteWorkspaceRequest
	if err := decodeWorkerActorRequest(r, &request, "workspace exec"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := validateWorkerWorkspaceRequest(request.RetrieveWorkspaceRequest); err != nil {
		writeError(w, badRequest(err))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil || idempotencyKey == "" {
		writeError(w, badRequest(errors.New("workspace exec idempotency_key is invalid")))
		return
	}
	timeout := time.Duration(0)
	if request.TimeoutMS != nil {
		timeout = time.Duration(*request.TimeoutMS) * time.Millisecond
	}
	worker := workerFromContext(r.Context())
	source, record, err := s.workerWorkspaceSourceAndRecord(r.Context(), worker, request.RetrieveWorkspaceRequest)
	if err != nil {
		if failure, ok := workerWorkspaceReferenceFailure(err); ok {
			writeJSON(w, http.StatusOK, workerapi.ExecuteWorkspaceResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		s.writeWorkerWorkspaceSourceError(w, "exec", request.Lease.ID, err)
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
			s.writeWorkerWorkspaceSourceError(w, "exec", request.Lease.ID, err)
			return
		}
		if failure, ok := workerWorkspaceExecFailure(err); ok {
			writeJSON(w, http.StatusOK, workerapi.ExecuteWorkspaceResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		writeError(w, errors.New("admit run-sourced workspace exec"))
		return
	}
	if workspaceExecTerminal(admission.Process.State) {
		result, resultErr := workspaceExecResult(admission.Process)
		if resultErr != nil {
			if failure, ok := workerWorkspaceExecFailure(resultErr); ok {
				writeJSON(w, http.StatusOK, workerapi.ExecuteWorkspaceResponse{
					CorrelationID: request.CorrelationID, Failed: &failure,
				})
				return
			}
			s.log.Error("project replayed run-sourced Workspace exec", "run_lease_id", request.Lease.ID, "error", resultErr)
			writeError(w, errors.New("project run-sourced workspace exec"))
			return
		}
		writeJSON(w, http.StatusOK, workerapi.ExecuteWorkspaceResponse{
			CorrelationID: request.CorrelationID, Completed: &result,
		})
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ExecuteWorkspaceResponse{
		CorrelationID: request.CorrelationID,
		Pending:       &workerapi.WorkspaceExecPending{ProcessID: pgvalue.MustUUIDValue(admission.Process.ID).String()},
	})
}

func (s *Server) workerPollWorkspaceExec(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.PollWorkspaceExecRequest
	if err := decodeWorkerActorRequest(r, &request, "workspace exec poll"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := validateWorkerWorkspaceRequest(request.RetrieveWorkspaceRequest); err != nil {
		writeError(w, badRequest(err))
		return
	}
	processID, err := parseCanonicalUUID("process_id", request.ProcessID)
	if err != nil || processID == uuid.Nil {
		writeError(w, badRequest(errors.New("workspace exec process_id is invalid")))
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
			writeJSON(w, http.StatusOK, workerapi.ExecuteWorkspaceResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		s.writeWorkerWorkspaceSourceError(w, "exec poll", request.Lease.ID, err)
		return
	}
	if !workspaceExecTerminal(process.State) {
		writeJSON(w, http.StatusOK, workerapi.ExecuteWorkspaceResponse{
			CorrelationID: request.CorrelationID,
			Pending:       &workerapi.WorkspaceExecPending{ProcessID: request.ProcessID},
		})
		return
	}
	result, err := workspaceExecResult(process)
	if err != nil {
		if failure, ok := workerWorkspaceExecFailure(err); ok {
			writeJSON(w, http.StatusOK, workerapi.ExecuteWorkspaceResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		s.log.Error("project run-sourced Workspace exec", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("project run-sourced workspace exec"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ExecuteWorkspaceResponse{
		CorrelationID: request.CorrelationID, Completed: &result,
	})
}

func (s *Server) workerDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.DeleteWorkspaceRequest
	if err := decodeWorkerActorRequest(r, &request, "workspace delete"); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := validateWorkerWorkspaceRequest(request.RetrieveWorkspaceRequest); err != nil {
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
		if failure, ok := workerWorkspaceReferenceFailure(err); ok {
			writeJSON(w, http.StatusOK, workerapi.DeleteWorkspaceResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		s.writeWorkerWorkspaceSourceError(w, "delete", request.Lease.ID, err)
		return
	}
	workspaceID, err := ids.Parse(request.Workspace.WorkspaceID)
	if err != nil {
		writeError(w, badRequest(errors.New("workspace ID is invalid")))
		return
	}
	result, err := s.deleteWorkspace(r.Context(), workspaceDeleteRequest{
		OrgID: pgvalue.MustUUIDValue(source.OrgID), ProjectID: pgvalue.MustUUIDValue(source.ProjectID),
		EnvironmentID: pgvalue.MustUUIDValue(source.EnvironmentID), WorkspaceID: workspaceID,
		IdempotencyKey: idempotencyKey,
		Authorize: func(ctx context.Context, q db.Querier) error {
			_, err := authorizeWorkerRunSource(ctx, q, worker, request.Lease)
			return err
		},
	})
	if err != nil {
		if errors.Is(err, errStaleWorkerRunSource) {
			s.writeWorkerWorkspaceSourceError(w, "delete", request.Lease.ID, err)
			return
		}
		if failure, ok := workerWorkspaceDeleteFailure(err); ok {
			writeJSON(w, http.StatusOK, workerapi.DeleteWorkspaceResponse{
				CorrelationID: request.CorrelationID, Failed: &failure,
			})
			return
		}
		writeError(w, errors.New("delete run-sourced workspace"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.DeleteWorkspaceResponse{
		CorrelationID: request.CorrelationID,
		Completed:     &api.DeleteWorkspaceReceipt{WorkspaceID: result.WorkspaceID.String()},
	})
}

func (s *Server) workerWorkspaceSourceAndRecord(
	ctx context.Context,
	worker workerActor,
	request workerapi.RetrieveWorkspaceRequest,
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
	address workerapi.WorkspaceAddress,
) (db.Workspace, error) {
	id, err := ids.Parse(address.WorkspaceID)
	if err != nil {
		return db.Workspace{}, err
	}
	return q.GetWorkspace(ctx, db.GetWorkspaceParams{
		OrgID: source.OrgID, ProjectID: source.ProjectID, EnvironmentID: source.EnvironmentID,
		ID: pgvalue.UUID(id),
	})
}

func validateWorkerWorkspaceRequest(request workerapi.RetrieveWorkspaceRequest) error {
	if err := validateWorkerWorkspaceCorrelation(request.CorrelationID); err != nil {
		return err
	}
	if err := ids.Validate(request.Workspace.WorkspaceID); err != nil {
		return errors.New("workspace ID is invalid")
	}
	return nil
}

func validateWorkerWorkspaceCorrelation(value string) error {
	if err := ids.Validate(value); err != nil {
		return errors.New("workspace runtime correlation ID is invalid")
	}
	return nil
}

func (s *Server) decodeWorkerWorkspaceFileRequest(
	w http.ResponseWriter,
	r *http.Request,
	request *workerapi.ReadWorkspaceFileRequest,
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
	if err := validateWorkerWorkspaceRequest(request.RetrieveWorkspaceRequest); err != nil {
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
		writeJSON(w, http.StatusOK, workerapi.RetrieveWorkspaceResponse{
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
) (*workerapi.RuntimeOperationFailure, bool) {
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
		return &workerapi.RuntimeOperationFailure{Code: "workspace_file_not_found", Message: "Workspace file was not found"}, true
	case errors.Is(err, errWorkspaceFileCursorExpired):
		return &workerapi.RuntimeOperationFailure{Code: "workspace_file_cursor_expired", Message: "Workspace file cursor expired"}, true
	case errors.Is(err, errWorkspaceFileCursorInvalid):
		return &workerapi.RuntimeOperationFailure{Code: "invalid_workspace_file_cursor", Message: "Workspace file cursor is invalid"}, true
	default:
		s.log.Error("read run-sourced Workspace file", "operation", operation, "run_id", runID, "error", err)
		writeError(w, errors.New("read run-sourced workspace file"))
		return nil, true
	}
}

func workerWorkspaceReferenceFailure(err error) (workerapi.RuntimeOperationFailure, bool) {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return workerapi.RuntimeOperationFailure{Code: "workspace_not_found", Message: "Workspace was not found"}, true
	case errors.Is(err, errStaleWorkerRunSource):
		return workerapi.RuntimeOperationFailure{}, false
	default:
		return workerapi.RuntimeOperationFailure{}, false
	}
}

func workerWorkspaceCreateFailure(err error) (workerapi.RuntimeOperationFailure, bool) {
	var keyConflict WorkspaceKeyConflictError
	var idempotencyConflict idempotency.ConflictError
	switch {
	case errors.Is(err, errWorkspaceCreateInvalid):
		return workerapi.RuntimeOperationFailure{Code: "invalid_workspace_create", Message: err.Error()}, true
	case errors.Is(err, errWorkspaceNotDeployed):
		return workerapi.RuntimeOperationFailure{Code: "workspace_not_deployed", Message: err.Error()}, true
	case errors.Is(err, errWorkspaceSecretUnavailable):
		return workerapi.RuntimeOperationFailure{Code: "secret_unavailable", Message: err.Error()}, true
	case errors.As(err, &keyConflict):
		return workerapi.RuntimeOperationFailure{Code: "workspace_key_conflict", Message: err.Error()}, true
	case errors.As(err, &idempotencyConflict):
		return workerapi.RuntimeOperationFailure{Code: "idempotency_conflict", Message: err.Error()}, true
	default:
		return workerapi.RuntimeOperationFailure{}, false
	}
}

func workerWorkspaceExecFailure(err error) (workerapi.RuntimeOperationFailure, bool) {
	var idempotencyConflict idempotency.ConflictError
	var coder errorCoder
	if errors.As(err, &coder) && coder.ErrorCode() != "" {
		retryable := false
		var retryer errorRetryer
		if errors.As(err, &retryer) {
			retryable = retryer.ErrorRetryable()
		}
		return workerapi.RuntimeOperationFailure{Code: coder.ErrorCode(), Message: err.Error(), Retryable: retryable}, true
	}
	switch {
	case errors.Is(err, errWorkspaceExecStdinTooLarge):
		return workerapi.RuntimeOperationFailure{Code: "workspace_stdin_too_large", Message: err.Error()}, true
	case errors.Is(err, errWorkspaceExecTooLarge):
		return workerapi.RuntimeOperationFailure{Code: "workspace_exec_request_too_large", Message: err.Error()}, true
	case errors.Is(err, errWorkspaceExecInvalid):
		return workerapi.RuntimeOperationFailure{Code: "invalid_workspace_exec", Message: err.Error()}, true
	case errors.Is(err, errWorkspaceSecretUnavailable):
		return workerapi.RuntimeOperationFailure{Code: "secret_unavailable", Message: err.Error()}, true
	case errors.Is(err, errWorkspaceNotFound), errors.Is(err, pgx.ErrNoRows):
		return workerapi.RuntimeOperationFailure{Code: "workspace_not_found", Message: "Workspace was not found"}, true
	case errors.Is(err, errWorkspaceBusy):
		return workerapi.RuntimeOperationFailure{Code: "workspace_busy", Message: err.Error(), Retryable: true}, true
	case errors.As(err, &idempotencyConflict):
		return workerapi.RuntimeOperationFailure{Code: "idempotency_conflict", Message: err.Error()}, true
	default:
		return workerapi.RuntimeOperationFailure{}, false
	}
}

func workerWorkspaceDeleteFailure(err error) (workerapi.RuntimeOperationFailure, bool) {
	var idempotencyConflict idempotency.ConflictError
	switch {
	case errors.Is(err, errWorkspaceNotFound):
		return workerapi.RuntimeOperationFailure{Code: "workspace_not_found", Message: err.Error()}, true
	case errors.Is(err, errWorkspaceBusy):
		return workerapi.RuntimeOperationFailure{Code: "workspace_busy", Message: err.Error(), Retryable: true}, true
	case errors.As(err, &idempotencyConflict):
		return workerapi.RuntimeOperationFailure{Code: "idempotency_conflict", Message: err.Error()}, true
	default:
		return workerapi.RuntimeOperationFailure{}, false
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
	writeError(w, errors.New("authorize worker workspace operation source"))
}
