package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

const (
	maxUpstreamRedirects         = 5
	defaultUpstreamTimeout       = 2 * time.Minute
	maxUpstreamErrorBodyBytes    = 64 << 10
	maxUpstreamMetadataBytes     = 8 << 20
	maxNodeDistributionBytes     = 256 << 20
	maxBunDistributionBytes      = 256 << 20
	maxRegistryDistributionBytes = maxManagerDistributionBytes
)

type upstreamObject struct {
	file      *os.File
	source    PlatformSource
	redirects []string
}

type upstreamOriginError struct {
	cause error
}

func (err *upstreamOriginError) Error() string { return err.cause.Error() }
func (err *upstreamOriginError) Unwrap() error { return err.cause }

func (object *upstreamObject) Close() error {
	if object == nil || object.file == nil {
		return nil
	}
	name := object.file.Name()
	err := object.file.Close()
	object.file = nil
	return errors.Join(err, os.Remove(name))
}

func fetchUpstream(
	ctx context.Context,
	client *http.Client,
	workDir string,
	rawURL string,
	allowedHosts []string,
	maxBytes int64,
) (_ *upstreamObject, returnErr error) {
	if ctx == nil {
		return nil, errors.New("upstream request context is nil")
	}
	if maxBytes < 1 {
		return nil, errors.New("upstream byte limit is invalid")
	}
	initial, err := parseUpstreamURL(rawURL, allowedHosts, false)
	if err != nil {
		return nil, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionOriginRejected,
			err,
		)
	}
	if client == nil {
		client = &http.Client{Timeout: defaultUpstreamTimeout}
	}
	redirects := make([]string, 0, maxUpstreamRedirects)
	requestClient := *client
	requestClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > maxUpstreamRedirects {
			return &upstreamOriginError{cause: errors.New("upstream redirect limit exceeded")}
		}
		if _, err := parseUpstreamURL(request.URL.String(), allowedHosts, true); err != nil {
			return &upstreamOriginError{cause: err}
		}
		redirects = append(redirects, request.URL.String())
		return nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, initial.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create upstream request: %w", err)
	}
	request.Header.Set("Accept", "application/octet-stream, application/json")
	request.Header.Set("User-Agent", "helmr-platform-acquisition/"+NodeRuntimeAdapterVersion)
	response, err := requestClient.Do(request)
	if err != nil {
		var origin *upstreamOriginError
		if errors.As(err, &origin) {
			return nil, deterministicAcquisitionFailure(
				api.WorkerPlatformAcquisitionOriginRejected,
				origin,
			)
		}
		return nil, fmt.Errorf("fetch upstream object: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		diagnostic, _ := io.ReadAll(io.LimitReader(response.Body, maxUpstreamErrorBodyBytes))
		cause := fmt.Errorf(
			"upstream response status = %d: %s",
			response.StatusCode,
			strings.TrimSpace(string(diagnostic)),
		)
		switch {
		case response.StatusCode == http.StatusNotFound ||
			response.StatusCode == http.StatusGone:
			return nil, deterministicAcquisitionFailure(
				api.WorkerPlatformAcquisitionUnsupportedSelector,
				cause,
			)
		case upstreamRateLimited(response, diagnostic):
			return nil, cause
		case response.StatusCode >= 400 && response.StatusCode < 500 &&
			response.StatusCode != http.StatusRequestTimeout &&
			response.StatusCode != http.StatusTooManyRequests:
			return nil, deterministicAcquisitionFailure(
				api.WorkerPlatformAcquisitionOriginRejected,
				cause,
			)
		default:
			return nil, cause
		}
	}
	if response.ContentLength > maxBytes {
		return nil, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionTopologyFailed,
			errors.New("upstream object exceeds its byte limit"),
		)
	}
	if err := os.MkdirAll(workDir, 0700); err != nil {
		return nil, fmt.Errorf("create upstream work directory: %w", err)
	}
	file, err := os.CreateTemp(workDir, ".upstream-*")
	if err != nil {
		return nil, fmt.Errorf("create upstream object: %w", err)
	}
	path := file.Name()
	keep := false
	defer func() {
		if file != nil {
			returnErr = errors.Join(returnErr, file.Close())
		}
		if !keep {
			returnErr = errors.Join(returnErr, os.Remove(path))
		}
	}()
	hash := sha256.New()
	size, err := copyExact(ctx, io.MultiWriter(file, hash), response.Body, maxBytes)
	if err != nil {
		if errors.Is(err, errCopyExceedsLimit) {
			return nil, deterministicAcquisitionFailure(
				api.WorkerPlatformAcquisitionTopologyFailed,
				err,
			)
		}
		return nil, fmt.Errorf("read upstream object: %w", err)
	}
	if size < 1 {
		return nil, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionTopologyFailed,
			errors.New("upstream object is empty"),
		)
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync upstream object: %w", err)
	}
	if err := file.Chmod(0400); err != nil {
		return nil, fmt.Errorf("seal upstream object: %w", err)
	}
	if err := file.Close(); err != nil {
		file = nil
		return nil, fmt.Errorf("close upstream object: %w", err)
	}
	file = nil
	sealed, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reopen upstream object: %w", err)
	}
	keep = true
	return &upstreamObject{
		file: sealed,
		source: PlatformSource{
			Digest:    "sha256:" + hex.EncodeToString(hash.Sum(nil)),
			Origin:    initial.String(),
			SizeBytes: size,
		},
		redirects: slices.Clone(redirects),
	}, nil
}

func upstreamRateLimited(response *http.Response, diagnostic []byte) bool {
	if response.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if response.StatusCode != http.StatusForbidden {
		return false
	}
	if strings.TrimSpace(response.Header.Get("Retry-After")) != "" ||
		strings.TrimSpace(response.Header.Get("X-RateLimit-Remaining")) == "0" {
		return true
	}
	if response.Request == nil ||
		response.Request.URL == nil ||
		response.Request.URL.Hostname() != "api.github.com" {
		return false
	}
	var body struct {
		Message string `json:"message"`
	}
	return json.Unmarshal(diagnostic, &body) == nil &&
		strings.Contains(strings.ToLower(body.Message), "secondary rate limit")
}

func parseUpstreamURL(raw string, allowedHosts []string, allowQuery bool) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil ||
		parsed.Scheme != "https" ||
		parsed.Hostname() == "" ||
		parsed.Host != parsed.Hostname() ||
		parsed.User != nil ||
		(!allowQuery && parsed.RawQuery != "") ||
		parsed.Fragment != "" ||
		parsed.Path == "" ||
		!slices.Contains(allowedHosts, parsed.Hostname()) {
		return nil, fmt.Errorf("upstream URL %q is outside the closed HTTPS origin policy", raw)
	}
	return parsed, nil
}

func readUpstream(object *upstreamObject) ([]byte, error) {
	if object == nil || object.file == nil {
		return nil, errors.New("upstream object is closed")
	}
	if _, err := object.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(object.file, maxUpstreamMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || int64(len(raw)) > maxUpstreamMetadataBytes {
		return nil, errors.New("upstream metadata is empty or excessive")
	}
	return raw, nil
}
