package auditquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	Action        string
	TargetKind    string
	TargetID      uuid.UUID
	TargetUserID  uuid.UUID
	SlackTeamID   string
	SlackUserID   string
	Outcome       string
}

func (f SearchFilter) validate() error {
	if f.From.IsZero() || f.To.IsZero() || !f.To.After(f.From) || f.To.Sub(f.From) > 31*24*time.Hour || f.Limit < 1 || f.Limit > 500 {
		return ErrSearchInput
	}
	if f.Before != nil && (f.Before.ID == uuid.Nil() || f.Before.RecordedAt.Before(f.From) || !f.Before.RecordedAt.Before(f.To)) {
		return ErrSearchInput
	}
	if f.Action != "" {
		if _, ok := audit.Lookup(f.Action); !ok {
			return ErrSearchInput
		}
	}
	if f.ActorKind != "" && !slices.Contains([]string{"human", "installation_operator", "api_key", "service", "session", "anonymous"}, f.ActorKind) {
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
// before returning any rows. Installation custody is a separate entry point.
func Search(ctx context.Context, database db.TxBeginner, policy auditpolicy.Policy, prior auth.Principal, f SearchFilter) (SearchResult, error) {
	return search(ctx, database, policy, prior, f, false)
}
func SearchInstallation(ctx context.Context, database db.TxBeginner, policy auditpolicy.Policy, prior auth.Principal, f SearchFilter) (SearchResult, error) {
	return search(ctx, database, policy, prior, f, true)
}
func search(ctx context.Context, database db.TxBeginner, policy auditpolicy.Policy, prior auth.Principal, f SearchFilter, installation bool) (SearchResult, error) {
	result := SearchResult{Events: []Record{}}
	if f.Limit == 0 {
		f.Limit = 100
	}
	if err := f.validate(); err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout='4s'`); err != nil {
			return err
		}
		var actor audit.Actor
		org := f.OrgID
		orgWide := false
		environments := []uuid.UUID{}
		accessOrg, accessEnv := uuid.Nil(), uuid.Nil()
		unresolved := false
		if installation {
			if prior.Kind != auth.PrincipalKindSession || !prior.Admin {
				return ErrSearchDenied
			}
			var active bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND admin)`, prior.UserID).Scan(&active); err != nil {
				return err
			}
			if !active {
				return ErrSearchDenied
			}
			var err error
			actor, err = audit.InstallationOperator(prior)
			if err != nil {
				return ErrSearchDenied
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
				return ErrSearchDenied
			}
			if err != nil {
				return err
			}
			orgWide, environments = current.AuditScope()
			if !orgWide && len(environments) == 0 {
				return ErrSearchDenied
			}
			if !orgWide && f.EnvironmentID != uuid.Nil() {
				if !slices.Contains(environments, f.EnvironmentID) {
					return ErrSearchDenied
				}
				environments = []uuid.UUID{f.EnvironmentID}
			}
			actor = audit.FromPrincipal(current)
			actor.Authorization = audit.Authorization{Basis: "permission", Decision: "allowed"}
			if orgWide {
				actor.Authorization.Permissions, _ = current.Decide(auth.PermissionAuditEventsRead, auth.Scope{OrgID: org})
			} else {
				for _, env := range environments {
					for _, grant := range current.Grants {
						if grant.EnvironmentID != env.String() {
							continue
						}
						decisions, ok := current.Decide(auth.PermissionAuditEventsRead, auth.Scope{OrgID: org, ProjectID: grant.ProjectID, EnvironmentID: env.String()})
						if ok {
							actor.Authorization.Permissions = append(actor.Authorization.Permissions, decisions...)
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
		rows, err := tx.Query(ctx, searchSQL, optionalUUID(org), orgWide, environments, optionalUUID(f.EnvironmentID), f.From, f.To, beforeTime, beforeID, f.ActorKind, optionalUUID(f.ActorID), f.Action, f.TargetKind, optionalUUID(f.TargetID), f.Outcome, optionalUUID(f.TargetUserID), f.SlackTeamID, f.SlackUserID, f.Limit+1)
		if err != nil {
			return err
		}
		var last Before
		for rows.Next() {
			var raw json.RawMessage
			var key Before
			if err = rows.Scan(&raw, &key.RecordedAt, &key.ID); err != nil {
				rows.Close()
				return err
			}
			if len(result.Events) == f.Limit {
				result.NextBefore = &last
				break
			}
			var event Record
			if err = json.Unmarshal(raw, &event); err != nil {
				rows.Close()
				return err
			}
			result.Events = append(result.Events, event)
			last = key
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		detail := audit.SearchDetails{From: f.From, To: f.To, ResultCount: len(result.Events), EnvironmentIDs: environments, Action: f.Action, ActorID: f.ActorID, TargetID: f.TargetID, Outcome: f.Outcome, ActorKind: f.ActorKind, TargetKind: f.TargetKind, TargetUserID: f.TargetUserID, SlackTeamID: f.SlackTeamID, SlackUserID: f.SlackUserID, RequestedEnvironmentID: f.EnvironmentID}
		event := audit.Event{Actor: actor, OrgID: accessOrg, EnvironmentID: accessEnv, Action: "audit.searched", TargetKind: "audit", Details: detail, Unresolved: unresolved}
		if unresolved {
			event.TargetID = org
		}
		return audit.Append(ctx, tx, policy, event)
	})
	if err != nil {
		return SearchResult{}, err
	}
	return result, nil
}

func optionalUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: id != uuid.Nil()} }

const searchSQL = `SELECT to_jsonb(e), e.recorded_at,e.id FROM audit_events e
   WHERE (e.org_id=$1::uuid OR ($1::uuid IS NULL AND e.org_id IS NULL)) AND ($2::boolean OR e.environment_id=ANY($3::uuid[]))
   AND ($4::uuid IS NULL OR e.environment_id=$4) AND e.recorded_at >= $5 AND e.recorded_at < $6
   AND e.expires_at>statement_timestamp() AND ($7::timestamptz IS NULL OR (e.recorded_at,e.id)<($7,$8::uuid))
   AND ($9::text='' OR e.actor_kind=$9) AND ($10::uuid IS NULL OR e.actor_id=$10)
   AND ($11::text='' OR e.action=$11) AND ($12::text='' OR e.target_kind=$12) AND ($13::uuid IS NULL OR e.target_id=$13)
   AND ($14::text='' OR e.outcome=$14) AND ($15::uuid IS NULL OR e.target_user_id=$15)
   AND ($16::text='' OR e.slack_team_id=$16) AND ($17::text='' OR e.slack_user_id=$17)
   ORDER BY e.recorded_at DESC,e.id DESC LIMIT $18`
