package enrollment

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
)

const (
	proofDomain              = "helmr.worker.enrollment.v0"
	workerSecretDecodedBytes = 32
	workerSecretEncodedBytes = 43
	maximumResourceID        = 512
)

type GroupSecret struct {
	GroupID string
	Secret  string
}

type Verifier struct {
	secrets map[string][]byte
}

func NewVerifier(groups []GroupSecret) (*Verifier, error) {
	if len(groups) == 0 {
		return nil, errors.New("at least one worker enrollment secret is required")
	}
	secrets := make(map[string][]byte, len(groups))
	secretOwners := make(map[string]string, len(groups))
	for _, group := range groups {
		groupID := strings.TrimSpace(group.GroupID)
		if groupID == "" || groupID != group.GroupID {
			return nil, errors.New("worker enrollment group id must be canonical and non-empty")
		}
		secret, err := decodeSecret(group.Secret)
		if err != nil {
			return nil, fmt.Errorf("worker enrollment secret for group %q: %w", groupID, err)
		}
		if _, exists := secrets[groupID]; exists {
			return nil, fmt.Errorf("worker enrollment group %q is duplicated", groupID)
		}
		secretIdentity := string(secret)
		if owner, exists := secretOwners[secretIdentity]; exists {
			return nil, fmt.Errorf("worker enrollment groups %q and %q must not share a secret", owner, groupID)
		}
		secretOwners[secretIdentity] = groupID
		secrets[groupID] = append([]byte(nil), secret...)
	}
	return &Verifier{secrets: secrets}, nil
}

func (v *Verifier) HasGroup(groupID string) bool {
	if v == nil {
		return false
	}
	_, ok := v.secrets[groupID]
	return ok
}

func (v *Verifier) Verify(request api.WorkerEnrollmentRequest) error {
	if v == nil {
		return errors.New("worker enrollment is not configured")
	}
	if err := validateRequest(request); err != nil {
		return err
	}
	secret, ok := v.secrets[request.WorkerGroupID]
	if !ok {
		return errors.New("worker group enrollment is not configured")
	}
	expected := proof(secret, request.WorkerEnrollmentIntent, request.ResourceID)
	provided, err := base64.RawURLEncoding.DecodeString(request.Proof)
	if err != nil || base64.RawURLEncoding.EncodeToString(provided) != request.Proof || len(provided) != sha256.Size {
		return errors.New("worker enrollment proof is invalid")
	}
	if subtle.ConstantTimeCompare(expected, provided) != 1 {
		return errors.New("worker enrollment proof is invalid")
	}
	return nil
}

func BuildRequest(groupID string, nonce string, supportsRun bool, supportsBuild bool, resourceID string, secret string) (api.WorkerEnrollmentRequest, error) {
	request := api.WorkerEnrollmentRequest{
		WorkerEnrollmentIntent: api.WorkerEnrollmentIntent{
			WorkerGroupID:   groupID,
			Nonce:           nonce,
			SupportsRun:     supportsRun,
			SupportsBuild:   supportsBuild,
			ProtocolVersion: auth.WorkerProtocolVersion,
		},
		ResourceID: resourceID,
	}
	secretBytes, err := decodeSecret(secret)
	if err != nil {
		return api.WorkerEnrollmentRequest{}, err
	}
	if err := validateRequest(request); err != nil {
		return api.WorkerEnrollmentRequest{}, err
	}
	request.Proof = base64.RawURLEncoding.EncodeToString(proof(secretBytes, request.WorkerEnrollmentIntent, request.ResourceID))
	return request, nil
}

func ValidateSecret(secret string) error {
	_, err := decodeSecret(secret)
	return err
}

func decodeSecret(secret string) ([]byte, error) {
	if len(secret) != workerSecretEncodedBytes {
		return nil, errors.New("worker enrollment secret must be a canonical base64url-no-pad encoding of exactly 32 bytes")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(decoded) != workerSecretDecodedBytes || base64.RawURLEncoding.EncodeToString(decoded) != secret {
		return nil, errors.New("worker enrollment secret must be a canonical base64url-no-pad encoding of exactly 32 bytes")
	}
	return decoded, nil
}

func validateRequest(request api.WorkerEnrollmentRequest) error {
	if request.WorkerGroupID == "" || strings.TrimSpace(request.WorkerGroupID) != request.WorkerGroupID {
		return errors.New("worker group id must be canonical and non-empty")
	}
	if request.Nonce == "" || strings.TrimSpace(request.Nonce) != request.Nonce {
		return errors.New("worker enrollment nonce must be canonical and non-empty")
	}
	if request.ProtocolVersion != auth.WorkerProtocolVersion {
		return errors.New("worker enrollment protocol version is unsupported")
	}
	if !request.SupportsRun && !request.SupportsBuild {
		return errors.New("worker enrollment must request at least one role")
	}
	if request.ResourceID == "" || strings.TrimSpace(request.ResourceID) != request.ResourceID || len(request.ResourceID) > maximumResourceID {
		return errors.New("worker resource id must be canonical, non-empty, and bounded")
	}
	return nil
}

func proof(secret []byte, intent api.WorkerEnrollmentIntent, resourceID string) []byte {
	mac := hmac.New(sha256.New, secret)
	for _, field := range []string{
		proofDomain,
		intent.ProtocolVersion,
		intent.WorkerGroupID,
		intent.Nonce,
		boolField(intent.SupportsRun),
		boolField(intent.SupportsBuild),
		resourceID,
	} {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(field)))
		_, _ = mac.Write(size[:])
		_, _ = mac.Write([]byte(field))
	}
	return mac.Sum(nil)
}

func boolField(value bool) string {
	if value {
		return "1"
	}
	return "0"
}
