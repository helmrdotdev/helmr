package computerhost

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

// Each file is private to the physical allocation's accounted staging directory.
// The caller retains that directory through all uploads/VM creation and removes
// it only after those jobs have joined.
func encryptCheckpointObject(ctx context.Context, cipher *CheckpointEncryptor, directory, checkpoint, role, media string, plaintext io.Reader, limit int64) (computercheckpoint.Object, string, error) {
	if cipher == nil || limit <= 0 {
		return computercheckpoint.Object{}, "", invalidCheckpoint(errors.New("checkpoint encryption requires a size bound"))
	}
	file, err := os.CreateTemp(directory, role+"-encrypted-")
	if err != nil {
		return computercheckpoint.Object{}, "", err
	}
	hash := sha256.New()
	input := &io.LimitedReader{R: plaintext, N: limit + 1}
	err = cipher.Encrypt(ctx, input, io.MultiWriter(file, hash), checkpoint+"/"+role)
	if err == nil && input.N == 0 {
		err = invalidCheckpoint(errors.New("checkpoint plaintext exceeds its staging bound"))
	}
	if err == nil {
		err = file.Chmod(0400)
	}
	// This file is upload staging, not a recovery record. Publication of the
	// exact object establishes durability before the checkpoint becomes ready.
	info, statErr := file.Stat()
	err = errors.Join(err, statErr, file.Close())
	if err != nil {
		return computercheckpoint.Object{}, file.Name(), err
	}
	return computercheckpoint.Object{Digest: sha256sum.FormatDigest(hash.Sum(nil)), SizeBytes: info.Size(), MediaType: media}, file.Name(), nil
}

func downloadCheckpointObject(ctx context.Context, store cas.Reader, cipher *CheckpointEncryptor, directory, checkpoint string, object computercheckpoint.RuntimeObject, limit int64) (string, error) {
	descriptor := cas.Descriptor{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}
	if err := cas.ValidateDescriptor(descriptor); err != nil {
		return "", invalidCheckpoint(err)
	}
	bound, err := cipher.EncryptedSize(limit)
	if err != nil {
		return "", err
	}
	if object.SizeBytes > bound {
		return "", invalidCheckpoint(errors.New("checkpoint ciphertext exceeds its staging bound"))
	}
	observed, err := store.Stat(ctx, object.Digest)
	if err != nil {
		return "", checkpointObjectReadError(err)
	}
	if err := cas.RequireExact(observed, descriptor); err != nil {
		return "", invalidCheckpoint(err)
	}
	plaintext, err := os.CreateTemp(directory, object.Role+"-plain-")
	if err != nil {
		return "", err
	}
	body, err := store.Get(ctx, object.Digest)
	if err != nil {
		return "", errors.Join(checkpointObjectReadError(err), plaintext.Close())
	}
	input := &checkpointDownloadReader{Reader: io.LimitReader(body, object.SizeBytes+1)}
	bounded := &checkpointBoundedWriter{writer: plaintext, remaining: limit}
	decryptErr := cipher.Decrypt(ctx, input, bounded, checkpoint+"/"+object.Role)
	// Finish verification even when a header or chunk is rejected early. An
	// unverified key id must not hide a corrupt or interrupted object stream.
	_, drainErr := io.Copy(io.Discard, input)
	streamErr := errors.Join(input.err, drainErr, body.Close(), ctx.Err())
	closeErr := plaintext.Close()
	if streamErr != nil {
		return "", errors.Join(checkpointObjectReadError(streamErr), closeErr)
	}
	if input.n != object.SizeBytes {
		return "", errors.Join(invalidCheckpoint(errors.New("checkpoint download size differs from manifest")), closeErr)
	}
	if err := errors.Join(decryptErr, closeErr); err != nil {
		return "", fmt.Errorf("read checkpoint %s: %w", object.Role, err)
	}

	return plaintext.Name(), nil
}

// Preserve read errors that decryption may wrap while it authenticates records.
type checkpointDownloadReader struct {
	io.Reader
	n   int64
	err error
}

func (r *checkpointDownloadReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.n += int64(n)
	if err != nil && err != io.EOF && r.err == nil {
		r.err = err
	}
	return n, err
}

type checkpointBoundedWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *checkpointBoundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, invalidCheckpoint(errors.New("checkpoint plaintext exceeds its staging bound"))
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}

// Only a verified content mismatch proves this object unusable. Missing
// payloads, transport and access errors retain the checkpoint for repair.
func checkpointObjectReadError(err error) error {
	if errors.Is(err, cas.ErrDigestMismatch) {
		return invalidCheckpoint(err)
	}
	return err
}

func encryptCheckpointMemory(ctx context.Context, cipher *CheckpointEncryptor, directory, purpose string, plaintext io.Reader, size int64) (computercheckpoint.Object, string, string, error) {
	file, err := os.CreateTemp(directory, "memory-encrypted-")
	if err != nil {
		return computercheckpoint.Object{}, "", "", err
	}
	hash := sha256.New()
	header, err := cipher.encryptMemory(ctx, plaintext, io.MultiWriter(file, hash), purpose, size)
	if err == nil {
		err = file.Chmod(0400)
	}
	info, statErr := file.Stat()
	err = errors.Join(err, statErr, file.Close())
	if err != nil {
		return computercheckpoint.Object{}, file.Name(), "", err
	}
	return computercheckpoint.Object{Digest: sha256sum.FormatDigest(hash.Sum(nil)), SizeBytes: info.Size(), MediaType: cas.CheckpointMemoryMediaType}, file.Name(), header, nil
}
