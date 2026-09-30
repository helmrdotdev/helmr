package computer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
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

// Member is one Session, Task Run or Command on a Computer.
type Member struct {
	Kind      string
	ID        string
	RunID     string
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
	_, err := membersParams(pgvalue.UUID(environmentID), pgvalue.UUID(computerID), query)
	return err
}

// ListMembers reads the Computer, then validates the query against it and
// lists one page of its members. q may be a pool or the caller's
// transaction; ListMembers takes no locks.
func ListMembers(ctx context.Context, q db.Querier, scope Scope, computerID uuid.UUID, query MembersQuery) (MembersPage, error) {
	record, err := getComputer(ctx, q, scope, computerID)
	if err != nil {
		return MembersPage{}, err
	}
	params, err := membersParams(record.EnvironmentID, record.ID, query)
	if err != nil {
		return MembersPage{}, err
	}
	rows, err := q.ListComputerMembers(ctx, params)
	if err != nil {
		return MembersPage{}, fmt.Errorf("list computer members: %w", err)
	}
	page := MembersPage{Members: make([]Member, 0, len(rows))}
	limit := int(params.RowLimit - 1)
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		cursor, err := json.Marshal(membersCursor{
			EnvironmentID: pgvalue.UUIDString(params.EnvironmentID), ComputerID: pgvalue.UUIDString(params.ComputerID),
			Kind: last.Kind, ID: pgvalue.UUIDString(last.ID), CreatedAt: pgvalue.Time(last.CreatedAt),
		})
		if err != nil {
			return MembersPage{}, err
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(cursor)
	}
	for _, row := range rows {
		page.Members = append(page.Members, Member{
			Kind: row.Kind, ID: pgvalue.UUIDString(row.ID), RunID: pgvalue.UUIDString(row.RunID), State: row.State, CreatedAt: pgvalue.Time(row.CreatedAt),
		})
	}
	return page, nil
}

func membersParams(environmentID, computerID pgtype.UUID, query MembersQuery) (db.ListComputerMembersParams, error) {
	limit := query.Limit
	if limit == 0 {
		limit = DefaultListLimit
	}
	if limit < 1 || limit > MaxListLimit {
		return db.ListComputerMembersParams{}, invalidInput("limit must be an integer in [1,100]")
	}
	params := db.ListComputerMembersParams{EnvironmentID: environmentID, ComputerID: computerID, RowLimit: limit + 1}
	if query.Cursor == "" {
		return params, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(query.Cursor)
	var cursor membersCursor
	if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.CreatedAt.IsZero() || ids.Validate(cursor.ID) != nil ||
		(cursor.Kind != "session" && cursor.Kind != "task" && cursor.Kind != "command") ||
		cursor.EnvironmentID != pgvalue.UUIDString(environmentID) || cursor.ComputerID != pgvalue.UUIDString(computerID) {
		return db.ListComputerMembersParams{}, invalidInput("computer member cursor is invalid for this Computer")
	}
	params.HasAfter = true
	params.AfterCreatedAt = pgtype.Timestamptz{Time: cursor.CreatedAt, Valid: true}
	params.AfterID = pgvalue.UUID(uuid.MustParse(cursor.ID))
	params.AfterKind = cursor.Kind
	return params, nil
}
