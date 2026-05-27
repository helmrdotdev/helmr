package cas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"os"
)

type stageFile struct {
	mediaType string
	file      *os.File
	path      string
	hash      hash.Hash
	size      int64
	closed    bool
	finished  bool
	aborted   bool
}

func newStageFile(mediaType string, file *os.File) *stageFile {
	return &stageFile{
		mediaType: mediaType,
		file:      file,
		path:      file.Name(),
		hash:      sha256.New(),
	}
}

func (s *stageFile) Write(p []byte) (int, error) {
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

func (s *stageFile) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.file.Close()
}

func (s *stageFile) beginCommit(ctx context.Context, syncBeforeClose bool) (string, error) {
	if s.finished {
		if s.aborted {
			return "", errStageAborted
		}
		return "", errStageCommitted
	}
	s.finished = true
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if syncBeforeClose && !s.closed {
		if err := s.file.Sync(); err != nil {
			return "", err
		}
	}
	if err := s.Close(); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(s.hash.Sum(nil)), nil
}

func (s *stageFile) Abort(context.Context) error {
	if s.finished {
		return nil
	}
	s.finished = true
	s.aborted = true
	closeErr := s.Close()
	removeErr := os.Remove(s.path)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	if closeErr != nil {
		return closeErr
	}
	return removeErr
}
