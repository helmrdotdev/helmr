// Package deployment owns persisted Deployments of an environment: finalizing
// an uploaded bundle into a Deployment, promoting its schedules and preparation
// refresh policy, and reading Deployments and their
// declared definitions. Operations take the calling principal where
// authorization applies, own their transactions and return the errors declared
// here; callers map them to their transport.
package deployment

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrFinalizationConflict = errors.New("finalization retry key belongs to another bundle")
	ErrPermissionRequired   = errors.New("permission is required")
	// ErrNotFound reports a Deployment that is absent from the environment.
	ErrNotFound = errors.New("deployment not found")
	// ErrNotDeployable reports a promotion target that is absent from the
	// environment.
	ErrNotDeployable = errors.New("deployment not found or is not deployable")
	// ErrNoCurrentDeployment reports an environment without a promoted
	// Deployment.
	ErrNoCurrentDeployment = errors.New("no current deployment")
	// ErrDefinitionNotFound reports a definition the Deployment does not
	// declare.
	ErrDefinitionNotFound = errors.New("definition not found")
	// ErrSelectedDeploymentNotFound reports that the Deployment selected for a
	// definition read is absent from the environment.
	ErrSelectedDeploymentNotFound = errors.New("selected deployment not found")
	// ErrNoCurrentDefinitions reports a definition read of the current
	// Deployment in an environment without one.
	ErrNoCurrentDefinitions = errors.New("environment has no current deployment")
)

// InputError reports a caller-supplied request or bundle that the Deployment
// domain rejects: an idempotency key, an uploaded bundle that cannot be
// admitted, or schedules that cannot be reconciled on promotion.
type InputError struct {
	Err error
}

func (e InputError) Error() string { return e.Err.Error() }
func (e InputError) Unwrap() error { return e.Err }

func invalidInput(err error) error {
	return InputError{Err: err}
}

// InvalidObjectError reports a bundle object whose stored bytes fail
// verification. Its message may describe object contents and is not public.
type InvalidObjectError struct {
	Err error
}

func (e InvalidObjectError) Error() string { return e.Err.Error() }
func (e InvalidObjectError) Unwrap() error { return e.Err }

func authorize(principal auth.Principal, scope auth.Scope, permissions ...auth.Permission) error {
	for _, permission := range permissions {
		if principal.HasPermission(permission, scope) {
			return nil
		}
	}
	return ErrPermissionRequired
}

func authorizeDeploy(principal auth.Principal, scope auth.Scope) error {
	return authorize(principal, scope, auth.PermissionDeploymentsWrite)
}

func authorizeRead(principal auth.Principal, scope auth.Scope) error {
	return authorize(principal, scope, auth.PermissionDeploymentsWrite, auth.PermissionSessionsRead)
}

func scopeIDs(scope auth.Scope) (pgtype.UUID, pgtype.UUID, error) {
	projectID, err := ids.Parse(scope.ProjectID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("project id: %w", err)
	}
	environmentID, err := ids.Parse(scope.EnvironmentID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("environment id: %w", err)
	}
	return pgvalue.UUID(projectID), pgvalue.UUID(environmentID), nil
}

// version names a Deployment by its creation date and ID.
func version(id uuid.UUID) string {
	milliseconds := int64(binary.BigEndian.Uint64(id[:]) >> 16)
	return time.UnixMilli(milliseconds).UTC().Format("20060102") + "." + id.String()
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
