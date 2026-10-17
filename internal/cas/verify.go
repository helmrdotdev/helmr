package cas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
)

// VerifyStored checks remote bytes, using a provider's stored full-object
// checksum when available. A metadata-only Stat never certifies content.
func VerifyStored(ctx context.Context, reader Reader, digest string) (Object, error) {
	if verified, ok := reader.(interface {
		Verify(context.Context, string) (Object, error)
	}); ok {
		return verified.Verify(ctx, digest)
	}
	object, err := reader.Stat(ctx, digest)
	if err != nil {
		return Object{}, err
	}
	if err = VerifyReadback(ctx, reader, object); err != nil {
		return Object{}, err
	}
	return object, nil
}

// VerifyReadback is also used by off-path audits and by stores without a usable
// full-object checksum. Limit the response to the advertised length plus one.
func VerifyReadback(ctx context.Context, reader Reader, object Object) error {
	if object.SizeBytes < 0 {
		return errors.New("negative stored object size")
	}
	body, err := reader.Get(ctx, object.Digest)
	if err != nil {
		return err
	}
	if body == nil {
		return errors.New("missing stored object body")
	}
	hash := sha256.New()
	n, readErr := io.Copy(hash, io.LimitReader(body, object.SizeBytes+1))
	if err = errors.Join(readErr, body.Close(), ctx.Err()); err != nil {
		return err
	}
	if n != object.SizeBytes || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != object.Digest {
		return ErrDigestMismatch
	}
	return nil
}
