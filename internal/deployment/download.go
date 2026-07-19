package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	managerMetadataMaxBytes  = int64(2 << 20)
	managerChecksumMaxBytes  = int64(2 << 20)
	bunSigningFingerprint    = "F3DCC08A8572C0749B3E18888EAB4D40A7B22B59"
	bunReleaseAPIOriginRoot  = "https://api.github.com/repos/oven-sh/bun/releases/tags/bun-v"
	bunChecksumAssetName     = "SHASUMS256.txt.asc"
	npmManifestOriginRoot    = "https://registry.npmjs.org/npm/"
	managerHTTPUserAgent     = "helmr-dependency-builder-v0"
	managerMaxRedirects      = 5
	managerResponseHeaderTTL = 30 * time.Second
	managerDownloadTTL       = 5 * time.Minute
)

var (
	ErrManagerNotFound            = errors.New("manager distribution was not found")
	ErrManagerProtocolUnsupported = errors.New("manager protocol is unsupported")
	ErrManagerIntegrity           = errors.New("manager distribution integrity failed")
)

//go:embed bun.asc
var managerKeys embed.FS

type ManagerDownloader struct {
	client    *http.Client
	bunKeys   openpgp.EntityList
	verifyBun func([]byte, string) (string, error)
}

type bunRelease struct {
	TagName string     `json:"tag_name"`
	Draft   bool       `json:"draft"`
	Assets  []bunAsset `json:"assets"`
}

type bunAsset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
	URL    string `json:"browser_download_url"`
	State  string `json:"state"`
}

type npmManifest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Dist    struct {
		Integrity string `json:"integrity"`
		Tarball   string `json:"tarball"`
	} `json:"dist"`
}

func NewManagerDownloader(client *http.Client) (*ManagerDownloader, error) {
	keys, err := loadBunKeys()
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{
			Transport: &http.Transport{
				ForceAttemptHTTP2:     true,
				ResponseHeaderTimeout: managerResponseHeaderTTL,
				TLSHandshakeTimeout:   10 * time.Second,
			},
		}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &ManagerDownloader{
		client:  &copyClient,
		bunKeys: keys,
		verifyBun: func(raw []byte, asset string) (string, error) {
			plaintext, err := verifyBunSignature(raw, keys)
			if err != nil {
				return "", err
			}
			return bunChecksumFor(plaintext, asset)
		},
	}, nil
}

func (d *ManagerDownloader) Download(
	ctx context.Context,
	selector ManagerSelector,
	destination *os.File,
) (source ManagerSource, returnErr error) {
	if d == nil || d.client == nil || len(d.bunKeys) == 0 || d.verifyBun == nil {
		return ManagerSource{}, errors.New("manager downloader is not initialized")
	}
	if ctx == nil {
		return ManagerSource{}, errors.New("manager download context is nil")
	}
	if err := validateManagerSelector(selector); err != nil {
		return ManagerSource{}, err
	}
	if err := validateManagerAcquireFile(destination, "download destination"); err != nil {
		return ManagerSource{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, managerDownloadTTL)
	defer cancel()
	committed := false
	defer func() {
		if committed {
			return
		}
		if err := resetManagerAcquireFile(destination); err != nil {
			returnErr = errors.Join(
				returnErr,
				fmt.Errorf("discard manager download: %w", err),
			)
		}
	}()

	switch selector.PackageManager.Name {
	case PackageManagerBun:
		source, returnErr = d.downloadBun(ctx, selector, destination)
	case PackageManagerNPM:
		source, returnErr = d.downloadNPM(ctx, selector, destination)
	default:
		return ManagerSource{}, fmt.Errorf(
			"%w: package manager %q",
			ErrManagerProtocolUnsupported,
			selector.PackageManager.Name,
		)
	}
	if returnErr != nil {
		return ManagerSource{}, returnErr
	}
	if _, err := destination.Seek(0, io.SeekStart); err != nil {
		return ManagerSource{}, fmt.Errorf("rewind manager download: %w", err)
	}
	committed = true
	return source, nil
}

func (d *ManagerDownloader) downloadNPM(
	ctx context.Context,
	selector ManagerSelector,
	destination *os.File,
) (ManagerSource, error) {
	manager := selector.PackageManager
	_, _, origin, err := managerDistribution(manager, selector.Architecture)
	if err != nil {
		return ManagerSource{}, fmt.Errorf("%w: %v", ErrManagerProtocolUnsupported, err)
	}
	manifestURL := npmManifestOriginRoot + manager.Version
	raw, err := d.getBytes(ctx, manifestURL, managerMetadataMaxBytes, false)
	if err != nil {
		return ManagerSource{}, err
	}
	if _, err := jsoncanon.Transform(raw); err != nil {
		return ManagerSource{}, fmt.Errorf("%w: npm manifest: %v", ErrManagerIntegrity, err)
	}
	var manifest npmManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return ManagerSource{}, fmt.Errorf("%w: decode npm manifest: %v", ErrManagerIntegrity, err)
	}
	if manifest.Name != "npm" ||
		manifest.Version != manager.Version ||
		manifest.Dist.Tarball != origin {
		return ManagerSource{}, fmt.Errorf(
			"%w: npm manifest does not identify the canonical distribution",
			ErrManagerIntegrity,
		)
	}
	if err := validatePackageIntegrity(manifest.Dist.Integrity); err != nil {
		return ManagerSource{}, fmt.Errorf("%w: npm dist.integrity: %v", ErrManagerProtocolUnsupported, err)
	}
	expected, err := decodeSHA512SRI(manifest.Dist.Integrity)
	if err != nil {
		return ManagerSource{}, fmt.Errorf("%w: npm dist.integrity: %v", ErrManagerProtocolUnsupported, err)
	}
	source, actualSHA512, err := d.downloadArchive(ctx, origin, destination, false)
	if err != nil {
		return ManagerSource{}, err
	}
	if !bytes.Equal(actualSHA512, expected) {
		return ManagerSource{}, fmt.Errorf("%w: npm distribution SHA-512 mismatch", ErrManagerIntegrity)
	}
	return source, nil
}

func (d *ManagerDownloader) downloadBun(
	ctx context.Context,
	selector ManagerSelector,
	destination *os.File,
) (ManagerSource, error) {
	manager := selector.PackageManager
	_, _, origin, err := managerDistribution(manager, selector.Architecture)
	if err != nil {
		return ManagerSource{}, fmt.Errorf("%w: %v", ErrManagerProtocolUnsupported, err)
	}
	assetName := origin[strings.LastIndexByte(origin, '/')+1:]
	listingURL := bunReleaseAPIOriginRoot + manager.Version
	raw, err := d.getBytes(ctx, listingURL, managerMetadataMaxBytes, false)
	if err != nil {
		return ManagerSource{}, err
	}
	if _, err := jsoncanon.Transform(raw); err != nil {
		return ManagerSource{}, fmt.Errorf("%w: Bun release listing: %v", ErrManagerIntegrity, err)
	}
	var release bunRelease
	if err := json.Unmarshal(raw, &release); err != nil {
		return ManagerSource{}, fmt.Errorf("%w: decode Bun release listing: %v", ErrManagerIntegrity, err)
	}
	if release.Draft || release.TagName != "bun-v"+manager.Version {
		return ManagerSource{}, fmt.Errorf(
			"%w: Bun release listing does not identify the canonical release",
			ErrManagerIntegrity,
		)
	}
	distribution, checksum, err := selectBunAssets(release.Assets, assetName, origin, manager.Version)
	if err != nil {
		return ManagerSource{}, err
	}

	var signedDigest string
	if checksum != nil {
		signedRaw, err := d.getBytes(ctx, checksum.URL, managerChecksumMaxBytes, true)
		if err != nil {
			if errors.Is(err, ErrManagerNotFound) {
				return ManagerSource{}, fmt.Errorf(
					"%w: listed Bun checksum asset was not found",
					ErrManagerIntegrity,
				)
			}
			return ManagerSource{}, err
		}
		if err := verifyAssetDigest(signedRaw, checksum.Digest); err != nil {
			return ManagerSource{}, err
		}
		if int64(len(signedRaw)) != checksum.Size {
			return ManagerSource{}, fmt.Errorf("%w: Bun checksum asset size mismatch", ErrManagerIntegrity)
		}
		signedDigest, err = d.verifyBun(signedRaw, assetName)
		if err != nil {
			return ManagerSource{}, fmt.Errorf("%w: Bun checksum: %v", ErrManagerIntegrity, err)
		}
	} else if !bunLegacyTOFU(manager.Version) {
		return ManagerSource{}, fmt.Errorf(
			"%w: Bun release does not list a signed checksum",
			ErrManagerIntegrity,
		)
	}

	source, _, err := d.downloadArchive(ctx, origin, destination, true)
	if err != nil {
		return ManagerSource{}, err
	}
	if signedDigest != "" && source.Digest != signedDigest {
		return ManagerSource{}, fmt.Errorf("%w: Bun signed checksum mismatch", ErrManagerIntegrity)
	}
	if err := verifyAssetDigestFromSource(source, distribution.Digest); err != nil {
		return ManagerSource{}, err
	}
	if distribution.Size > 0 && source.SizeBytes != distribution.Size {
		return ManagerSource{}, fmt.Errorf("%w: Bun release asset size mismatch", ErrManagerIntegrity)
	}
	return source, nil
}

func (d *ManagerDownloader) getBytes(
	ctx context.Context,
	origin string,
	maxBytes int64,
	bunRedirects bool,
) ([]byte, error) {
	response, err := d.get(ctx, origin, bunRedirects)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read manager origin %q: %w", origin, err)
	}
	if int64(len(raw)) == 0 || int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf(
			"%w: manager metadata size is outside [1,%d]",
			ErrManagerProtocolUnsupported,
			maxBytes,
		)
	}
	return raw, nil
}

func (d *ManagerDownloader) downloadArchive(
	ctx context.Context,
	origin string,
	destination *os.File,
	bunRedirects bool,
) (ManagerSource, []byte, error) {
	response, err := d.get(ctx, origin, bunRedirects)
	if err != nil {
		return ManagerSource{}, nil, err
	}
	defer response.Body.Close()
	sha256Hash := sha256.New()
	sha512Hash := sha512.New()
	written, err := io.Copy(
		io.MultiWriter(destination, sha256Hash, sha512Hash),
		io.LimitReader(response.Body, maxManagerDistributionBytes+1),
	)
	if err != nil {
		return ManagerSource{}, nil, fmt.Errorf("read manager distribution %q: %w", origin, err)
	}
	if written < 1 || written > maxManagerDistributionBytes {
		return ManagerSource{}, nil, fmt.Errorf(
			"%w: manager distribution size is outside [1,%d]",
			ErrManagerProtocolUnsupported,
			maxManagerDistributionBytes,
		)
	}
	return ManagerSource{
		Digest:    "sha256:" + hex.EncodeToString(sha256Hash.Sum(nil)),
		Origin:    origin,
		SizeBytes: written,
	}, sha512Hash.Sum(nil), nil
}

func (d *ManagerDownloader) get(
	ctx context.Context,
	origin string,
	bunRedirects bool,
) (*http.Response, error) {
	current, err := url.Parse(origin)
	if err != nil {
		return nil, fmt.Errorf("parse manager origin: %w", err)
	}
	for redirects := 0; ; redirects++ {
		if err := validateManagerURL(current, origin, redirects, bunRedirects); err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("create manager origin request: %w", err)
		}
		request.Header.Set("Accept-Encoding", "identity")
		request.Header.Set("User-Agent", managerHTTPUserAgent)
		response, err := d.client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("request manager origin %q: %w", current.String(), err)
		}
		if response.Header.Get("Content-Encoding") != "" &&
			response.Header.Get("Content-Encoding") != "identity" {
			response.Body.Close()
			return nil, fmt.Errorf("%w: manager origin used content encoding", ErrManagerIntegrity)
		}
		if response.StatusCode >= 300 && response.StatusCode <= 399 {
			location := response.Header.Get("Location")
			response.Body.Close()
			if !bunRedirects || redirects >= managerMaxRedirects || location == "" {
				return nil, fmt.Errorf("%w: manager origin redirect is unsupported", ErrManagerIntegrity)
			}
			next, err := current.Parse(location)
			if err != nil {
				return nil, fmt.Errorf("%w: invalid manager redirect: %v", ErrManagerIntegrity, err)
			}
			current = next
			continue
		}
		switch {
		case response.StatusCode == http.StatusNotFound:
			response.Body.Close()
			return nil, ErrManagerNotFound
		case response.StatusCode == http.StatusRequestTimeout ||
			response.StatusCode == http.StatusTooManyRequests ||
			response.StatusCode == http.StatusForbidden &&
				(response.Header.Get("Retry-After") != "" ||
					response.Header.Get("X-RateLimit-Remaining") == "0") ||
			response.StatusCode >= 500:
			response.Body.Close()
			return nil, fmt.Errorf(
				"manager origin %q returned retryable status %d",
				current.String(),
				response.StatusCode,
			)
		case response.StatusCode != http.StatusOK:
			response.Body.Close()
			return nil, fmt.Errorf(
				"%w: manager origin %q returned status %d",
				ErrManagerIntegrity,
				current.String(),
				response.StatusCode,
			)
		default:
			return response, nil
		}
	}
}

func validateManagerURL(
	current *url.URL,
	origin string,
	redirects int,
	bunRedirects bool,
) error {
	if current == nil ||
		current.Scheme != "https" ||
		current.User != nil ||
		current.Host == "" ||
		current.Port() != "" {
		return fmt.Errorf("%w: manager origin URL is unsupported", ErrManagerIntegrity)
	}
	host := strings.ToLower(current.Hostname())
	if !bunRedirects {
		want, err := url.Parse(origin)
		if err != nil || redirects != 0 || current.String() != want.String() {
			return fmt.Errorf("%w: manager origin redirect is unsupported", ErrManagerIntegrity)
		}
		switch want.Hostname() {
		case "registry.npmjs.org", "api.github.com":
		default:
			return fmt.Errorf("%w: manager origin host is unsupported", ErrManagerIntegrity)
		}
		return nil
	}
	if redirects == 0 {
		if host != "github.com" || current.String() != origin {
			return fmt.Errorf("%w: Bun origin is not canonical", ErrManagerIntegrity)
		}
		return nil
	}
	if host != "release-assets.githubusercontent.com" {
		return fmt.Errorf("%w: Bun redirect host %q is unsupported", ErrManagerIntegrity, host)
	}
	return nil
}

func selectBunAssets(
	assets []bunAsset,
	assetName string,
	origin string,
	version string,
) (bunAsset, *bunAsset, error) {
	var distribution bunAsset
	var checksum *bunAsset
	for i := range assets {
		asset := assets[i]
		switch asset.Name {
		case assetName:
			if distribution.Name != "" {
				return bunAsset{}, nil, fmt.Errorf("%w: duplicate Bun distribution asset", ErrManagerIntegrity)
			}
			distribution = asset
		case bunChecksumAssetName:
			if checksum != nil {
				return bunAsset{}, nil, fmt.Errorf("%w: duplicate Bun checksum asset", ErrManagerIntegrity)
			}
			copyAsset := asset
			checksum = &copyAsset
		}
	}
	if distribution.Name == "" {
		return bunAsset{}, nil, ErrManagerNotFound
	}
	if err := validateBunAsset(distribution, origin, maxManagerDistributionBytes); err != nil {
		return bunAsset{}, nil, err
	}
	if checksum != nil {
		checksumOrigin := strings.TrimSuffix(origin, assetName) + bunChecksumAssetName
		if err := validateBunAsset(*checksum, checksumOrigin, managerChecksumMaxBytes); err != nil {
			return bunAsset{}, nil, err
		}
	} else if !bunLegacyTOFU(version) {
		return bunAsset{}, nil, fmt.Errorf(
			"%w: Bun release does not list a signed checksum",
			ErrManagerIntegrity,
		)
	}
	return distribution, checksum, nil
}

func validateBunAsset(asset bunAsset, origin string, maxBytes int64) error {
	if asset.State != "uploaded" ||
		asset.URL != origin ||
		asset.Size < 1 {
		return fmt.Errorf("%w: Bun release asset metadata is invalid", ErrManagerIntegrity)
	}
	if asset.Size > maxBytes {
		return fmt.Errorf(
			"%w: Bun release asset size exceeds %d bytes",
			ErrManagerProtocolUnsupported,
			maxBytes,
		)
	}
	if asset.Digest != "" && !sha256DigestPattern.MatchString(asset.Digest) {
		return fmt.Errorf("%w: Bun release asset digest is invalid", ErrManagerIntegrity)
	}
	return nil
}

func verifyAssetDigest(raw []byte, expected string) error {
	if expected == "" {
		return nil
	}
	sum := sha256.Sum256(raw)
	actual := "sha256:" + hex.EncodeToString(sum[:])
	if actual != expected {
		return fmt.Errorf("%w: Bun release asset digest mismatch", ErrManagerIntegrity)
	}
	return nil
}

func verifyAssetDigestFromSource(source ManagerSource, expected string) error {
	if expected != "" && source.Digest != expected {
		return fmt.Errorf("%w: Bun release asset digest mismatch", ErrManagerIntegrity)
	}
	return nil
}

func verifyBunSignature(raw []byte, keyring openpgp.KeyRing) ([]byte, error) {
	block, rest := clearsign.Decode(raw)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("signed checksum is not one clear-signed document")
	}
	signer, err := block.VerifySignature(keyring, nil)
	if err != nil {
		return nil, err
	}
	if signer == nil ||
		strings.ToUpper(hex.EncodeToString(signer.PrimaryKey.Fingerprint)) != bunSigningFingerprint {
		return nil, errors.New("signed checksum used an unpinned key")
	}
	return block.Plaintext, nil
}

func bunChecksumFor(plaintext []byte, assetName string) (string, error) {
	if len(plaintext) == 0 {
		return "", errors.New("Bun checksum manifest is empty")
	}
	var result string
	for line := range strings.SplitSeq(string(plaintext), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		if len(line) < 67 || line[64:66] != "  " {
			return "", errors.New("Bun checksum manifest has an invalid entry")
		}
		digest := line[:64]
		name := line[66:]
		if _, err := hex.DecodeString(digest); err != nil ||
			digest != strings.ToLower(digest) ||
			name == "" ||
			strings.ContainsAny(name, "/\\\x00") ||
			!managerChecksumName(name) {
			return "", errors.New("Bun checksum manifest has an invalid entry")
		}
		if name == assetName {
			if result != "" {
				return "", errors.New("Bun checksum manifest has a duplicate selected entry")
			}
			result = "sha256:" + digest
		}
	}
	if result == "" {
		return "", errors.New("Bun checksum manifest does not contain the selected asset")
	}
	return result, nil
}

func managerChecksumName(value string) bool {
	for index := range len(value) {
		if value[index] < 0x21 || value[index] > 0x7e {
			return false
		}
	}
	return true
}

func bunLegacyTOFU(version string) bool {
	matches := packageManagerVersionPattern.FindStringSubmatch(version)
	if len(matches) < 4 {
		return false
	}
	major, errMajor := strconv.ParseUint(matches[1], 10, 64)
	minor, errMinor := strconv.ParseUint(matches[2], 10, 64)
	patch, errPatch := strconv.ParseUint(matches[3], 10, 64)
	if errMajor != nil || errMinor != nil || errPatch != nil {
		return false
	}
	return major == 0 &&
		(minor < 5 ||
			minor == 5 && (patch < 3 || patch == 3 && len(matches) > 4 && matches[4] != ""))
}

func decodeSHA512SRI(integrity string) ([]byte, error) {
	if err := validatePackageIntegrity(integrity); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(strings.TrimPrefix(integrity, "sha512-"))
}

func loadBunKeys() (openpgp.EntityList, error) {
	raw, err := managerKeys.ReadFile("bun.asc")
	if err != nil {
		return nil, fmt.Errorf("read Bun keyring: %w", err)
	}
	keys, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse Bun keyring: %w", err)
	}
	if len(keys) != 1 ||
		strings.ToUpper(hex.EncodeToString(keys[0].PrimaryKey.Fingerprint)) != bunSigningFingerprint {
		return nil, errors.New("Bun keyring does not match its pinned fingerprint")
	}
	return keys, nil
}
