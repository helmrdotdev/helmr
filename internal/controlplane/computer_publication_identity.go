package controlplane

import (
	"crypto/sha256"

	"github.com/jackc/pgx/v5/pgtype"
)

// A caller-selected operation UUID is not globally unique across publication
// kinds or Run leases. This key binds retention to the authoritative owner and
// operation without changing transport IDs or introducing another receipt table.
func computerPublicationKey(kind string, owner, operation pgtype.UUID) []byte {
	h := sha256.New()
	h.Write([]byte("helmr.computer.publication.v1\x00"))
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write(owner.Bytes[:])
	h.Write(operation.Bytes[:])
	return h.Sum(nil)
}
