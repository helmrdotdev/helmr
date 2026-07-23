package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
)

type managerRoundTrip func(*http.Request) (*http.Response, error)

func (f managerRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type managerCancelBody struct {
	ctx  context.Context
	data []byte
	sent bool
}

func (b *managerCancelBody) Read(destination []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(destination, b.data), nil
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (*managerCancelBody) Close() error {
	return nil
}

func TestManagerDownloaderNPM(t *testing.T) {
	archive := []byte("npm archive")
	sha512Sum := sha512.Sum512(archive)
	manifest := `{"name":"npm","version":"11.4.2","dist":{"tarball":"https://registry.npmjs.org/npm/-/npm-11.4.2.tgz","integrity":"sha512-` +
		base64.StdEncoding.EncodeToString(sha512Sum[:]) + `"}}`
	client := &http.Client{Transport: managerRoundTrip(func(request *http.Request) (*http.Response, error) {
		switch request.URL.String() {
		case "https://registry.npmjs.org/npm/11.4.2":
			return managerResponse(http.StatusOK, manifest), nil
		case "https://registry.npmjs.org/npm/-/npm-11.4.2.tgz":
			return managerResponse(http.StatusOK, string(archive)), nil
		default:
			t.Fatalf("unexpected request %q", request.URL.String())
			return nil, nil
		}
	})}
	downloader, err := NewManagerDownloader(client)
	if err != nil {
		t.Fatal(err)
	}
	destination := managerDownloadFile(t)
	source, err := downloader.Download(
		context.Background(),
		NewManagerSelector(
			PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
			ArchitectureAArch64,
		),
		destination,
	)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive)
	if source.Digest != "sha256:"+hex.EncodeToString(sum[:]) ||
		source.Origin != "https://registry.npmjs.org/npm/-/npm-11.4.2.tgz" ||
		source.SizeBytes != int64(len(archive)) {
		t.Fatalf("source = %#v", source)
	}
	actual, err := io.ReadAll(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, archive) {
		t.Fatalf("archive = %q, want %q", actual, archive)
	}
}

func TestManagerDownloaderNPMRejectsIntegrityMismatchAndDiscardsBytes(t *testing.T) {
	archive := []byte("retargeted")
	expected := sha512.Sum512([]byte("expected"))
	manifest := `{"name":"npm","version":"11.4.2","dist":{"tarball":"https://registry.npmjs.org/npm/-/npm-11.4.2.tgz","integrity":"sha512-` +
		base64.StdEncoding.EncodeToString(expected[:]) + `"}}`
	client := &http.Client{Transport: managerRoundTrip(func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, ".tgz") {
			return managerResponse(http.StatusOK, string(archive)), nil
		}
		return managerResponse(http.StatusOK, manifest), nil
	})}
	downloader, err := NewManagerDownloader(client)
	if err != nil {
		t.Fatal(err)
	}
	destination := managerDownloadFile(t)
	_, err = downloader.Download(
		context.Background(),
		NewManagerSelector(
			PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
			ArchitectureX8664,
		),
		destination,
	)
	if !errors.Is(err, ErrManagerIntegrity) {
		t.Fatalf("error = %v, want ErrManagerIntegrity", err)
	}
	assertEmptyManagerDownload(t, destination)
}

func TestManagerDownloaderNPMRequiresIntegrity(t *testing.T) {
	manifest := `{"name":"npm","version":"11.4.2","dist":{"tarball":"https://registry.npmjs.org/npm/-/npm-11.4.2.tgz"}}`
	client := &http.Client{Transport: managerRoundTrip(func(_ *http.Request) (*http.Response, error) {
		return managerResponse(http.StatusOK, manifest), nil
	})}
	downloader, err := NewManagerDownloader(client)
	if err != nil {
		t.Fatal(err)
	}
	_, err = downloader.Download(
		context.Background(),
		NewManagerSelector(
			PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
			ArchitectureX8664,
		),
		managerDownloadFile(t),
	)
	if !errors.Is(err, ErrManagerProtocolUnsupported) {
		t.Fatalf("error = %v, want ErrManagerProtocolUnsupported", err)
	}
}

func TestManagerDownloaderTreatsRequestTimeoutAsRetryable(t *testing.T) {
	client := &http.Client{Transport: managerRoundTrip(func(_ *http.Request) (*http.Response, error) {
		return managerResponse(http.StatusRequestTimeout, ""), nil
	})}
	downloader, err := NewManagerDownloader(client)
	if err != nil {
		t.Fatal(err)
	}
	_, err = downloader.Download(
		context.Background(),
		NewManagerSelector(
			PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
			ArchitectureX8664,
		),
		managerDownloadFile(t),
	)
	if err == nil ||
		errors.Is(err, ErrManagerIntegrity) ||
		errors.Is(err, ErrManagerNotFound) ||
		errors.Is(err, ErrManagerProtocolUnsupported) {
		t.Fatalf("error = %v, want retryable infrastructure failure", err)
	}
}

func TestManagerDownloaderCancellationDiscardsPartialBytes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &http.Client{Transport: managerRoundTrip(func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, ".tgz") {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: &managerCancelBody{
					ctx:  request.Context(),
					data: []byte("partial"),
				},
			}, nil
		}
		sum := sha512.Sum512([]byte("partial"))
		manifest := `{"name":"npm","version":"11.4.2","dist":{"tarball":"https://registry.npmjs.org/npm/-/npm-11.4.2.tgz","integrity":"sha512-` +
			base64.StdEncoding.EncodeToString(sum[:]) + `"}}`
		return managerResponse(http.StatusOK, manifest), nil
	})}
	downloader, err := NewManagerDownloader(client)
	if err != nil {
		t.Fatal(err)
	}
	destination := managerDownloadFile(t)
	_, err = downloader.Download(
		ctx,
		NewManagerSelector(
			PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
			ArchitectureX8664,
		),
		destination,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	assertEmptyManagerDownload(t, destination)
}

func TestManagerDownloaderBunSignedRelease(t *testing.T) {
	archive := []byte("bun archive")
	sum := sha256.Sum256(archive)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	origin := "https://github.com/oven-sh/bun/releases/download/bun-v1.3.11/bun-linux-x64-baseline.zip"
	checksumOrigin := "https://github.com/oven-sh/bun/releases/download/bun-v1.3.11/SHASUMS256.txt.asc"
	listing := bunReleaseJSON(
		"1.3.11",
		"bun-linux-x64-baseline.zip",
		origin,
		int64(len(archive)),
		digest,
		&bunAsset{
			Name:   bunChecksumAssetName,
			Size:   9,
			Digest: "",
			URL:    checksumOrigin,
			State:  "uploaded",
		},
	)
	client := &http.Client{Transport: managerRoundTrip(func(request *http.Request) (*http.Response, error) {
		switch request.URL.String() {
		case bunReleaseAPIOriginRoot + "1.3.11":
			return managerResponse(http.StatusOK, listing), nil
		case checksumOrigin:
			return managerResponse(http.StatusOK, "signature"), nil
		case origin:
			return managerResponse(http.StatusOK, string(archive)), nil
		default:
			t.Fatalf("unexpected request %q", request.URL.String())
			return nil, nil
		}
	})}
	downloader, err := NewManagerDownloader(client)
	if err != nil {
		t.Fatal(err)
	}
	downloader.verifyBun = func(raw []byte, asset string) (string, error) {
		if string(raw) != "signature" || asset != "bun-linux-x64-baseline.zip" {
			t.Fatalf("signature input = %q, %q", raw, asset)
		}
		return digest, nil
	}
	source, err := downloader.Download(
		context.Background(),
		NewManagerSelector(
			PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
			ArchitectureX8664,
		),
		managerDownloadFile(t),
	)
	if err != nil {
		t.Fatal(err)
	}
	if source.Digest != digest {
		t.Fatalf("source digest = %q, want %q", source.Digest, digest)
	}
}

func TestManagerDownloaderBunRejectsMissingModernChecksum(t *testing.T) {
	origin := "https://github.com/oven-sh/bun/releases/download/bun-v1.3.11/bun-linux-x64-baseline.zip"
	listing := bunReleaseJSON(
		"1.3.11",
		"bun-linux-x64-baseline.zip",
		origin,
		11,
		"",
		nil,
	)
	client := &http.Client{Transport: managerRoundTrip(func(_ *http.Request) (*http.Response, error) {
		return managerResponse(http.StatusOK, listing), nil
	})}
	downloader, err := NewManagerDownloader(client)
	if err != nil {
		t.Fatal(err)
	}
	_, err = downloader.Download(
		context.Background(),
		NewManagerSelector(
			PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
			ArchitectureX8664,
		),
		managerDownloadFile(t),
	)
	if !errors.Is(err, ErrManagerIntegrity) {
		t.Fatalf("error = %v, want ErrManagerIntegrity", err)
	}
}

func TestVerifyBunSignatureUsesPinnedUpstreamKey(t *testing.T) {
	raw, err := os.ReadFile("testdata/bun-shasums-1.3.11.asc")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := loadBunKeys()
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := verifyBunSignature(raw, keys)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := bunChecksumFor(plaintext, "bun-linux-x64-baseline.zip")
	if err != nil {
		t.Fatal(err)
	}
	if digest != "sha256:abe346f63414547cdf6b35b7a649a490c728b93d006226156923918a84c0e59b" {
		t.Fatalf("digest = %q", digest)
	}
}

func TestManagerDownloaderRejectsBunRedirectOutsideAssetHost(t *testing.T) {
	archive := []byte("bun")
	sum := sha256.Sum256(archive)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	origin := "https://github.com/oven-sh/bun/releases/download/bun-v0.5.2/bun-linux-aarch64.zip"
	listing := bunReleaseJSON(
		"0.5.2",
		"bun-linux-aarch64.zip",
		origin,
		int64(len(archive)),
		digest,
		nil,
	)
	client := &http.Client{Transport: managerRoundTrip(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "api.github.com" {
			return managerResponse(http.StatusOK, listing), nil
		}
		response := managerResponse(http.StatusFound, "")
		response.Header.Set("Location", "https://example.com/retargeted")
		return response, nil
	})}
	downloader, err := NewManagerDownloader(client)
	if err != nil {
		t.Fatal(err)
	}
	_, err = downloader.Download(
		context.Background(),
		NewManagerSelector(
			PackageManager{Name: PackageManagerBun, Version: "0.5.2"},
			ArchitectureAArch64,
		),
		managerDownloadFile(t),
	)
	if !errors.Is(err, ErrManagerIntegrity) {
		t.Fatalf("error = %v, want ErrManagerIntegrity", err)
	}
}

func managerResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func bunReleaseJSON(
	version string,
	assetName string,
	origin string,
	size int64,
	digest string,
	checksum *bunAsset,
) string {
	assets := `[{"name":"` + assetName +
		`","size":` + strconv.FormatInt(size, 10) +
		`,"digest":"` + digest +
		`","browser_download_url":"` + origin +
		`","state":"uploaded"}`
	if checksum != nil {
		assets += `,{"name":"` + checksum.Name +
			`","size":` + strconv.FormatInt(checksum.Size, 10) +
			`,"digest":"` + checksum.Digest +
			`","browser_download_url":"` + checksum.URL +
			`","state":"uploaded"}`
	}
	return `{"tag_name":"bun-v` + version + `","draft":false,"assets":` + assets + `]}`
}

func managerDownloadFile(t *testing.T) *os.File {
	t.Helper()
	file, err := os.OpenFile(
		t.TempDir()+"/download",
		os.O_CREATE|os.O_EXCL|os.O_RDWR,
		0600,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		file.Close()
	})
	return file
}

func assertEmptyManagerDownload(t *testing.T, file *os.File) {
	t.Helper()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	offset, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 || offset != 0 {
		t.Fatalf("download file size/offset = %d/%d, want 0/0", info.Size(), offset)
	}
}
