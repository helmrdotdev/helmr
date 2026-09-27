package computer

import (
	"crypto/sha256"
	"uuid"
)

// PublicationKey binds object retention to its publication kind, owner and
// operation. Registration and publication must derive the same identity.
func PublicationKey(kind string, owner, operation uuid.UUID) []byte {
	h := sha256.New()
	h.Write([]byte("helmr.computer.publication.v1\x00"))
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write(owner[:])
	h.Write(operation[:])
	return h.Sum(nil)
}
