package cas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
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
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	UploadPart(context.Context, *s3.UploadPartInput, ...func(*s3.Options)) (*s3.UploadPartOutput, error)
	CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput, ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
}

type S3 struct {
	client  s3Client
	bucket  string
	prefix  string
	tempDir string
	sharded bool

	multipartThresholdBytes int64
	multipartPartSizeBytes  int64
}

type ImmutableS3 struct {
	store *S3
}

type S3Option func(*S3)

func WithS3TempDir(path string) S3Option {
	return func(store *S3) {
		store.tempDir = strings.TrimSpace(path)
	}
}

func WithS3ShardedKeys() S3Option {
	return func(store *S3) {
		store.sharded = true
	}
}

func NewS3(ctx context.Context, rawURI string, opts ...S3Option) (*S3, error) {
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
	client := s3.NewFromConfig(cfg, func(options *s3.Options) {
		if endpoint := uri.Query().Get("endpoint"); endpoint != "" {
			options.BaseEndpoint = aws.String(endpoint)
			options.UsePathStyle = true
		}
	})
	store := &S3{
		client: client,
		bucket: uri.Host,
		prefix: strings.Trim(uri.Path, "/"),
	}
	for _, opt := range opts {
		opt(store)
	}
	return store, nil
}

func NewImmutableS3(ctx context.Context, rawURI string, opts ...S3Option) (*ImmutableS3, error) {
	store, err := NewS3(ctx, rawURI, opts...)
	if err != nil {
		return nil, err
	}
	return &ImmutableS3{store: store}, nil
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

func (c *S3) Put(ctx context.Context, mediaType string, body io.Reader) (Object, error) {
	stage, err := c.Stage(ctx, mediaType)
	if err != nil {
		return Object{}, err
	}
	return putStage(ctx, stage, body)
}

func (c *S3) Stage(ctx context.Context, mediaType string) (Stage, error) {
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
	return &s3Stage{store: c, stageFile: newStageFile(mediaType, tmp)}, nil
}

func (c *S3) uploadFile(ctx context.Context, key, mediaType, path string, size int64) error {
	if size < c.multipartThreshold() {
		return c.putObject(ctx, key, mediaType, path, size)
	}
	return c.putMultipartObject(ctx, key, mediaType, path, size)
}

func (c *S3) uploadDescriptor(
	ctx context.Context,
	key string,
	expected Descriptor,
	file *os.File,
) error {
	if expected.SizeBytes < c.multipartThreshold() {
		return c.putDescriptor(ctx, key, expected, file)
	}
	return c.putMultipartDescriptor(ctx, key, expected, file)
}

func (c *S3) putDescriptor(
	ctx context.Context,
	key string,
	expected Descriptor,
	file *os.File,
) error {
	_, err := c.client.PutObject(ctx, &s3.PutObjectInput{
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

func (c *S3) putMultipartDescriptor(
	ctx context.Context,
	key string,
	expected Descriptor,
	file *os.File,
) error {
	created, err := c.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
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
			part, err := c.client.UploadPart(groupCtx, &s3.UploadPartInput{
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
	_, err = c.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
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

func (c *S3) abortMultipartUpload(ctx context.Context, key, uploadID string) error {
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s3MultipartAbortTimeout)
	defer cancel()
	_, err := c.client.AbortMultipartUpload(abortCtx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
	if err != nil {
		return fmt.Errorf("abort multipart upload %q: %w", uploadID, err)
	}
	return nil
}

func (c *S3) putObject(ctx context.Context, key, mediaType, path string, size int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	input := &s3.PutObjectInput{
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

func (c *S3) putMultipartObject(ctx context.Context, key, mediaType, path string, size int64) error {
	createInput := &s3.CreateMultipartUploadInput{
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
			_, _ = c.client.AbortMultipartUpload(context.Background(), &s3.AbortMultipartUploadInput{
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
			part, err := c.client.UploadPart(groupCtx, &s3.UploadPartInput{
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
	completeInput := &s3.CompleteMultipartUploadInput{
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

func (c *S3) multipartThreshold() int64 {
	if c.multipartThresholdBytes > 0 {
		return c.multipartThresholdBytes
	}
	return s3MultipartThresholdBytes
}

func (c *S3) multipartPartSize(size int64) int64 {
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

func (c *S3) Stat(ctx context.Context, digest string) (Object, error) {
	key, err := c.objectKey(digest)
	if err != nil {
		return Object{}, err
	}
	output, err := c.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return Object{}, err
	}
	return Object{
		Digest:    digest,
		SizeBytes: aws.ToInt64(output.ContentLength),
		Key:       key,
		MediaType: aws.ToString(output.ContentType),
	}, nil
}

func (c *S3) Get(ctx context.Context, digest string) (io.ReadCloser, error) {
	key, err := c.objectKey(digest)
	if err != nil {
		return nil, err
	}
	output, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	return newVerifyingReadCloser(output.Body, digest), nil
}

func (c *S3) Delete(ctx context.Context, digest string) error {
	key, err := c.objectKey(digest)
	if err != nil {
		return err
	}
	_, err = c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	return err
}

func objectTagging(mediaType string) string {
	if strings.TrimSpace(mediaType) == DeploymentSourceArtifactMediaType {
		return ""
	}
	return url.QueryEscape(ExpirableTagKey) + "=" + url.QueryEscape(ExpirableTagValue)
}

type s3Stage struct {
	store *S3
	*stageFile
}

func (s *s3Stage) Commit(ctx context.Context) (Object, error) {
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(s.path)
		}
	}()
	digest, err := s.beginCommit(ctx, false)
	if err != nil {
		return Object{}, err
	}
	key, err := s.store.objectKey(digest)
	if err != nil {
		return Object{}, err
	}
	if err := s.store.uploadFile(ctx, key, s.mediaType, s.path, s.size); err != nil {
		return Object{}, err
	}
	_ = os.Remove(s.path)
	cleanup = false
	return Object{Digest: digest, SizeBytes: s.size, Key: key, MediaType: s.mediaType}, nil
}

var _ Store = (*S3)(nil)

var (
	errImmutableObjectExists   = errors.New("immutable object already exists")
	errImmutableObjectConflict = errors.New("immutable object publication conflict")
)

func (c *ImmutableS3) Publish(
	ctx context.Context,
	expected Descriptor,
	file *os.File,
) (Object, error) {
	if ctx == nil {
		return Object{}, errors.New("immutable publication context is nil")
	}
	if err := ctx.Err(); err != nil {
		return Object{}, err
	}
	if err := validateDescriptor(expected); err != nil {
		return Object{}, err
	}
	before, err := inspectPublishedFile(file)
	if err != nil {
		return Object{}, err
	}
	if before.size != expected.SizeBytes {
		return Object{}, fmt.Errorf(
			"published file size = %d, want %d",
			before.size,
			expected.SizeBytes,
		)
	}
	if err := verifyDescriptorFile(ctx, expected, file); err != nil {
		return Object{}, err
	}
	key, err := c.store.objectKey(expected.Digest)
	if err != nil {
		return Object{}, err
	}

	var uploadErr error
	for attempt := 0; attempt < immutablePublishAttempts; attempt++ {
		uploadErr = c.store.uploadDescriptor(ctx, key, expected, file)
		if !errors.Is(uploadErr, errImmutableObjectConflict) {
			break
		}
	}
	var object Object
	if errors.Is(uploadErr, errImmutableObjectExists) {
		object, err = c.Stat(ctx, expected.Digest)
		if err != nil {
			return Object{}, fmt.Errorf("stat existing immutable object: %w", err)
		}
		if object.SizeBytes != expected.SizeBytes ||
			object.MediaType != expected.MediaType {
			return Object{}, fmt.Errorf(
				"immutable object %s metadata differs from published content",
				expected.Digest,
			)
		}
	} else if uploadErr != nil {
		return Object{}, uploadErr
	} else {
		object = Object{
			Digest:    expected.Digest,
			SizeBytes: expected.SizeBytes,
			Key:       key,
			MediaType: expected.MediaType,
		}
	}
	after, err := inspectPublishedFile(file)
	if err != nil {
		return Object{}, fmt.Errorf("inspect published file after upload: %w", err)
	}
	if before != after {
		return Object{}, errors.New("published file identity changed during upload")
	}
	return object, nil
}

func (c *ImmutableS3) Stat(ctx context.Context, digest string) (Object, error) {
	return c.store.Stat(ctx, digest)
}

func (c *ImmutableS3) Get(ctx context.Context, digest string) (io.ReadCloser, error) {
	return c.store.Get(ctx, digest)
}

var _ ImmutableStore = (*ImmutableS3)(nil)

func validateDescriptor(expected Descriptor) error {
	hash, ok := strings.CutPrefix(expected.Digest, "sha256:")
	if !ok || len(hash) != sha256.Size*2 {
		return errors.New("immutable descriptor digest is not a lowercase SHA-256 digest")
	}
	for _, character := range hash {
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'f') {
			return errors.New("immutable descriptor digest is not a lowercase SHA-256 digest")
		}
	}
	if expected.SizeBytes < 1 {
		return errors.New("immutable descriptor size must be positive")
	}
	if expected.MediaType == "" || strings.TrimSpace(expected.MediaType) != expected.MediaType {
		return errors.New("immutable descriptor media type is invalid")
	}
	return nil
}

func verifyDescriptorFile(
	ctx context.Context,
	expected Descriptor,
	file *os.File,
) error {
	reader := io.NewSectionReader(file, 0, expected.SizeBytes+1)
	digest := sha256.New()
	buffer := make([]byte, 128<<10)
	var sizeBytes int64
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("hash immutable file: %w", err)
		}
		count, readErr := reader.Read(buffer)
		if count > 0 {
			if _, err := digest.Write(buffer[:count]); err != nil {
				return fmt.Errorf("hash immutable file: %w", err)
			}
			sizeBytes += int64(count)
			if sizeBytes > expected.SizeBytes {
				return fmt.Errorf(
					"immutable file exceeds expected size %d",
					expected.SizeBytes,
				)
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return fmt.Errorf("read immutable file: %w", readErr)
			}
			break
		}
		if count == 0 {
			return io.ErrNoProgress
		}
	}
	if sizeBytes != expected.SizeBytes {
		return fmt.Errorf(
			"immutable file size = %d, want %d",
			sizeBytes,
			expected.SizeBytes,
		)
	}
	actual := "sha256:" + hex.EncodeToString(digest.Sum(nil))
	if actual != expected.Digest {
		return fmt.Errorf(
			"immutable file digest = %s, want %s",
			actual,
			expected.Digest,
		)
	}
	return nil
}

func (c *S3) objectKey(digest string) (string, error) {
	if c.sharded {
		return ShardedObjectKey(c.prefix, digest)
	}
	return ObjectKey(c.prefix, digest)
}

type verifyingReadCloser struct {
	body     io.ReadCloser
	hash     hash.Hash
	expected string
	eof      bool
	closed   bool
	err      error
}

func newVerifyingReadCloser(body io.ReadCloser, expected string) io.ReadCloser {
	return &verifyingReadCloser{
		body:     body,
		hash:     sha256.New(),
		expected: expected,
	}
}

func (r *verifyingReadCloser) Read(p []byte) (int, error) {
	n, readErr := r.body.Read(p)
	if n > 0 {
		_, _ = r.hash.Write(p[:n])
	}
	if errors.Is(readErr, io.EOF) {
		r.eof = true
		if err := r.verify(); err != nil {
			return n, err
		}
	}
	return n, readErr
}

func (r *verifyingReadCloser) Close() error {
	if r.closed {
		return r.err
	}
	r.closed = true
	var drainErr error
	if !r.eof {
		_, drainErr = io.Copy(r.hash, r.body)
		if drainErr == nil {
			r.eof = true
		}
	}
	closeErr := r.body.Close()
	verifyErr := r.verify()
	r.err = errors.Join(drainErr, closeErr, verifyErr)
	return r.err
}

func (r *verifyingReadCloser) verify() error {
	if r.err != nil {
		return r.err
	}
	actual := "sha256:" + hex.EncodeToString(r.hash.Sum(nil))
	if actual != r.expected {
		r.err = fmt.Errorf(
			"%w: expected %s, got %s",
			ErrDigestMismatch,
			r.expected,
			actual,
		)
	}
	return r.err
}
