package cas

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

type verifyingReadCloser struct {
	body     io.ReadCloser
	hash     hash.Hash
	expected string
	eof      bool
	closed   bool
	err      error
}

func NewVerifyingReadCloser(body io.ReadCloser, expected string) io.ReadCloser {
	return &verifyingReadCloser{
		body:     body,
		hash:     sha256.New(),
		expected: expected,
	}
}

func (r *verifyingReadCloser) Read(p []byte) (int, error) {
	n, readErr := r.body.Read(p)
	if n > 0 {
		_, _ = r.hash.Write(p[:n])
	}
	if errors.Is(readErr, io.EOF) {
		r.eof = true
		if err := r.verify(); err != nil {
			return n, err
		}
	}
	return n, readErr
}

func (r *verifyingReadCloser) Close() error {
	if r.closed {
		return r.err
	}
	r.closed = true
	var drainErr error
	if !r.eof {
		_, drainErr = io.Copy(r.hash, r.body)
		if drainErr == nil {
			r.eof = true
		}
	}
	closeErr := r.body.Close()
	verifyErr := r.verify()
	r.err = errors.Join(drainErr, closeErr, verifyErr)
	return r.err
}

func (r *verifyingReadCloser) verify() error {
	if r.err != nil {
		return r.err
	}
	actual := sha256sum.FormatDigest(r.hash.Sum(nil))
	if actual != r.expected {
		r.err = fmt.Errorf("%w: expected %s, got %s", ErrDigestMismatch, r.expected, actual)
	}
	return r.err
}
