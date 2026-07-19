package deployment

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

func TestManagerDocumentS3UsesConditionalCreate(t *testing.T) {
	client := &managerS3Client{}
	store := &managerDocumentS3{
		client: client,
		bucket: "manager-authority",
		prefix: "managers",
	}
	body := []byte(`{"formatVersion":0}`)
	document := managerDocument{
		Key: managerSelectorClaimKeyRoot +
			strings.Repeat("1", 64),
		MediaType: ManagerClaimMediaType,
		SizeBytes: int64(len(body)),
	}

	created, err := store.Create(context.Background(), document, body)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("Create reported an existing object")
	}
	if client.put == nil {
		t.Fatal("Create did not call PutObject")
	}
	if aws.ToString(client.put.Bucket) != "manager-authority" ||
		aws.ToString(client.put.Key) != "managers/"+document.Key ||
		aws.ToString(client.put.ContentType) != ManagerClaimMediaType ||
		aws.ToString(client.put.IfNoneMatch) != "*" ||
		aws.ToInt64(client.put.ContentLength) != int64(len(body)) {
		t.Fatalf("PutObject input = %#v", client.put)
	}
	got, err := io.ReadAll(client.put.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body = %q, want %q", got, body)
	}
}

func TestManagerDocumentS3ConvergesOnExistingObject(t *testing.T) {
	client := &managerS3Client{
		putErrors: []error{&smithy.GenericAPIError{
			Code:    "PreconditionFailed",
			Message: "exists",
		}},
	}
	store := &managerDocumentS3{client: client, bucket: "manager-authority"}
	body := []byte(`{"formatVersion":0}`)
	created, err := store.Create(context.Background(), managerDocument{
		Key: managerCapsuleObjectKeyRoot +
			strings.Repeat("2", 64),
		MediaType: ManagerCapsuleMediaType,
		SizeBytes: int64(len(body)),
	}, body)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("Create reported a new object")
	}
	if client.putCalls != 1 {
		t.Fatalf("PutObject calls = %d, want 1", client.putCalls)
	}
}

func TestManagerDocumentS3RetriesConditionalConflict(t *testing.T) {
	client := &managerS3Client{
		putErrors: []error{
			&smithy.GenericAPIError{
				Code:    "ConditionalRequestConflict",
				Message: "race",
			},
			nil,
		},
	}
	store := &managerDocumentS3{client: client, bucket: "manager-authority"}
	body := []byte(`{"formatVersion":0}`)
	created, err := store.Create(context.Background(), managerDocument{
		Key: managerCapsuleObjectKeyRoot +
			strings.Repeat("2", 64),
		MediaType: ManagerCapsuleMediaType,
		SizeBytes: int64(len(body)),
	}, body)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("Create did not report the retried publication")
	}
	if client.putCalls != 2 {
		t.Fatalf("PutObject calls = %d, want 2", client.putCalls)
	}
}

func TestManagerDocumentS3ReadsExactMetadata(t *testing.T) {
	body := []byte(`{"formatVersion":0}`)
	client := &managerS3Client{get: &s3.GetObjectOutput{
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: aws.Int64(int64(len(body))),
		ContentType:   aws.String(ManagerCapsuleMediaType),
	}}
	store := &managerDocumentS3{
		client: client,
		bucket: "manager-authority",
		prefix: "authority",
	}
	key := managerCapsuleObjectKeyRoot + strings.Repeat("3", 64)
	document, reader, err := store.Read(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if document != (managerDocument{
		Key:       key,
		MediaType: ManagerCapsuleMediaType,
		SizeBytes: int64(len(body)),
	}) {
		t.Fatalf("document = %#v", document)
	}
	if client.getInput == nil ||
		aws.ToString(client.getInput.Key) != "authority/"+key {
		t.Fatalf("GetObject input = %#v", client.getInput)
	}
}

func TestManagerDocumentS3MapsMissingObject(t *testing.T) {
	client := &managerS3Client{getErr: &smithy.GenericAPIError{
		Code:    "NoSuchKey",
		Message: "missing",
	}}
	store := &managerDocumentS3{client: client, bucket: "manager-authority"}
	_, _, err := store.Read(
		context.Background(),
		managerSelectorClaimKeyRoot+strings.Repeat("4", 64),
	)
	if !errors.Is(err, errManagerObjectNotFound) {
		t.Fatalf("error = %v, want errManagerObjectNotFound", err)
	}
}

func TestManagerStoreURIClosesNamespace(t *testing.T) {
	valid, err := parseManagerStoreURI(
		"s3://manager-authority/root?endpoint=https%3A%2F%2Fs3.example.test",
	)
	if err != nil {
		t.Fatal(err)
	}
	if valid.Host != "manager-authority" ||
		valid.Path != "/root" ||
		valid.Query().Get("endpoint") != "https://s3.example.test" {
		t.Fatalf("URI = %s", valid)
	}
	for _, raw := range []string{
		"",
		" s3://manager-authority",
		"https://manager-authority",
		"s3:///root",
		"s3://user@manager-authority",
		"s3://manager-authority/root#fragment",
		"s3://manager-authority/root%2Fother",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseManagerStoreURI(raw); err == nil {
				t.Fatal("parseManagerStoreURI accepted an invalid URI")
			}
		})
	}
}

func TestManagerDocumentValidationRejectsOpenNamespace(t *testing.T) {
	body := []byte(`{"formatVersion":0}`)
	tests := []managerDocument{
		{
			Key:       "v0/other/sha256/" + strings.Repeat("1", 64),
			MediaType: ManagerClaimMediaType,
			SizeBytes: int64(len(body)),
		},
		{
			Key: managerSelectorClaimKeyRoot +
				strings.Repeat("A", 64),
			MediaType: ManagerClaimMediaType,
			SizeBytes: int64(len(body)),
		},
		{
			Key: managerSelectorClaimKeyRoot +
				strings.Repeat("1", 64),
			MediaType: ManagerCapsuleMediaType,
			SizeBytes: int64(len(body)),
		},
		{
			Key: managerSelectorClaimKeyRoot +
				strings.Repeat("1", 64),
			MediaType: ManagerClaimMediaType,
			SizeBytes: int64(len(body)) + 1,
		},
	}
	for index, document := range tests {
		t.Run(string(rune('a'+index)), func(t *testing.T) {
			if err := validateManagerDocument(document, body); err == nil {
				t.Fatal("validateManagerDocument accepted an invalid document")
			}
		})
	}
}

type managerS3Client struct {
	put       *s3.PutObjectInput
	putCalls  int
	putErrors []error
	get       *s3.GetObjectOutput
	getErr    error
	getInput  *s3.GetObjectInput
}

func (c *managerS3Client) PutObject(
	_ context.Context,
	input *s3.PutObjectInput,
	_ ...func(*s3.Options),
) (*s3.PutObjectOutput, error) {
	c.put = input
	c.putCalls++
	if c.putCalls <= len(c.putErrors) {
		if err := c.putErrors[c.putCalls-1]; err != nil {
			return nil, err
		}
	}
	return &s3.PutObjectOutput{}, nil
}

func (c *managerS3Client) GetObject(
	_ context.Context,
	input *s3.GetObjectInput,
	_ ...func(*s3.Options),
) (*s3.GetObjectOutput, error) {
	c.getInput = input
	if c.getErr != nil {
		return nil, c.getErr
	}
	return c.get, nil
}

var _ managerDocumentS3Client = (*managerS3Client)(nil)
