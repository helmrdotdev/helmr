package deployment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/helmrdotdev/helmr/internal/cas"
)

const managerDocumentCreateAttempts = 3

type managerDocumentS3Client interface {
	PutObject(
		context.Context,
		*s3.PutObjectInput,
		...func(*s3.Options),
	) (*s3.PutObjectOutput, error)
	GetObject(
		context.Context,
		*s3.GetObjectInput,
		...func(*s3.Options),
	) (*s3.GetObjectOutput, error)
}

type managerDocumentS3 struct {
	client managerDocumentS3Client
	bucket string
	prefix string
}

func NewManagerS3(
	ctx context.Context,
	rawURI string,
) (*ManagerStore, error) {
	if ctx == nil {
		return nil, errors.New("manager store context is nil")
	}
	uri, err := parseManagerStoreURI(rawURI)
	if err != nil {
		return nil, err
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load manager store AWS configuration: %w", err)
	}
	client := s3.NewFromConfig(cfg, func(options *s3.Options) {
		if endpoint := uri.Query().Get("endpoint"); endpoint != "" {
			options.BaseEndpoint = aws.String(endpoint)
			options.UsePathStyle = true
		}
	})
	documents := &managerDocumentS3{
		client: client,
		bucket: uri.Host,
		prefix: strings.Trim(uri.Path, "/"),
	}
	treeURI := *uri
	treeURI.Path = "/" + joinManagerStoreKey(documents.prefix, "v0/trees")
	treeURI.RawPath = ""
	trees, err := cas.NewImmutableS3(ctx, treeURI.String())
	if err != nil {
		return nil, fmt.Errorf("configure manager tree store: %w", err)
	}
	return newManagerStore(documents, trees)
}

func (s *managerDocumentS3) Create(
	ctx context.Context,
	document managerDocument,
	body []byte,
) (bool, error) {
	if ctx == nil {
		return false, errors.New("manager document context is nil")
	}
	if err := validateManagerDocument(document, body); err != nil {
		return false, err
	}
	key := joinManagerStoreKey(s.prefix, document.Key)
	var err error
	for attempt := 0; attempt < managerDocumentCreateAttempts; attempt++ {
		_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(s.bucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(body),
			ContentLength: aws.Int64(document.SizeBytes),
			ContentType:   aws.String(document.MediaType),
			IfNoneMatch:   aws.String("*"),
		})
		switch managerConditionalWrite(err) {
		case managerWriteCreated:
			return true, nil
		case managerWriteExists:
			return false, nil
		case managerWriteConflict:
			continue
		default:
			return false, err
		}
	}
	return false, fmt.Errorf(
		"manager document publication remained conflicted after %d attempts: %w",
		managerDocumentCreateAttempts,
		err,
	)
}

func (s *managerDocumentS3) Read(
	ctx context.Context,
	key string,
) (managerDocument, io.ReadCloser, error) {
	if ctx == nil {
		return managerDocument{}, nil, errors.New("manager document context is nil")
	}
	if err := validateManagerDocumentKey(key); err != nil {
		return managerDocument{}, nil, err
	}
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(joinManagerStoreKey(s.prefix, key)),
	})
	if managerObjectMissing(err) {
		return managerDocument{}, nil, errManagerObjectNotFound
	}
	if err != nil {
		return managerDocument{}, nil, err
	}
	if output.Body == nil {
		return managerDocument{}, nil, errors.New(
			"manager document response body is missing",
		)
	}
	return managerDocument{
		Key:       key,
		MediaType: aws.ToString(output.ContentType),
		SizeBytes: aws.ToInt64(output.ContentLength),
	}, output.Body, nil
}

func parseManagerStoreURI(raw string) (*url.URL, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return nil, errors.New("manager store URI is required and must be normalized")
	}
	uri, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse manager store URI: %w", err)
	}
	if uri.Scheme != "s3" || uri.Host == "" {
		return nil, fmt.Errorf("invalid manager store URI %q", raw)
	}
	if uri.User != nil || uri.Fragment != "" {
		return nil, errors.New("manager store URI forbids user info and fragments")
	}
	if uri.EscapedPath() != uri.Path {
		return nil, errors.New("manager store URI path must not use escapes")
	}
	return uri, nil
}

func validateManagerDocument(
	document managerDocument,
	body []byte,
) error {
	if err := validateManagerDocumentKey(document.Key); err != nil {
		return err
	}
	wantMediaType := ManagerCapsuleMediaType
	maxBytes := maxManagerCapsuleBytes
	if strings.HasPrefix(document.Key, managerSelectorClaimKeyRoot) {
		wantMediaType = ManagerClaimMediaType
		maxBytes = maxManagerClaimBytes
	}
	if document.MediaType != wantMediaType {
		return fmt.Errorf(
			"manager document media type = %q, want %q",
			document.MediaType,
			wantMediaType,
		)
	}
	if document.SizeBytes < 1 ||
		document.SizeBytes > int64(maxBytes) ||
		int64(len(body)) != document.SizeBytes {
		return errors.New("manager document body does not match its size")
	}
	return nil
}

func validateManagerDocumentKey(key string) error {
	var root string
	switch {
	case strings.HasPrefix(key, managerSelectorClaimKeyRoot):
		root = managerSelectorClaimKeyRoot
	case strings.HasPrefix(key, managerCapsuleObjectKeyRoot):
		root = managerCapsuleObjectKeyRoot
	default:
		return errors.New("manager document key is outside the closed namespace")
	}
	suffix := strings.TrimPrefix(key, root)
	if len(suffix) != 64 {
		return errors.New("manager document key has an invalid digest")
	}
	for _, character := range suffix {
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'f') {
			return errors.New("manager document key has an invalid digest")
		}
	}
	return nil
}

func joinManagerStoreKey(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "/" + key
}

type managerWriteResult int

const (
	managerWriteCreated managerWriteResult = iota
	managerWriteExists
	managerWriteConflict
	managerWriteFailed
)

func managerConditionalWrite(err error) managerWriteResult {
	if err == nil {
		return managerWriteCreated
	}
	var apiError smithy.APIError
	if !errors.As(err, &apiError) {
		return managerWriteFailed
	}
	switch apiError.ErrorCode() {
	case "PreconditionFailed":
		return managerWriteExists
	case "ConditionalRequestConflict":
		return managerWriteConflict
	default:
		return managerWriteFailed
	}
}

func managerObjectMissing(err error) bool {
	var apiError smithy.APIError
	if !errors.As(err, &apiError) {
		return false
	}
	switch apiError.ErrorCode() {
	case "NoSuchKey", "NotFound":
		return true
	default:
		return false
	}
}

var _ managerDocuments = (*managerDocumentS3)(nil)
