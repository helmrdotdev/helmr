package computer

import (
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

// WriteCapability derives the write capability of the Instance's current
// writer generation and checks it against the writer token hash the
// Instance recorded when that generation was granted.
func WriteCapability(key disk.FencingKey, instance db.ComputerInstance) (disk.FencingCapability, error) {
	instanceID, err := pgvalue.UUIDValue(instance.ID)
	if err != nil {
		return disk.FencingCapability{}, errors.New("computer instance ID is invalid")
	}
	computerID, err := pgvalue.UUIDValue(instance.ComputerID)
	if err != nil {
		return disk.FencingCapability{}, errors.New("computer ID is invalid")
	}
	capability, err := key.Derive(disk.FenceInput{
		InstanceID:       instanceID,
		ComputerID:       computerID,
		WriterGeneration: instance.WriterGeneration,
	})
	if err != nil {
		return disk.FencingCapability{}, err
	}
	hash, err := hex.DecodeString(strings.TrimPrefix(capability.Hash, "sha256:"))
	if err != nil {
		return disk.FencingCapability{}, err
	}
	if subtle.ConstantTimeCompare(hash, instance.WriterTokenHash) != 1 {
		return disk.FencingCapability{}, errors.New("computer write capability does not match its Instance")
	}
	return capability, nil
}
