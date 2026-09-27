package controlplane

import (
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/pgvalue"

	"github.com/jackc/pgx/v5/pgtype"
)

// A caller-selected operation UUID is not globally unique across publication
// kinds or Run leases. This key binds retention to the authoritative owner and
// operation without changing transport IDs or introducing another receipt table.
func computerPublicationKey(kind string, owner, operation pgtype.UUID) []byte {
	return computer.PublicationKey(kind, pgvalue.MustUUIDValue(owner), pgvalue.MustUUIDValue(operation))
}
