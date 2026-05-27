package cas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
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
	return putStage(ctx, stage, body)
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
	return &fileStage{
		store:     c,
		mediaType: mediaType,
		file:      file,
		path:      file.Name(),
		hash:      sha256.New(),
	}, nil
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
	return os.Open(path)
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
	store     *File
	mediaType string
	file      *os.File
	path      string
	hash      hash.Hash
	size      int64
	closed    bool
	finished  bool
	aborted   bool
}

func (s *fileStage) Write(p []byte) (int, error) {
	if s.finished {
		if s.aborted {
			return 0, errStageAborted
		}
		return 0, errStageCommitted
	}
	if s.closed {
		return 0, errStageClosed
	}
	n, err := s.file.Write(p)
	if n > 0 {
		_, _ = s.hash.Write(p[:n])
		s.size += int64(n)
	}
	return n, err
}

func (s *fileStage) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.file.Close()
}

func (s *fileStage) Commit(ctx context.Context) (Object, error) {
	if s.finished {
		if s.aborted {
			return Object{}, errStageAborted
		}
		return Object{}, errStageCommitted
	}
	s.finished = true
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(s.path)
			_ = os.Remove(s.path + ".json")
		}
	}()
	if err := ctx.Err(); err != nil {
		return Object{}, err
	}
	if err := s.Close(); err != nil {
		return Object{}, err
	}
	digest := "sha256:" + hex.EncodeToString(s.hash.Sum(nil))
	key, err := ObjectKey("", digest)
	if err != nil {
		return Object{}, err
	}
	finalPath := filepath.Join(s.store.root, filepath.FromSlash(key))
	if err := s.store.writeMetadata(s.path, s.mediaType); err != nil {
		return Object{}, err
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return Object{}, err
	}
	if err := os.Chmod(s.path, 0o644); err != nil {
		return Object{}, err
	}
	if err := os.Rename(s.path, finalPath); err != nil {
		return Object{}, err
	}
	if err := os.Rename(s.path+".json", finalPath+".json"); err != nil {
		return Object{}, err
	}
	cleanup = false
	return Object{Digest: digest, SizeBytes: s.size, Key: key, MediaType: s.mediaType}, nil
}

func (s *fileStage) Abort(context.Context) error {
	if s.finished {
		return nil
	}
	s.finished = true
	s.aborted = true
	closeErr := s.Close()
	removeErr := os.Remove(s.path)
	if removeErr != nil && os.IsNotExist(removeErr) {
		removeErr = nil
	}
	metadataRemoveErr := os.Remove(s.path + ".json")
	if metadataRemoveErr != nil && os.IsNotExist(metadataRemoveErr) {
		metadataRemoveErr = nil
	}
	if closeErr != nil {
		return closeErr
	}
	if removeErr != nil {
		return removeErr
	}
	return metadataRemoveErr
}

var _ Store = (*File)(nil)
var _ StagingStore = (*File)(nil)
