package cas

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type File struct {
	root string
}

type fileObjectMetadata struct {
	MediaType string `json:"media_type"`
}

func NewFile(root string) (*File, error) {
	if root == "" {
		return nil, fmt.Errorf("file CAS root is required")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &File{root: root}, nil
}

func (c *File) Put(ctx context.Context, mediaType string, body io.Reader) (Object, error) {
	stage, err := c.Stage(ctx, mediaType)
	if err != nil {
		return Object{}, err
	}
	return WriteStage(ctx, stage, body)
}

func (c *File) Stage(ctx context.Context, mediaType string) (Stage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stagingDir := filepath.Join(c.root, ".staging")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(stagingDir, "stage-*")
	if err != nil {
		return nil, err
	}
	return &fileStage{store: c, FileStage: NewFileStage(mediaType, file)}, nil
}

func (c *File) Stat(_ context.Context, digest string) (Object, error) {
	path, key, err := c.path(digest)
	if err != nil {
		return Object{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Object{}, err
	}
	return Object{Digest: digest, SizeBytes: info.Size(), Key: key, MediaType: c.readMediaType(path)}, nil
}

func (c *File) Get(_ context.Context, digest string) (io.ReadCloser, error) {
	path, _, err := c.path(digest)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return NewVerifyingReadCloser(file, digest), nil
}

func (c *File) Delete(_ context.Context, digest string) error {
	path, _, err := c.path(digest)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(path + ".json"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (c *File) path(digest string) (string, string, error) {
	key, err := ObjectKey("", digest)
	if err != nil {
		return "", "", err
	}
	return filepath.Join(c.root, filepath.FromSlash(key)), key, nil
}

func (c *File) writeMetadata(path string, mediaType string) error {
	bytes, err := json.Marshal(fileObjectMetadata{MediaType: mediaType})
	if err != nil {
		return err
	}
	return os.WriteFile(path+".json", bytes, 0o644)
}

func (c *File) readMediaType(path string) string {
	bytes, err := os.ReadFile(path + ".json")
	if err != nil {
		return ""
	}
	var metadata fileObjectMetadata
	if err := json.Unmarshal(bytes, &metadata); err != nil {
		return ""
	}
	return metadata.MediaType
}

type fileStage struct {
	store *File
	*FileStage
}

func (s *fileStage) Commit(ctx context.Context) (Object, error) {
	cleanup := true
	var finalPath string
	var finalMetadataPath string
	var finalDataStagePath string
	cleanupFinalData := false
	cleanupFinalMetadata := false
	defer func() {
		if cleanup {
			_ = os.Remove(s.Path())
			_ = os.Remove(s.Path() + ".json")
			if cleanupFinalData && finalPath != "" {
				_ = os.Remove(finalPath)
			}
			if finalDataStagePath != "" {
				_ = os.Remove(finalDataStagePath)
			}
			if cleanupFinalMetadata && finalMetadataPath != "" {
				_ = os.Remove(finalMetadataPath)
			}
		}
	}()
	digest, err := s.BeginCommit(ctx, true)
	if err != nil {
		return Object{}, err
	}
	key, err := ObjectKey("", digest)
	if err != nil {
		return Object{}, err
	}
	finalPath = filepath.Join(s.store.root, filepath.FromSlash(key))
	finalMetadataPath = finalPath + ".json"
	if err := s.store.writeMetadata(s.Path(), s.MediaType()); err != nil {
		return Object{}, err
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return Object{}, err
	}
	if err := os.Chmod(s.Path(), 0o644); err != nil {
		return Object{}, err
	}
	if _, err := os.Stat(finalPath); err == nil {
		if err := os.Rename(s.Path()+".json", finalMetadataPath); err != nil {
			return Object{}, err
		}
		cleanup = false
		_ = os.Remove(s.Path())
		return Object{Digest: digest, SizeBytes: s.Size(), Key: key, MediaType: s.MediaType()}, nil
	} else if !os.IsNotExist(err) {
		return Object{}, err
	}
	finalDataStage, err := os.CreateTemp(filepath.Dir(finalPath), "."+filepath.Base(finalPath)+".data-*")
	if err != nil {
		return Object{}, err
	}
	finalDataStagePath = finalDataStage.Name()
	if err := finalDataStage.Close(); err != nil {
		return Object{}, err
	}
	if err := os.Remove(finalDataStagePath); err != nil {
		return Object{}, err
	}
	if err := os.Rename(s.Path(), finalDataStagePath); err != nil {
		return Object{}, err
	}
	if err := os.Rename(s.Path()+".json", finalMetadataPath); err != nil {
		return Object{}, err
	}
	cleanupFinalMetadata = true
	if err := os.Rename(finalDataStagePath, finalPath); err != nil {
		return Object{}, err
	}
	finalDataStagePath = ""
	cleanupFinalData = true
	cleanup = false
	return Object{Digest: digest, SizeBytes: s.Size(), Key: key, MediaType: s.MediaType()}, nil
}

var _ Store = (*File)(nil)
