package deployment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/helmrdotdev/helmr/internal/cas"
)

var (
	ErrManagerNotClaimed        = errors.New("manager selector is not claimed")
	ErrManagerAuthorityInvalid  = errors.New("manager authority is invalid")
	ErrManagerAuthorityDiverged = errors.New("manager selector resolved to different bytes")
	errManagerObjectNotFound    = errors.New("manager store object not found")
	errManagerDocumentInvalid   = errors.New("manager document is invalid")
	errManagerTreeInvalid       = errors.New("manager tree is invalid")
)

type managerDocument struct {
	Key       string
	MediaType string
	SizeBytes int64
}

type managerDocuments interface {
	Create(
		context.Context,
		managerDocument,
		[]byte,
	) (bool, error)
	Read(context.Context, string) (managerDocument, io.ReadCloser, error)
}

type ManagerStore struct {
	documents managerDocuments
	trees     cas.ImmutableStore
}

func newManagerStore(
	documents managerDocuments,
	trees cas.ImmutableStore,
) (*ManagerStore, error) {
	if documents == nil {
		return nil, errors.New("manager document store is required")
	}
	if trees == nil {
		return nil, errors.New("manager tree store is required")
	}
	return &ManagerStore{documents: documents, trees: trees}, nil
}

func (s *ManagerStore) Resolve(
	ctx context.Context,
	selector ManagerSelector,
) (ManagerCapsule, error) {
	if s == nil || s.documents == nil || s.trees == nil {
		return ManagerCapsule{}, errors.New("manager store is required")
	}
	if ctx == nil {
		return ManagerCapsule{}, errors.New("manager store context is nil")
	}
	if err := validateManagerSelector(selector); err != nil {
		return ManagerCapsule{}, err
	}
	claimKey, err := ManagerSelectorKey(selector)
	if err != nil {
		return ManagerCapsule{}, err
	}
	claimRaw, err := s.readDocument(
		ctx,
		claimKey,
		ManagerClaimMediaType,
		maxManagerClaimBytes,
	)
	if errors.Is(err, errManagerObjectNotFound) {
		return ManagerCapsule{}, ErrManagerNotClaimed
	}
	if err != nil {
		if errors.Is(err, errManagerDocumentInvalid) {
			return ManagerCapsule{}, fmt.Errorf(
				"%w: selector claim: %v",
				ErrManagerAuthorityInvalid,
				err,
			)
		}
		return ManagerCapsule{}, fmt.Errorf(
			"read selector claim: %w",
			err,
		)
	}
	claim, err := ParseManagerClaim(claimRaw)
	if err != nil {
		return ManagerCapsule{}, fmt.Errorf(
			"%w: parse selector claim: %v",
			ErrManagerAuthorityInvalid,
			err,
		)
	}
	if claim.Architecture != selector.Architecture ||
		claim.PackageManager != selector.PackageManager {
		return ManagerCapsule{}, fmt.Errorf(
			"%w: selector claim does not match its key",
			ErrManagerAuthorityInvalid,
		)
	}

	capsuleKey, err := ManagerCapsuleKey(claim.ManagerCapsuleDigest)
	if err != nil {
		return ManagerCapsule{}, fmt.Errorf(
			"%w: capsule key: %v",
			ErrManagerAuthorityInvalid,
			err,
		)
	}
	capsuleRaw, err := s.readDocument(
		ctx,
		capsuleKey,
		ManagerCapsuleMediaType,
		maxManagerCapsuleBytes,
	)
	if err != nil {
		if errors.Is(err, errManagerObjectNotFound) ||
			errors.Is(err, errManagerDocumentInvalid) {
			return ManagerCapsule{}, fmt.Errorf(
				"%w: capsule: %v",
				ErrManagerAuthorityInvalid,
				err,
			)
		}
		return ManagerCapsule{}, fmt.Errorf(
			"read capsule: %w",
			err,
		)
	}
	capsule, err := ParseManagerCapsule(capsuleRaw)
	if err != nil {
		return ManagerCapsule{}, fmt.Errorf(
			"%w: parse capsule: %v",
			ErrManagerAuthorityInvalid,
			err,
		)
	}
	if err := ValidateManagerAuthority(selector, claim, capsule); err != nil {
		return ManagerCapsule{}, fmt.Errorf(
			"%w: %v",
			ErrManagerAuthorityInvalid,
			err,
		)
	}
	if err := s.statTree(ctx, capsule.Tree); err != nil {
		if !errors.Is(err, errManagerTreeInvalid) {
			return ManagerCapsule{}, err
		}
		return ManagerCapsule{}, fmt.Errorf(
			"%w: %v",
			ErrManagerAuthorityInvalid,
			err,
		)
	}
	return capsule, nil
}

func (s *ManagerStore) Publish(
	ctx context.Context,
	selector ManagerSelector,
	capsule ManagerCapsule,
	tree *os.File,
) (ManagerCapsule, error) {
	if s == nil || s.documents == nil || s.trees == nil {
		return ManagerCapsule{}, errors.New("manager store is required")
	}
	if ctx == nil {
		return ManagerCapsule{}, errors.New("manager store context is nil")
	}
	if tree == nil {
		return ManagerCapsule{}, errors.New("manager capsule tree is required")
	}
	if err := validateManagerSelector(selector); err != nil {
		return ManagerCapsule{}, err
	}
	if err := validateManagerCapsule(capsule); err != nil {
		return ManagerCapsule{}, err
	}
	if selector.Architecture != capsule.Architecture ||
		selector.PackageManager != capsule.PackageManager {
		return ManagerCapsule{}, errors.New(
			"manager selector and candidate capsule do not match",
		)
	}
	capsuleDigest, err := ManagerCapsuleDigest(capsule)
	if err != nil {
		return ManagerCapsule{}, err
	}
	winner, err := s.Resolve(ctx, selector)
	if err == nil {
		return s.verifyWinner(ctx, winner, capsuleDigest)
	}
	if !errors.Is(err, ErrManagerNotClaimed) {
		return ManagerCapsule{}, err
	}

	_, err = s.trees.Publish(ctx, cas.Descriptor{
		Digest:    capsule.Tree.Digest,
		SizeBytes: capsule.Tree.SizeBytes,
		MediaType: capsule.Tree.MediaType,
	}, tree)
	if err != nil {
		return ManagerCapsule{}, fmt.Errorf("publish manager tree: %w", err)
	}

	capsuleRaw, err := CanonicalManagerCapsule(capsule)
	if err != nil {
		return ManagerCapsule{}, err
	}
	capsuleKey, err := ManagerCapsuleKey(capsuleDigest)
	if err != nil {
		return ManagerCapsule{}, err
	}
	if _, err := s.documents.Create(ctx, managerDocument{
		Key:       capsuleKey,
		MediaType: ManagerCapsuleMediaType,
		SizeBytes: int64(len(capsuleRaw)),
	}, capsuleRaw); err != nil {
		return ManagerCapsule{}, fmt.Errorf("publish manager capsule: %w", err)
	}
	if err := s.verifyCandidate(ctx, capsule, capsuleDigest); err != nil {
		return ManagerCapsule{}, err
	}

	claim := ManagerClaim{
		Architecture:         selector.Architecture,
		FormatVersion:        ManagerSelectorFormatVersion,
		ManagerCapsuleDigest: capsuleDigest,
		PackageManager:       selector.PackageManager,
	}
	claimRaw, err := CanonicalManagerClaim(claim)
	if err != nil {
		return ManagerCapsule{}, err
	}
	claimKey, err := ManagerSelectorKey(selector)
	if err != nil {
		return ManagerCapsule{}, err
	}
	if _, err := s.documents.Create(ctx, managerDocument{
		Key:       claimKey,
		MediaType: ManagerClaimMediaType,
		SizeBytes: int64(len(claimRaw)),
	}, claimRaw); err != nil {
		return ManagerCapsule{}, fmt.Errorf("publish manager claim: %w", err)
	}

	winner, err = s.Resolve(ctx, selector)
	if err != nil {
		return ManagerCapsule{}, err
	}
	return s.verifyWinner(ctx, winner, capsuleDigest)
}

func (s *ManagerStore) verifyWinner(
	ctx context.Context,
	winner ManagerCapsule,
	candidateDigest string,
) (ManagerCapsule, error) {
	if err := s.verifyTree(ctx, winner.Tree); err != nil {
		if !errors.Is(err, errManagerTreeInvalid) {
			return ManagerCapsule{}, err
		}
		return ManagerCapsule{}, fmt.Errorf(
			"%w: %v",
			ErrManagerAuthorityInvalid,
			err,
		)
	}
	winnerDigest, err := ManagerCapsuleDigest(winner)
	if err != nil {
		return ManagerCapsule{}, err
	}
	if winnerDigest != candidateDigest {
		return ManagerCapsule{}, ErrManagerAuthorityDiverged
	}
	return winner, nil
}

func (s *ManagerStore) verifyCandidate(
	ctx context.Context,
	expected ManagerCapsule,
	expectedDigest string,
) error {
	key, err := ManagerCapsuleKey(expectedDigest)
	if err != nil {
		return err
	}
	raw, err := s.readDocument(
		ctx,
		key,
		ManagerCapsuleMediaType,
		maxManagerCapsuleBytes,
	)
	if err != nil {
		if errors.Is(err, errManagerObjectNotFound) ||
			errors.Is(err, errManagerDocumentInvalid) {
			return fmt.Errorf(
				"%w: candidate capsule: %v",
				ErrManagerAuthorityInvalid,
				err,
			)
		}
		return fmt.Errorf("read candidate capsule: %w", err)
	}
	candidate, err := ParseManagerCapsule(raw)
	if err != nil {
		return fmt.Errorf(
			"%w: parse candidate capsule: %v",
			ErrManagerAuthorityInvalid,
			err,
		)
	}
	actualDigest, err := ManagerCapsuleDigest(candidate)
	if err != nil {
		return err
	}
	if actualDigest != expectedDigest || candidate != expected {
		return fmt.Errorf(
			"%w: candidate capsule does not match",
			ErrManagerAuthorityInvalid,
		)
	}
	if err := s.verifyTree(ctx, candidate.Tree); err != nil {
		if errors.Is(err, errManagerTreeInvalid) {
			return fmt.Errorf(
				"%w: %v",
				ErrManagerAuthorityInvalid,
				err,
			)
		}
		return err
	}
	return nil
}

func (s *ManagerStore) readDocument(
	ctx context.Context,
	key,
	mediaType string,
	maxBytes int,
) ([]byte, error) {
	object, body, err := s.documents.Read(ctx, key)
	if err != nil {
		return nil, err
	}
	if object.Key != key ||
		object.MediaType != mediaType ||
		object.SizeBytes < 1 ||
		object.SizeBytes > int64(maxBytes) {
		_ = body.Close()
		return nil, fmt.Errorf(
			"%w: metadata does not match",
			errManagerDocumentInvalid,
		)
	}
	raw, readErr := io.ReadAll(io.LimitReader(body, int64(maxBytes)+1))
	closeErr := body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(raw)) != object.SizeBytes {
		return nil, fmt.Errorf(
			"%w: length does not match",
			errManagerDocumentInvalid,
		)
	}
	return raw, nil
}

func (s *ManagerStore) statTree(
	ctx context.Context,
	expected ManagerArtifact,
) error {
	object, err := s.trees.Stat(ctx, expected.Digest)
	if err != nil {
		if managerStoreObjectMissing(err) {
			return fmt.Errorf("%w: object is missing", errManagerTreeInvalid)
		}
		return fmt.Errorf("stat manager tree: %w", err)
	}
	if object.Digest != expected.Digest ||
		object.MediaType != expected.MediaType ||
		object.SizeBytes != expected.SizeBytes {
		return fmt.Errorf(
			"%w: metadata does not match",
			errManagerTreeInvalid,
		)
	}
	return nil
}

func (s *ManagerStore) verifyTree(
	ctx context.Context,
	expected ManagerArtifact,
) error {
	if err := s.statTree(ctx, expected); err != nil {
		return err
	}
	body, err := s.trees.Get(ctx, expected.Digest)
	if err != nil {
		if managerStoreObjectMissing(err) {
			return fmt.Errorf("%w: object is missing", errManagerTreeInvalid)
		}
		return fmt.Errorf("open manager tree: %w", err)
	}
	written, copyErr := io.Copy(io.Discard, io.LimitReader(
		body,
		expected.SizeBytes+1,
	))
	closeErr := body.Close()
	if copyErr != nil {
		if errors.Is(copyErr, cas.ErrDigestMismatch) {
			return fmt.Errorf("%w: %v", errManagerTreeInvalid, copyErr)
		}
		return fmt.Errorf("verify manager tree: %w", copyErr)
	}
	if closeErr != nil {
		if errors.Is(closeErr, cas.ErrDigestMismatch) {
			return fmt.Errorf("%w: %v", errManagerTreeInvalid, closeErr)
		}
		return fmt.Errorf("close manager tree: %w", closeErr)
	}
	if written != expected.SizeBytes {
		return fmt.Errorf(
			"%w: length does not match",
			errManagerTreeInvalid,
		)
	}
	return nil
}

func managerStoreObjectMissing(err error) bool {
	return errors.Is(err, errManagerObjectNotFound) ||
		errors.Is(err, os.ErrNotExist) ||
		managerObjectMissing(err)
}
