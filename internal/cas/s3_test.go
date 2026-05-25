package cas

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestS3PutUsesSinglePutBelowMultipartThreshold(t *testing.T) {
	client := &fakeS3Client{}
	store := &S3{
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

func TestS3PutUsesMultipartAtOrAboveThreshold(t *testing.T) {
	client := &fakeS3Client{uploadID: "upload-1"}
	store := &S3{
		client:                  client,
		bucket:                  "bucket",
		multipartThresholdBytes: 6,
		multipartPartSizeBytes:  4,
	}

	object, err := store.Put(t.Context(), CheckpointScratchDiskMediaType, bytes.NewReader([]byte("hello world")))
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
	if got := string(client.uploadedParts[0].body); got != "hell" {
		t.Fatalf("part 1 = %q", got)
	}
	if got := string(client.uploadedParts[1].body); got != "o wo" {
		t.Fatalf("part 2 = %q", got)
	}
	if got := string(client.uploadedParts[2].body); got != "rld" {
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
	store := &S3{
		client:                  client,
		bucket:                  "bucket",
		multipartThresholdBytes: 1,
		multipartPartSizeBytes:  4,
	}

	_, err := store.Put(t.Context(), CheckpointScratchDiskMediaType, bytes.NewReader([]byte("hello")))
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

type uploadedPart struct {
	number int32
	body   []byte
}

type fakeS3Client struct {
	putObject          *s3.PutObjectInput
	putObjectBody      []byte
	createMultipart    *s3.CreateMultipartUploadInput
	createdMultipart   bool
	completedMultipart *s3.CompleteMultipartUploadInput
	abortedMultipart   bool
	uploadedParts      []uploadedPart
	uploadID           string
	uploadPartErr      error
}

func (f *fakeS3Client) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.putObject = input
	body, err := io.ReadAll(input.Body)
	if err != nil {
		return nil, err
	}
	f.putObjectBody = body
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3Client) HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	return nil, fmt.Errorf("not implemented")
}

func (f *fakeS3Client) GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return nil, fmt.Errorf("not implemented")
}

func (f *fakeS3Client) DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	return nil, fmt.Errorf("not implemented")
}

func (f *fakeS3Client) CreateMultipartUpload(_ context.Context, input *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	f.createMultipart = input
	f.createdMultipart = true
	uploadID := f.uploadID
	if uploadID == "" {
		uploadID = "upload"
	}
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String(uploadID)}, nil
}

func (f *fakeS3Client) UploadPart(_ context.Context, input *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	if f.uploadPartErr != nil {
		return nil, f.uploadPartErr
	}
	body, err := io.ReadAll(input.Body)
	if err != nil {
		return nil, err
	}
	f.uploadedParts = append(f.uploadedParts, uploadedPart{
		number: aws.ToInt32(input.PartNumber),
		body:   body,
	})
	return &s3.UploadPartOutput{ETag: aws.String(fmt.Sprintf("etag-%d", aws.ToInt32(input.PartNumber)))}, nil
}

func (f *fakeS3Client) CompleteMultipartUpload(_ context.Context, input *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	f.completedMultipart = input
	return &s3.CompleteMultipartUploadOutput{}, nil
}

func (f *fakeS3Client) AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	f.abortedMultipart = true
	return &s3.AbortMultipartUploadOutput{}, nil
}

var _ s3Client = (*fakeS3Client)(nil)
