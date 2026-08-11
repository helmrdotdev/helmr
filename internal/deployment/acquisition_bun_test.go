package deployment

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const (
	testBunVersion     = "1.3.10"
	testBunRedirectURL = "https://release-assets.githubusercontent.com/github-production-release-asset/fixture?sp=r&sig=fixture"
)

func TestAcquireBunAcceptsExactReleaseAssetRedirect(t *testing.T) {
	distribution := testBunDistribution(t)
	origin := testBunOrigin(t)
	metadata := testBunMetadata(origin, digestBytes(distribution))
	acquirer := PlatformAcquirer{HTTP: testBunClient(t, metadata, distribution, testBunRedirectURL)}

	acquisition, err := acquirer.acquireBun(
		context.Background(),
		t.TempDir(),
		PackageManager{Name: PackageManagerBun, Version: testBunVersion},
		testBunPolicy(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer acquisition.Close()

	if acquisition.distribution.source.Origin != origin {
		t.Fatalf("Bun source origin = %q, want %q", acquisition.distribution.source.Origin, origin)
	}
	if len(acquisition.distribution.redirects) != 1 ||
		acquisition.distribution.redirects[0] != testBunRedirectURL {
		t.Fatalf("Bun redirects = %v, want [%s]", acquisition.distribution.redirects, testBunRedirectURL)
	}
	entrypoint := filepath.Join(acquisition.root, "bun")
	info, err := os.Stat(entrypoint)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("Bun entrypoint mode = %s", info.Mode())
	}
}

func TestAcquireBunRejectsRedirectAuthorityEscapes(t *testing.T) {
	distribution := testBunDistribution(t)
	origin := testBunOrigin(t)
	metadata := testBunMetadata(origin, digestBytes(distribution))
	redirects := map[string]string{
		"sibling host":   "https://media.githubusercontent.com/oven-sh/bun/fixture.zip?sig=fixture",
		"userinfo":       "https://user@release-assets.githubusercontent.com/fixture.zip?sig=fixture",
		"port":           "https://release-assets.githubusercontent.com:443/fixture.zip?sig=fixture",
		"fragment":       "https://release-assets.githubusercontent.com/fixture.zip?sig=fixture#fragment",
		"HTTP downgrade": "http://release-assets.githubusercontent.com/fixture.zip?sig=fixture",
	}
	for name, redirect := range redirects {
		t.Run(name, func(t *testing.T) {
			acquirer := PlatformAcquirer{HTTP: testBunClient(t, metadata, distribution, redirect)}
			_, err := acquirer.acquireBun(
				context.Background(),
				t.TempDir(),
				PackageManager{Name: PackageManagerBun, Version: testBunVersion},
				testBunPolicy(),
			)
			requirePlatformAcquisitionReason(t, err, workerapi.PlatformAcquisitionOriginRejected)
		})
	}
}

func TestAcquireBunRetainsOfficialDigestAuthorityAcrossRedirect(t *testing.T) {
	distribution := testBunDistribution(t)
	origin := testBunOrigin(t)
	metadata := testBunMetadata(origin, "sha256:"+strings.Repeat("0", 64))
	acquirer := PlatformAcquirer{HTTP: testBunClient(t, metadata, distribution, testBunRedirectURL)}
	_, err := acquirer.acquireBun(
		context.Background(),
		t.TempDir(),
		PackageManager{Name: PackageManagerBun, Version: testBunVersion},
		testBunPolicy(),
	)
	requirePlatformAcquisitionReason(t, err, workerapi.PlatformAcquisitionIntegrityFailed)
}

func TestAcquireBunRejectsInvalidReleaseMetadata(t *testing.T) {
	origin := testBunOrigin(t)
	validDigest := "sha256:" + strings.Repeat("a", 64)
	tests := map[string]string{
		"wrong tag": fmt.Sprintf(
			`{"tag_name":"bun-v0.0.0","assets":[{"name":"bun-linux-x64-baseline.zip","browser_download_url":%q,"digest":%q}]}`,
			origin,
			validDigest,
		),
		"wrong asset": fmt.Sprintf(
			`{"tag_name":"bun-v%s","assets":[{"name":"bun-linux-x64.zip","browser_download_url":%q,"digest":%q}]}`,
			testBunVersion,
			origin,
			validDigest,
		),
		"duplicate asset": fmt.Sprintf(
			`{"tag_name":"bun-v%s","assets":[{"name":"bun-linux-x64-baseline.zip","browser_download_url":%q,"digest":%q},{"name":"bun-linux-x64-baseline.zip","browser_download_url":%q,"digest":%q}]}`,
			testBunVersion,
			origin,
			validDigest,
			origin,
			validDigest,
		),
		"wrong browser URL": fmt.Sprintf(
			`{"tag_name":"bun-v%s","assets":[{"name":"bun-linux-x64-baseline.zip","browser_download_url":"https://example.com/bun.zip","digest":%q}]}`,
			testBunVersion,
			validDigest,
		),
		"missing digest": fmt.Sprintf(
			`{"tag_name":"bun-v%s","assets":[{"name":"bun-linux-x64-baseline.zip","browser_download_url":%q,"digest":""}]}`,
			testBunVersion,
			origin,
		),
		"non-SHA-256 digest": fmt.Sprintf(
			`{"tag_name":"bun-v%s","assets":[{"name":"bun-linux-x64-baseline.zip","browser_download_url":%q,"digest":"sha512:fixture"}]}`,
			testBunVersion,
			origin,
		),
	}
	for name, metadata := range tests {
		t.Run(name, func(t *testing.T) {
			acquirer := PlatformAcquirer{HTTP: testBunMetadataClient(t, metadata)}
			_, err := acquirer.acquireBun(
				context.Background(),
				t.TempDir(),
				PackageManager{Name: PackageManagerBun, Version: testBunVersion},
				testBunPolicy(),
			)
			requirePlatformAcquisitionReason(t, err, workerapi.PlatformAcquisitionIntegrityFailed)
		})
	}
}

func TestFetchUpstreamRejectsQueryOnInitialURL(t *testing.T) {
	client := &http.Client{Transport: upstreamRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("initial URL with query reached the transport")
		return nil, nil
	})}
	_, err := fetchUpstream(
		context.Background(),
		client,
		t.TempDir(),
		testBunOrigin(t)+"?sig=fixture",
		testBunPolicy().AllowedRedirectHosts,
		maxBunDistributionBytes,
	)
	requirePlatformAcquisitionReason(t, err, workerapi.PlatformAcquisitionOriginRejected)
}

func testBunPolicy() ManagerPolicy {
	return ManagerPolicy{
		AdapterVersion:       ManagerAdapterVersion,
		AllowedOrigin:        BunReleaseOrigin,
		AllowedRedirectHosts: []string{"api.github.com", "github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com"},
		Domain:               VersionDomain{Major: 1, Minimum: testBunVersion},
		MetadataOrigin:       BunMetadataOrigin,
		Name:                 PackageManagerBun,
	}
}

func testBunOrigin(t *testing.T) string {
	t.Helper()
	origin, err := ManagerSourceOrigin(PackageManager{Name: PackageManagerBun, Version: testBunVersion})
	if err != nil {
		t.Fatal(err)
	}
	return origin
}

func testBunMetadata(origin, digest string) string {
	return fmt.Sprintf(
		`{"tag_name":"bun-v%s","assets":[{"name":"bun-linux-x64-baseline.zip","browser_download_url":%q,"digest":%q}]}`,
		testBunVersion,
		origin,
		digest,
	)
}

func testBunDistribution(t *testing.T) []byte {
	t.Helper()
	var raw bytes.Buffer
	writer := zip.NewWriter(&raw)
	header := &zip.FileHeader{Name: "bun-linux-x64-baseline/bun", Method: zip.Store}
	header.SetMode(0o755)
	entry, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("test Bun executable")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func testBunClient(t *testing.T, metadata string, distribution []byte, redirect string) *http.Client {
	t.Helper()
	origin := testBunOrigin(t)
	metadataURL := BunMetadataOrigin + "bun-v" + testBunVersion
	return &http.Client{Transport: upstreamRoundTripper(func(request *http.Request) (*http.Response, error) {
		switch request.URL.String() {
		case metadataURL:
			return testUpstreamResponse(request, http.StatusOK, nil, []byte(metadata)), nil
		case origin:
			return testUpstreamResponse(
				request,
				http.StatusFound,
				http.Header{"Location": []string{redirect}},
				nil,
			), nil
		case redirect:
			return testUpstreamResponse(request, http.StatusOK, nil, distribution), nil
		default:
			t.Fatalf("unexpected Bun request URL %q", request.URL.String())
			return nil, nil
		}
	})}
}

func testBunMetadataClient(t *testing.T, metadata string) *http.Client {
	t.Helper()
	metadataURL := BunMetadataOrigin + "bun-v" + testBunVersion
	return &http.Client{Transport: upstreamRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != metadataURL {
			t.Fatalf("unexpected request after invalid Bun metadata: %q", request.URL.String())
		}
		return testUpstreamResponse(request, http.StatusOK, nil, []byte(metadata)), nil
	})}
}

func testUpstreamResponse(
	request *http.Request,
	status int,
	header http.Header,
	body []byte,
) *http.Response {
	return &http.Response{
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Header:        header.Clone(),
		Request:       request,
		StatusCode:    status,
	}
}

func requirePlatformAcquisitionReason(
	t *testing.T,
	err error,
	want workerapi.PlatformAcquisitionFailureReason,
) {
	t.Helper()
	if err == nil {
		t.Fatalf("platform acquisition succeeded, want %q", want)
	}
	var value interface {
		PlatformAcquisitionFailureReason() workerapi.PlatformAcquisitionFailureReason
	}
	if !errors.As(err, &value) {
		t.Fatalf("platform acquisition error is not deterministic: %v", err)
	}
	if got := value.PlatformAcquisitionFailureReason(); got != want {
		t.Fatalf("platform acquisition reason = %q, want %q: %v", got, want, err)
	}
}
