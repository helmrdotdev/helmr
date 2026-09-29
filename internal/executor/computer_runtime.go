package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/ids"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (task *guestRunLeaseTask) handleComputerRuntime(
	ctx context.Context,
	event *programv0.RunEvent,
) error {
	controlPlane := task.controlPlane.Computers
	var correlationID string
	var completed any
	var failed *workerapi.RuntimeOperationFailure
	switch value := event.Event.(type) {
	case *programv0.RunEvent_ComputerCreateRequested:
		request, err := workerComputerCreateRequest(value.ComputerCreateRequested)
		if err != nil {
			return err
		}
		correlationID = request.CorrelationID
		var response workerapi.CreateComputerResponse
		if err := task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease workerapi.RunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = controlPlane.CreateRunComputer(callCtx, request)
			return callErr
		}); err != nil {
			return fmt.Errorf("create computer: %w", err)
		}
		if response.CorrelationID != correlationID {
			return errors.New("computer create response correlation mismatch")
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
	case *programv0.RunEvent_ComputerRetrieveRequested:
		request, err := workerComputerRetrieveRequest(
			value.ComputerRetrieveRequested.GetCorrelationId(),
			value.ComputerRetrieveRequested.GetComputer(),
		)
		if err != nil {
			return err
		}
		correlationID = request.CorrelationID
		var response workerapi.RetrieveComputerResponse
		if err := task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease workerapi.RunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = controlPlane.RetrieveRunComputer(callCtx, request)
			return callErr
		}); err != nil {
			return fmt.Errorf("retrieve computer: %w", err)
		}
		if response.CorrelationID != correlationID {
			return errors.New("computer retrieve response correlation mismatch")
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
	case *programv0.RunEvent_ComputerMembersRequested:
		base, err := workerComputerRetrieveRequest(
			value.ComputerMembersRequested.GetCorrelationId(),
			value.ComputerMembersRequested.GetComputer(),
		)
		if err != nil {
			return err
		}
		request := workerapi.ComputerMembersRequest{RetrieveComputerRequest: base, ComputerMembersQuery: api.ComputerMembersQuery{Cursor: value.ComputerMembersRequested.GetCursor(), Limit: value.ComputerMembersRequested.GetLimit()}}
		correlationID = request.CorrelationID
		var response workerapi.ComputerMembersResponse
		if err := task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease workerapi.RunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = controlPlane.ListRunComputerMembers(callCtx, request)
			return callErr
		}); err != nil {
			return fmt.Errorf("list Computer members: %w", err)
		}
		if response.CorrelationID != correlationID {
			return errors.New("computer members response correlation mismatch")
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
	case *programv0.RunEvent_ComputerDeleteRequested:
		base, err := workerComputerRetrieveRequest(
			value.ComputerDeleteRequested.GetCorrelationId(),
			value.ComputerDeleteRequested.GetComputer(),
		)
		if err != nil {
			return err
		}
		request := workerapi.DeleteComputerRequest{
			RetrieveComputerRequest: base,
			IdempotencyKey:          value.ComputerDeleteRequested.GetIdempotencyKey(),
		}
		correlationID = request.CorrelationID
		var response workerapi.DeleteComputerResponse
		if err := task.callRunSourceRuntime(ctx, func(
			callCtx context.Context,
			lease workerapi.RunLeaseAssignment,
		) error {
			request.Lease = lease.Fence()
			var callErr error
			response, callErr = controlPlane.DeleteRunComputer(callCtx, request)
			return callErr
		}); err != nil {
			return fmt.Errorf("delete computer: %w", err)
		}
		if response.CorrelationID != correlationID {
			return errors.New("computer delete response correlation mismatch")
		}
		if response.Completed != nil {
			completed = response.Completed
		}
		failed = response.Failed
	default:
		return errors.New("unsupported computer runtime event")
	}
	if (completed == nil) == (failed == nil) {
		return errors.New("computer runtime response must contain exactly one result")
	}
	kind := "completed"
	payload := completed
	if failed != nil {
		if strings.TrimSpace(failed.Code) == "" || strings.TrimSpace(failed.Message) == "" {
			return errors.New("computer runtime failure is invalid")
		}
		kind, payload = "failed", failed
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode computer runtime decision: %w", err)
	}
	return wire.WriteResumeDecision(task.programStream(), &programv0.ResumeDecision{
		CorrelationId: correlationID,
		Kind:          kind,
		DataJson:      string(data),
	})
}

func workerComputerCreateRequest(
	requested *programv0.ComputerCreateRequested,
) (workerapi.CreateComputerRequest, error) {
	if requested == nil {
		return workerapi.CreateComputerRequest{}, errors.New("computer create request is required")
	}
	if err := validateRuntimeComputerCorrelation(requested.GetCorrelationId()); err != nil {
		return workerapi.CreateComputerRequest{}, err
	}
	if err := definition.ValidateSandboxDeclaredID(requested.GetDeclaredId()); err != nil {
		return workerapi.CreateComputerRequest{}, err
	}
	secrets := make([]secretbinding.Binding, 0, len(requested.GetSecrets()))
	for _, placement := range requested.GetSecrets() {
		if placement == nil {
			return workerapi.CreateComputerRequest{}, errors.New("computer secret placement is required")
		}
		secret := secretbinding.Binding{Name: placement.GetSecret()}
		switch value := placement.GetPlacement().(type) {
		case *programv0.ComputerSecretPlacement_Env:
			secret.Env = &secretbinding.Env{Name: value.Env.GetName(), Mode: value.Env.GetMode(), AllowedOrigins: value.Env.GetAllowedOrigins()}
		case *programv0.ComputerSecretPlacement_File:
			secret.File = &secretbinding.File{Path: value.File.GetPath()}
		default:
			return workerapi.CreateComputerRequest{}, errors.New("computer secret target is required")
		}
		if err := secretbinding.ValidateBinding(secret); err != nil {
			return workerapi.CreateComputerRequest{}, err
		}
		secrets = append(secrets, secret)
	}
	return workerapi.CreateComputerRequest{
		CorrelationID: requested.GetCorrelationId(), SandboxDeclaredID: requested.GetDeclaredId(),
		Key: requested.Key, Secrets: secrets, IdempotencyKey: requested.GetIdempotencyKey(),
	}, nil
}

func workerComputerRetrieveRequest(
	correlationID string,
	address *programv0.ComputerAddress,
) (workerapi.RetrieveComputerRequest, error) {
	if err := validateRuntimeComputerCorrelation(correlationID); err != nil {
		return workerapi.RetrieveComputerRequest{}, err
	}
	if address == nil {
		return workerapi.RetrieveComputerRequest{}, errors.New("computer address is required")
	}
	if err := api.ValidateComputerID(address.GetComputerId()); err != nil {
		return workerapi.RetrieveComputerRequest{}, err
	}
	return workerapi.RetrieveComputerRequest{
		CorrelationID: correlationID,
		Computer:      workerapi.ComputerAddress{ComputerID: address.GetComputerId()},
	}, nil
}

func validateRuntimeComputerCorrelation(value string) error {
	if err := ids.Validate(value); err != nil {
		return errors.New("computer runtime correlation ID is invalid")
	}
	return nil
}
