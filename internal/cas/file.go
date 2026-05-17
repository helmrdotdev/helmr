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

func (c *File) Put(_ context.Context, mediaType string, body io.Reader) (Object, error) {
	bytes, digest, err := DigestReader(body)
	if err != nil {
		return Object{}, err
	}
	key, err := ObjectKey("", digest)
	if err != nil {
		return Object{}, err
	}
	path := filepath.Join(c.root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Object{}, err
	}
	if err := os.WriteFile(path, bytes, 0o644); err != nil {
		return Object{}, err
	}
	if err := c.writeMetadata(path, mediaType); err != nil {
		return Object{}, err
	}
	return Object{Digest: digest, SizeBytes: int64(len(bytes)), Key: key, MediaType: mediaType}, nil
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

var _ Store = (*File)(nil)
