//go:build linux && computerproof

package controlplane

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

func TestNativeRemoteFootprintMultipartTransition(t *testing.T) {
	for _, mode := range []string{"completed", "persistent-race", "access-denied"} {
		t.Run(mode, func(t *testing.T) {
			var versions atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/xml")
				switch {
				case r.URL.Query().Has("versions"):
					versions.Add(1)
					version := ""
					if mode == "completed" && versions.Load() >= 2 {
						version = "<Version><Key>prefix/object</Key><VersionId>v1</VersionId><Size>11</Size><IsLatest>true</IsLatest></Version>"
					}
					fmt.Fprintf(w, "<ListVersionsResult><IsTruncated>false</IsTruncated>%s</ListVersionsResult>", version)
				case r.URL.Query().Has("uploads"):
					upload := "<Upload><Key>prefix/object</Key><UploadId>pending</UploadId></Upload>"
					if mode == "completed" && versions.Load() >= 2 {
						upload = ""
					}
					fmt.Fprintf(w, "<ListMultipartUploadsResult><IsTruncated>false</IsTruncated>%s</ListMultipartUploadsResult>", upload)
				case r.URL.Query().Get("uploadId") == "pending":
					code, status := "NoSuchUpload", http.StatusNotFound
					if mode == "access-denied" {
						code, status = "AccessDenied", http.StatusForbidden
					}
					w.WriteHeader(status)
					fmt.Fprintf(w, "<Error><Code>%s</Code><Message>fixture</Message></Error>", code)
				default:
					http.Error(w, "unexpected request", http.StatusBadRequest)
				}
			}))
			defer server.Close()
			store := awss3.New(awss3.Options{Region: "us-east-1", Credentials: aws.AnonymousCredentials{}, BaseEndpoint: aws.String(server.URL), UsePathStyle: true, RetryMaxAttempts: 1})
			got, err := nativeRemoteFootprint(t.Context(), store, "bucket", "prefix/")
			if mode == "completed" {
				if err != nil || got.Versions != 1 || got.VersionBytes != 11 || got.CurrentBytes != 11 || got.MultipartUploads != 0 || got.MultipartBytes != 0 || got.InventoryAttempts != 2 || got.MultipartRaces != 1 || got.ObserverPageCalls != 5 {
					t.Fatalf("completed transition: %+v, %v", got, err)
				}
				return
			}
			var apiError smithy.APIError
			code, attempts, races, calls := "NoSuchUpload", int64(3), int64(3), int64(9)
			if mode == "access-denied" {
				code, attempts, races, calls = "AccessDenied", 1, 0, 3
			}
			if !errors.As(err, &apiError) || apiError.ErrorCode() != code || got.InventoryAttempts != attempts || got.MultipartRaces != races || got.ObserverPageCalls != calls {
				t.Fatalf("failed observation: %+v, %v", got, err)
			}
		})
	}
}
