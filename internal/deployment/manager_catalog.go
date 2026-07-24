package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

var (
	errManagerCatalogUnauthenticated = errors.New("authenticated Manager catalog is required")
	ErrManagerNotCertified           = errors.New("Manager is not certified")
)

const (
	ManagerCatalogFormatVersion = 0
	ManagerCatalogMediaType     = "application/vnd.helmr.manager-catalog.v0+json"
	maxManagerCatalogBytes      = maxProgramFileSizeBytes
)

type ManagerCatalog struct {
	managers      []Manager
	digest        string
	authenticated bool
}

type managerCatalogDocument struct {
	FormatVersion int       `json:"formatVersion"`
	Managers      []Manager `json:"managers"`
}

func ParseManagerCatalog(raw []byte) (*ManagerCatalog, error) {
	if len(raw) == 0 || int64(len(raw)) > maxManagerCatalogBytes {
		return nil, fmt.Errorf(
			"Manager catalog size is outside [1,%d]",
			maxManagerCatalogBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Manager catalog: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return nil, errors.New("Manager catalog is not RFC 8785 canonical JSON")
	}

	var document managerCatalogDocument
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode Manager catalog: %w", err)
	}
	if err := ensureEOF(decoder, "Manager catalog"); err != nil {
		return nil, err
	}
	if err := validateManagerCatalogDocument(document); err != nil {
		return nil, err
	}
	complete, err := canonicalManagerCatalogDocument(document)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(raw, complete) {
		return nil, errors.New("Manager catalog does not match the complete canonical v0 shape")
	}
	hash := sha256.Sum256(raw)
	return &ManagerCatalog{
		managers: append([]Manager(nil), document.Managers...),
		digest:   fmt.Sprintf("sha256:%x", hash[:]),
	}, nil
}

func CanonicalManagerCatalog(managers []Manager) ([]byte, error) {
	document := managerCatalogDocument{
		FormatVersion: ManagerCatalogFormatVersion,
		Managers:      append([]Manager(nil), managers...),
	}
	if err := validateManagerCatalogDocument(document); err != nil {
		return nil, err
	}
	return canonicalManagerCatalogDocument(document)
}

func (c *ManagerCatalog) Resolve(
	manager PackageManager,
	architecture RuntimeArchitecture,
) (Manager, error) {
	if c == nil || !c.authenticated {
		return Manager{}, errManagerCatalogUnauthenticated
	}
	for _, certified := range c.managers {
		if certified.PackageManager == manager &&
			certified.Architecture == architecture {
			return certified, nil
		}
	}
	return Manager{}, fmt.Errorf(
		"%w: %s@%s for %s",
		ErrManagerNotCertified,
		manager.Name,
		manager.Version,
		architecture,
	)
}

func (c *ManagerCatalog) ResolvePinned(
	manager PackageManager,
	architecture RuntimeArchitecture,
	digest string,
) (Manager, error) {
	certified, err := c.Resolve(manager, architecture)
	if err != nil {
		return Manager{}, err
	}
	if certified.Tree.Digest != digest {
		return Manager{}, fmt.Errorf(
			"%w: %s@%s digest does not match the certified bytes",
			ErrManagerNotCertified,
			manager.Name,
			manager.Version,
		)
	}
	return certified, nil
}

func (c *ManagerCatalog) Digest() (string, error) {
	if c == nil || !c.authenticated || !sha256DigestPattern.MatchString(c.digest) {
		return "", errManagerCatalogUnauthenticated
	}
	return c.digest, nil
}

func validateManagerCatalogDocument(document managerCatalogDocument) error {
	if document.FormatVersion != ManagerCatalogFormatVersion {
		return fmt.Errorf(
			"Manager catalog formatVersion = %d, want %d",
			document.FormatVersion,
			ManagerCatalogFormatVersion,
		)
	}
	if len(document.Managers) == 0 {
		return errors.New("Manager catalog managers must be a non-empty array")
	}
	var previous string
	for position, manager := range document.Managers {
		if err := validateManager(manager); err != nil {
			return fmt.Errorf("Manager catalog entry %d: %w", position, err)
		}
		if manager.Architecture != ArchitectureX8664 {
			return fmt.Errorf(
				"Manager catalog entry %d architecture = %q, want %q",
				position,
				manager.Architecture,
				ArchitectureX8664,
			)
		}
		key := string(manager.PackageManager.Name) + "\x00" +
			manager.PackageManager.Version
		if position > 0 && previous >= key {
			return fmt.Errorf(
				"Manager catalog entries are not in name/version order at position %d",
				position,
			)
		}
		previous = key
	}
	return nil
}

func canonicalManagerCatalogDocument(document managerCatalogDocument) ([]byte, error) {
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode Manager catalog: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Manager catalog: %w", err)
	}
	if len(canonical) == 0 || int64(len(canonical)) > maxManagerCatalogBytes {
		return nil, fmt.Errorf(
			"Manager catalog size is outside [1,%d]",
			maxManagerCatalogBytes,
		)
	}
	return canonical, nil
}
