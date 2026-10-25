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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

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
	ID          uuid.UUID
	OperationID uuid.UUID
	OccurredAt  time.Time
	Evidence
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
	if err := e.Evidence.validate(); err != nil {
		return err
	}
	if (e.Authentication.Result == "absent" || e.Authentication.Result == "failed") && e.Outcome != "denied" && e.Outcome != "failed" {
		if e.Authentication.Result != "absent" || !slices.Contains([]string{"identity.device_started", "identity.magic_link_requested"}, e.Action) {
			return errors.New("established actor required for operation")
		}
	}
	a := e.Actor
	var identity *Identity
	if a.UserID != uuid.Nil() {
		identity = &Identity{Source: "account"}
		if err := tx.QueryRow(ctx, `SELECT display_name,COALESCE(primary_email,'') FROM users WHERE id=$1`, a.UserID).Scan(&identity.DisplayName, &identity.Email); err != nil {
			return fmt.Errorf("read audit actor identity: %w", err)
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
	identityJSON, err := boundedJSON(identity, 4096)
	if err != nil {
		return err
	}
	if identity == nil {
		identityJSON = nil
	}
	witness, err := boundedJSON(e.Authorization, 16384)
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
	if len(identityJSON)+len(witness)+len(causality)+len(details)+len(a.Component)+len(e.Authentication.CredentialKind)+len(channel)+1024 > 65536 {
		return errors.New("audit envelope exceeds limit")
	}
	c := e.Authentication
	_, err = tx.Exec(ctx, `INSERT INTO audit_events (
 id,org_id,project_id,environment_id,schema_version,occurred_at,recorded_at,retention_seconds,expires_at,
 actor_user_id,actor_api_key_id,actor_session_id,actor_worker_host_id,actor_component,actor_identity,
 authentication_result,authentication_origin,identity_provider,credential_kind,credential_id,credential_version,runtime_generation,process_epoch,
 action,outcome,target_kind,target_id,target_user_id,slack_team_id,slack_user_id,target_resolution,authorization_evidence,
 operation_id,session_id,turn_id,computer_id,deployment_id,cause_event_id,causality,source_ip,channel,details)
 VALUES ($1,$2,$3,$4,1,COALESCE($5::timestamptz,statement_timestamp()),statement_timestamp(),$6,
 statement_timestamp()+$6::bigint*interval '1 second',$7,$8,$9,$10,NULLIF($11,''),$12,
 $13,NULLIF($14,''),NULLIF($15,''),NULLIF($16,''),$17,$18,$19,$20,
 $21,$22,$23,$24,$25,NULLIF($26,''),NULLIF($27,''),$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39)`,
		e.ID, optionalUUID(e.OrgID), optionalUUID(e.ProjectID), optionalUUID(e.EnvironmentID), occurred, policy.RetentionSeconds,
		optionalUUID(a.UserID), optionalUUID(a.APIKeyID), optionalUUID(a.SessionID), optionalUUID(a.WorkerHostID), a.Component, identityJSON,
		c.Result, c.Origin, c.Provider, c.CredentialKind, optionalUUID(c.CredentialID), positive(c.CredentialVersion), positive(e.RuntimeGeneration), positive(e.ProcessEpoch),
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
