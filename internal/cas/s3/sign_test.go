package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func signingStore(client s3Client) *Store {
	signer := awss3.New(awss3.Options{
		Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		BaseEndpoint: aws.String("https://storage.example"), UsePathStyle: true,
	})
	return &Store{client: client, presign: awss3.NewPresignClient(signer), bucket: "bucket", prefix: "cas"}
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
	if request.Method != "PUT" || !strings.Contains(request.URL, "/bucket/cas/sha256/"+strings.TrimPrefix(descriptor.Digest, "sha256:")+"?") ||
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
}

func (c *completionClient) CompleteMultipartUpload(_ context.Context, input *awss3.CompleteMultipartUploadInput, _ ...func(*awss3.Options)) (*awss3.CompleteMultipartUploadOutput, error) {
	c.completed = aws.ToString(input.Key)
	return &awss3.CompleteMultipartUploadOutput{}, nil
}

func (c *completionClient) GetObject(_ context.Context, input *awss3.GetObjectInput, _ ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	return &awss3.GetObjectOutput{ETag: aws.String(`"staged"`), VersionId: aws.String("v1"), Body: io.NopCloser(bytes.NewReader(c.staged)), ContentLength: aws.Int64(int64(len(c.staged))), ContentType: aws.String(cas.CheckpointMemoryMediaType)}, nil
}

func (c *completionClient) CopyObject(_ context.Context, input *awss3.CopyObjectInput, _ ...func(*awss3.Options)) (*awss3.CopyObjectOutput, error) {
	if aws.ToString(input.CopySourceIfMatch) != `"staged"` || !strings.HasSuffix(aws.ToString(input.CopySource), "?versionId=v1") {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "copied a version other than the verified one"}
	}
	if c.exists || aws.ToString(input.IfNoneMatch) != "*" {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed"}
	}
	c.copied = aws.ToString(input.Key)
	return &awss3.CopyObjectOutput{}, nil
}

func (c *completionClient) HeadObject(context.Context, *awss3.HeadObjectInput, ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error) {
	checksum, _ := descriptorChecksum(sha256sum.DigestBytes(c.staged))
	return &awss3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(c.staged))), ContentType: aws.String(cas.CheckpointMemoryMediaType), ChecksumSHA256: aws.String(checksum)}, nil
}

func (c *completionClient) DeleteObject(_ context.Context, input *awss3.DeleteObjectInput, _ ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error) {
	c.deleted = append(c.deleted, aws.ToString(input.Key))
	return &awss3.DeleteObjectOutput{}, nil
}

// Parts come from a holder outside the digest namespace's trust. They are
// assembled under the owner's staging key, and only bytes that authenticate as
// the digest are copied create-only to the digest key; staging is removed.
func TestCompleteUploadPublishesOnlyVerifiedStagedBytes(t *testing.T) {
	want := bytes.Repeat([]byte{7}, 96)
	descriptor := cas.Descriptor{Digest: sha256sum.DigestBytes(want), SizeBytes: int64(len(want)), MediaType: cas.CheckpointMemoryMediaType}
	upload := cas.Upload{ID: "upload-1", Owner: "019abcde-1234", PartSize: 64, Parts: 2}
	parts := []cas.CompletedPart{{Number: 1, ETag: "a"}, {Number: 2, ETag: "b"}}
	hash := strings.TrimPrefix(descriptor.Digest, "sha256:")
	staging, final := "cas/staging/019abcde-1234/sha256/"+hash, "cas/sha256/"+hash
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
			store := &Store{client: client, bucket: "bucket", prefix: "cas", multipartThresholdBytes: 64, multipartPartSizeBytes: 64}
			object, err := store.CompleteUpload(t.Context(), descriptor, upload, test.parts)
			if test.valid {
				if err != nil || object.Digest != descriptor.Digest || client.completed != staging || (client.copied != final) == !test.exists || !slices.Contains(client.deleted, staging) {
					t.Fatalf("complete = %+v, %v; completed %q copied %q deleted %v", object, err, client.completed, client.copied, client.deleted)
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
