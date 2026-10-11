package secret

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"uuid"
)

// RuntimeSelector identifies an immutable binding and logical delivery owner.
// It grants no authority: every resolution checks the owner's current physical
// attachment, eligibility, allowed origin and Secret revocation.
func RuntimeSelector(env uuid.UUID, kind string, owner uuid.UUID, epoch int64, binding string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("helmr.runtime-secret-selector.v1|%s|%s|%s|%d|%s", env, kind, owner, epoch, binding)))
	return "hlmr_protected_" + hex.EncodeToString(digest[:])
}
