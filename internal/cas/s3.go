package cas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type S3 struct {
	client *s3.Client
	bucket string
	prefix string
}

func NewS3(ctx context.Context, rawURI string) (*S3, error) {
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
	return &S3{
		client: client,
		bucket: uri.Host,
		prefix: strings.Trim(uri.Path, "/"),
	}, nil
}

func (c *S3) Put(ctx context.Context, mediaType string, body io.Reader) (Object, error) {
	tmp, err := os.CreateTemp("", "helmr-cas-*")
	if err != nil {
		return Object{}, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(tmp, hash), body)
	closeErr := tmp.Close()
	if copyErr != nil {
		return Object{}, copyErr
	}
	if closeErr != nil {
		return Object{}, closeErr
	}
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	key, err := ObjectKey(c.prefix, digest)
	if err != nil {
		return Object{}, err
	}
	file, err := os.Open(tmpPath)
	if err != nil {
		return Object{}, err
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
	if err != nil {
		return Object{}, err
	}
	return Object{Digest: digest, SizeBytes: size, Key: key, MediaType: mediaType}, nil
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
	return output.Body, nil
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

var _ Store = (*S3)(nil)
