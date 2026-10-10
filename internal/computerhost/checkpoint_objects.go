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
		return computercheckpoint.Object{}, "", errors.New("checkpoint encryption requires a size bound")
	}
	file, err := os.CreateTemp(directory, role+"-encrypted-")
	if err != nil {
		return computercheckpoint.Object{}, "", err
	}
	hash := sha256.New()
	input := &io.LimitedReader{R: plaintext, N: limit + 1}
	err = cipher.Encrypt(ctx, input, io.MultiWriter(file, hash), checkpoint+"/"+role)
	if err == nil && input.N == 0 {
		err = errors.New("checkpoint plaintext exceeds its staging bound")
	}
	if err == nil {
		err = file.Chmod(0400)
	}
	if err == nil {
		err = file.Sync()
	}
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
	ciphertext, err := os.CreateTemp(directory, object.Role+"-download-")
	if err != nil {
		return "", err
	}
	defer ciphertext.Close()
	body, err := store.Get(ctx, object.Digest)
	if err != nil {
		return "", checkpointObjectReadError(err)
	}
	n, copyErr := io.Copy(ciphertext, io.LimitReader(body, object.SizeBytes+1))
	err = errors.Join(copyErr, body.Close())
	if err != nil {
		if errors.Is(err, cas.ErrDigestMismatch) {
			return "", invalidCheckpoint(err)
		}
		return "", err
	}
	if n != object.SizeBytes {
		return "", invalidCheckpoint(errors.New("checkpoint download size differs from manifest"))
	}
	if err := cas.VerifyDescriptorFile(ctx, descriptor, ciphertext); err != nil {
		if errors.Is(err, cas.ErrDigestMismatch) {
			return "", invalidCheckpoint(err)
		}
		return "", err
	}
	plaintext, err := os.CreateTemp(directory, object.Role+"-plain-")
	if err != nil {
		return "", err
	}
	bounded := &checkpointBoundedWriter{writer: plaintext, remaining: limit}
	err = cipher.Decrypt(ctx, io.NewSectionReader(ciphertext, 0, object.SizeBytes), bounded, checkpoint+"/"+object.Role)
	err = errors.Join(err, plaintext.Close())
	if err != nil {
		return "", fmt.Errorf("read checkpoint %s: %w", object.Role, err)
	}
	if err := ciphertext.Close(); err != nil {
		return "", err
	}
	if err := os.Remove(ciphertext.Name()); err != nil {
		return "", err
	}
	return plaintext.Name(), nil
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

// Absence and content mismatch are definitive immutable-object observations.
// Transport, local filesystem and permission errors do not prove state loss.
func checkpointObjectReadError(err error) error {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, cas.ErrDigestMismatch) {
		return invalidCheckpoint(err)
	}
	return err
}
