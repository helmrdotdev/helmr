package deployment

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/helmrdotdev/helmr/internal/cas"
)

type ManagerStore struct {
	trees cas.ImmutableStore
}

func NewManagerS3(
	ctx context.Context,
	rawURI string,
) (*ManagerStore, error) {
	if ctx == nil {
		return nil, errors.New("Manager store context is nil")
	}
	uri, err := parseManagerStoreURI(rawURI)
	if err != nil {
		return nil, err
	}
	uri.Path = "/" + joinManagerStoreKey(
		strings.Trim(uri.Path, "/"),
		"v0/trees",
	)
	uri.RawPath = ""
	trees, err := cas.NewImmutableS3(ctx, uri.String())
	if err != nil {
		return nil, fmt.Errorf("configure Manager tree store: %w", err)
	}
	return &ManagerStore{trees: trees}, nil
}

func newManagerStore(trees cas.ImmutableStore) (*ManagerStore, error) {
	if trees == nil {
		return nil, errors.New("Manager tree store is required")
	}
	return &ManagerStore{trees: trees}, nil
}

func (s *ManagerStore) Validate(
	ctx context.Context,
	manager Manager,
) error {
	if s == nil || s.trees == nil {
		return errors.New("Manager store is required")
	}
	if ctx == nil {
		return errors.New("Manager store context is nil")
	}
	if err := validateManager(manager); err != nil {
		return err
	}
	object, err := s.trees.Stat(ctx, manager.Tree.Digest)
	if err != nil {
		return fmt.Errorf("stat Manager tree: %w", err)
	}
	if object.Digest != manager.Tree.Digest ||
		object.MediaType != manager.Tree.MediaType ||
		object.SizeBytes != manager.Tree.SizeBytes {
		return errors.New("Manager tree does not match its certified descriptor")
	}
	return nil
}

func (s *ManagerStore) Snapshot(
	ctx context.Context,
	directory string,
	manager Manager,
) (*ArtifactSnapshot, error) {
	if err := s.Validate(ctx, manager); err != nil {
		return nil, err
	}
	body, err := s.trees.Get(ctx, manager.Tree.Digest)
	if err != nil {
		return nil, fmt.Errorf("open Manager tree: %w", err)
	}
	content, snapshotErr := snapshotArtifact(
		ctx,
		directory,
		managerArtifact,
		artifactSnapshotDescriptor{
			Digest:    manager.Tree.Digest,
			SizeBytes: manager.Tree.SizeBytes,
			MediaType: manager.Tree.MediaType,
		},
		body,
	)
	closeErr := body.Close()
	if snapshotErr != nil {
		return nil, snapshotErr
	}
	if closeErr != nil {
		_ = content.Close()
		return nil, fmt.Errorf("close Manager tree: %w", closeErr)
	}
	return &ArtifactSnapshot{content: content}, nil
}

func parseManagerStoreURI(raw string) (*url.URL, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return nil, errors.New("Manager store URI is required and must be normalized")
	}
	uri, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse Manager store URI: %w", err)
	}
	if uri.Scheme != "s3" || uri.Host == "" {
		return nil, fmt.Errorf("invalid Manager store URI %q", raw)
	}
	if uri.User != nil || uri.Fragment != "" {
		return nil, errors.New("Manager store URI forbids user info and fragments")
	}
	if uri.EscapedPath() != uri.Path {
		return nil, errors.New("Manager store URI path must not use escapes")
	}
	return uri, nil
}

func joinManagerStoreKey(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "/" + key
}
