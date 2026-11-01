package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsv4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/helmrdotdev/helmr/internal/cas"
)

// MultipartThresholdBytes is the smallest descriptor published in parts.
const MultipartThresholdBytes = s3MultipartThresholdBytes

const (
	// s3CopyObjectMaxBytes is the largest single server-side copy.
	s3CopyObjectMaxBytes = 5 << 30
	s3CopyPartBytes      = 1 << 30
)

type presigner interface {
	PresignGetObject(context.Context, *awss3.GetObjectInput, ...func(*awss3.PresignOptions)) (*awsv4.PresignedHTTPRequest, error)
	PresignHeadObject(context.Context, *awss3.HeadObjectInput, ...func(*awss3.PresignOptions)) (*awsv4.PresignedHTTPRequest, error)
	PresignPutObject(context.Context, *awss3.PutObjectInput, ...func(*awss3.PresignOptions)) (*awsv4.PresignedHTTPRequest, error)
	PresignUploadPart(context.Context, *awss3.UploadPartInput, ...func(*awss3.PresignOptions)) (*awsv4.PresignedHTTPRequest, error)
}

func signed(request *awsv4.PresignedHTTPRequest, err error, expiresAt time.Time) (cas.SignedRequest, error) {
	if err != nil {
		return cas.SignedRequest{}, err
	}
	if request == nil || request.URL == "" || request.Method == "" {
		return cas.SignedRequest{}, errors.New("storage signer returned no request")
	}
	header := http.Header{}
	for name, values := range request.SignedHeader {
		// The transport derives Host and Content-Length from the request itself.
		if name := http.CanonicalHeaderKey(name); name != "Host" && name != "Content-Length" {
			header[name] = slices.Clone(values)
		}
	}
	return cas.SignedRequest{Method: request.Method, URL: request.URL, Header: header, ExpiresAt: expiresAt}, nil
}

func (c *Store) signer(expiresAt time.Time) (func(*awss3.PresignOptions), error) {
	if c.presign == nil {
		return nil, errors.New("storage signing is not configured")
	}
	ttl := time.Until(expiresAt).Truncate(time.Second)
	if ttl < time.Second {
		return nil, errors.New("storage grant expiry has passed")
	}
	return awss3.WithPresignExpires(ttl), nil
}

// SignGet authorizes complete and ranged reads of one immutable object. Range
// is not signed, so one grant serves every bounded range of the same object.
func (c *Store) SignGet(ctx context.Context, digest string, expiresAt time.Time) (cas.SignedRequest, error) {
	key, err := c.objectKey(digest)
	if err != nil {
		return cas.SignedRequest{}, err
	}
	expires, err := c.signer(expiresAt)
	if err != nil {
		return cas.SignedRequest{}, err
	}
	request, err := c.presign.PresignGetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)}, expires)
	return signed(request, err, expiresAt)
}

// SignHead authorizes metadata reads of one immutable object.
func (c *Store) SignHead(ctx context.Context, digest string, expiresAt time.Time) (cas.SignedRequest, error) {
	key, err := c.objectKey(digest)
	if err != nil {
		return cas.SignedRequest{}, err
	}
	expires, err := c.signer(expiresAt)
	if err != nil {
		return cas.SignedRequest{}, err
	}
	request, err := c.presign.PresignHeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)}, expires)
	return signed(request, err, expiresAt)
}

// SignPut authorizes one create-only write of exactly the descriptor's bytes:
// the key is the digest, storage rejects a body whose SHA-256 differs, and
// If-None-Match prevents replacing an existing object.
func (c *Store) SignPut(ctx context.Context, expected cas.Descriptor, expiresAt time.Time) (cas.SignedRequest, error) {
	if err := cas.ValidateDescriptor(expected); err != nil {
		return cas.SignedRequest{}, err
	}
	if expected.SizeBytes >= c.multipartThreshold() {
		return cas.SignedRequest{}, errors.New("object requires multipart publication")
	}
	key, err := c.objectKey(expected.Digest)
	if err != nil {
		return cas.SignedRequest{}, err
	}
	checksum, err := descriptorChecksum(expected.Digest)
	if err != nil {
		return cas.SignedRequest{}, err
	}
	expires, err := c.signer(expiresAt)
	if err != nil {
		return cas.SignedRequest{}, err
	}
	request, err := c.presign.PresignPutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(c.bucket), Key: aws.String(key),
		ChecksumSHA256: aws.String(checksum), ContentLength: aws.Int64(expected.SizeBytes),
		ContentType: aws.String(expected.MediaType), IfNoneMatch: aws.String("*"),
	}, expires)
	return signed(request, err, expiresAt)
}

// UploadLayout returns the deterministic part layout of a multipart
// publication, or false when the descriptor is published by one request.
func (c *Store) UploadLayout(expected cas.Descriptor) (cas.Upload, bool) {
	if expected.SizeBytes < c.multipartThreshold() {
		return cas.Upload{}, false
	}
	partSize := c.multipartPartSize(expected.SizeBytes)
	return cas.Upload{PartSize: partSize, Parts: int32((expected.SizeBytes + partSize - 1) / partSize)}, true
}

// stagingKey is the per-owner key that receives untrusted multipart parts.
// Only verified bytes are ever copied to the digest key.
func (c *Store) stagingKey(owner, digest string) (string, error) {
	key, err := c.quarantineKey(owner, digest)
	if err != nil {
		return "", err
	}
	return strings.Replace(key, "quarantine/", "staging/", 1), nil
}

// CreateUpload starts a multipart publication into owner's staging key; its
// parts are uploaded through SignPart grants. The storage owner completes or
// aborts it.
func (c *Store) CreateUpload(ctx context.Context, owner string, expected cas.Descriptor) (cas.Upload, error) {
	if err := cas.ValidateDescriptor(expected); err != nil {
		return cas.Upload{}, err
	}
	if expected.SizeBytes < c.multipartThreshold() {
		return cas.Upload{}, errors.New("object is below the multipart threshold")
	}
	key, err := c.stagingKey(owner, expected.Digest)
	if err != nil {
		return cas.Upload{}, err
	}
	created, err := c.client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
		Bucket: aws.String(c.bucket), Key: aws.String(key), ContentType: aws.String(expected.MediaType),
	})
	if err != nil {
		return cas.Upload{}, storageFailure(ctx, err)
	}
	if created == nil || aws.ToString(created.UploadId) == "" {
		return cas.Upload{}, errors.New("missing multipart upload identity")
	}
	upload, _ := c.UploadLayout(expected)
	upload.ID, upload.Owner = aws.ToString(created.UploadId), owner
	return upload, nil
}

// SignPart authorizes one staged part of an owned multipart publication with
// its exact length.
func (c *Store) SignPart(ctx context.Context, expected cas.Descriptor, upload cas.Upload, part int32, expiresAt time.Time) (cas.SignedRequest, error) {
	length, err := partLength(expected, upload, part)
	if err != nil {
		return cas.SignedRequest{}, err
	}
	key, err := c.stagingKey(upload.Owner, expected.Digest)
	if err != nil {
		return cas.SignedRequest{}, err
	}
	expires, err := c.signer(expiresAt)
	if err != nil {
		return cas.SignedRequest{}, err
	}
	request, err := c.presign.PresignUploadPart(ctx, &awss3.UploadPartInput{
		Bucket: aws.String(c.bucket), Key: aws.String(key), UploadId: aws.String(upload.ID),
		PartNumber: aws.Int32(part), ContentLength: aws.Int64(length),
	}, expires)
	return signed(request, err, expiresAt)
}

func partLength(expected cas.Descriptor, upload cas.Upload, part int32) (int64, error) {
	if err := cas.ValidateDescriptor(expected); err != nil {
		return 0, err
	}
	if upload.ID == "" || upload.PartSize <= 0 || upload.Parts <= 0 || upload.Parts > s3MultipartMaxParts ||
		int64(upload.Parts) != (expected.SizeBytes+upload.PartSize-1)/upload.PartSize || part < 1 || part > upload.Parts {
		return 0, errors.New("invalid multipart part")
	}
	offset := int64(part-1) * upload.PartSize
	return min(upload.PartSize, expected.SizeBytes-offset), nil
}

// CompleteUpload assembles the staged parts, authenticates the whole staged
// object, and only then copies it create-only to the digest key. Parts come
// from a holder that is not trusted with the digest namespace, so a mismatch,
// crash or cancellation never leaves unverified bytes under a digest key;
// staging expires on its own.
func (c *Store) CompleteUpload(ctx context.Context, expected cas.Descriptor, upload cas.Upload, parts []cas.CompletedPart) (cas.Object, error) {
	if _, err := partLength(expected, upload, 1); err != nil {
		return cas.Object{}, err
	}
	if len(parts) != int(upload.Parts) {
		return cas.Object{}, errors.New("multipart completion does not cover the object")
	}
	staging, err := c.stagingKey(upload.Owner, expected.Digest)
	if err != nil {
		return cas.Object{}, err
	}
	completed := make([]types.CompletedPart, len(parts))
	for i, part := range parts {
		if part.Number != int32(i+1) || part.ETag == "" {
			return cas.Object{}, errors.New("multipart completion parts are not ordered")
		}
		completed[i] = types.CompletedPart{PartNumber: aws.Int32(part.Number), ETag: aws.String(part.ETag)}
	}
	if _, err := c.client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket: aws.String(c.bucket), Key: aws.String(staging), UploadId: aws.String(upload.ID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	}); err != nil {
		return cas.Object{}, storageFailure(ctx, err)
	}
	defer c.removeStaged(ctx, staging)
	// Promotion copies only the exact staged version that was verified, even if
	// another completion of the same owner replaces the staging key meanwhile.
	etag, version, err := c.verifyStaged(ctx, staging, expected)
	if err != nil {
		return cas.Object{}, err
	}
	if err := c.promoteStaged(ctx, staging, etag, version, expected); err != nil {
		return cas.Object{}, err
	}
	return c.VerifyDescriptor(ctx, expected)
}

func (c *Store) removeStaged(ctx context.Context, staging string) {
	removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s3MultipartAbortTimeout)
	defer cancel()
	// The staging lifecycle rule removes a copy this call cannot.
	_, _ = c.client.DeleteObject(removeCtx, &awss3.DeleteObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(staging)})
}

// verifyStaged reads the staged object back, requires the descriptor's exact
// bytes, size and media type, and returns the entity tag and, in a versioned
// bucket, the version of what it read.
func (c *Store) verifyStaged(ctx context.Context, staging string, expected cas.Descriptor) (string, string, error) {
	output, err := c.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(staging)})
	if err != nil {
		return "", "", storageFailure(ctx, err)
	}
	if output == nil || output.Body == nil {
		return "", "", errors.New("missing staged object body")
	}
	etag := aws.ToString(output.ETag)
	if etag == "" || aws.ToInt64(output.ContentLength) != expected.SizeBytes || aws.ToString(output.ContentType) != expected.MediaType {
		_ = output.Body.Close()
		return "", "", fmt.Errorf("%w: staged object metadata differs from its descriptor", cas.ErrRemoteIntegrity)
	}
	hash := sha256.New()
	n, readErr := io.Copy(hash, io.LimitReader(output.Body, expected.SizeBytes+1))
	if err := errors.Join(readErr, output.Body.Close()); err != nil {
		return "", "", storageFailure(ctx, err)
	}
	if n != expected.SizeBytes || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != expected.Digest {
		return "", "", fmt.Errorf("%w: staged bytes differ from digest %s", cas.ErrRemoteIntegrity, expected.Digest)
	}
	return etag, aws.ToString(output.VersionId), nil
}

// promoteStaged copies verified staged bytes to the digest key create-only.
// Objects above one copy request are copied in server-side parts.
func (c *Store) promoteStaged(ctx context.Context, staging, etag, version string, expected cas.Descriptor) error {
	key, err := c.objectKey(expected.Digest)
	if err != nil {
		return err
	}
	source := url.PathEscape(c.bucket + "/" + staging)
	if version != "" {
		source += "?versionId=" + url.QueryEscape(version)
	}
	if expected.SizeBytes <= s3CopyObjectMaxBytes {
		_, err = c.client.CopyObject(ctx, &awss3.CopyObjectInput{
			Bucket: aws.String(c.bucket), Key: aws.String(key), CopySource: aws.String(source), CopySourceIfMatch: aws.String(etag),
			ChecksumAlgorithm: types.ChecksumAlgorithmSha256, ContentType: aws.String(expected.MediaType),
			IfNoneMatch: aws.String("*"), MetadataDirective: types.MetadataDirectiveReplace,
		})
		if conditionalWriteError(err) == conditionalWriteExists {
			return nil
		}
		return storageFailure(ctx, err)
	}
	created, err := c.client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
		Bucket: aws.String(c.bucket), Key: aws.String(key), ContentType: aws.String(expected.MediaType),
	})
	if err != nil {
		return storageFailure(ctx, err)
	}
	uploadID := aws.ToString(created.UploadId)
	var parts []types.CompletedPart
	for offset, number := int64(0), int32(1); offset < expected.SizeBytes; offset, number = offset+s3CopyPartBytes, number+1 {
		end := min(offset+s3CopyPartBytes, expected.SizeBytes) - 1
		copied, err := c.client.UploadPartCopy(ctx, &awss3.UploadPartCopyInput{
			Bucket: aws.String(c.bucket), Key: aws.String(key), UploadId: aws.String(uploadID), PartNumber: aws.Int32(number),
			CopySource: aws.String(source), CopySourceIfMatch: aws.String(etag), CopySourceRange: aws.String(fmt.Sprintf("bytes=%d-%d", offset, end)),
		})
		if err != nil || copied == nil || copied.CopyPartResult == nil {
			return errors.Join(storageFailure(ctx, err), c.abortMultipartUpload(ctx, key, uploadID))
		}
		parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(number), ETag: copied.CopyPartResult.ETag})
	}
	_, err = c.client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket: aws.String(c.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}, IfNoneMatch: aws.String("*"),
	})
	if err == nil {
		return nil
	}
	abortErr := c.abortMultipartUpload(ctx, key, uploadID)
	if conditionalWriteError(err) == conditionalWriteExists {
		return abortErr
	}
	return errors.Join(storageFailure(ctx, err), abortErr)
}

// AbortUpload releases an unfinished staged multipart publication.
func (c *Store) AbortUpload(ctx context.Context, upload cas.Upload, digest string) error {
	key, err := c.stagingKey(upload.Owner, digest)
	if err != nil {
		return err
	}
	if upload.ID == "" {
		return errors.New("multipart upload identity is required")
	}
	return c.abortMultipartUpload(ctx, key, upload.ID)
}

// VerifyDescriptor authenticates the stored object's bytes and metadata against
// a descriptor. ErrRemoteIntegrity marks stored bytes that are not the descriptor.
func (c *Store) VerifyDescriptor(ctx context.Context, expected cas.Descriptor) (cas.Object, error) {
	object, err := c.Verify(ctx, expected.Digest)
	if err != nil {
		if errors.Is(err, cas.ErrDigestMismatch) {
			return cas.Object{}, fmt.Errorf("%w: %w", cas.ErrRemoteIntegrity, err)
		}
		return cas.Object{}, storageFailure(ctx, err)
	}
	if object.SizeBytes != expected.SizeBytes || object.MediaType != expected.MediaType {
		return cas.Object{}, fmt.Errorf("%w: object %s metadata differs from its descriptor", cas.ErrRemoteIntegrity, expected.Digest)
	}
	return object, nil
}

func (c *ImmutableStore) SignGet(ctx context.Context, digest string, expiresAt time.Time) (cas.SignedRequest, error) {
	return c.store.SignGet(ctx, digest, expiresAt)
}

func (c *ImmutableStore) SignHead(ctx context.Context, digest string, expiresAt time.Time) (cas.SignedRequest, error) {
	return c.store.SignHead(ctx, digest, expiresAt)
}
