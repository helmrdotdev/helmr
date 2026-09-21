package s3

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// ReclaimVersions removes one bounded page of versions for a permanently retired
// digest. The caller must first commit retirement, prevent all future adoption,
// and protect existing references. This method does not decide retirement.
//
// Success is not a quiescence or complete-reclamation receipt: concurrent uploads
// may arrive later. The owner must retain its retirement record and reconcile
// again, including unfinished multipart uploads. Starting each pass at the first
// page avoids cursors into deleted versions; other keys matching the prefix are
// never removed. Only version-specific deletes are issued, not delete markers.
func (c *Store) ReclaimVersions(ctx context.Context, digest string) error {
	key, err := c.objectKey(digest)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	page, err := c.client.ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{
		Bucket: aws.String(c.bucket), Prefix: aws.String(key), MaxKeys: aws.Int32(100),
	})
	if err != nil {
		return fmt.Errorf("list retired object versions: %w", err)
	}
	if page == nil {
		return errors.New("missing retired object versions response")
	}
	remove := func(objectKey, versionID *string) error {
		if aws.ToString(objectKey) != key {
			return nil
		}
		if aws.ToString(versionID) == "" {
			return errors.New("retired object version has no version ID")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := c.client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key), VersionId: versionID})
		if err != nil {
			return fmt.Errorf("delete retired object version: %w", err)
		}
		return nil
	}
	for _, v := range page.Versions {
		if err := remove(v.Key, v.VersionId); err != nil {
			return err
		}
	}
	for _, v := range page.DeleteMarkers {
		if err := remove(v.Key, v.VersionId); err != nil {
			return err
		}
	}
	return nil
}

// RetiredUploads discovers one bounded page of upload IDs for an exact key.
// The owner persists these IDs before calling ReclaimUpload, and keeps them even
// after an empty ListParts response; no response fences outstanding part writes.
func (c *Store) RetiredUploads(ctx context.Context, digest string) ([]string, error) {
	key, err := c.objectKey(digest)
	if err != nil {
		return nil, err
	}
	page, err := c.client.ListMultipartUploads(ctx, &awss3.ListMultipartUploadsInput{
		Bucket: aws.String(c.bucket), Prefix: aws.String(key), MaxUploads: aws.Int32(100),
	})
	if err != nil {
		return nil, fmt.Errorf("list retired multipart uploads: %w", err)
	}
	if page == nil {
		return nil, errors.New("missing retired multipart uploads response")
	}
	var ids []string
	for _, upload := range page.Uploads {
		if aws.ToString(upload.Key) != key {
			continue
		}
		if aws.ToString(upload.UploadId) == "" {
			return nil, errors.New("retired multipart upload has no ID")
		}
		ids = append(ids, *upload.UploadId)
	}
	return ids, nil
}

// ReclaimUpload retries aborting a retained upload ID and checks for remaining
// parts. Even success is provisional: the caller must retain and revisit the ID.
func (c *Store) ReclaimUpload(ctx context.Context, digest, uploadID string) error {
	key, err := c.objectKey(digest)
	if err != nil {
		return err
	}
	if uploadID == "" {
		return errors.New("missing retired multipart upload ID")
	}
	_, err = c.client.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{
		Bucket: aws.String(c.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
	})
	if err != nil && !noSuchUpload(err) {
		return fmt.Errorf("abort retired multipart upload: %w", err)
	}
	parts, err := c.client.ListParts(ctx, &awss3.ListPartsInput{
		Bucket: aws.String(c.bucket), Key: aws.String(key), UploadId: aws.String(uploadID), MaxParts: aws.Int32(1),
	})
	if noSuchUpload(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check retired multipart parts: %w", err)
	}
	if parts == nil {
		return errors.New("missing retired multipart parts response")
	}
	if len(parts.Parts) != 0 || aws.ToBool(parts.IsTruncated) {
		return errors.New("retired multipart upload still has parts")
	}
	return nil
}

func noSuchUpload(err error) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchUpload"
}
