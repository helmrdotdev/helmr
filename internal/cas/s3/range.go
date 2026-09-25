package s3

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"io"
	"io/fs"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/helmrdotdev/helmr/internal/cas"
)

// GetRange requires the exact range and total object size in the response. It
// never substitutes a full GET. Callers authenticate the returned ciphertext;
// a range alone cannot certify the full content-addressed object digest.
func (c *Store) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := cas.ValidateRange(size, offset, length); err != nil {
		return nil, err
	}
	key, err := c.objectKey(digest)
	if err != nil {
		return nil, err
	}
	end := offset + length - 1
	output, err := c.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key), Range: aws.String(fmt.Sprintf("bytes=%d-%d", offset, end))})
	if err != nil {
		var missing *types.NoSuchKey
		if errors.As(err, &missing) {
			return nil, errors.Join(fs.ErrNotExist, err)
		}
		return nil, err
	}
	if output == nil {
		return nil, errors.New("missing object range response")
	}
	if output.Body == nil || output.ContentLength == nil || *output.ContentLength != length || aws.ToString(output.ContentRange) != fmt.Sprintf("bytes %d-%d/%d", offset, end, size) {
		if output.Body != nil {
			_ = output.Body.Close()
		}
		return nil, errors.New("object range response does not match requested identity")
	}
	return output.Body, nil
}

func (c *ImmutableStore) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	return c.store.GetRange(ctx, digest, size, offset, length)
}
