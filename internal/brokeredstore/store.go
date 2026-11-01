// Package brokeredstore reads and publishes immutable objects through storage
// grants issued by the Control Plane. A Host using it holds no storage
// credential: every request is signed by the Control Plane for one object,
// one bounded lifetime and the authority carried by the request context.
package brokeredstore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"golang.org/x/sync/errgroup"
)

// Client requests storage grants from the Control Plane.
type Client interface {
	StorageRead(context.Context, workerapi.StorageReadRequest) (workerapi.StorageReadGrant, error)
	StorageWrite(context.Context, workerapi.StorageWriteRequest) (workerapi.StorageWriteResponse, error)
	StorageParts(context.Context, workerapi.StoragePartsRequest) (workerapi.StoragePartsResponse, error)
	StorageComplete(context.Context, workerapi.StorageCompleteRequest) error
	StorageAbort(context.Context, workerapi.StorageAbortRequest) error
}

type scopeKey struct{}

// WithScope binds storage requests made with ctx to one authority. The scope
// is read on every request, so clearing an executor credential it references
// ends that authority for later requests.
func WithScope(ctx context.Context, scope workerapi.StorageScope) context.Context {
	return context.WithValue(ctx, scopeKey{}, &scope)
}

// ErrNoAuthority reports a storage request made outside any storage authority.
var ErrNoAuthority = errors.New("storage request has no allocation or preparation-source authority")

// Scope returns the storage authority bound to ctx.
func Scope(ctx context.Context) (workerapi.StorageScope, error) {
	scope, _, err := scopeOf(ctx)
	return scope, err
}

func scopeOf(ctx context.Context) (workerapi.StorageScope, string, error) {
	scope, ok := ctx.Value(scopeKey{}).(*workerapi.StorageScope)
	if !ok || scope == nil {
		return workerapi.StorageScope{}, "", ErrNoAuthority
	}
	// The cache key names the authority without retaining credential material.
	key, err := json.Marshal(scope)
	if err != nil {
		return workerapi.StorageScope{}, "", err
	}
	sum := sha256.Sum256(key)
	return *scope, string(sum[:]), nil
}

// renewBefore re-requests a cached grant this long before it expires, so a
// request never starts on a grant about to lapse.
const renewBefore = 30 * time.Second

const (
	publishAttempts = 3
	partConcurrency = 4
	// partAttempts and partRetryBase give each part the budget of the storage
	// SDK's standard retryer: network failures, server errors, throttling and
	// refused grants are retried with backoff.
	partAttempts  = 3
	partRetryBase = time.Second
)

type readKey struct{ scope, digest string }

// Store is one brokered object store: the organization content store or the
// release platform store.
type Store struct {
	client Client
	store  string
	http   *http.Client

	mu    sync.Mutex
	reads map[readKey]workerapi.StorageReadGrant
}

func New(client Client, store string, httpClient *http.Client) (*Store, error) {
	if client == nil || httpClient == nil || (store != workerapi.StorageCAS && store != workerapi.StoragePlatform) {
		return nil, errors.New("brokered storage requires a grant client, an HTTP client and a known store")
	}
	return &Store{client: client, store: store, http: httpClient, reads: map[readKey]workerapi.StorageReadGrant{}}, nil
}

func (s *Store) readGrant(ctx context.Context, digest string, renew bool) (workerapi.StorageReadGrant, error) {
	scope, scopeKey, err := scopeOf(ctx)
	if err != nil {
		return workerapi.StorageReadGrant{}, err
	}
	key := readKey{scope: scopeKey, digest: digest}
	now := time.Now()
	s.mu.Lock()
	grant, ok := s.reads[key]
	s.mu.Unlock()
	if ok && !renew && now.Add(renewBefore).Before(grant.Get.ExpiresAt) && now.Add(renewBefore).Before(grant.Head.ExpiresAt) {
		return grant, nil
	}
	grant, err = s.client.StorageRead(ctx, workerapi.StorageReadRequest{Scope: scope, Store: s.store, Digest: digest})
	if err != nil {
		return workerapi.StorageReadGrant{}, err
	}
	if grant.Digest != digest {
		return workerapi.StorageReadGrant{}, errors.New("storage read grant does not match its request")
	}
	s.mu.Lock()
	for k, old := range s.reads {
		if !now.Before(old.Get.ExpiresAt) {
			delete(s.reads, k)
		}
	}
	s.reads[key] = grant
	s.mu.Unlock()
	return grant, nil
}

// read issues one signed read, renewing the grant once if storage rejects it.
func (s *Store) read(ctx context.Context, digest string, head bool, header http.Header) (*http.Response, error) {
	for attempt := range 2 {
		grant, err := s.readGrant(ctx, digest, attempt > 0)
		if err != nil {
			return nil, err
		}
		signed := grant.Get
		if head {
			signed = grant.Head
		}
		response, err := s.send(ctx, signed, header, nil, 0)
		if err != nil {
			return nil, err
		}
		if response.StatusCode == http.StatusForbidden && attempt == 0 {
			drain(response)
			continue
		}
		return response, nil
	}
	return nil, errors.New("storage read retry exhausted")
}

func (s *Store) send(ctx context.Context, signed cas.SignedRequest, extra http.Header, body io.Reader, length int64) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, signed.Method, signed.URL, body)
	if err != nil {
		return nil, err
	}
	for name, values := range signed.Header {
		request.Header[name] = append([]string(nil), values...)
	}
	for name, values := range extra {
		request.Header[name] = append([]string(nil), values...)
	}
	if body != nil {
		request.ContentLength = length
		if length == 0 {
			request.Body = http.NoBody
		}
	}
	response, err := s.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", cas.ErrUnavailable, err)
	}
	return response, nil
}

func drain(response *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	_ = response.Body.Close()
}

// statusError classifies a storage response the caller did not accept.
func statusError(response *http.Response) error {
	drain(response)
	err := fmt.Errorf("storage responded %s", response.Status)
	switch {
	case response.StatusCode == http.StatusNotFound:
		return errors.Join(os.ErrNotExist, err)
	case response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500:
		return fmt.Errorf("%w: %w", cas.ErrUnavailable, err)
	default:
		return err
	}
}

func (s *Store) Stat(ctx context.Context, digest string) (cas.Object, error) {
	response, err := s.read(ctx, digest, true, nil)
	if err != nil {
		return cas.Object{}, err
	}
	if response.StatusCode != http.StatusOK {
		return cas.Object{}, statusError(response)
	}
	drain(response)
	if response.ContentLength < 0 {
		return cas.Object{}, errors.New("storage metadata omits the object size")
	}
	return cas.Object{Digest: digest, SizeBytes: response.ContentLength, MediaType: response.Header.Get("Content-Type")}, nil
}

// Get returns a digest-verifying body.
func (s *Store) Get(ctx context.Context, digest string) (io.ReadCloser, error) {
	response, err := s.read(ctx, digest, false, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, statusError(response)
	}
	return cas.NewVerifyingReadCloser(response.Body, digest), nil
}

// GetRange requires the exact range and total object size in the response.
// Callers authenticate the returned bytes; a range cannot certify the digest.
func (s *Store) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := cas.ValidateRange(size, offset, length); err != nil {
		return nil, err
	}
	end := offset + length - 1
	response, err := s.read(ctx, digest, false, http.Header{"Range": {fmt.Sprintf("bytes=%d-%d", offset, end)}})
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusPartialContent {
		return nil, statusError(response)
	}
	if response.ContentLength != length || response.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", offset, end, size) {
		drain(response)
		return nil, errors.New("object range response does not match requested identity")
	}
	return response.Body, nil
}

// Publish uploads caller-owned, read-only bytes create-only under their
// digest. It returns only after storage holds exactly the descriptor's bytes.
func (s *Store) Publish(ctx context.Context, expected cas.Descriptor, file *os.File) (cas.Object, error) {
	if s.store != workerapi.StorageCAS {
		return cas.Object{}, errors.New("platform storage is read-only")
	}
	if err := ctx.Err(); err != nil {
		return cas.Object{}, err
	}
	if err := cas.ValidateDescriptor(expected); err != nil {
		return cas.Object{}, err
	}
	scope, _, err := scopeOf(ctx)
	if err != nil {
		return cas.Object{}, err
	}
	before, err := cas.InspectPublishedFile(file)
	if err != nil {
		return cas.Object{}, err
	}
	if before.Size() != expected.SizeBytes {
		return cas.Object{}, fmt.Errorf("published file size = %d, want %d", before.Size(), expected.SizeBytes)
	}
	if err := cas.VerifyDescriptorFile(ctx, expected, file); err != nil {
		return cas.Object{}, err
	}
	object := workerapi.CASObject{Digest: expected.Digest, SizeBytes: expected.SizeBytes, MediaType: expected.MediaType}
	published := false
	var uploadErr error
	for attempt := 0; attempt < publishAttempts && !published; attempt++ {
		var grant workerapi.StorageWriteResponse
		grant, uploadErr = s.client.StorageWrite(ctx, workerapi.StorageWriteRequest{Scope: scope, Object: object})
		switch {
		case uploadErr != nil:
			return cas.Object{}, uploadErr
		case grant.Exists:
			published = true
		case grant.Put != nil:
			published, uploadErr = s.put(ctx, *grant.Put, file, expected.SizeBytes)
		case grant.Upload != nil:
			uploadErr = s.multipart(ctx, scope, object, *grant.Upload, file)
			published = uploadErr == nil
		default:
			return cas.Object{}, errors.New("storage write grant is empty")
		}
		if uploadErr != nil && !errors.Is(uploadErr, cas.ErrUnavailable) {
			return cas.Object{}, uploadErr
		}
	}
	if !published {
		return cas.Object{}, errors.Join(cas.ErrUnavailable, uploadErr)
	}
	after, err := cas.InspectPublishedFile(file)
	if err != nil {
		return cas.Object{}, fmt.Errorf("inspect published file after upload: %w", err)
	}
	if before != after {
		return cas.Object{}, errors.New("published file identity changed during upload")
	}
	return cas.Object{Digest: expected.Digest, SizeBytes: expected.SizeBytes, MediaType: expected.MediaType}, nil
}

// put reports false without error when storage refused a create-only write
// because another write won; the next grant then verifies the stored object.
func (s *Store) put(ctx context.Context, grant cas.SignedRequest, file *os.File, size int64) (bool, error) {
	response, err := s.send(ctx, grant, nil, io.NewSectionReader(file, 0, size), size)
	if err != nil {
		return false, err
	}
	switch response.StatusCode {
	case http.StatusOK:
		drain(response)
		return true, nil
	case http.StatusPreconditionFailed, http.StatusConflict:
		drain(response)
		return false, fmt.Errorf("%w: create-only write lost a race", cas.ErrUnavailable)
	default:
		return false, statusError(response)
	}
}

func (s *Store) multipart(ctx context.Context, scope workerapi.StorageScope, object workerapi.CASObject, upload cas.Upload, file *os.File) (resultErr error) {
	if upload.ID == "" || upload.PartSize <= 0 || upload.Parts <= 0 || int64(upload.Parts) != (object.SizeBytes+upload.PartSize-1)/upload.PartSize {
		return errors.New("storage multipart grant does not cover the object")
	}
	defer func() {
		if resultErr != nil {
			abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if err := s.client.StorageAbort(abortCtx, workerapi.StorageAbortRequest{Scope: scope, Object: object, UploadID: upload.ID}); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("abort multipart upload: %w", err))
			}
		}
	}()
	parts := make([]cas.CompletedPart, upload.Parts)
	request := func(ctx context.Context, numbers []int32) ([]cas.SignedRequest, error) {
		response, err := s.client.StorageParts(ctx, workerapi.StoragePartsRequest{Scope: scope, Object: object, UploadID: upload.ID, Parts: numbers})
		if err == nil && len(response.Parts) != len(numbers) {
			err = errors.New("storage part grants do not match their request")
		}
		return response.Parts, err
	}
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(partConcurrency)
	for first := int32(1); first <= upload.Parts; first += workerapi.MaxStorageParts {
		numbers := make([]int32, 0, workerapi.MaxStorageParts)
		for n := first; n <= upload.Parts && len(numbers) < workerapi.MaxStorageParts; n++ {
			numbers = append(numbers, n)
		}
		grants, err := request(groupCtx, numbers)
		if err != nil {
			_ = group.Wait()
			return err
		}
		for i, number := range numbers {
			grant := grants[i]
			group.Go(func() error {
				offset := int64(number-1) * upload.PartSize
				length := min(upload.PartSize, object.SizeBytes-offset)
				var err error
				for attempt := range partAttempts {
					if attempt > 0 {
						if err := partBackoff(groupCtx, attempt); err != nil {
							return err
						}
					}
					// Every retry renews the grant: a refused or failed request
					// may have outlived the one it used.
					if attempt > 0 || !time.Now().Add(renewBefore).Before(grant.ExpiresAt) {
						renewed, err := request(groupCtx, []int32{number})
						if err != nil {
							return err
						}
						grant = renewed[0]
					}
					var etag string
					if etag, err = s.part(groupCtx, grant, file, offset, length); err == nil {
						parts[number-1] = cas.CompletedPart{Number: number, ETag: etag}
						return nil
					}
					if !errors.Is(err, errPartRefused) && !errors.Is(err, cas.ErrUnavailable) {
						return err
					}
				}
				return fmt.Errorf("storage part %d retry exhausted: %w", number, err)
			})
		}
	}
	if err := group.Wait(); err != nil {
		return err
	}
	return s.client.StorageComplete(ctx, workerapi.StorageCompleteRequest{Scope: scope, Object: object, UploadID: upload.ID, Parts: parts})
}

// errPartRefused marks a part storage refused as unauthorized, typically
// because its grant expired while the part waited or uploaded.
var errPartRefused = errors.New("storage refused the part grant")

// part uploads one part and returns its entity tag. Network failures and
// server errors are cas.ErrUnavailable.
func (s *Store) part(ctx context.Context, grant cas.SignedRequest, file *os.File, offset, length int64) (string, error) {
	response, err := s.send(ctx, grant, nil, io.NewSectionReader(file, offset, length), length)
	if err != nil {
		return "", err
	}
	if response.StatusCode == http.StatusForbidden {
		drain(response)
		return "", errPartRefused
	}
	if response.StatusCode != http.StatusOK {
		return "", statusError(response)
	}
	etag := response.Header.Get("ETag")
	drain(response)
	if etag == "" {
		return "", errors.New("storage part response has no entity tag")
	}
	return etag, nil
}

// partBackoff waits before retry attempt n (n >= 1) with full jitter, doubling
// from partRetryBase as the storage SDK's standard retryer does.
var partBackoff = func(ctx context.Context, n int) error {
	delay := time.Duration(rand.Int64N(int64(partRetryBase << (n - 1))))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var _ cas.ImmutableStore = (*Store)(nil)

// RangeSource reads bounded ranges of immutable objects.
type RangeSource interface {
	GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error)
}

type boundRanges struct {
	store *Store
	scope workerapi.StorageScope
}

func (b boundRanges) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	return b.store.GetRange(WithScope(ctx, b.scope), digest, size, offset, length)
}

// BindRanges fixes the storage authority carried by ctx into a brokered range
// source, for demand readers such as device servers and memory pagers whose
// request contexts outlive or precede that authority's context. Other sources
// are returned unchanged.
func BindRanges(ctx context.Context, source RangeSource) RangeSource {
	store, ok := source.(*Store)
	if !ok {
		return source
	}
	scope, err := Scope(ctx)
	if err != nil {
		// Requests fail closed with ErrNoAuthority.
		return source
	}
	return boundRanges{store: store, scope: scope}
}
