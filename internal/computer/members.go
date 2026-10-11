package computer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
)

const (
	// DefaultListLimit is the page size of Computer and member lists that do
	// not choose one.
	DefaultListLimit = int32(50)
	// MaxListLimit is the largest page size of Computer and member lists.
	MaxListLimit = int32(100)
)

// MembersQuery selects a page of a Computer's members. A zero Limit selects
// DefaultListLimit; Cursor continues a previous page of the same Computer.
type MembersQuery struct {
	Cursor string
	Limit  int32
}

// Member is one live or physically unreconciled Session or Command.
type Member struct {
	Kind      string
	ID        string
	State     string
	CreatedAt time.Time
}

// MembersPage is one page of members; NextCursor is empty on the last page.
type MembersPage struct {
	Members    []Member
	NextCursor string
}

type membersCursor struct {
	EnvironmentID string    `json:"environment_id"`
	ComputerID    string    `json:"computer_id"`
	Kind          string    `json:"kind"`
	ID            string    `json:"id"`
	CreatedAt     time.Time `json:"created_at"`
}

// ValidateMembersQuery checks a members query for a Computer without reading
// it, so callers can reject a malformed query before addressing the
// Computer.
func ValidateMembersQuery(environmentID, computerID uuid.UUID, query MembersQuery) error {
	_, _, err := parseMembersQuery(environmentID, computerID, query)
	return err
}
func parseMembersQuery(env, computer uuid.UUID, query MembersQuery) (int32, membersCursor, error) {
	limit := query.Limit
	if limit == 0 {
		limit = DefaultListLimit
	}
	if limit < 1 || limit > MaxListLimit {
		return 0, membersCursor{}, invalidInput("limit must be an integer in [1,100]")
	}
	var cursor membersCursor
	if query.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(query.Cursor)
		if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.CreatedAt.IsZero() || ids.Validate(cursor.ID) != nil || (cursor.Kind != "session" && cursor.Kind != "command") || cursor.EnvironmentID != env.String() || cursor.ComputerID != computer.String() {
			return 0, cursor, invalidInput("computer member cursor is invalid for this Computer")
		}
	}
	return limit, cursor, nil
}
func ListMembers(ctx context.Context, database db.DBTX, scope Scope, computer uuid.UUID, query MembersQuery) (MembersPage, error) {
	limit, cursor, err := parseMembersQuery(scope.EnvironmentID, computer, query)
	if err != nil {
		return MembersPage{}, err
	}
	if _, err = scanComputer(database.QueryRow(ctx, computerProjection+` AND c.id=$4`, scope.OrgID, scope.ProjectID, scope.EnvironmentID, computer)); err != nil {
		return MembersPage{}, err
	}
	after := uuid.Nil()
	if query.Cursor != "" {
		after = uuid.MustParse(cursor.ID)
	}
	rows, err := database.Query(ctx, `WITH members AS (
 SELECT 'session'::text kind,s.id,
 CASE WHEN s.status IN ('closed','cancelled') THEN 'unreconciled' WHEN s.status='closing' THEN 'draining'
 WHEN EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=s.environment_id AND t.session_id=s.id AND t.status IN ('running','finalizing')) THEN 'running'
 WHEN EXISTS(SELECT 1 FROM computer_checkpoints cp WHERE cp.environment_id=s.environment_id AND cp.computer_id=s.computer_id AND cp.status IN ('ready','restoring')) THEN 'parked' ELSE 'admitted' END state,s.created_at
 FROM sessions s WHERE s.environment_id=$1 AND s.computer_id=$2 AND (s.status IN ('open','closing') OR EXISTS(SELECT 1 FROM session_processes p WHERE p.environment_id=s.environment_id AND p.session_id=s.id AND p.fenced_at IS NULL))
 UNION ALL SELECT 'command',c.id,CASE WHEN c.terminal_at IS NOT NULL THEN 'unreconciled' WHEN c.status='stopping' THEN 'draining' WHEN c.status='running' THEN 'running' ELSE 'admitted' END,c.created_at
 FROM computer_commands c WHERE c.environment_id=$1 AND c.computer_id=$2 AND (c.terminal_at IS NULL OR (c.computer_lease_epoch IS NOT NULL AND c.process_reconciled_at IS NULL))
 ) SELECT kind,id::text,state,created_at FROM members WHERE NOT $3 OR (created_at,id,kind)<($4,$5,$6) ORDER BY created_at DESC,id DESC,kind DESC LIMIT $7`, scope.EnvironmentID, computer, query.Cursor != "", cursor.CreatedAt, after, cursor.Kind, limit+1)
	if err != nil {
		return MembersPage{}, err
	}
	members, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Member, error) {
		var m Member
		err := row.Scan(&m.Kind, &m.ID, &m.State, &m.CreatedAt)
		m.CreatedAt = m.CreatedAt.UTC()
		return m, err
	})
	if err != nil {
		return MembersPage{}, err
	}
	if members == nil {
		members = []Member{}
	}
	page := MembersPage{Members: members}
	if len(members) > int(limit) {
		page.Members = members[:limit]
		last := page.Members[len(page.Members)-1]
		raw, err := json.Marshal(membersCursor{EnvironmentID: scope.EnvironmentID.String(), ComputerID: computer.String(), Kind: last.Kind, ID: last.ID, CreatedAt: last.CreatedAt})
		if err != nil {
			return MembersPage{}, err
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}
