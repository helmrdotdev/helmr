// Package audit captures bounded security facts in the operation owner's transaction.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Actor identifies the actual executor, separately from its credential and initiator.
// Identity is loaded from the account inside the capture transaction for humans.
type Actor struct {
	Kind              string
	ID                uuid.UUID
	MemberID          uuid.UUID
	CredentialKind    string
	CredentialID      uuid.UUID
	CredentialVersion int64
	RuntimeGeneration int64
	ProcessEpoch      int64
	Source            string
	Authorization     Authorization
}

type Authorization struct {
	Basis       string                    `json:"basis"`
	Decision    string                    `json:"decision"`
	Reason      string                    `json:"reason,omitempty"`
	Permissions []auth.PermissionDecision `json:"permissions,omitempty"`
}

func FromPrincipal(p auth.Principal) Actor {
	if p.Kind == auth.PrincipalKindAPIKey {
		return Actor{Kind: "api_key", ID: p.APIKeyID, MemberID: p.MemberID, CredentialKind: "api_key", CredentialID: p.APIKeyID, CredentialVersion: p.CredentialVersion}
	}
	if p.Kind == auth.PrincipalKindSession {
		kind := "human"
		member := p.MemberID
		if p.Admin && p.OrgID == uuid.Nil() {
			kind = "installation_operator"
			member = uuid.Nil()
		}
		return Actor{Kind: kind, ID: p.UserID, MemberID: member, CredentialKind: "login_session", CredentialID: p.SessionID}
	}
	return Actor{}
}

// Authorized records the exact decision path evaluated on the owner's current
// principal. It does not re-read policy after the mutation has changed it.
func Authorized(p auth.Principal, permission auth.Permission, scope auth.Scope) (Actor, error) {
	decisions, ok := p.Decide(permission, scope)
	if !ok {
		return Actor{}, errors.New("audit permission decision denied")
	}
	a := FromPrincipal(p)
	a.Authorization = Authorization{Basis: "permission", Decision: "allowed", Permissions: decisions}
	return a, nil
}

func VerifiedLogin(user uuid.UUID, provider string) Actor {
	return Actor{Kind: "human", ID: user, CredentialKind: provider, Authorization: Authorization{Basis: "verified_identity", Decision: "allowed"}}
}
func System(source string) Actor {
	return Actor{Kind: "service", Source: source, CredentialKind: "service", Authorization: Authorization{Basis: "service", Decision: "allowed"}}
}
func CapacityAuthority() Actor {
	a := System("capacity-controller")
	a.CredentialKind = "capacity_token"
	return a
}
func InstallationOperator(p auth.Principal) (Actor, error) {
	if p.Kind != auth.PrincipalKindSession || !p.Admin {
		return Actor{}, errors.New("installation authority is required")
	}
	a := FromPrincipal(p)
	a.Kind = "installation_operator"
	a.MemberID = uuid.Nil()
	a.Authorization = Authorization{Basis: "installation_admin", Decision: "allowed"}
	return a, nil
}
func RuntimeActor(session uuid.UUID, generation, epoch int64) Actor {
	return Actor{Kind: "session", ID: session, CredentialKind: "session_runtime", RuntimeGeneration: generation, ProcessEpoch: epoch, Authorization: Authorization{Basis: "runtime", Decision: "allowed"}}
}

// Request contains only trusted transport metadata; service dependencies do not
// travel through context. Its operation ID correlates facts from the same request.
type Request struct {
	OperationID uuid.UUID
	SourceIP    netip.Addr
	Channel     string
}
type requestKey struct{}

func WithRequest(ctx context.Context, request Request) context.Context {
	return context.WithValue(ctx, requestKey{}, request)
}
func RequestFrom(ctx context.Context) Request { r, _ := ctx.Value(requestKey{}).(Request); return r }

type Identity struct {
	Source      string `json:"source"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email,omitempty"`
}
type Causality struct {
	InitiatorKind string    `json:"initiator_kind,omitempty"`
	InitiatorID   uuid.UUID `json:"initiator_id,omitzero"`
	SourceID      uuid.UUID `json:"source_id,omitzero"`
}

type Event struct {
	ID            uuid.UUID
	OperationID   uuid.UUID
	OccurredAt    time.Time
	Actor         Actor
	OrgID         uuid.UUID
	ProjectID     uuid.UUID
	EnvironmentID uuid.UUID
	Action        string
	Outcome       string
	TargetKind    string
	TargetID      uuid.UUID
	TargetUserID  uuid.UUID
	SlackTeamID   string
	SlackUserID   string
	Unresolved    bool
	SessionID     uuid.UUID
	TurnID        uuid.UUID
	ComputerID    uuid.UUID
	DeploymentID  uuid.UUID
	CauseEventID  uuid.UUID
	Causality     Causality
	Details       Details
}

// Append never commits, retries or starts a goroutine. The owner must return an
// error so business changes roll back. Policy is explicit and has no default.
func Append(ctx context.Context, tx pgx.Tx, policy auditpolicy.Policy, e Event) (resultErr error) {
	defer func() {
		if resultErr != nil && !errors.Is(resultErr, ErrCapture) {
			resultErr = errors.Join(ErrCapture, resultErr)
		}
	}()
	if tx == nil {
		return errors.New("audit transaction is required")
	}
	if err := policy.Validate(); err != nil {
		return err
	}
	d, ok := Lookup(e.Action)
	if !ok || e.TargetKind != d.TargetKind {
		return errors.New("unclassified audit action or target")
	}
	if e.Outcome == "" {
		e.Outcome = "succeeded"
	}
	if !slices.Contains(d.Outcomes, e.Outcome) {
		return errors.New("invalid audit outcome")
	}
	details, err := marshalEventDetails(e.Action, e.Outcome, e.Details)
	if err != nil {
		return err
	}
	a := e.Actor
	var identity *Identity
	switch a.Kind {
	case "human", "installation_operator":
		if a.ID == uuid.Nil() || a.Source != "" {
			return errors.New("invalid human audit actor")
		}
		identity = &Identity{Source: "account"}
		// users.primary_email is set only after verification by the identity owner.
		if err := tx.QueryRow(ctx, `SELECT display_name,COALESCE(primary_email,'') FROM users WHERE id=$1`, a.ID).Scan(&identity.DisplayName, &identity.Email); err != nil {
			return fmt.Errorf("read audit actor identity: %w", err)
		}
	case "api_key":
		if a.ID == uuid.Nil() || a.CredentialID != a.ID || a.CredentialKind != "api_key" || a.CredentialVersion <= 0 || a.Source != "" {
			return errors.New("invalid audit key actor")
		}
	case "session":
		if a.ID == uuid.Nil() || a.RuntimeGeneration <= 0 || a.Source != "" {
			return errors.New("invalid audit runtime actor")
		}
	case "service":
		if a.ID != uuid.Nil() || a.Source == "" || len(a.Source) > 128 {
			return errors.New("invalid audit service actor")
		}
	case "anonymous":
		if a.ID != uuid.Nil() || a.Source != "" {
			return errors.New("invalid anonymous audit actor")
		}
	default:
		return errors.New("invalid audit actor")
	}
	if a.CredentialKind == "" || len(a.CredentialKind) > 64 {
		return errors.New("invalid audit credential kind")
	}
	if !slices.Contains([]string{"permission", "ownership", "verified_identity", "credential", "runtime", "origin", "service", "installation_admin", "unauthenticated"}, a.Authorization.Basis) || !slices.Contains([]string{"allowed", "denied"}, a.Authorization.Decision) {
		return errors.New("audit authorization witness is required")
	}
	if a.Authorization.Basis == "permission" {
		if len(a.Authorization.Permissions) == 0 {
			return errors.New("audit permission witness is empty")
		}
		for _, decision := range a.Authorization.Permissions {
			if !auth.IsPermission(decision.Permission) || decision.AssignmentID == "" || decision.AssignmentRevision <= 0 || decision.RoleID == "" || decision.RoleRevision <= 0 {
				return errors.New("incomplete audit permission witness")
			}
		}
	}
	// Authoritative live parents are resolved while the operation owns them. No
	// retained-event FK makes deletion erase history.
	if e.EnvironmentID != uuid.Nil() {
		var org, project uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT org_id,project_id FROM environments WHERE id=$1`, e.EnvironmentID).Scan(&org, &project); err != nil {
			return fmt.Errorf("resolve audit environment: %w", err)
		}
		if e.OrgID != uuid.Nil() && e.OrgID != org || e.ProjectID != uuid.Nil() && e.ProjectID != project {
			return errors.New("audit scope mismatch")
		}
		e.OrgID, e.ProjectID = org, project
	} else if e.ProjectID != uuid.Nil() {
		var org uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT org_id FROM projects WHERE id=$1`, e.ProjectID).Scan(&org); err != nil {
			return fmt.Errorf("resolve audit project: %w", err)
		}
		if e.OrgID != uuid.Nil() && e.OrgID != org {
			return errors.New("audit scope mismatch")
		}
		e.OrgID = org
	}
	if e.OrgID == uuid.Nil() {
		a.MemberID = uuid.Nil()
	}
	identityJSON, err := boundedJSON(identity, 4096)
	if err != nil {
		return err
	}
	if identity == nil {
		identityJSON = nil
	}
	witness, err := boundedJSON(a.Authorization, 16384)
	if err != nil {
		return err
	}
	causality, err := boundedJSON(e.Causality, 4096)
	if err != nil {
		return err
	}
	if len(details) > 16384 {
		return errors.New("audit details exceed limit")
	}
	if e.ID == uuid.Nil() {
		e.ID = uuid.NewV7()
	}
	request := RequestFrom(ctx)
	if e.OperationID == uuid.Nil() {
		e.OperationID = request.OperationID
	}
	if e.OperationID == uuid.Nil() {
		e.OperationID = e.ID
	}
	channel := request.Channel
	if channel == "" {
		channel = "internal"
	}
	if len(channel) > 64 {
		return errors.New("audit channel exceeds limit")
	}
	var ip any
	if request.SourceIP.IsValid() {
		ip = request.SourceIP.String()
	}
	resolution := "verified"
	if e.Unresolved {
		resolution = "unresolved"
	}
	var occurred any
	if !e.OccurredAt.IsZero() {
		occurred = e.OccurredAt.UTC()
	}
	if len(identityJSON)+len(witness)+len(causality)+len(details)+len(a.Source)+len(a.CredentialKind)+len(channel)+1024 > 65536 {
		return errors.New("audit envelope exceeds limit")
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events (
 id,org_id,project_id,environment_id,schema_version,occurred_at,recorded_at,retention_seconds,expires_at,
 actor_kind,actor_id,actor_service,actor_member_id,actor_identity,credential_kind,credential_id,credential_version,runtime_generation,process_epoch,
 action,outcome,target_kind,target_id,target_user_id,slack_team_id,slack_user_id,target_resolution,authorization_witness,
 operation_id,session_id,turn_id,computer_id,deployment_id,cause_event_id,causality,source_ip,channel,details)
 VALUES ($1,$2,$3,$4,1,COALESCE($5::timestamptz,statement_timestamp()),statement_timestamp(),$6,
 statement_timestamp()+$6::bigint*interval '1 second',$7,$8,NULLIF($9,''),$10,$11,$12,$13,$14,$15,$16,
 $17,$18,$19,$20,$21,NULLIF($22,''),NULLIF($23,''),$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35)`,
		e.ID, optionalUUID(e.OrgID), optionalUUID(e.ProjectID), optionalUUID(e.EnvironmentID), occurred, policy.RetentionSeconds,
		a.Kind, optionalUUID(a.ID), a.Source, optionalUUID(a.MemberID), identityJSON, a.CredentialKind, optionalUUID(a.CredentialID), positive(a.CredentialVersion), positive(a.RuntimeGeneration), positive(a.ProcessEpoch),
		e.Action, e.Outcome, e.TargetKind, optionalUUID(e.TargetID), optionalUUID(e.TargetUserID), e.SlackTeamID, e.SlackUserID, resolution, witness, e.OperationID,
		optionalUUID(e.SessionID), optionalUUID(e.TurnID), optionalUUID(e.ComputerID), optionalUUID(e.DeploymentID), optionalUUID(e.CauseEventID), causality, ip, channel, details)
	if err != nil {
		return fmt.Errorf("append audit event: %w", err)
	}
	return nil
}

func boundedJSON(value any, limit int) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errors.New("audit field exceeds limit")
	}
	return data, nil
}
func optionalUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: id != uuid.Nil()} }
func positive(n int64) any {
	if n > 0 {
		return n
	}
	return nil
}
