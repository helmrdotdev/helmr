package secret

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

var ErrRegistryCredentialUnavailable = errors.New("registry credential authority is unavailable")

// OpenRegistryCredential decrypts one version already selected and locked by
// the caller's current-Build-Lease transaction. It deliberately has no
// Workspace placement semantics; registry_credential_resolutions is the
// separate audit and ownership domain.
func (s *Store) OpenRegistryCredential(
	environmentID uuid.UUID,
	secretValue db.Secret,
	version db.SecretVersion,
) ([]byte, error) {
	if environmentID == uuid.Nil ||
		secretValue.EnvironmentID != pgvalue.UUID(environmentID) ||
		secretValue.State != "active" ||
		version.SecretID != secretValue.ID ||
		!version.ID.Valid {
		return nil, ErrRegistryCredentialUnavailable
	}
	value, err := s.decrypt(environmentID, secretValue, version)
	if err != nil {
		return nil, UnavailableError{
			Err: fmt.Errorf("open registry credential secret version: %w", err),
		}
	}
	return value, nil
}
