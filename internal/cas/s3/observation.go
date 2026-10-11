//go:build !computerproof

package s3

import awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

// The ordinary worker has no benchmark transport or diagnostic control surface.
func observeNativeHTTP(*awss3.Options, string, string) {}
