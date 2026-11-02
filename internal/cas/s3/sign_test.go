package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func signingStore(client s3Client) *Store {
	signer := awss3.New(awss3.Options{
		Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		BaseEndpoint: aws.String("https://storage.example"), UsePathStyle: true,
	})
	return &Store{org: testOrg, client: client, presign: awss3.NewPresignClient(signer), bucket: "bucket", prefix: "cas"}
}

func signedHeaders(t *testing.T, request cas.SignedRequest) []string {
	t.Helper()
	parsed, err := url.Parse(request.URL)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(parsed.Query().Get("X-Amz-SignedHeaders"), ";")
}

// A write grant names the digest key and binds the content checksum and the
// create-only precondition into the signature, so its holder cannot choose
// another key, replace an object or store bytes other than the digest's.
func TestSignPutBindsDigestChecksumAndCreateOnly(t *testing.T) {
	body := []byte("encrypted checkpoint object")
	descriptor := cas.Descriptor{Digest: sha256sum.DigestBytes(body), SizeBytes: int64(len(body)), MediaType: cas.CheckpointVMStateMediaType}
	store := signingStore(&fakeS3Client{})
	expires := time.Now().Add(2 * time.Minute)
	request, err := store.SignPut(t.Context(), descriptor, expires)
	if err != nil {
		t.Fatal(err)
	}
	checksum, _ := descriptorChecksum(descriptor.Digest)
	if request.Method != "PUT" || !strings.Contains(request.URL, "/bucket/cas/orgs/0198c0de-0000-7000-8000-000000000001/sha256/"+strings.TrimPrefix(descriptor.Digest, "sha256:")+"?") ||
		request.Header.Get("X-Amz-Checksum-Sha256") != checksum || request.Header.Get("If-None-Match") != "*" ||
		request.Header.Get("Content-Type") != descriptor.MediaType || !request.ExpiresAt.Equal(expires) {
		t.Fatalf("signed put = %+v", request)
	}
	signed := strings.Join(signedHeaders(t, request), ";")
	for _, header := range []string{"if-none-match", "x-amz-checksum-sha256", "content-type", "host"} {
		if !strings.Contains(";"+signed+";", ";"+header+";") {
			t.Fatalf("signed headers %q omit %s", signed, header)
		}
	}
	if strings.Contains(request.URL, "X-Amz-Checksum") {
		t.Fatalf("checksum was hoisted into the query: %s", request.URL)
	}
	if _, err := store.SignPut(t.Context(), cas.Descriptor{Digest: descriptor.Digest, SizeBytes: MultipartThresholdBytes, MediaType: descriptor.MediaType}, expires); err == nil {
		t.Fatal("single write signed above the multipart threshold")
	}
	if _, err := store.SignPut(t.Context(), descriptor, time.Now().Add(-time.Second)); err == nil {
		t.Fatal("write signed after its expiry")
	}
}

// Reads sign no Range, so one grant serves bounded ranges of one object only.
func TestSignGetAndPartAreObjectBound(t *testing.T) {
	store := signingStore(&fakeS3Client{})
	digest := sha256sum.DigestBytes([]byte("object"))
	expires := time.Now().Add(time.Minute)
	get, err := store.SignGet(t.Context(), digest, expires)
	if err != nil {
		t.Fatal(err)
	}
	if get.Method != "GET" || strings.Join(signedHeaders(t, get), ";") != "host" || !strings.Contains(get.URL, strings.TrimPrefix(digest, "sha256:")) {
		t.Fatalf("signed get = %+v", get)
	}
	descriptor := cas.Descriptor{Digest: digest, SizeBytes: 2*MultipartThresholdBytes + 7, MediaType: cas.CheckpointMemoryMediaType}
	upload := cas.Upload{ID: "upload-1", Owner: "019abcde-1234", PartSize: MultipartThresholdBytes, Parts: 3}
	part, err := store.SignPart(t.Context(), descriptor, upload, 3, expires)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(part.URL)
	if part.Method != "PUT" || parsed.Query().Get("partNumber") != "3" || parsed.Query().Get("uploadId") != "upload-1" {
		t.Fatalf("signed part = %+v", part)
	}
	for _, invalid := range []int32{0, 4} {
		if _, err := store.SignPart(t.Context(), descriptor, upload, invalid, expires); err == nil {
			t.Fatalf("part %d signed outside the upload", invalid)
		}
	}
	if _, err := store.SignPart(t.Context(), descriptor, cas.Upload{ID: "upload-1", Owner: "019abcde-1234", PartSize: MultipartThresholdBytes, Parts: 2}, 1, expires); err == nil {
		t.Fatal("part signed for an upload that does not cover the descriptor")
	}
}

type completionClient struct {
	fakeS3Client
	staged    []byte
	completed string
	copied    string
	deleted   []string
	exists    bool
	// digestHeads counts metadata reads of the digest key.
	digestHeads int
	// unversioned storage returns no version IDs; suspended returns "null".
	unversioned bool
	suspended   bool
	// existingMedia is the media type of an object already at the digest key.
	existingMedia string
	mu            sync.Mutex
	ranges        []string
	pinnedReads   int
	// Read back: inverted serves later ranges sooner, failRangeAt fails the
	// range at that nonzero offset, and peakReads is the most reads at once.
	inverted    bool
	failRangeAt int64
	activeReads int
	peakReads   int
	// Promotion: failPart fails that part copy; promotedParts are the part
	// numbers completed, in order; aborted reports an aborted promotion.
	failPart      int32
	promotedParts []int32
	aborted       bool
}

func (c *completionClient) version(id string) *string {
	if c.unversioned {
		return nil
	}
	if c.suspended {
		return aws.String("null")
	}
	return aws.String(id)
}

func (c *completionClient) CompleteMultipartUpload(_ context.Context, input *awss3.CompleteMultipartUploadInput, _ ...func(*awss3.Options)) (*awss3.CompleteMultipartUploadOutput, error) {
	if aws.ToString(input.UploadId) == "promotion" {
		if c.exists || aws.ToString(input.IfNoneMatch) != "*" {
			return nil, &smithy.GenericAPIError{Code: "PreconditionFailed"}
		}
		for _, part := range input.MultipartUpload.Parts {
			c.promotedParts = append(c.promotedParts, aws.ToInt32(part.PartNumber))
		}
		c.copied = aws.ToString(input.Key)
		return &awss3.CompleteMultipartUploadOutput{}, nil
	}
	c.completed = aws.ToString(input.Key)
	return &awss3.CompleteMultipartUploadOutput{}, nil
}

func (c *completionClient) CreateMultipartUpload(context.Context, *awss3.CreateMultipartUploadInput, ...func(*awss3.Options)) (*awss3.CreateMultipartUploadOutput, error) {
	return &awss3.CreateMultipartUploadOutput{UploadId: aws.String("promotion")}, nil
}

func (c *completionClient) UploadPartCopy(_ context.Context, input *awss3.UploadPartCopyInput, _ ...func(*awss3.Options)) (*awss3.UploadPartCopyOutput, error) {
	if !c.pinned(aws.ToString(input.CopySourceIfMatch), aws.ToString(input.CopySource)) {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "copied a version other than the verified one"}
	}
	if aws.ToInt32(input.PartNumber) == c.failPart {
		return nil, &smithy.GenericAPIError{Code: "InternalError"}
	}
	return &awss3.UploadPartCopyOutput{CopyPartResult: &types.CopyPartResult{ETag: aws.String(fmt.Sprintf("part-%d", aws.ToInt32(input.PartNumber)))}}, nil
}

func (c *completionClient) AbortMultipartUpload(context.Context, *awss3.AbortMultipartUploadInput, ...func(*awss3.Options)) (*awss3.AbortMultipartUploadOutput, error) {
	c.aborted = true
	return &awss3.AbortMultipartUploadOutput{}, nil
}

// pinned holds when a copy names the version that was read back.
func (c *completionClient) pinned(etag, source string) bool {
	return etag == `"staged"` && strings.HasSuffix(source, "?versionId=v1")
}

// GetObject serves ranges of the staged object, which is version v1. A read
// that names a version must also name its entity tag.
func (c *completionClient) GetObject(_ context.Context, input *awss3.GetObjectInput, _ ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	c.mu.Lock()
	c.ranges = append(c.ranges, aws.ToString(input.Range))
	if input.VersionId != nil {
		c.pinnedReads++
	}
	c.activeReads++
	c.peakReads = max(c.peakReads, c.activeReads)
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.activeReads--
		c.mu.Unlock()
	}()
	if input.VersionId != nil && (aws.ToString(input.VersionId) != "v1" || aws.ToString(input.IfMatch) != `"staged"`) {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "read a version other than the first one read"}
	}
	var start, end int64
	if _, err := fmt.Sscanf(aws.ToString(input.Range), "bytes=%d-%d", &start, &end); err != nil || start > end || start >= int64(len(c.staged)) {
		return nil, fmt.Errorf("unexpected range %q", aws.ToString(input.Range))
	}
	end = min(end, int64(len(c.staged))-1)
	if c.failRangeAt != 0 && start == c.failRangeAt {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "the staged version was replaced"}
	}
	if c.inverted {
		time.Sleep(time.Duration(int64(len(c.staged))-start) * 20 * time.Microsecond)
	}
	return &awss3.GetObjectOutput{
		ETag: aws.String(`"staged"`), VersionId: c.version("v1"), Body: io.NopCloser(bytes.NewReader(c.staged[start : end+1])),
		ContentLength: aws.Int64(end - start + 1), ContentRange: aws.String(fmt.Sprintf("bytes %d-%d/%d", start, end, len(c.staged))),
		ContentType: aws.String(cas.CheckpointMemoryMediaType),
	}, nil
}

func (c *completionClient) HeadObject(_ context.Context, input *awss3.HeadObjectInput, _ ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error) {
	if strings.Contains(aws.ToString(input.Key), "/staging/") {
		return nil, errors.New("metadata read of the unpinned staging key")
	}
	c.digestHeads++
	media := cas.CheckpointMemoryMediaType
	if c.existingMedia != "" {
		media = c.existingMedia
	}
	return &awss3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(c.staged))), ContentType: aws.String(media)}, nil
}

func (c *completionClient) DeleteObject(_ context.Context, input *awss3.DeleteObjectInput, _ ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error) {
	c.deleted = append(c.deleted, aws.ToString(input.Key))
	return &awss3.DeleteObjectOutput{}, nil
}

// Parts come from a holder outside the digest namespace's trust. They are
// assembled under the owner's staging key, read back in version-pinned ranges,
// and only bytes that authenticate as the digest are copied create-only to the
// digest key; staging is removed.
func TestCompleteUploadPublishesOnlyVerifiedStagedBytes(t *testing.T) {
	want := make([]byte, 96)
	for i := range want {
		want[i] = byte(i)
	}
	descriptor := cas.Descriptor{Digest: sha256sum.DigestBytes(want), SizeBytes: int64(len(want)), MediaType: cas.CheckpointMemoryMediaType}
	upload := cas.Upload{ID: "upload-1", Owner: "019abcde-1234", PartSize: 64, Parts: 2}
	parts := []cas.CompletedPart{{Number: 1, ETag: "a"}, {Number: 2, ETag: "b"}}
	hash := strings.TrimPrefix(descriptor.Digest, "sha256:")
	staging, final := "cas/staging/019abcde-1234/sha256/"+hash, "cas/orgs/0198c0de-0000-7000-8000-000000000001/sha256/"+hash
	for _, test := range []struct {
		name   string
		staged []byte
		parts  []cas.CompletedPart
		exists bool
		valid  bool
	}{
		{"matching", want, parts, false, true},
		{"already published", want, parts, true, true},
		{"poisoned", bytes.Repeat([]byte{8}, 96), parts, false, false},
		{"reordered", want, []cas.CompletedPart{parts[1], parts[0]}, false, false},
		{"partial", want, parts[:1], false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &completionClient{staged: test.staged, exists: test.exists}
			store := &Store{org: testOrg, client: client, bucket: "bucket", prefix: "cas", multipartThresholdBytes: 64, multipartPartSizeBytes: 64, readBackRangeBytes: 16}
			object, err := store.CompleteUpload(t.Context(), descriptor, upload, test.parts)
			if test.valid {
				if err != nil || object.Digest != descriptor.Digest || object.Key != final || client.completed != staging || (client.copied != final) == !test.exists || !slices.Contains(client.deleted, staging) || len(client.ranges) != 6 || client.pinnedReads != 5 || client.digestHeads != btoi(test.exists) {
					t.Fatalf("complete = %+v, %v; completed %q copied %q deleted %v ranges %d", object, err, client.completed, client.copied, client.deleted, len(client.ranges))
				}
				return
			}
			if err == nil || client.copied != "" || slices.Contains(client.deleted, final) {
				t.Fatalf("invalid completion = %+v, %v; copied %q deleted %v", object, err, client.copied, client.deleted)
			}
			if test.name == "poisoned" && (!errors.Is(err, cas.ErrRemoteIntegrity) || !slices.Contains(client.deleted, staging)) {
				t.Fatalf("poisoned completion = %v, deleted %v", err, client.deleted)
			}
		})
	}
}

// An entity tag alone does not pin the verified bytes, so completion in
// storage that returns no version, or the replaceable "null" version of a
// bucket with versioning suspended, publishes nothing.
func TestCompleteUploadRequiresVersionedStorage(t *testing.T) {
	staged := bytes.Repeat([]byte{7}, 96)
	descriptor := cas.Descriptor{Digest: sha256sum.DigestBytes(staged), SizeBytes: int64(len(staged)), MediaType: cas.CheckpointMemoryMediaType}
	upload := cas.Upload{ID: "upload-1", Owner: "019abcde-1234", PartSize: 64, Parts: 2}
	for _, client := range []*completionClient{{staged: staged, unversioned: true}, {staged: staged, suspended: true}} {
		store := &Store{org: testOrg, client: client, bucket: "bucket", prefix: "cas", multipartThresholdBytes: 64, multipartPartSizeBytes: 64}
		if _, err := store.CompleteUpload(t.Context(), descriptor, upload, []cas.CompletedPart{{Number: 1, ETag: "a"}, {Number: 2, ETag: "b"}}); err == nil || client.copied != "" {
			t.Fatalf("suspended %t: completion = %v, copied %q", client.suspended, err, client.copied)
		}
	}
}

// Losing a create-only race to an object of the same digest completes the
// upload only when the stored object carries the descriptor's metadata.
func TestCompleteUploadRefusesExistingObjectOfOtherMetadata(t *testing.T) {
	staged := bytes.Repeat([]byte{7}, 96)
	descriptor := cas.Descriptor{Digest: sha256sum.DigestBytes(staged), SizeBytes: int64(len(staged)), MediaType: cas.CheckpointMemoryMediaType}
	upload := cas.Upload{ID: "upload-1", Owner: "019abcde-1234", PartSize: 64, Parts: 2}
	client := &completionClient{staged: staged, exists: true, existingMedia: "application/octet-stream"}
	store := &Store{org: testOrg, client: client, bucket: "bucket", prefix: "cas", multipartThresholdBytes: 64, multipartPartSizeBytes: 64}
	if _, err := store.CompleteUpload(t.Context(), descriptor, upload, []cas.CompletedPart{{Number: 1, ETag: "a"}, {Number: 2, ETag: "b"}}); !errors.Is(err, cas.ErrRemoteIntegrity) {
		t.Fatalf("completion over other metadata = %v", err)
	}
}

// A read back hashes version-pinned ranges in object order however they
// arrive, keeps at most eight ranges outstanding, and publishes nothing when a
// range of the fixed version can no longer be read.
func TestCompleteUploadReadBackOrdersAndBoundsRanges(t *testing.T) {
	staged := make([]byte, 96)
	for i := range staged {
		staged[i] = byte(i * 7)
	}
	descriptor := cas.Descriptor{Digest: sha256sum.DigestBytes(staged), SizeBytes: int64(len(staged)), MediaType: cas.CheckpointMemoryMediaType}
	upload := cas.Upload{ID: "upload-1", Owner: "019abcde-1234", PartSize: 64, Parts: 2}
	parts := []cas.CompletedPart{{Number: 1, ETag: "a"}, {Number: 2, ETag: "b"}}
	for _, failRangeAt := range []int64{0, 40} {
		client := &completionClient{staged: staged, inverted: true, failRangeAt: failRangeAt}
		store := &Store{org: testOrg, client: client, bucket: "bucket", prefix: "cas", multipartThresholdBytes: 64, multipartPartSizeBytes: 64, readBackRangeBytes: 4}
		_, err := store.CompleteUpload(t.Context(), descriptor, upload, parts)
		if failRangeAt != 0 {
			if err == nil || client.copied != "" {
				t.Fatalf("completion after a failed range = %v, copied %q", err, client.copied)
			}
			continue
		}
		if err != nil || client.copied == "" || len(client.ranges) != 24 || client.pinnedReads != 23 || client.peakReads > s3ReadBackConcurrency || client.peakReads < 2 {
			t.Fatalf("completion = %v, copied %q; %d ranges, %d pinned, peak %d", err, client.copied, len(client.ranges), client.pinnedReads, client.peakReads)
		}
	}
}

// Promotion copies parts of the verified version concurrently, completes them
// in part order, and aborts without publishing when any part copy fails.
func TestCompleteUploadPromotesPartsInOrderOrAborts(t *testing.T) {
	staged := bytes.Repeat([]byte{7}, 96)
	descriptor := cas.Descriptor{Digest: sha256sum.DigestBytes(staged), SizeBytes: int64(len(staged)), MediaType: cas.CheckpointMemoryMediaType}
	upload := cas.Upload{ID: "upload-1", Owner: "019abcde-1234", PartSize: 64, Parts: 2}
	parts := []cas.CompletedPart{{Number: 1, ETag: "a"}, {Number: 2, ETag: "b"}}
	for _, failPart := range []int32{0, 2} {
		client := &completionClient{staged: staged, failPart: failPart}
		store := &Store{org: testOrg, client: client, bucket: "bucket", prefix: "cas", multipartThresholdBytes: 64, multipartPartSizeBytes: 64, copyPartBytes: 32}
		_, err := store.CompleteUpload(t.Context(), descriptor, upload, parts)
		if failPart != 0 {
			if err == nil || client.copied != "" || !client.aborted {
				t.Fatalf("completion after a failed part copy = %v, copied %q, aborted %t", err, client.copied, client.aborted)
			}
			continue
		}
		if err != nil || !slices.Equal(client.promotedParts, []int32{1, 2, 3}) || client.aborted {
			t.Fatalf("completion = %v, promoted parts %v, aborted %t", err, client.promotedParts, client.aborted)
		}
	}
}

// A promotion copy keeps its part size until the object would need more parts
// than multipart allows.
func TestPromotionPartBytesStaysWithinThePartLimit(t *testing.T) {
	for _, size := range []int64{64 << 20, 8 << 30, s3CopyPartBytes * s3MultipartMaxParts, s3CopyPartBytes*s3MultipartMaxParts + 1, 5 << 40} {
		part := promotionPartBytes(size, s3CopyPartBytes)
		if part < s3CopyPartBytes || (size+part-1)/part > s3MultipartMaxParts || (size <= s3CopyPartBytes*s3MultipartMaxParts && part != s3CopyPartBytes) {
			t.Fatalf("size %d: part %d", size, part)
		}
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// A read back waits for one of the process's read back slots and gives up
// with its context, publishing nothing.
func TestCompleteUploadWaitsForAReadBackSlot(t *testing.T) {
	for range s3ReadBackLimit {
		readBackSlots <- struct{}{}
	}
	defer func() {
		for range s3ReadBackLimit {
			<-readBackSlots
		}
	}()
	staged := bytes.Repeat([]byte{7}, 96)
	descriptor := cas.Descriptor{Digest: sha256sum.DigestBytes(staged), SizeBytes: int64(len(staged)), MediaType: cas.CheckpointMemoryMediaType}
	client := &completionClient{staged: staged}
	store := &Store{org: testOrg, client: client, bucket: "bucket", prefix: "cas", multipartThresholdBytes: 64, multipartPartSizeBytes: 64}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	upload := cas.Upload{ID: "upload-1", Owner: "019abcde-1234", PartSize: 64, Parts: 2}
	if _, err := store.CompleteUpload(ctx, descriptor, upload, []cas.CompletedPart{{Number: 1, ETag: "a"}, {Number: 2, ETag: "b"}}); err == nil || len(client.ranges) != 0 || client.copied != "" {
		t.Fatalf("completion without a read back slot = %v, %d ranges, copied %q", err, len(client.ranges), client.copied)
	}
}
