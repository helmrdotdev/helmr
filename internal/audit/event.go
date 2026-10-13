// Package audit records committed administrative changes in their owner's transaction.
package audit

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Actor identifies the authority that performed the operation, without credentials.
type Actor struct {
	Kind         string
	UserID       uuid.UUID
	CredentialID uuid.UUID
	Source       string
}

func FromPrincipal(p auth.Principal) Actor {
	switch p.Kind {
	case auth.PrincipalKindAPIKey:
		return Actor{Kind: "api_key", CredentialID: p.APIKeyID}
	case auth.PrincipalKindSession:
		return Actor{Kind: "login", UserID: p.UserID, CredentialID: p.SessionID}
	default:
		return Actor{}
	}
}

func VerifiedLogin(user uuid.UUID, provider string) Actor {
	return Actor{Kind: "verified_login", UserID: user, Source: provider}
}

func System(source string) Actor { return Actor{Kind: "system", Source: source} }
func CapacityAuthority() Actor   { return Actor{Kind: "capacity"} }

// Event contains identities and bounded grant facts, never credential material or
// arbitrary request metadata. Environment scope is resolved from authoritative
// parent IDs before insertion; installation events leave all scope IDs empty.
type Event struct {
	Actor         Actor
	OrgID         uuid.UUID
	EnvironmentID uuid.UUID
	Action        string
	TargetKind    string
	TargetID      uuid.UUID
	TargetUserID  uuid.UUID
	SlackTeamID   string
	SlackUserID   string
	Details       Details
}

// Append joins the mutation transaction: failure must prevent its commit.
func Append(ctx context.Context, tx pgx.Tx, e Event) error {
	if tx == nil || e.Action == "" || e.TargetKind == "" {
		return errors.New("incomplete audit event")
	}
	details, err := marshalDetails(e.Action, e.Details)
	if err != nil {
		return err
	}
	a := e.Actor
	valid := false
	switch a.Kind {
	case "login":
		valid = a.UserID != uuid.Nil() && a.CredentialID != uuid.Nil() && a.Source == ""
	case "api_key":
		valid = a.UserID == uuid.Nil() && a.CredentialID != uuid.Nil() && a.Source == ""
	case "verified_login":
		valid = a.UserID != uuid.Nil() && a.CredentialID == uuid.Nil() && a.Source != ""
	case "capacity":
		valid = a.UserID == uuid.Nil() && a.CredentialID == uuid.Nil() && a.Source == ""
	case "system":
		valid = a.UserID == uuid.Nil() && a.CredentialID == uuid.Nil() && a.Source != ""
	}
	if !valid {
		return errors.New("invalid audit actor")
	}
	var project uuid.UUID
	if e.EnvironmentID != uuid.Nil() {
		var org uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT p.org_id,e.project_id FROM environments e JOIN projects p ON p.id=e.project_id WHERE e.id=$1`, e.EnvironmentID).Scan(&org, &project); err != nil {
			return fmt.Errorf("resolve audit scope: %w", err)
		}
		if e.OrgID != uuid.Nil() && e.OrgID != org {
			return errors.New("audit scope mismatch")
		}
		e.OrgID = org
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events (
 id,actor_kind,actor_user_id,actor_credential_id,actor_source,org_id,project_id,environment_id,
 action,target_kind,target_id,target_user_id,slack_team_id,slack_user_id,details)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		uuid.NewV7(), a.Kind, optionalUUID(a.UserID), optionalUUID(a.CredentialID), a.Source,
		optionalUUID(e.OrgID), optionalUUID(project), optionalUUID(e.EnvironmentID),
		e.Action, e.TargetKind, optionalUUID(e.TargetID), optionalUUID(e.TargetUserID), e.SlackTeamID, e.SlackUserID, details)
	if err != nil {
		return fmt.Errorf("append audit event: %w", err)
	}
	return nil
}

func optionalUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: id != uuid.Nil()} }
