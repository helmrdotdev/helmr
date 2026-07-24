package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

const ManagerAttestationPredicateType = "https://helmr.dev/attestations/manager-release/v0"

type managerAttestationDocument struct {
	Type          string                      `json:"_type"`
	Subject       []releaseAttestationSubject `json:"subject"`
	PredicateType string                      `json:"predicateType"`
	Predicate     managerAttestationPredicate `json:"predicate"`
}

type managerAttestationPredicate struct {
	CatalogDigest    string             `json:"catalogDigest"`
	CatalogMediaType string             `json:"catalogMediaType"`
	FormatVersion    int                `json:"formatVersion"`
	Managers         []Manager          `json:"managers"`
	Predecessor      *RuntimeReleaseRef `json:"predecessor"`
}

func VerifyManagerCatalog(
	catalogBytes,
	bundleBytes,
	trustedRootBytes []byte,
) (*ManagerCatalog, error) {
	catalog, err := ParseManagerCatalog(catalogBytes)
	if err != nil {
		return nil, err
	}
	entity, err := parseReleaseBundle(bundleBytes)
	if err != nil {
		return nil, err
	}
	trustedMaterial, err := parseReleaseTrustedRoot(trustedRootBytes)
	if err != nil {
		return nil, err
	}
	if err := authenticateManagerCatalog(
		catalogBytes,
		catalog,
		entity,
		trustedMaterial,
	); err != nil {
		return nil, err
	}
	catalog.authenticated = true
	return catalog, nil
}

func authenticateManagerCatalog(
	catalogBytes []byte,
	catalog *ManagerCatalog,
	entity verify.SignedEntity,
	trustedMaterial root.TrustedMaterial,
) error {
	if catalog == nil {
		return errors.New("Manager catalog is required")
	}
	if entity == nil {
		return errors.New("Manager attestation bundle is required")
	}
	if trustedMaterial == nil {
		return errors.New("Manager trusted root is required")
	}
	catalogHash := sha256.Sum256(catalogBytes)
	raw, err := verifyReleasePayload(
		entity,
		trustedMaterial,
		catalogHash[:],
		releaseCertificateSANPattern,
	)
	if err != nil {
		return err
	}
	var statement managerAttestationDocument
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&statement); err != nil {
		return fmt.Errorf("decode Manager release attestation: %w", err)
	}
	if err := ensureEOF(decoder, "Manager release attestation"); err != nil {
		return err
	}
	if statement.Type != RuntimeAttestationType ||
		statement.PredicateType != ManagerAttestationPredicateType ||
		statement.Predicate.FormatVersion != ManagerCatalogFormatVersion ||
		statement.Predicate.CatalogMediaType != ManagerCatalogMediaType ||
		statement.Predicate.Predecessor != nil {
		return errors.New("Manager release attestation has an invalid v0 envelope")
	}
	wantDigest := "sha256:" + hex.EncodeToString(catalogHash[:])
	if statement.Predicate.CatalogDigest != wantDigest ||
		len(statement.Subject) != 1 ||
		statement.Subject[0].Name != "manager-release/catalog.json" ||
		statement.Subject[0].Digest["sha256"] != hex.EncodeToString(catalogHash[:]) ||
		len(statement.Subject[0].Digest) != 1 {
		return errors.New("Manager release attestation does not bind the catalog")
	}
	if !managerCatalogEntriesEqual(statement.Predicate.Managers, catalog.managers) {
		return errors.New("Manager release attestation entries do not match the catalog")
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return fmt.Errorf("canonicalize Manager release attestation: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return errors.New("Manager release attestation is not canonical JSON")
	}
	return nil
}

func managerCatalogEntriesEqual(left, right []Manager) bool {
	leftRaw, err := json.Marshal(left)
	if err != nil {
		return false
	}
	rightRaw, err := json.Marshal(right)
	if err != nil {
		return false
	}
	return bytes.Equal(leftRaw, rightRaw)
}
