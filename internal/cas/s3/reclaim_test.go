package s3

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

type reclaimS3Client struct {
	fakeS3Client
	page                   *awss3.ListObjectVersionsOutput
	listed                 *awss3.ListObjectVersionsInput
	deleted                []*awss3.DeleteObjectInput
	listErr, errorOnDelete error
}

func (c *reclaimS3Client) ListObjectVersions(_ context.Context, in *awss3.ListObjectVersionsInput, _ ...func(*awss3.Options)) (*awss3.ListObjectVersionsOutput, error) {
	c.listed = in
	return c.page, c.listErr
}
func (c *reclaimS3Client) DeleteObject(_ context.Context, in *awss3.DeleteObjectInput, _ ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error) {
	c.deleted = append(c.deleted, in)
	return &awss3.DeleteObjectOutput{}, c.errorOnDelete
}

func TestReclaimVersionsOnlyDeletesExactKeyVersions(t *testing.T) {
	client := &reclaimS3Client{}
	store := &Store{client: client, bucket: "test", prefix: "cas"}
	digest := sha256sum.DigestBytes([]byte("retired ciphertext"))
	key, err := store.objectKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	client.page = &awss3.ListObjectVersionsOutput{IsTruncated: aws.Bool(true), Versions: []types.ObjectVersion{
		{Key: aws.String(key), VersionId: aws.String("version-1")},
		{Key: aws.String(key + "-other"), VersionId: aws.String("untouched")},
	}, DeleteMarkers: []types.DeleteMarkerEntry{{Key: aws.String(key), VersionId: aws.String("marker-1")}}}
	if err := store.ReclaimVersions(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	if len(client.deleted) != 2 || aws.ToString(client.deleted[0].VersionId) != "version-1" || aws.ToString(client.deleted[1].VersionId) != "marker-1" {
		t.Fatalf("wrong deletion targets: %+v", client.deleted)
	}
	if aws.ToString(client.listed.Prefix) != key || aws.ToInt32(client.listed.MaxKeys) != 100 || client.listed.KeyMarker != nil {
		t.Fatal("unbounded or wrong listing")
	}
	// A late completed PUT after a successful pass still belongs to the same
	// retirement owner and must be found by its next reconciliation.
	client.page = &awss3.ListObjectVersionsOutput{Versions: []types.ObjectVersion{{Key: aws.String(key), VersionId: aws.String("late-version")}}}
	if err := store.ReclaimVersions(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	if len(client.deleted) != 3 || aws.ToString(client.deleted[2].VersionId) != "late-version" {
		t.Fatal("late object was skipped")
	}
}

func TestReclaimVersionsRetainsFailureAndNeverDeletesCurrentKey(t *testing.T) {
	digest := sha256sum.DigestBytes([]byte("retired ciphertext"))
	for _, scenario := range []string{"list", "delete", "missing-version", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			client := &reclaimS3Client{}
			store := &Store{client: client, bucket: "test"}
			key, _ := store.objectKey(digest)
			version := aws.String("version")
			if scenario == "missing-version" {
				version = nil
			}
			client.page = &awss3.ListObjectVersionsOutput{Versions: []types.ObjectVersion{{Key: aws.String(key), VersionId: version}}}
			if scenario == "list" {
				client.listErr = errors.New("list failed")
			}
			if scenario == "delete" {
				client.errorOnDelete = errors.New("delete failed")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if scenario == "cancelled" {
				cancel()
			}
			if err := store.ReclaimVersions(ctx, digest); err == nil {
				t.Fatal("failure was hidden")
			}
			for _, in := range client.deleted {
				if in.VersionId == nil {
					t.Fatal("issued key-only delete")
				}
			}
		})
	}
}

type multipartReclaimClient struct {
	fakeS3Client
	uploads            *awss3.ListMultipartUploadsOutput
	parts              *awss3.ListPartsOutput
	abortErr, partsErr error
	aborted            *awss3.AbortMultipartUploadInput
	checked            *awss3.ListPartsInput
}

func (c *multipartReclaimClient) ListMultipartUploads(context.Context, *awss3.ListMultipartUploadsInput, ...func(*awss3.Options)) (*awss3.ListMultipartUploadsOutput, error) {
	return c.uploads, nil
}
func (c *multipartReclaimClient) AbortMultipartUpload(_ context.Context, in *awss3.AbortMultipartUploadInput, _ ...func(*awss3.Options)) (*awss3.AbortMultipartUploadOutput, error) {
	c.aborted = in
	return &awss3.AbortMultipartUploadOutput{}, c.abortErr
}
func (c *multipartReclaimClient) ListParts(_ context.Context, in *awss3.ListPartsInput, _ ...func(*awss3.Options)) (*awss3.ListPartsOutput, error) {
	c.checked = in
	return c.parts, c.partsErr
}

func TestReclaimMultipartKeepsExactIdentityAndReportsLateParts(t *testing.T) {
	c := &multipartReclaimClient{}
	store := &Store{client: c, bucket: "bucket", prefix: "cas"}
	digest := sha256sum.DigestBytes([]byte("retired multipart"))
	key, _ := store.objectKey(digest)
	c.uploads = &awss3.ListMultipartUploadsOutput{IsTruncated: aws.Bool(true), Uploads: []types.MultipartUpload{
		{Key: aws.String(key), UploadId: aws.String("owned")},
		{Key: aws.String(key + "-other"), UploadId: aws.String("foreign")},
	}}
	ids, err := store.RetiredUploads(t.Context(), digest)
	if err != nil || len(ids) != 1 || ids[0] != "owned" {
		t.Fatalf("wrong discovery: %v %v", ids, err)
	}
	c.parts = &awss3.ListPartsOutput{Parts: []types.Part{{PartNumber: aws.Int32(1)}}}
	if err := store.ReclaimUpload(t.Context(), digest, ids[0]); err == nil {
		t.Fatal("late parts treated as reclaimed")
	}
	if aws.ToString(c.aborted.Key) != key || aws.ToString(c.aborted.UploadId) != "owned" || aws.ToString(c.checked.UploadId) != "owned" {
		t.Fatal("wrong abort/check target")
	}
	c.parts = &awss3.ListPartsOutput{}
	if err := store.ReclaimUpload(t.Context(), digest, ids[0]); err != nil {
		t.Fatal(err)
	}
	c.abortErr = &types.NoSuchUpload{}
	c.partsErr = &types.NoSuchUpload{}
	if err := store.ReclaimUpload(t.Context(), digest, ids[0]); err != nil {
		t.Fatal(err)
	}
	c.abortErr = errors.New("access denied")
	if err := store.ReclaimUpload(t.Context(), digest, ids[0]); err == nil {
		t.Fatal("abort failure hidden")
	}
	c.abortErr = nil
	c.partsErr = errors.New("parts unavailable")
	if err := store.ReclaimUpload(t.Context(), digest, ids[0]); err == nil {
		t.Fatal("parts failure hidden")
	}
}

type versionPages struct {
	fakeS3Client
	key       string
	versions  []types.ObjectVersion
	deletions int
}

func (c *versionPages) ListObjectVersions(_ context.Context, in *awss3.ListObjectVersionsInput, _ ...func(*awss3.Options)) (*awss3.ListObjectVersionsOutput, error) {
	n := min(len(c.versions), int(aws.ToInt32(in.MaxKeys)))
	// Return a copy because deletion mutates the backing inventory.
	page := append([]types.ObjectVersion(nil), c.versions[:n]...)
	return &awss3.ListObjectVersionsOutput{Versions: page, IsTruncated: aws.Bool(n < len(c.versions))}, nil
}
func (c *versionPages) DeleteObject(_ context.Context, in *awss3.DeleteObjectInput, _ ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error) {
	if aws.ToString(in.Key) != c.key || in.VersionId == nil {
		return nil, errors.New("wrong version deletion")
	}
	for i, v := range c.versions {
		if aws.ToString(v.VersionId) == *in.VersionId {
			c.versions = append(c.versions[:i], c.versions[i+1:]...)
			c.deletions++
			break
		}
	}
	return &awss3.DeleteObjectOutput{}, nil
}
func TestReclaimVersionsMakesBoundedProgressAcrossPages(t *testing.T) {
	c := &versionPages{}
	store := &Store{client: c, bucket: "bucket"}
	digest := sha256sum.DigestBytes([]byte("many versions"))
	c.key, _ = store.objectKey(digest)
	for i := range 205 {
		c.versions = append(c.versions, types.ObjectVersion{Key: aws.String(c.key), VersionId: aws.String(fmt.Sprint(i))})
	}
	for _, remaining := range []int{105, 5, 0} {
		if err := store.ReclaimVersions(t.Context(), digest); err != nil {
			t.Fatal(err)
		}
		if len(c.versions) != remaining {
			t.Fatalf("remaining=%d want %d", len(c.versions), remaining)
		}
	}
	if c.deletions != 205 {
		t.Fatal("version omitted")
	}
}
