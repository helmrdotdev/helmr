package secret

import (
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

const maxComputerSecretPlacements = 64

var ErrDeliveryUnavailable = errors.New("secret delivery authority is unavailable")

// ErrDeliveryRevoked denies an otherwise valid recorded delivery whose Secret
// status or revocation generation no longer permits use.
var ErrDeliveryRevoked = fmt.Errorf("secret delivery is no longer authorized: %w", ErrDeliveryUnavailable)

type DeliveryEnvelope struct {
	Mode            string
	PlacementKind   string
	PlacementTarget string
	Secret          db.Secret
	Version         db.SecretVersion
}

type DeliveryMaterial struct {
	PlacementKind   string
	PlacementTarget string
	Value           []byte
}

func (s *Store) OpenDeliveries(environmentID uuid.UUID, envelopes []DeliveryEnvelope) ([]DeliveryMaterial, error) {
	if len(envelopes) > maxComputerSecretPlacements {
		return nil, ErrDeliveryUnavailable
	}
	materials := make([]DeliveryMaterial, 0, len(envelopes))
	for _, envelope := range envelopes {
		if envelope.Mode == "protected" {
			continue
		}
		if envelope.Mode != "raw" {
			return nil, ErrDeliveryUnavailable
		}
		if envelope.Secret.EnvironmentID != pgvalue.UUID(environmentID) ||
			envelope.Secret.Status != "active" ||
			envelope.Version.SecretID != envelope.Secret.ID ||
			!envelope.Version.ID.Valid ||
			(envelope.PlacementKind != "env" && envelope.PlacementKind != "file") ||
			envelope.PlacementTarget == "" {
			return nil, ErrDeliveryUnavailable
		}
		value, err := s.decrypt(environmentID, envelope.Secret, envelope.Version)
		if err != nil {
			return nil, UnavailableError{Err: fmt.Errorf("open resolved secret version: %w", err)}
		}
		materials = append(materials, DeliveryMaterial{
			PlacementKind:   envelope.PlacementKind,
			PlacementTarget: envelope.PlacementTarget,
			Value:           value,
		})
	}
	return materials, nil
}
