package controlplane

import (
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

const computerCommandBodyMaxBytes = int64(2 << 20)

func computerCommandCreatorFromPrincipal(principal auth.Principal) command.Creator {
	creator := command.Creator{SubjectType: string(principal.Kind)}
	switch principal.Kind {
	case auth.PrincipalKindAPIKey:
		if principal.APIKeyID != uuid.Nil() {
			creator.SubjectID = principal.APIKeyID.String()
			return creator
		}
	case auth.PrincipalKindSession:
		if principal.SessionID != uuid.Nil() {
			creator.SubjectID = principal.SessionID.String()
			return creator
		}
	}
	return creator
}

func publicCommandInfo(process db.ComputerCommand) (api.CommandInfo, error) {
	if process.ResultPrunedAt.Valid {
		return api.CommandInfo{}, gone(codedError{code: "command_result_expired", message: "exec result has expired"})
	}
	resource := api.CommandInfo{ID: pgvalue.MustUUIDValue(process.ID).String(), ComputerID: pgvalue.MustUUIDValue(process.ComputerID).String(), Status: string(process.Status)}
	switch process.Status {
	case db.ComputerCommandStatusPending, db.ComputerCommandStatusStarting, db.ComputerCommandStatusRunning:
		return resource, nil
	case db.ComputerCommandStatusStopping:
		return resource, nil
	case db.ComputerCommandStatusExited, db.ComputerCommandStatusFailed, db.ComputerCommandStatusCancelled, db.ComputerCommandStatusTimedOut, db.ComputerCommandStatusLost:
	default:
		return api.CommandInfo{}, errors.New("exec state is invalid")
	}
	if !process.TerminalAt.Valid {
		return api.CommandInfo{}, errors.New("exec terminal timestamp is missing")
	}
	outcome := &api.CommandOutcome{CommandID: resource.ID, TerminalAt: process.TerminalAt.Time, Kind: "system_failed"}
	if process.ExitCode.Valid {
		code := process.ExitCode.Int32
		outcome.ExitCode = &code
	}
	if process.Status == db.ComputerCommandStatusExited {
		if outcome.ExitCode == nil {
			return api.CommandInfo{}, errors.New("exec exit code is missing")
		}
		outcome.Kind = "exited"
	} else if process.Status == db.ComputerCommandStatusCancelled || process.Status == db.ComputerCommandStatusTimedOut {
		outcome.Kind = process.Status
	} else {
		switch process.FailureReason.String {
		case "guest_failure", "dispatch_failed", "scope_termination_failed":
			outcome.Failure = &api.CommandFailure{Reason: process.FailureReason.String}
		default:
			return api.CommandInfo{}, errors.New("command failure reason is invalid")
		}
	}
	resource.Outcome = outcome
	resource.ProcessReconciled = !process.ComputerLeaseEpoch.Valid || process.ProcessReconciledAt.Valid
	return resource, nil
}
