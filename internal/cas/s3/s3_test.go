package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsv4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestValidateDisjointS3Stores(t *testing.T) {
	for _, tt := range []struct {
		name   string
		first  string
		second string
		wantOK bool
	}{
		{name: "separate prefixes", first: "s3://bucket/cas", second: "s3://bucket/runtimes", wantOK: true},
		{name: "root and separate prefix", first: "s3://bucket", second: "s3://bucket/runtimes", wantOK: true},
		{name: "different buckets", first: "s3://first/cas", second: "s3://second/cas", wantOK: true},
		{name: "different endpoints", first: "s3://bucket/cas?endpoint=https://first.example", second: "s3://bucket/cas?endpoint=https://second.example", wantOK: true},
		{name: "equal", first: "s3://bucket/cas", second: "s3://bucket/cas"},
		{name: "first contains second", first: "s3://bucket/cas", second: "s3://bucket/cas/sha256/runtimes"},
		{name: "second contains first", first: "s3://bucket/cas/sha256/runtimes", second: "s3://bucket/cas"},
		{name: "root CAS contains nested namespace", first: "s3://bucket", second: "s3://bucket/sha256/runtimes"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDisjointS3Stores(tt.first, tt.second)
			if tt.wantOK && err != nil {
				t.Fatal(err)
			}
			if !tt.wantOK && err == nil {
				t.Fatal("expected overlapping namespace error")
			}
		})
	}
}

func TestS3PresignQuarantineBindsDescriptorAndOwner(t *testing.T) {
	descriptor := cas.Descriptor{
		Digest:    sha256sum.DigestBytes([]byte("bundle object")),
		SizeBytes: int64(len("bundle object")),
		MediaType: "application/vnd.helmr.deployment-program.v0+squashfs",
	}
	presigner := &fakeS3Presigner{}
	store := &Store{
		client:    &fakeS3Client{},
		presigner: presigner,
		bucket:    "bucket",
		prefix:    "cas",
	}
	upload, err := store.PresignQuarantine(t.Context(), "019abcde-1234", descriptor, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if upload.Method != http.MethodPut || upload.URL != "https://upload.example.test/object" {
		t.Fatalf("upload = %+v", upload)
	}
	if presigner.input == nil || aws.ToString(presigner.input.Key) !=
		"cas/quarantine/019abcde-1234/sha256/"+descriptor.Digest[len("sha256:"):] {
		t.Fatalf("presigned input = %+v", presigner.input)
	}
	checksum, _ := descriptorChecksum(descriptor.Digest)
	if aws.ToString(presigner.input.ChecksumSHA256) != checksum ||
		presigner.input.ChecksumAlgorithm != types.ChecksumAlgorithmSha256 ||
		aws.ToInt64(presigner.input.ContentLength) != descriptor.SizeBytes ||
		aws.ToString(presigner.input.ContentType) != descriptor.MediaType ||
		aws.ToString(presigner.input.IfNoneMatch) != "*" ||
		presigner.expires != 5*time.Minute {
		t.Fatalf("presigned descriptor was not exact: %+v expires=%s", presigner.input, presigner.expires)
	}
	if upload.Headers["Content-Length"] != "13" || upload.Headers["Content-Type"] != descriptor.MediaType {
		t.Fatalf("upload headers = %+v", upload.Headers)
	}
}

func TestS3PutAndPromoteQuarantineVerifiesChecksums(t *testing.T) {
	body := []byte("bundle root")
	descriptor := cas.Descriptor{
		Digest:    sha256sum.DigestBytes(body),
		SizeBytes: int64(len(body)),
		MediaType: "application/vnd.helmr.deployment-bundle.v0+json",
	}
	checksum, _ := descriptorChecksum(descriptor.Digest)
	client := &fakeS3Client{}
	store := &Store{client: client, bucket: "bucket", prefix: "cas"}
	if err := store.PutQuarantine(t.Context(), "019abcde-1234", descriptor, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	quarantineKey := "cas/quarantine/019abcde-1234/sha256/" + descriptor.Digest[len("sha256:"):]
	if aws.ToString(client.putObject.Key) != quarantineKey ||
		aws.ToString(client.putObject.ChecksumSHA256) != checksum ||
		string(client.putObjectBody) != string(body) {
		t.Fatalf("quarantine put = %+v body=%q", client.putObject, client.putObjectBody)
	}

	verified := &awss3.HeadObjectOutput{
		ChecksumSHA256: aws.String(checksum),
		ContentLength:  aws.Int64(descriptor.SizeBytes),
		ContentType:    aws.String(descriptor.MediaType),
	}
	client.headObjectOutputs = []*awss3.HeadObjectOutput{verified, nil, verified}
	client.headObjectErrors = []error{
		nil,
		&smithy.GenericAPIError{Code: "NotFound", Message: "missing"},
		nil,
	}
	object, err := store.PromoteQuarantine(t.Context(), "019abcde-1234", descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if object.Digest != descriptor.Digest || object.SizeBytes != descriptor.SizeBytes || object.MediaType != descriptor.MediaType {
		t.Fatalf("object = %+v", object)
	}
	if client.copyObject == nil || aws.ToString(client.copyObject.Key) !=
		"cas/sha256/"+descriptor.Digest[len("sha256:"):] ||
		aws.ToString(client.copyObject.CopySource) != "bucket%2Fcas%2Fquarantine%2F019abcde-1234%2Fsha256%2F"+descriptor.Digest[len("sha256:"):] ||
		aws.ToString(client.copyObject.IfNoneMatch) != "*" {
		t.Fatalf("copy object = %+v", client.copyObject)
	}
}

func TestS3QuarantineFailsClosed(t *testing.T) {
	descriptor := cas.Descriptor{
		Digest:    sha256sum.DigestBytes([]byte("expected")),
		SizeBytes: int64(len("expected")),
		MediaType: "application/vnd.helmr.deployment-program.v0+squashfs",
	}
	store := &Store{client: &fakeS3Client{}, presigner: &fakeS3Presigner{}, bucket: "bucket"}
	if err := store.PutQuarantine(t.Context(), "../other", descriptor, bytes.NewReader([]byte("expected"))); err == nil {
		t.Fatal("PutQuarantine accepted an invalid owner")
	}
	if err := store.PutQuarantine(t.Context(), "019abcde-1234", descriptor, bytes.NewReader([]byte("wrong"))); !errors.Is(err, cas.ErrDigestMismatch) {
		t.Fatalf("PutQuarantine digest error = %v", err)
	}
	if _, err := store.PresignQuarantine(t.Context(), "019abcde-1234", descriptor, 16*time.Minute); err == nil {
		t.Fatal("PresignQuarantine accepted an excessive expiry")
	}
}

func TestObjectTaggingKeepsDeploymentSourcesNonExpirable(t *testing.T) {
	if got := objectTagging(archive.SourceMediaType); got != "" {
		t.Fatalf("deployment source tagging = %q", got)
	}
	if got := objectTagging(cas.CheckpointVMStateMediaType); got != "helmr-expirable=true" {
		t.Fatalf("checkpoint tagging = %q", got)
	}
}

func TestValidateDistinctS3Stores(t *testing.T) {
	for _, tt := range []struct {
		name   string
		first  string
		second string
		wantOK bool
	}{
		{name: "different buckets", first: "s3://first/cas", second: "s3://second/cas", wantOK: true},
		{name: "different endpoints", first: "s3://bucket/cas?endpoint=https://first.example", second: "s3://bucket/cas?endpoint=https://second.example", wantOK: true},
		{name: "same bucket separate prefixes", first: "s3://bucket/cas", second: "s3://bucket/runtimes"},
		{name: "same bucket root and prefix", first: "s3://bucket", second: "s3://bucket/runtimes"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDistinctS3Stores(tt.first, tt.second)
			if tt.wantOK && err != nil {
				t.Fatal(err)
			}
			if !tt.wantOK && err == nil {
				t.Fatal("expected distinct bucket authority error")
			}
		})
	}
}

func TestS3ShardedObjectKey(t *testing.T) {
	store := &Store{prefix: "retained"}
	WithShardedKeys()(store)
	key, err := store.objectKey("sha256:7b927bbd759163db342b22ac0329b49998afa33e911c060e112998b1a7d5339e")
	if err != nil {
		t.Fatal(err)
	}
	const want = "retained/sha256/7b/927bbd759163db342b22ac0329b49998afa33e911c060e112998b1a7d5339e"
	if key != want {
		t.Fatalf("key = %q, want %q", key, want)
	}
}

func TestS3PutUsesSinglePutBelowMultipartThreshold(t *testing.T) {
	client := &fakeS3Client{}
	store := &Store{
		client:                  client,
		bucket:                  "bucket",
		prefix:                  "prefix",
		multipartThresholdBytes: 10,
		multipartPartSizeBytes:  5,
	}

	object, err := store.Put(t.Context(), "text/plain", bytes.NewReader([]byte("hello")))
	if err != nil {
		t.Fatal(err)
	}

	if object.Digest != "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("digest = %s", object.Digest)
	}
	if client.putObject == nil {
		t.Fatal("expected PutObject")
	}
	if client.createdMultipart {
		t.Fatal("did not expect multipart upload")
	}
	if got := aws.ToString(client.putObject.Key); got != "prefix/sha256/2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("key = %q", got)
	}
	if got := client.putObject.ContentLength; got == nil || *got != 5 {
		t.Fatalf("content length = %v", got)
	}
	if string(client.putObjectBody) != "hello" {
		t.Fatalf("body = %q", client.putObjectBody)
	}
}

func TestImmutableS3PublishIsCreateOnlyAndUntagged(t *testing.T) {
	requireDescriptorPublication(t)
	client := &fakeS3Client{}
	store := &ImmutableStore{store: &Store{
		client:                  client,
		bucket:                  "bucket",
		prefix:                  "runtime",
		multipartThresholdBytes: 10,
	}}

	expected, file := sealedPublicationFile(t, []byte("hello"))
	object, err := store.Publish(t.Context(), expected, file)
	if err != nil {
		t.Fatal(err)
	}
	if object.Digest != "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("digest = %s", object.Digest)
	}
	if got := aws.ToString(client.putObject.IfNoneMatch); got != "*" {
		t.Fatalf("If-None-Match = %q", got)
	}
	if client.putObject.Tagging != nil {
		t.Fatalf("tagging = %q", aws.ToString(client.putObject.Tagging))
	}
}

func TestImmutableS3PublishReusesExactExistingObject(t *testing.T) {
	requireDescriptorPublication(t)
	const digest = "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	client := &fakeS3Client{
		putObjectErr: &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "exists"},
		headObject: &awss3.HeadObjectOutput{
			ContentLength: aws.Int64(5),
			ContentType:   aws.String("application/vnd.helmr.runtime.v0+squashfs"),
		},
	}
	store := &ImmutableStore{store: &Store{
		client:                  client,
		bucket:                  "bucket",
		prefix:                  "runtime",
		multipartThresholdBytes: 10,
	}}

	expected, file := sealedPublicationFile(t, []byte("hello"))
	object, err := store.Publish(t.Context(), expected, file)
	if err != nil {
		t.Fatal(err)
	}
	if object.Digest != digest || object.SizeBytes != 5 {
		t.Fatalf("object = %+v", object)
	}
}

func TestImmutableS3PublishRejectsExistingMetadataMismatch(t *testing.T) {
	requireDescriptorPublication(t)
	client := &fakeS3Client{
		putObjectErr: &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "exists"},
		headObject: &awss3.HeadObjectOutput{
			ContentLength: aws.Int64(5),
			ContentType:   aws.String("application/octet-stream"),
		},
	}
	store := &ImmutableStore{store: &Store{
		client:                  client,
		bucket:                  "bucket",
		prefix:                  "runtime",
		multipartThresholdBytes: 10,
	}}

	expected, file := sealedPublicationFile(t, []byte("hello"))
	if _, err := store.Publish(t.Context(), expected, file); err == nil {
		t.Fatal("expected metadata mismatch")
	}
}

func TestImmutableS3PublishRetriesConditionalConflict(t *testing.T) {
	requireDescriptorPublication(t)
	client := &fakeS3Client{
		putObjectErrors: []error{
			&smithy.GenericAPIError{Code: "ConditionalRequestConflict", Message: "racing writer"},
			&smithy.GenericAPIError{Code: "PreconditionFailed", Message: "writer won"},
		},
		headObject: &awss3.HeadObjectOutput{
			ContentLength: aws.Int64(5),
			ContentType:   aws.String("application/vnd.helmr.runtime.v0+squashfs"),
		},
	}
	store := &ImmutableStore{store: &Store{
		client:                  client,
		bucket:                  "bucket",
		prefix:                  "runtime",
		multipartThresholdBytes: 10,
	}}

	expected, file := sealedPublicationFile(t, []byte("hello"))
	if _, err := store.Publish(t.Context(), expected, file); err != nil {
		t.Fatal(err)
	}
	if client.putObjectCalls != 2 {
		t.Fatalf("PutObject calls = %d", client.putObjectCalls)
	}
}

func TestImmutableS3MultipartPublishIsCreateOnlyAndUntagged(t *testing.T) {
	requireDescriptorPublication(t)
	client := &fakeS3Client{uploadID: "upload-1"}
	store := &ImmutableStore{store: &Store{
		client:                  client,
		bucket:                  "bucket",
		prefix:                  "runtime",
		multipartThresholdBytes: 1,
		multipartPartSizeBytes:  4,
	}}

	expected, file := sealedPublicationFile(t, []byte("hello"))
	if _, err := store.Publish(t.Context(), expected, file); err != nil {
		t.Fatal(err)
	}
	if client.createMultipart.Tagging != nil {
		t.Fatalf("tagging = %q", aws.ToString(client.createMultipart.Tagging))
	}
	if got := aws.ToString(client.completedMultipart.IfNoneMatch); got != "*" {
		t.Fatalf("If-None-Match = %q", got)
	}
}

func TestImmutableS3MultipartPublishUsesOnlySealedDescriptor(t *testing.T) {
	requireDescriptorPublication(t)
	client := &fakeS3Client{uploadID: "upload-1"}
	store := &ImmutableStore{store: &Store{
		client:                  client,
		bucket:                  "bucket",
		prefix:                  "runtime",
		multipartThresholdBytes: 1,
		multipartPartSizeBytes:  4,
	}}
	expected, file := sealedPublicationFile(t, []byte("hello world"))
	if err := os.Remove(file.Name()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(t.Context(), expected, file); err != nil {
		t.Fatal(err)
	}
	if len(client.uploadedParts) != 3 {
		t.Fatalf("uploaded parts = %d", len(client.uploadedParts))
	}
	if got := string(uploadedPartBody(t, client, 1)); got != "hell" {
		t.Fatalf("part 1 = %q", got)
	}
	if got := string(uploadedPartBody(t, client, 2)); got != "o wo" {
		t.Fatalf("part 2 = %q", got)
	}
	if got := string(uploadedPartBody(t, client, 3)); got != "rld" {
		t.Fatalf("part 3 = %q", got)
	}
}

func TestImmutableS3MultipartPublishRestartsAfterConditionalConflict(t *testing.T) {
	requireDescriptorPublication(t)
	client := &fakeS3Client{
		uploadID: "upload-1",
		completeMultipartErrors: []error{
			&smithy.GenericAPIError{Code: "ConditionalRequestConflict", Message: "racing writer"},
			nil,
		},
	}
	store := &ImmutableStore{store: &Store{
		client:                  client,
		bucket:                  "bucket",
		prefix:                  "runtime",
		multipartThresholdBytes: 1,
		multipartPartSizeBytes:  4,
	}}

	expected, file := sealedPublicationFile(t, []byte("hello"))
	if _, err := store.Publish(t.Context(), expected, file); err != nil {
		t.Fatal(err)
	}
	if client.createMultipartCalls != 2 || client.completeMultipartCalls != 2 {
		t.Fatalf("multipart calls: create=%d complete=%d", client.createMultipartCalls, client.completeMultipartCalls)
	}
	if !client.abortedMultipart {
		t.Fatal("first multipart upload was not aborted")
	}
}

func TestImmutableS3MultipartPublishDoesNotRetryAfterAmbiguousAbort(t *testing.T) {
	requireDescriptorPublication(t)
	client := &fakeS3Client{
		uploadID: "upload-1",
		completeMultipartErrors: []error{
			&smithy.GenericAPIError{Code: "ConditionalRequestConflict", Message: "racing writer"},
		},
		abortMultipartErr: errors.New("abort failed"),
	}
	store := &ImmutableStore{store: &Store{
		client:                  client,
		bucket:                  "bucket",
		prefix:                  "runtime",
		multipartThresholdBytes: 1,
		multipartPartSizeBytes:  4,
	}}

	expected, file := sealedPublicationFile(t, []byte("hello"))
	if _, err := store.Publish(t.Context(), expected, file); err == nil {
		t.Fatal("expected abort failure")
	}
	if client.createMultipartCalls != 1 || client.completeMultipartCalls != 1 {
		t.Fatalf("multipart calls: create=%d complete=%d", client.createMultipartCalls, client.completeMultipartCalls)
	}
	if client.abortMultipartCalls != 1 {
		t.Fatalf("abort calls = %d", client.abortMultipartCalls)
	}
}

func TestImmutableS3PublishUsesOnlySealedDescriptor(t *testing.T) {
	requireDescriptorPublication(t)
	client := &fakeS3Client{}
	store := &ImmutableStore{store: &Store{
		client:                  client,
		bucket:                  "bucket",
		prefix:                  "runtime",
		multipartThresholdBytes: 10,
	}}
	expected, file := sealedPublicationFile(t, []byte("hello"))
	if err := os.Remove(file.Name()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(t.Context(), expected, file); err != nil {
		t.Fatal(err)
	}
	if string(client.putObjectBody) != "hello" {
		t.Fatalf("body = %q", client.putObjectBody)
	}
}

func TestImmutableS3PublishRejectsDescriptorMismatchBeforeUpload(t *testing.T) {
	requireDescriptorPublication(t)
	client := &fakeS3Client{}
	store := &ImmutableStore{store: &Store{
		client:                  client,
		bucket:                  "bucket",
		multipartThresholdBytes: 10,
	}}
	expected, file := sealedPublicationFile(t, []byte("hello"))
	expected.Digest = sha256sum.DigestBytes([]byte("other"))
	if _, err := store.Publish(t.Context(), expected, file); err == nil {
		t.Fatal("expected digest mismatch")
	}
	if client.putObjectCalls != 0 {
		t.Fatalf("PutObject calls = %d", client.putObjectCalls)
	}
}

func TestImmutableS3PublishRejectsWritableDescriptor(t *testing.T) {
	requireDescriptorPublication(t)
	expected, file := sealedPublicationFile(t, []byte("hello"))
	if err := os.Chmod(file.Name(), 0o600); err != nil {
		t.Fatal(err)
	}
	writer, err := os.OpenFile(file.Name(), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := os.Chmod(file.Name(), 0o400); err != nil {
		t.Fatal(err)
	}
	store := &ImmutableStore{store: &Store{client: &fakeS3Client{}, bucket: "bucket"}}
	if _, err := store.Publish(t.Context(), expected, writer); err == nil {
		t.Fatal("expected writable descriptor rejection")
	}
}

func TestImmutableS3PublishRejectsIdentityChangeAfterUpload(t *testing.T) {
	requireDescriptorPublication(t)
	expected, file := sealedPublicationFile(t, []byte("hello"))
	client := &fakeS3Client{
		putObjectHook: func() {
			if err := os.Chmod(file.Name(), 0o600); err != nil {
				t.Error(err)
			}
		},
	}
	store := &ImmutableStore{store: &Store{
		client:                  client,
		bucket:                  "bucket",
		multipartThresholdBytes: 10,
	}}
	if _, err := store.Publish(t.Context(), expected, file); err == nil {
		t.Fatal("expected final identity rejection")
	}
}

func TestS3PutUsesMultipartAtOrAboveThreshold(t *testing.T) {
	client := &fakeS3Client{uploadID: "upload-1"}
	store := &Store{
		client:                  client,
		bucket:                  "bucket",
		multipartThresholdBytes: 6,
		multipartPartSizeBytes:  4,
	}

	object, err := store.Put(t.Context(), cas.CheckpointScratchDiskMediaType, bytes.NewReader([]byte("hello world")))
	if err != nil {
		t.Fatal(err)
	}

	if object.SizeBytes != 11 {
		t.Fatalf("size = %d", object.SizeBytes)
	}
	if client.putObject != nil {
		t.Fatal("did not expect PutObject")
	}
	if !client.createdMultipart {
		t.Fatal("expected CreateMultipartUpload")
	}
	if client.abortedMultipart {
		t.Fatal("did not expect AbortMultipartUpload")
	}
	if got := client.createMultipart.Tagging; got == nil || *got != "helmr-expirable=true" {
		t.Fatalf("tagging = %v", got)
	}
	if len(client.uploadedParts) != 3 {
		t.Fatalf("uploaded parts = %d", len(client.uploadedParts))
	}
	if got := string(uploadedPartBody(t, client, 1)); got != "hell" {
		t.Fatalf("part 1 = %q", got)
	}
	if got := string(uploadedPartBody(t, client, 2)); got != "o wo" {
		t.Fatalf("part 2 = %q", got)
	}
	if got := string(uploadedPartBody(t, client, 3)); got != "rld" {
		t.Fatalf("part 3 = %q", got)
	}
	if client.completedMultipart == nil {
		t.Fatal("expected CompleteMultipartUpload")
	}
	if got := client.completedMultipart.MultipartUpload.Parts; len(got) != 3 ||
		aws.ToInt32(got[0].PartNumber) != 1 ||
		aws.ToInt32(got[1].PartNumber) != 2 ||
		aws.ToInt32(got[2].PartNumber) != 3 {
		t.Fatalf("completed parts = %+v", got)
	}
}

func TestS3MultipartAbortsOnUploadFailure(t *testing.T) {
	client := &fakeS3Client{uploadID: "upload-1", uploadPartErr: fmt.Errorf("upload failed")}
	store := &Store{
		client:                  client,
		bucket:                  "bucket",
		multipartThresholdBytes: 1,
		multipartPartSizeBytes:  4,
	}

	_, err := store.Put(t.Context(), cas.CheckpointScratchDiskMediaType, bytes.NewReader([]byte("hello")))
	if err == nil {
		t.Fatal("expected error")
	}
	if !client.abortedMultipart {
		t.Fatal("expected AbortMultipartUpload")
	}
	if client.completedMultipart != nil {
		t.Fatal("did not expect CompleteMultipartUpload")
	}
}

func TestS3StageCommitUsesFinalDigestKeyAndCleansTemp(t *testing.T) {
	client := &fakeS3Client{}
	store := &Store{
		client:                  client,
		bucket:                  "bucket",
		prefix:                  "prefix",
		multipartThresholdBytes: 10,
		multipartPartSizeBytes:  5,
	}
	stage, err := store.Stage(t.Context(), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	s3Stage := stage.(*s3Stage)
	stagedPath := s3Stage.Path()
	if _, err := stage.Write([]byte("he")); err != nil {
		t.Fatal(err)
	}
	if _, err := stage.Write([]byte("llo")); err != nil {
		t.Fatal(err)
	}

	object, err := stage.Commit(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if object.Digest != "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("digest = %s", object.Digest)
	}
	if object.Key != "prefix/sha256/2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("object key = %q", object.Key)
	}
	if got := aws.ToString(client.putObject.Key); got != "prefix/sha256/2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("key = %q", got)
	}
	if string(client.putObjectBody) != "hello" {
		t.Fatalf("body = %q", client.putObjectBody)
	}
	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Fatalf("staged file stat error = %v", err)
	}
}

func TestS3StageAbortCleansTempWithoutUpload(t *testing.T) {
	client := &fakeS3Client{}
	store := &Store{client: client, bucket: "bucket"}
	stage, err := store.Stage(t.Context(), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	s3Stage := stage.(*s3Stage)
	stagedPath := s3Stage.Path()
	if _, err := stage.Write([]byte("discard")); err != nil {
		t.Fatal(err)
	}

	if err := stage.Abort(t.Context()); err != nil {
		t.Fatal(err)
	}

	if client.putObject != nil || client.createdMultipart {
		t.Fatal("did not expect upload")
	}
	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Fatalf("staged file stat error = %v", err)
	}
}

func TestS3StageCommitCleansTempAndAbortsMultipartOnUploadFailure(t *testing.T) {
	client := &fakeS3Client{uploadID: "upload-1", uploadPartErr: fmt.Errorf("upload failed")}
	store := &Store{
		client:                  client,
		bucket:                  "bucket",
		multipartThresholdBytes: 1,
		multipartPartSizeBytes:  4,
	}
	stage, err := store.Stage(t.Context(), cas.CheckpointScratchDiskMediaType)
	if err != nil {
		t.Fatal(err)
	}
	s3Stage := stage.(*s3Stage)
	stagedPath := s3Stage.Path()
	if _, err := stage.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}

	_, err = stage.Commit(t.Context())
	if err == nil {
		t.Fatal("expected error")
	}

	if !client.abortedMultipart {
		t.Fatal("expected AbortMultipartUpload")
	}
	if client.completedMultipart != nil {
		t.Fatal("did not expect CompleteMultipartUpload")
	}
	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Fatalf("staged file stat error = %v", err)
	}
}

func TestS3GetVerifiesDigest(t *testing.T) {
	content := []byte("hello")
	client := &fakeS3Client{getObjectBody: content}
	store := &Store{client: client, bucket: "bucket"}

	body, err := store.Get(t.Context(), sha256sum.DigestBytes(content))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := body.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if string(got) != "hello" {
		t.Fatalf("body = %q", got)
	}
}

func TestS3GetRejectsDigestMismatch(t *testing.T) {
	client := &fakeS3Client{getObjectBody: []byte("HELLO")}
	store := &Store{client: client, bucket: "bucket"}

	body, err := store.Get(t.Context(), sha256sum.DigestBytes([]byte("hello")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(body); err == nil {
		t.Fatal("expected digest mismatch")
	}
	if err := body.Close(); err == nil {
		t.Fatal("expected close to report digest mismatch")
	}
}

func TestVerifyingReadCloserCloseDrainsPartialBody(t *testing.T) {
	content := []byte("hello world")
	raw := &trackingReadCloser{Reader: bytes.NewReader(content)}
	body := cas.NewVerifyingReadCloser(raw, sha256sum.DigestBytes(content))

	buf := make([]byte, 5)
	n, err := body.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(buf) {
		t.Fatalf("read bytes = %d", n)
	}

	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if !raw.closed {
		t.Fatal("expected underlying body to be closed")
	}
	if raw.Len() != 0 {
		t.Fatal("expected Close to drain the unread body")
	}
}

func TestVerifyingReadCloserCloseRejectsPartialDigestMismatch(t *testing.T) {
	expected := []byte("hello world")
	actual := []byte("HELLO world")
	raw := &trackingReadCloser{Reader: bytes.NewReader(actual)}
	body := cas.NewVerifyingReadCloser(raw, sha256sum.DigestBytes(expected))

	buf := make([]byte, 5)
	if _, err := body.Read(buf); err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err == nil {
		t.Fatal("expected digest mismatch")
	}
	if !raw.closed {
		t.Fatal("expected underlying body to be closed")
	}
	if raw.Len() != 0 {
		t.Fatal("expected Close to drain the unread body")
	}
}

type uploadedPart struct {
	number int32
	body   []byte
}

type trackingReadCloser struct {
	*bytes.Reader
	closed bool
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}

type fakeS3Client struct {
	mu                      sync.Mutex
	putObject               *awss3.PutObjectInput
	putObjectBody           []byte
	createMultipart         *awss3.CreateMultipartUploadInput
	createdMultipart        bool
	completedMultipart      *awss3.CompleteMultipartUploadInput
	abortedMultipart        bool
	uploadedParts           []uploadedPart
	uploadID                string
	uploadPartErr           error
	getObjectBody           []byte
	putObjectErr            error
	putObjectErrors         []error
	putObjectCalls          int
	putObjectHook           func()
	headObject              *awss3.HeadObjectOutput
	headObjectErr           error
	headObjectOutputs       []*awss3.HeadObjectOutput
	headObjectErrors        []error
	headObjectCalls         int
	copyObject              *awss3.CopyObjectInput
	copyObjectErr           error
	createMultipartCalls    int
	completeMultipartCalls  int
	completeMultipartErrors []error
	abortMultipartCalls     int
	abortMultipartErr       error
}

func (f *fakeS3Client) PutObject(_ context.Context, input *awss3.PutObjectInput, _ ...func(*awss3.Options)) (*awss3.PutObjectOutput, error) {
	f.putObjectCalls++
	f.putObject = input
	body, err := io.ReadAll(input.Body)
	if err != nil {
		return nil, err
	}
	f.putObjectBody = body
	if f.putObjectHook != nil {
		f.putObjectHook()
	}
	if f.putObjectCalls <= len(f.putObjectErrors) {
		return &awss3.PutObjectOutput{}, f.putObjectErrors[f.putObjectCalls-1]
	}
	return &awss3.PutObjectOutput{}, f.putObjectErr
}

func (f *fakeS3Client) HeadObject(context.Context, *awss3.HeadObjectInput, ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error) {
	f.headObjectCalls++
	if f.headObjectCalls <= len(f.headObjectOutputs) || f.headObjectCalls <= len(f.headObjectErrors) {
		var output *awss3.HeadObjectOutput
		var err error
		if f.headObjectCalls <= len(f.headObjectOutputs) {
			output = f.headObjectOutputs[f.headObjectCalls-1]
		}
		if f.headObjectCalls <= len(f.headObjectErrors) {
			err = f.headObjectErrors[f.headObjectCalls-1]
		}
		return output, err
	}
	if f.headObject == nil && f.headObjectErr == nil {
		return nil, fmt.Errorf("not implemented")
	}
	return f.headObject, f.headObjectErr
}

func (f *fakeS3Client) GetObject(context.Context, *awss3.GetObjectInput, ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	return &awss3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(f.getObjectBody))}, nil
}

func (f *fakeS3Client) DeleteObject(context.Context, *awss3.DeleteObjectInput, ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error) {
	return nil, fmt.Errorf("not implemented")
}

func (f *fakeS3Client) CopyObject(_ context.Context, input *awss3.CopyObjectInput, _ ...func(*awss3.Options)) (*awss3.CopyObjectOutput, error) {
	f.copyObject = input
	return &awss3.CopyObjectOutput{}, f.copyObjectErr
}

func (f *fakeS3Client) CreateMultipartUpload(_ context.Context, input *awss3.CreateMultipartUploadInput, _ ...func(*awss3.Options)) (*awss3.CreateMultipartUploadOutput, error) {
	f.createMultipartCalls++
	f.createMultipart = input
	f.createdMultipart = true
	uploadID := f.uploadID
	if uploadID == "" {
		uploadID = "upload"
	}
	return &awss3.CreateMultipartUploadOutput{UploadId: aws.String(uploadID)}, nil
}

func (f *fakeS3Client) UploadPart(_ context.Context, input *awss3.UploadPartInput, _ ...func(*awss3.Options)) (*awss3.UploadPartOutput, error) {
	if f.uploadPartErr != nil {
		return nil, f.uploadPartErr
	}
	body, err := io.ReadAll(input.Body)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploadedParts = append(f.uploadedParts, uploadedPart{
		number: aws.ToInt32(input.PartNumber),
		body:   body,
	})
	return &awss3.UploadPartOutput{ETag: aws.String(fmt.Sprintf("etag-%d", aws.ToInt32(input.PartNumber)))}, nil
}

func (f *fakeS3Client) CompleteMultipartUpload(_ context.Context, input *awss3.CompleteMultipartUploadInput, _ ...func(*awss3.Options)) (*awss3.CompleteMultipartUploadOutput, error) {
	f.completeMultipartCalls++
	f.completedMultipart = input
	if f.completeMultipartCalls <= len(f.completeMultipartErrors) {
		return &awss3.CompleteMultipartUploadOutput{}, f.completeMultipartErrors[f.completeMultipartCalls-1]
	}
	return &awss3.CompleteMultipartUploadOutput{}, nil
}

func (f *fakeS3Client) AbortMultipartUpload(context.Context, *awss3.AbortMultipartUploadInput, ...func(*awss3.Options)) (*awss3.AbortMultipartUploadOutput, error) {
	f.abortMultipartCalls++
	f.abortedMultipart = true
	return &awss3.AbortMultipartUploadOutput{}, f.abortMultipartErr
}

var _ s3Client = (*fakeS3Client)(nil)

type fakeS3Presigner struct {
	input   *awss3.PutObjectInput
	expires time.Duration
}

func (f *fakeS3Presigner) PresignPutObject(
	_ context.Context,
	input *awss3.PutObjectInput,
	options ...func(*awss3.PresignOptions),
) (*awsv4.PresignedHTTPRequest, error) {
	f.input = input
	var resolved awss3.PresignOptions
	for _, option := range options {
		option(&resolved)
	}
	f.expires = resolved.Expires
	return &awsv4.PresignedHTTPRequest{
		Method: http.MethodPut,
		URL:    "https://upload.example.test/object",
		SignedHeader: http.Header{
			"X-Amz-Checksum-Sha256": []string{aws.ToString(input.ChecksumSHA256)},
		},
	}, nil
}

var _ s3Presigner = (*fakeS3Presigner)(nil)

func uploadedPartBody(t *testing.T, client *fakeS3Client, number int32) []byte {
	t.Helper()
	for _, part := range client.uploadedParts {
		if part.number == number {
			return part.body
		}
	}
	t.Fatalf("missing uploaded part %d: %+v", number, client.uploadedParts)
	return nil
}

func requireDescriptorPublication(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("descriptor-bound publication requires Linux")
	}
}

func sealedPublicationFile(t *testing.T, content []byte) (cas.Descriptor, *os.File) {
	t.Helper()
	path := t.TempDir() + "/snapshot"
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	return cas.Descriptor{
		Digest:    sha256sum.DigestBytes(content),
		SizeBytes: int64(len(content)),
		MediaType: "application/vnd.helmr.runtime.v0+squashfs",
	}, file
}
