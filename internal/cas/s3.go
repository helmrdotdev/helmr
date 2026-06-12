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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"golang.org/x/sync/errgroup"
)

const (
	s3MultipartThresholdBytes    = 64 << 20
	s3MultipartPartSizeBytes     = 64 << 20
	s3MultipartMaxParts          = 10000
	s3MultipartUploadConcurrency = 4
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

	multipartThresholdBytes int64
	multipartPartSizeBytes  int64
}

type S3Option func(*S3)

func WithS3TempDir(path string) S3Option {
	return func(store *S3) {
		store.tempDir = strings.TrimSpace(path)
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
	_, err = c.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: parts,
		},
	})
	if err != nil {
		return err
	}
	completed = true
	return nil
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
	key, err := ObjectKey(c.prefix, digest)
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
	key, err := ObjectKey(c.prefix, digest)
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
	key, err := ObjectKey(c.prefix, digest)
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
	key, err := ObjectKey(s.store.prefix, digest)
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
		r.err = fmt.Errorf("cas object digest mismatch: expected %s, got %s", r.expected, actual)
	}
	return r.err
}
