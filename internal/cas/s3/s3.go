package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/cas"
	"golang.org/x/sync/errgroup"
)

const (
	s3MultipartThresholdBytes    = 64 << 20
	s3MultipartPartSizeBytes     = 64 << 20
	s3MultipartMaxParts          = 10000
	s3MultipartUploadConcurrency = 4
	s3MultipartAbortTimeout      = 30 * time.Second
	immutablePublishAttempts     = 3
)

type s3Client interface {
	PutObject(context.Context, *awss3.PutObjectInput, ...func(*awss3.Options)) (*awss3.PutObjectOutput, error)
	HeadObject(context.Context, *awss3.HeadObjectInput, ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error)
	GetObject(context.Context, *awss3.GetObjectInput, ...func(*awss3.Options)) (*awss3.GetObjectOutput, error)
	DeleteObject(context.Context, *awss3.DeleteObjectInput, ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error)
	CreateMultipartUpload(context.Context, *awss3.CreateMultipartUploadInput, ...func(*awss3.Options)) (*awss3.CreateMultipartUploadOutput, error)
	UploadPart(context.Context, *awss3.UploadPartInput, ...func(*awss3.Options)) (*awss3.UploadPartOutput, error)
	CompleteMultipartUpload(context.Context, *awss3.CompleteMultipartUploadInput, ...func(*awss3.Options)) (*awss3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(context.Context, *awss3.AbortMultipartUploadInput, ...func(*awss3.Options)) (*awss3.AbortMultipartUploadOutput, error)
}

type Store struct {
	client  s3Client
	bucket  string
	prefix  string
	tempDir string
	sharded bool

	multipartThresholdBytes int64
	multipartPartSizeBytes  int64
}

type ImmutableStore struct {
	store *Store
}

type Option func(*Store)

func WithTempDir(path string) Option {
	return func(store *Store) {
		store.tempDir = strings.TrimSpace(path)
	}
}

func WithShardedKeys() Option {
	return func(store *Store) {
		store.sharded = true
	}
}

func New(ctx context.Context, rawURI string, opts ...Option) (*Store, error) {
	uri, err := url.Parse(rawURI)
	if err != nil {
		return nil, err
	}
	if uri.Scheme != "s3" {
		return nil, fmt.Errorf("unsupported CAS URI scheme %q", uri.Scheme)
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	client := awss3.NewFromConfig(cfg, func(options *awss3.Options) {
		if endpoint := uri.Query().Get("endpoint"); endpoint != "" {
			options.BaseEndpoint = aws.String(endpoint)
			options.UsePathStyle = true
		}
	})
	store := &Store{
		client: client,
		bucket: uri.Host,
		prefix: strings.Trim(uri.Path, "/"),
	}
	for _, opt := range opts {
		opt(store)
	}
	return store, nil
}

func NewImmutable(ctx context.Context, rawURI string, opts ...Option) (*ImmutableStore, error) {
	store, err := New(ctx, rawURI, opts...)
	if err != nil {
		return nil, err
	}
	return &ImmutableStore{store: store}, nil
}

func ValidateDisjointS3Stores(firstURI, secondURI string) error {
	first, err := s3Namespace(firstURI)
	if err != nil {
		return err
	}
	second, err := s3Namespace(secondURI)
	if err != nil {
		return err
	}
	if first.authority != second.authority {
		return nil
	}
	if first.key == second.key ||
		strings.HasPrefix(first.key, second.key+"/") ||
		strings.HasPrefix(second.key, first.key+"/") {
		return errors.New("S3 stores have overlapping object namespaces")
	}
	return nil
}

func ValidateDistinctS3Stores(firstURI, secondURI string) error {
	first, err := s3Namespace(firstURI)
	if err != nil {
		return err
	}
	second, err := s3Namespace(secondURI)
	if err != nil {
		return err
	}
	if first.authority == second.authority {
		return errors.New("S3 stores do not have distinct bucket authority")
	}
	return nil
}

type namespace struct {
	authority string
	key       string
}

func s3Namespace(rawURI string) (namespace, error) {
	uri, err := url.Parse(rawURI)
	if err != nil {
		return namespace{}, err
	}
	if uri.Scheme != "s3" || uri.Host == "" {
		return namespace{}, fmt.Errorf("invalid S3 store URI %q", rawURI)
	}
	prefix := strings.Trim(uri.Path, "/")
	key := "sha256"
	if prefix != "" {
		key = prefix + "/" + key
	}
	return namespace{
		authority: uri.Host + "\x00" + uri.Query().Get("endpoint"),
		key:       key,
	}, nil
}

func (c *Store) Put(ctx context.Context, mediaType string, body io.Reader) (cas.Object, error) {
	stage, err := c.Stage(ctx, mediaType)
	if err != nil {
		return cas.Object{}, err
	}
	return cas.WriteStage(ctx, stage, body)
}

func (c *Store) Stage(ctx context.Context, mediaType string) (cas.Stage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.tempDir != "" {
		if err := os.MkdirAll(c.tempDir, 0o700); err != nil {
			return nil, err
		}
	}
	tmp, err := os.CreateTemp(c.tempDir, "helmr-cas-*")
	if err != nil {
		return nil, err
	}
	return &s3Stage{store: c, FileStage: cas.NewFileStage(mediaType, tmp)}, nil
}

func (c *Store) uploadFile(ctx context.Context, key, mediaType, path string, size int64) error {
	if size < c.multipartThreshold() {
		return c.putObject(ctx, key, mediaType, path, size)
	}
	return c.putMultipartObject(ctx, key, mediaType, path, size)
}

func (c *Store) uploadDescriptor(
	ctx context.Context,
	key string,
	expected cas.Descriptor,
	file *os.File,
) error {
	if expected.SizeBytes < c.multipartThreshold() {
		return c.putDescriptor(ctx, key, expected, file)
	}
	return c.putMultipartDescriptor(ctx, key, expected, file)
}

func (c *Store) putDescriptor(
	ctx context.Context,
	key string,
	expected cas.Descriptor,
	file *os.File,
) error {
	_, err := c.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		Body:          io.NewSectionReader(file, 0, expected.SizeBytes),
		ContentLength: aws.Int64(expected.SizeBytes),
		ContentType:   aws.String(expected.MediaType),
		IfNoneMatch:   aws.String("*"),
	})
	switch conditionalWriteError(err) {
	case conditionalWriteExists:
		return errImmutableObjectExists
	case conditionalWriteConflict:
		return errImmutableObjectConflict
	default:
		return err
	}
}

func (c *Store) putMultipartDescriptor(
	ctx context.Context,
	key string,
	expected cas.Descriptor,
	file *os.File,
) error {
	created, err := c.client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(expected.MediaType),
	})
	if err != nil {
		return err
	}
	uploadID := aws.ToString(created.UploadId)

	partSize := c.multipartPartSize(expected.SizeBytes)
	partCount := int((expected.SizeBytes + partSize - 1) / partSize)
	parts := make([]types.CompletedPart, partCount)
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(s3MultipartUploadConcurrency)
	for offset, partNumber := int64(0), int32(1); offset < expected.SizeBytes; offset, partNumber = offset+partSize, partNumber+1 {
		index := int(partNumber - 1)
		offset := offset
		partNumber := partNumber
		currentSize := min(partSize, expected.SizeBytes-offset)
		group.Go(func() error {
			part, err := c.client.UploadPart(groupCtx, &awss3.UploadPartInput{
				Bucket:     aws.String(c.bucket),
				Key:        aws.String(key),
				UploadId:   aws.String(uploadID),
				PartNumber: aws.Int32(partNumber),
				Body:       io.NewSectionReader(file, offset, currentSize),
			})
			if err != nil {
				return err
			}
			parts[index] = types.CompletedPart{
				ETag:       part.ETag,
				PartNumber: aws.Int32(partNumber),
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		if abortErr := c.abortMultipartUpload(ctx, key, uploadID); abortErr != nil {
			return errors.Join(err, abortErr)
		}
		return err
	}
	_, err = c.client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: parts,
		},
		IfNoneMatch: aws.String("*"),
	})
	if err != nil {
		if abortErr := c.abortMultipartUpload(ctx, key, uploadID); abortErr != nil {
			return abortErr
		}
		switch conditionalWriteError(err) {
		case conditionalWriteExists:
			return errImmutableObjectExists
		case conditionalWriteConflict:
			return errImmutableObjectConflict
		}
		return err
	}
	return nil
}

func (c *Store) abortMultipartUpload(ctx context.Context, key, uploadID string) error {
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s3MultipartAbortTimeout)
	defer cancel()
	_, err := c.client.AbortMultipartUpload(abortCtx, &awss3.AbortMultipartUploadInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
	if err != nil {
		return fmt.Errorf("abort multipart upload %q: %w", uploadID, err)
	}
	return nil
}

func (c *Store) putObject(ctx context.Context, key, mediaType, path string, size int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	input := &awss3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		Body:          file,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(mediaType),
	}
	if tagging := objectTagging(mediaType); tagging != "" {
		input.Tagging = aws.String(tagging)
	}
	_, err = c.client.PutObject(ctx, input)
	return err
}

func (c *Store) putMultipartObject(ctx context.Context, key, mediaType, path string, size int64) error {
	createInput := &awss3.CreateMultipartUploadInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(mediaType),
	}
	if tagging := objectTagging(mediaType); tagging != "" {
		createInput.Tagging = aws.String(tagging)
	}
	created, err := c.client.CreateMultipartUpload(ctx, createInput)
	if err != nil {
		return err
	}
	uploadID := aws.ToString(created.UploadId)
	completed := false
	defer func() {
		if !completed {
			_, _ = c.client.AbortMultipartUpload(context.Background(), &awss3.AbortMultipartUploadInput{
				Bucket:   aws.String(c.bucket),
				Key:      aws.String(key),
				UploadId: aws.String(uploadID),
			})
		}
	}()
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	partSize := c.multipartPartSize(size)
	partCount := int((size + partSize - 1) / partSize)
	parts := make([]types.CompletedPart, partCount)
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(s3MultipartUploadConcurrency)
	for offset, partNumber := int64(0), int32(1); offset < size; offset, partNumber = offset+partSize, partNumber+1 {
		index := int(partNumber - 1)
		offset := offset
		partNumber := partNumber
		remaining := size - offset
		currentSize := min(partSize, remaining)
		group.Go(func() error {
			part, err := c.client.UploadPart(groupCtx, &awss3.UploadPartInput{
				Bucket:     aws.String(c.bucket),
				Key:        aws.String(key),
				UploadId:   aws.String(uploadID),
				PartNumber: aws.Int32(partNumber),
				Body:       io.NewSectionReader(file, offset, currentSize),
			})
			if err != nil {
				return err
			}
			parts[index] = types.CompletedPart{
				ETag:       part.ETag,
				PartNumber: aws.Int32(partNumber),
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	completeInput := &awss3.CompleteMultipartUploadInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: parts,
		},
	}
	_, err = c.client.CompleteMultipartUpload(ctx, completeInput)
	if err != nil {
		return err
	}
	completed = true
	return nil
}

type conditionalWriteResult int

const (
	conditionalWriteOther conditionalWriteResult = iota
	conditionalWriteExists
	conditionalWriteConflict
)

func conditionalWriteError(err error) conditionalWriteResult {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return conditionalWriteOther
	}
	switch apiErr.ErrorCode() {
	case "PreconditionFailed":
		return conditionalWriteExists
	case "ConditionalRequestConflict":
		return conditionalWriteConflict
	default:
		return conditionalWriteOther
	}
}

func (c *Store) multipartThreshold() int64 {
	if c.multipartThresholdBytes > 0 {
		return c.multipartThresholdBytes
	}
	return s3MultipartThresholdBytes
}

func (c *Store) multipartPartSize(size int64) int64 {
	partSize := c.multipartPartSizeBytes
	if partSize <= 0 {
		partSize = s3MultipartPartSizeBytes
	}
	minPartSize := (size + s3MultipartMaxParts - 1) / s3MultipartMaxParts
	if partSize < minPartSize {
		return minPartSize
	}
	return partSize
}

func (c *Store) Stat(ctx context.Context, digest string) (cas.Object, error) {
	key, err := c.objectKey(digest)
	if err != nil {
		return cas.Object{}, err
	}
	output, err := c.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return cas.Object{}, err
	}
	return cas.Object{
		Digest:    digest,
		SizeBytes: aws.ToInt64(output.ContentLength),
		Key:       key,
		MediaType: aws.ToString(output.ContentType),
	}, nil
}

func (c *Store) Get(ctx context.Context, digest string) (io.ReadCloser, error) {
	key, err := c.objectKey(digest)
	if err != nil {
		return nil, err
	}
	output, err := c.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	return cas.NewVerifyingReadCloser(output.Body, digest), nil
}

func (c *Store) Delete(ctx context.Context, digest string) error {
	key, err := c.objectKey(digest)
	if err != nil {
		return err
	}
	_, err = c.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	return err
}

func objectTagging(mediaType string) string {
	if strings.TrimSpace(mediaType) == archive.SourceMediaType {
		return ""
	}
	return url.QueryEscape(cas.ExpirableTagKey) + "=" + url.QueryEscape(cas.ExpirableTagValue)
}

type s3Stage struct {
	store *Store
	*cas.FileStage
}

func (s *s3Stage) Commit(ctx context.Context) (cas.Object, error) {
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(s.Path())
		}
	}()
	digest, err := s.BeginCommit(ctx, false)
	if err != nil {
		return cas.Object{}, err
	}
	key, err := s.store.objectKey(digest)
	if err != nil {
		return cas.Object{}, err
	}
	if err := s.store.uploadFile(ctx, key, s.MediaType(), s.Path(), s.Size()); err != nil {
		return cas.Object{}, err
	}
	_ = os.Remove(s.Path())
	cleanup = false
	return cas.Object{Digest: digest, SizeBytes: s.Size(), Key: key, MediaType: s.MediaType()}, nil
}

var _ cas.Store = (*Store)(nil)

var (
	errImmutableObjectExists   = errors.New("immutable object already exists")
	errImmutableObjectConflict = errors.New("immutable object publication conflict")
)

func (c *ImmutableStore) Publish(
	ctx context.Context,
	expected cas.Descriptor,
	file *os.File,
) (cas.Object, error) {
	if ctx == nil {
		return cas.Object{}, errors.New("immutable publication context is nil")
	}
	if err := ctx.Err(); err != nil {
		return cas.Object{}, err
	}
	if err := cas.ValidateDescriptor(expected); err != nil {
		return cas.Object{}, err
	}
	before, err := cas.InspectPublishedFile(file)
	if err != nil {
		return cas.Object{}, err
	}
	if before.Size() != expected.SizeBytes {
		return cas.Object{}, fmt.Errorf(
			"published file size = %d, want %d",
			before.Size(),
			expected.SizeBytes,
		)
	}
	if err := cas.VerifyDescriptorFile(ctx, expected, file); err != nil {
		return cas.Object{}, err
	}
	key, err := c.store.objectKey(expected.Digest)
	if err != nil {
		return cas.Object{}, err
	}

	var uploadErr error
	for range immutablePublishAttempts {
		uploadErr = c.store.uploadDescriptor(ctx, key, expected, file)
		if !errors.Is(uploadErr, errImmutableObjectConflict) {
			break
		}
	}
	var object cas.Object
	if errors.Is(uploadErr, errImmutableObjectExists) {
		object, err = c.Stat(ctx, expected.Digest)
		if err != nil {
			return cas.Object{}, fmt.Errorf("stat existing immutable object: %w", err)
		}
		if object.SizeBytes != expected.SizeBytes ||
			object.MediaType != expected.MediaType {
			return cas.Object{}, fmt.Errorf(
				"immutable object %s metadata differs from published content",
				expected.Digest,
			)
		}
	} else if uploadErr != nil {
		return cas.Object{}, uploadErr
	} else {
		object = cas.Object{
			Digest:    expected.Digest,
			SizeBytes: expected.SizeBytes,
			Key:       key,
			MediaType: expected.MediaType,
		}
	}
	after, err := cas.InspectPublishedFile(file)
	if err != nil {
		return cas.Object{}, fmt.Errorf("inspect published file after upload: %w", err)
	}
	if before != after {
		return cas.Object{}, errors.New("published file identity changed during upload")
	}
	return object, nil
}

func (c *ImmutableStore) Stat(ctx context.Context, digest string) (cas.Object, error) {
	return c.store.Stat(ctx, digest)
}

func (c *ImmutableStore) Get(ctx context.Context, digest string) (io.ReadCloser, error) {
	return c.store.Get(ctx, digest)
}

var _ cas.ImmutableStore = (*ImmutableStore)(nil)

func (c *Store) objectKey(digest string) (string, error) {
	if c.sharded {
		return cas.ShardedObjectKey(c.prefix, digest)
	}
	return cas.ObjectKey(c.prefix, digest)
}
