package computer

import (
	"crypto/sha256"
	"strconv"
	"uuid"
)

// publicationKey is the retention identity of the object pins one
// publication holds on an Instance: registration pins candidates under it and
// publication requires them under the same key. A caller-selected operation
// UUID is not unique across publication kinds or Instances, so the key binds
// the kind, its owner and the operation.
type publicationKey []byte

func derivePublicationKey(kind string, owner, operation uuid.UUID) publicationKey {
	h := sha256.New()
	h.Write([]byte("helmr.computer.publication.v1\x00"))
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write(owner[:])
	h.Write(operation[:])
	return h.Sum(nil)
}

// initialPublicationKey is the key of an Instance's initial version.
func initialPublicationKey(instanceID uuid.UUID) publicationKey {
	return derivePublicationKey("initial", instanceID, instanceID)
}

// checkpointPublicationKey is the key of a checkpoint's disk version.
func checkpointPublicationKey(checkpointID uuid.UUID) publicationKey {
	return derivePublicationKey("checkpoint", checkpointID, checkpointID)
}

// savePublicationKey is the key of one save of an Instance.
func savePublicationKey(instanceID uuid.UUID, sequence int64, saveID uuid.UUID) publicationKey {
	return derivePublicationKey("save/"+strconv.FormatInt(sequence, 10), instanceID, saveID)
}
