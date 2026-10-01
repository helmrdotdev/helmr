package computer

import (
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/jackc/pgx/v5"
)

const (
	// PreparationTTL bounds how long an allocated Instance may take to become
	// ready, including restoring a checkpoint before activation opens it.
	PreparationTTL = 5 * time.Minute
	// WriterTTL is the lifetime of an Instance writer grant; the worker host
	// renews it while it keeps the Instance.
	WriterTTL = 5 * time.Minute
	// RestoreActivationTopic is the control outbox topic of a committed
	// checkpoint restore: one intent per destination Instance incarnation.
	RestoreActivationTopic = "computer.restore.activate"
)

// ErrAuthorityChanged reports that the Instance, its Computer or the worker
// worker epoch no longer holds the authority the operation requires: the
// Instance incarnation, writer generation, desired version or admission
// changed, or a deadline passed.
var ErrAuthorityChanged = errors.New("computer authority changed")

// Host is a worker host at one worker epoch. Operations that take a Host
// fence the locked host row's epoch and status and never compare credential
// claim versions; operations that compare claims take a
// workergroup.HostPrincipal.
type Host struct {
	GroupID uuid.UUID
	HostID  uuid.UUID
	Epoch   int64
}

// InstanceRef addresses one Instance incarnation on a worker epoch at the
// desired version its observer acted on.
type InstanceRef struct {
	Host           Host
	ID             uuid.UUID
	DesiredVersion int64
}

// authorityChanged reports a fence that no longer matches, which the internal
// transaction signals with pgx.ErrNoRows, as ErrAuthorityChanged.
func authorityChanged(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAuthorityChanged
	}
	return err
}

// WriterTokenHash derives the writer token hash an Instance records when it
// is granted writer generation: the hash of the write capability that
// WriteCapability later re-derives for that generation.
func WriterTokenHash(key disk.FencingKey, instanceID, computerID uuid.UUID, generation int64) ([]byte, error) {
	capability, err := key.Derive(disk.FenceInput{InstanceID: instanceID, ComputerID: computerID, WriterGeneration: generation})
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(strings.TrimPrefix(capability.Hash, "sha256:"))
}
