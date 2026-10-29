package auditquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"slices"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrSearchDenied = errors.New("audit reading permission is required")
var ErrSearchInput = errors.New("invalid audit search")

// Before is navigation within a repeated time range, never authority or an
// export checkpoint. Every page reauthorizes against current grants.

type SearchFilter struct {
	From          time.Time
	To            time.Time
	Before        *Before
	Limit         int
	OrgID         uuid.UUID
	EnvironmentID uuid.UUID
	ActorKind     string
	ActorID       uuid.UUID
	// Actions matches any of the listed actions; empty matches all.
	Actions      []string
	TargetKind   string
	TargetID     uuid.UUID
	TargetUserID uuid.UUID
	SlackTeamID  string
	SlackUserID  string
	Outcome      string
	// OperationID finds the events one operation recorded; EventID finds one
	// event. Both stay within the time range like every other filter.
	OperationID uuid.UUID
	EventID     uuid.UUID
}

// maxSearchActions bounds the actions one search may list.
const maxSearchActions = 100

func (f SearchFilter) validate(adminView bool) error {
	if f.From.IsZero() || f.To.IsZero() || !f.To.After(f.From) || f.To.Sub(f.From) > 31*24*time.Hour || f.Limit < 1 || f.Limit > 500 {
		return ErrSearchInput
	}
	if f.Before != nil && (f.Before.ID == uuid.Nil() || f.Before.RecordedAt.Before(f.From) || !f.Before.RecordedAt.Before(f.To)) {
		return ErrSearchInput
	}
	if len(f.Actions) > maxSearchActions {
		return ErrSearchInput
	}
	for _, action := range f.Actions {
		if _, ok := audit.Lookup(action); !ok {
			return ErrSearchInput
		}
	}
	kinds := []string{"operator", "user", "api_key", "session", "platform_service", "unidentified"}
	if adminView {
		kinds = []string{"operator", "user", "api_key", "session", "worker_host", "component", "unidentified"}
	}
	if !adminView && f.ActorID != uuid.Nil() && (f.ActorKind == "platform_service" || f.ActorKind == "unidentified") {
		return ErrSearchInput
	}
	if !adminView && f.TargetKind != "" && !publicTargetKind(f.TargetKind) {
		return ErrSearchInput
	}
	if !adminView {
		for _, action := range f.Actions {
			d, _ := audit.Lookup(action)
			if d.Audience != audit.OrganizationAudience {
				return ErrSearchInput
			}
		}
	}
	if f.ActorKind != "" && !slices.Contains(kinds, f.ActorKind) {
		return ErrSearchInput
	}
	if f.Outcome != "" && !slices.Contains([]string{"accepted", "succeeded", "denied", "failed", "cancelled", "unknown"}, f.Outcome) {
		return ErrSearchInput
	}
	if len(f.TargetKind) > 64 || len(f.SlackTeamID) > 100 || len(f.SlackUserID) > 100 {
		return ErrSearchInput
	}
	return nil
}

// Search authorizes, buffers a bounded selection and commits its access fact
// before returning any rows. Admin reads use a separate entry point.
func Search(ctx context.Context, database db.TxBeginner, policy auditpolicy.Policy, prior auth.Principal, f SearchFilter) (SearchResult, error) {
	result, err := search(ctx, database, policy, prior, f, false)
	return SearchResult{Events: result.PublicEvents, NextBefore: result.NextBefore}, err
}
func SearchAdmin(ctx context.Context, database db.TxBeginner, policy auditpolicy.Policy, prior auth.Principal, f SearchFilter) (AdminSearchResult, error) {
	result, err := search(ctx, database, policy, prior, f, true)
	return AdminSearchResult{Events: result.Events, NextBefore: result.NextBefore}, err
}
func search(ctx context.Context, database db.TxBeginner, policy auditpolicy.Policy, prior auth.Principal, f SearchFilter, adminView bool) (searchResult, error) {
	result := searchResult{Events: []Record{}, PublicEvents: []api.AuditEvent{}}
	if f.Limit == 0 {
		f.Limit = 100
	}
	if err := f.validate(adminView); err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout='4s'`); err != nil {
			return err
		}
		var actor audit.Evidence
		org := f.OrgID
		orgWide := false
		environments := []uuid.UUID{}
		accessOrg, accessEnv := uuid.Nil(), uuid.Nil()
		unresolved := false
		if adminView {
			if prior.Kind != auth.PrincipalKindOperator {
				return ErrSearchDenied
			}
			var err error
			actor, err = audit.PlatformOperator(prior)
			if err != nil {
				return &audit.Refusal{Authorization: actor.Authorization, Cause: ErrSearchDenied}
			}
			var active bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM login_sessions s JOIN platform_operators o ON o.id=s.operator_id JOIN passkey_credentials k ON k.id=s.privileged_key_id AND k.operator_id=o.id WHERE s.id=$1 AND s.operator_id=$2 AND s.channel='operator' AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp() AND o.disabled_at IS NULL AND o.access_epoch=s.privileged_epoch AND o.access_epoch=$3 AND k.revoked_at IS NULL)`, prior.SessionID, prior.OperatorID, prior.AccessEpoch).Scan(&active); err != nil {
				return err
			}
			check := audit.OperatorCheck{Type: "platform_operator", OperatorID: prior.OperatorID, SessionID: prior.SessionID, AccessEpoch: prior.AccessEpoch, ObservationSource: "transaction", Result: "passed"}
			if !active {
				check.Result = "failed"
				actor.Authorization.Decision = "denied"
			}
			actor.Authorization.Checks = append(actor.Authorization.Checks, check)
			if !active {
				return &audit.Refusal{Authorization: actor.Authorization, Cause: ErrSearchDenied}
			}

			orgWide = true
			if org != uuid.Nil() {
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organizations WHERE id=$1)`, org).Scan(&active); err != nil {
					return err
				}
				if active {
					accessOrg = org
				} else {
					unresolved = true
				}
			}
		} else {
			if org == uuid.Nil() {
				org = prior.OrgID
			}
			if org == uuid.Nil() || org != prior.OrgID {
				return ErrSearchDenied
			}
			scope := auth.Scope{OrgID: org}
			if prior.Kind == auth.PrincipalKindAPIKey {
				scope.ProjectID = prior.ProjectID
				scope.EnvironmentID = prior.EnvironmentID
			}
			current, err := access.Current(ctx, tx, prior, scope)
			if errors.Is(err, access.ErrDenied) {
				return errors.Join(ErrSearchDenied, err)
			}
			if err != nil {
				return err
			}
			orgWide, environments = current.AuditScope()
			if !orgWide && len(environments) == 0 {
				return auth.Require(current, auth.PermissionAuditEventsRead, scope, ErrSearchDenied)
			}
			if !orgWide && f.EnvironmentID != uuid.Nil() {
				if !slices.Contains(environments, f.EnvironmentID) {
					var project uuid.UUID
					if err := tx.QueryRow(ctx, `SELECT project_id FROM environments WHERE org_id=$1 AND id=$2`, org, f.EnvironmentID).Scan(&project); err != nil {
						if errors.Is(err, pgx.ErrNoRows) {
							return ErrSearchDenied
						}
						return err
					}
					return auth.Require(current, auth.PermissionAuditEventsRead, auth.Scope{OrgID: org, ProjectID: project.String(), EnvironmentID: f.EnvironmentID.String()}, ErrSearchDenied)
				}
				environments = []uuid.UUID{f.EnvironmentID}
			}
			actor = audit.FromPrincipal(current)
			actor.Authorization = audit.Authorization{Decision: "allowed"}
			if orgWide {
				decisions, _ := current.Decide(auth.PermissionAuditEventsRead, auth.Scope{OrgID: org})
				actor.Authorization.Checks = audit.PermissionEvidence(current, decisions)
			} else {
				for _, env := range environments {
					for _, grant := range current.Grants {
						if grant.EnvironmentID != env.String() {
							continue
						}
						decisions, ok := current.Decide(auth.PermissionAuditEventsRead, auth.Scope{OrgID: org, ProjectID: grant.ProjectID, EnvironmentID: env.String()})
						if ok {
							actor.Authorization.Checks = append(actor.Authorization.Checks, audit.PermissionEvidence(current, decisions)...)
							break
						}
					}
				}
			}
			accessOrg = org
			if f.EnvironmentID != uuid.Nil() {
				if !orgWide && !slices.Contains(environments, f.EnvironmentID) {
					return ErrSearchDenied
				}
				var belongs bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM environments WHERE org_id=$1 AND id=$2)`, org, f.EnvironmentID).Scan(&belongs); err != nil {
					return err
				}
				if !belongs {
					return ErrSearchDenied
				}
				accessEnv = f.EnvironmentID
			} else if len(environments) == 1 {
				accessEnv = environments[0]
			}
		}
		// Oversized scope is invalid admission, not a capture outage. The
		// caller can narrow to one Environment without changing its Grants.
		witness, err := json.Marshal(actor.Authorization)
		if err != nil {
			return err
		}
		if len(witness) > 16384 {
			return fmt.Errorf("%w: specify environment_id to bound the authorization evidence", ErrSearchInput)
		}
		var beforeTime any
		var beforeID any
		if f.Before != nil {
			beforeTime = f.Before.RecordedAt
			beforeID = f.Before.ID
		}
		rows, err := tx.Query(ctx, searchSQL, optionalUUID(org), orgWide, environments, optionalUUID(f.EnvironmentID), f.From, f.To, beforeTime, beforeID, f.ActorKind, optionalUUID(f.ActorID), f.Actions, f.TargetKind, optionalUUID(f.TargetID), f.Outcome, optionalUUID(f.TargetUserID), f.SlackTeamID, f.SlackUserID, f.Limit+1, optionalUUID(f.OperationID), optionalUUID(f.EventID), adminView, audit.PlatformActions())
		if err != nil {
			return err
		}
		var last Before
		count := 0
		scope := readScope{org: org, organizationWide: orgWide, environments: environments}
		for rows.Next() {
			var raw json.RawMessage
			var key Before
			if err = rows.Scan(&raw, &key.RecordedAt, &key.ID); err != nil {
				rows.Close()
				return err
			}
			if count == f.Limit {
				result.NextBefore = &last
				break
			}
			var event Record
			if err = json.Unmarshal(raw, &event); err != nil {
				rows.Close()
				return err
			}
			if adminView {
				result.Events = append(result.Events, event)
			} else {
				public, err := projectRecord(event, scope)
				if err != nil {
					rows.Close()
					return err
				}
				result.PublicEvents = append(result.PublicEvents, public)
			}
			count++
			last = key
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		readModel := "organization"
		if adminView {
			readModel = "platform"
		}
		detail := audit.SearchDetails{ReadModel: readModel, From: f.From, To: f.To, ResultCount: count, EnvironmentIDs: environments, Actions: f.Actions, OperationID: f.OperationID, EventID: f.EventID, ActorID: f.ActorID, TargetID: f.TargetID, Outcome: f.Outcome, ActorKind: f.ActorKind, TargetKind: f.TargetKind, TargetUserID: f.TargetUserID, SlackTeamID: f.SlackTeamID, SlackUserID: f.SlackUserID, RequestedEnvironmentID: f.EnvironmentID}
		event := audit.Event{Evidence: actor, OrgID: accessOrg, EnvironmentID: accessEnv, Action: "audit.searched", TargetKind: "audit", Details: detail, Unresolved: unresolved}
		if unresolved {
			event.TargetID = org
		}
		return audit.Append(ctx, tx, policy, event)
	})
	if err != nil {
		return searchResult{}, err
	}
	return result, nil
}

func optionalUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: id != uuid.Nil()} }

const searchSQL = `SELECT to_jsonb(e), e.recorded_at,e.id FROM audit_events e
   WHERE (e.org_id=$1::uuid OR ($1::uuid IS NULL AND e.org_id IS NULL)) AND ($2::boolean OR e.environment_id=ANY($3::uuid[]))
   AND ($4::uuid IS NULL OR e.environment_id=$4) AND e.recorded_at >= $5 AND e.recorded_at < $6
   AND e.expires_at>statement_timestamp() AND ($7::timestamptz IS NULL OR (e.recorded_at,e.id)<($7,$8::uuid))
   AND ($9::text='' OR CASE WHEN e.actor_operator_id IS NOT NULL THEN 'operator' WHEN e.actor_user_id IS NOT NULL THEN 'user' WHEN e.actor_api_key_id IS NOT NULL THEN 'api_key' WHEN e.actor_session_id IS NOT NULL THEN 'session' WHEN e.actor_worker_host_id IS NOT NULL THEN CASE WHEN $21::boolean THEN 'worker_host' ELSE 'platform_service' END WHEN e.actor_component IS NOT NULL THEN CASE WHEN $21::boolean THEN 'component' ELSE 'platform_service' END ELSE 'unidentified' END=$9) AND ($10::uuid IS NULL OR COALESCE(e.actor_operator_id,e.actor_user_id,e.actor_api_key_id,e.actor_session_id,CASE WHEN $21::boolean THEN e.actor_worker_host_id END)=$10)
   AND (COALESCE(cardinality($11::text[]),0)=0 OR e.action=ANY($11::text[])) AND ($12::text='' OR e.target_kind=$12) AND ($13::uuid IS NULL OR e.target_id=$13)
   AND ($14::text='' OR e.outcome=$14) AND ($15::uuid IS NULL OR e.target_user_id=$15)
   AND ($16::text='' OR e.slack_team_id=$16) AND ($17::text='' OR e.slack_user_id=$17)
   AND ($21::boolean OR NOT(e.action=ANY($22::text[])))
   AND ($19::uuid IS NULL OR e.operation_id=$19) AND ($20::uuid IS NULL OR e.id=$20)
   ORDER BY e.recorded_at DESC,e.id DESC LIMIT $18`
