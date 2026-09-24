package s3

import (
	"bytes"
	"context"
	"io"
	"math"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

type rangeClient struct {
	s3Client
	output *awss3.GetObjectOutput
	input  *awss3.GetObjectInput
	calls  int
}

func (c *rangeClient) GetObject(_ context.Context, in *awss3.GetObjectInput, _ ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	c.calls++
	c.input = in
	return c.output, nil
}

type rangeBody struct {
	*bytes.Reader
	closed bool
}

func (b *rangeBody) Close() error { b.closed = true; return nil }
func TestGetRange(t *testing.T) {
	digest := sha256sum.DigestBytes([]byte("whole object"))
	for _, name := range []string{"valid", "ignored range", "wrong total", "wrong start", "wrong length", "missing length", "nil body", "nil response"} {
		t.Run(name, func(t *testing.T) {
			body := &rangeBody{Reader: bytes.NewReader([]byte("abc"))}
			output := &awss3.GetObjectOutput{Body: body, ContentLength: aws.Int64(3), ContentRange: aws.String("bytes 2-4/10")}
			switch name {
			case "ignored range":
				output.ContentRange = nil
			case "wrong total":
				output.ContentRange = aws.String("bytes 2-4/11")
			case "wrong start":
				output.ContentRange = aws.String("bytes 3-5/10")
			case "wrong length":
				output.ContentLength = aws.Int64(4)
			case "missing length":
				output.ContentLength = nil
			case "nil body":
				output.Body = nil
			case "nil response":
				output = nil
			}
			client := &rangeClient{output: output}
			store := &Store{client: client, bucket: "bucket", prefix: "cas"}
			got, err := store.GetRange(t.Context(), digest, 10, 2, 3)
			if client.calls != 1 || aws.ToString(client.input.Range) != "bytes=2-4" || aws.ToString(client.input.Bucket) != "bucket" {
				t.Fatal("wrong range request")
			}
			key, _ := store.objectKey(digest)
			if aws.ToString(client.input.Key) != key {
				t.Fatal("wrong object identity")
			}
			if name == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				b, err := io.ReadAll(got)
				if err != nil || string(b) != "abc" {
					t.Fatal("wrong range body")
				}
				_ = got.Close()
				if !body.closed {
					t.Fatal("body leaked")
				}
				return
			}
			if err == nil || got != nil {
				t.Fatal("invalid response accepted")
			}
			if output != nil && output.Body != nil && (!body.closed || body.Len() != 3) {
				t.Fatal("rejected body read or leaked")
			}
		})
	}
	client := &rangeClient{}
	store := &Store{client: client}
	for _, bounds := range [][3]int64{{0, 0, 1}, {10, -1, 1}, {10, 0, 0}, {10, 9, 2}, {math.MaxInt64, math.MaxInt64, 1}} {
		if _, err := store.GetRange(t.Context(), digest, bounds[0], bounds[1], bounds[2]); err == nil {
			t.Fatal("invalid bounds")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.GetRange(ctx, digest, 10, 0, 1); err == nil || client.calls != 0 {
		t.Fatal("invalid or cancelled request performed IO")
	}
}
