package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	initialKeyPath       = "/worker/v1/run/computer-instances/initialization/key"
	computerSourcePath   = "/worker/v1/run/computer-instances/computer-source"
	objectRegisterPath   = "/worker/v1/run/computer-instances/initialization/objects/register"
	objectCertifyPath    = "/worker/v1/run/computer-instances/initialization/objects/certify"
	initialVersionPath   = "/worker/v1/run/computer-instances/initialization/generation"
	injectedFailureText  = "injected preparation failure"
	claimReadStatement   = "SELECT w.claim_version,g.claim_version"
	firstFenceStatement  = "SELECT environment_id,computer_id,region_id,observed_state FROM computer_instances"
	deadlineStatement    = "-- name: GetComputerPreparationDeadlinesValid"
	pinWriteStatement    = "-- name: PinRuntimeComputerKey"
	sourceKeysStatement  = "-- name: ListInstanceComputerSourceKeys"
	registrationSQLMatch = "INSERT INTO computer_objects"
)

var errInjectedPreparation = errors.New(injectedFailureText)

// sqlFaults fails, once armed, one statement whose text contains match after
// letting skip of them through, or the next commit. Authority locks before
// the fault still run, so the failure reaches exactly the chosen step.
type sqlFaults struct {
	db.TxDB
	mu     sync.Mutex
	armed  bool
	match  string
	skip   int
	commit bool
}

func (f *sqlFaults) arm(match string, skip int, commit bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed, f.match, f.skip, f.commit = true, match, skip, commit
}

func (f *sqlFaults) fails(sql string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.armed || f.commit || !strings.Contains(sql, f.match) {
		return false
	}
	if f.skip > 0 {
		f.skip--
		return false
	}
	f.armed = false
	return true
}

func (f *sqlFaults) failsCommit() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.armed || !f.commit {
		return false
	}
	f.armed = false
	return true
}

func (f *sqlFaults) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := f.TxDB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return sqlFaultTx{Tx: tx, faults: f}, nil
}

type sqlFaultTx struct {
	pgx.Tx
	faults *sqlFaults
}

func (t sqlFaultTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if t.faults.fails(sql) {
		return pgconn.CommandTag{}, errInjectedPreparation
	}
	return t.Tx.Exec(ctx, sql, args...)
}

func (t sqlFaultTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if t.faults.fails(sql) {
		return nil, errInjectedPreparation
	}
	return t.Tx.Query(ctx, sql, args...)
}

func (t sqlFaultTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if t.faults.fails(sql) {
		return sqlFaultRow{}
	}
	return t.Tx.QueryRow(ctx, sql, args...)
}

func (t sqlFaultTx) Commit(ctx context.Context) error {
	if t.faults.failsCommit() {
		// The owner's transaction runner rolls back after a failed commit.
		return errInjectedPreparation
	}
	return t.Tx.Commit(ctx)
}

type sqlFaultRow struct{}

func (sqlFaultRow) Scan(...any) error { return errInjectedPreparation }

// faultingKeys is the fixture's key provider with injectable failures. Once
// recording, it keeps every plaintext it returns and a copy of it, so a test
// can require the plaintext cleared and absent from the response.
type faultingKeys struct {
	computer.KeyWrapper
	mu          sync.Mutex
	recording   bool
	failWrap    bool
	failUnwrap  bool
	afterUnwrap func()
	returned    [][]byte
	copies      [][]byte
}

func (k *faultingKeys) Wrap(ctx context.Context, scope, id string, key []byte) (computerkey.Envelope, error) {
	k.mu.Lock()
	fail := k.failWrap
	k.mu.Unlock()
	if fail {
		return computerkey.Envelope{}, errInjectedPreparation
	}
	return k.KeyWrapper.Wrap(ctx, scope, id, key)
}

func (k *faultingKeys) Unwrap(ctx context.Context, scope, id string, e computerkey.Envelope) ([]byte, error) {
	key, err := k.KeyWrapper.Unwrap(ctx, scope, id, e)
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.recording {
		k.returned = append(k.returned, key)
		k.copies = append(k.copies, bytes.Clone(key))
	}
	if k.afterUnwrap != nil {
		k.afterUnwrap()
		k.afterUnwrap = nil
	}
	if k.failUnwrap {
		return key, errInjectedPreparation
	}
	return key, err
}

// faultingObjects is the fixture's object storage with an injectable Stat
// failure.
type faultingObjects struct {
	cas.UploadStore
	mu       sync.Mutex
	failStat bool
}

func (o *faultingObjects) Stat(ctx context.Context, digest string) (cas.Object, error) {
	o.mu.Lock()
	fail := o.failStat
	o.mu.Unlock()
	if fail {
		return cas.Object{}, errInjectedPreparation
	}
	return o.UploadStore.Stat(ctx, digest)
}

// lockedBuffer collects the server's log records.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// preparationStage is how far the Instance's preparation has progressed
// before the faulted request.
type preparationStage int

const (
	preparationStarted preparationStage = iota
	preparationRootCertified
	preparationPublished
)

// preparationErrorServer serves an initial preparation fixture through
// NewServer with injectable database, key provider and storage failures.
type preparationErrorServer struct {
	f        initialPublicationFixture
	handler  http.Handler
	faults   *sqlFaults
	keys     *faultingKeys
	objects  *faultingObjects
	logs     *lockedBuffer
	token    string
	object   workerapi.InitialComputerObjectRequest
	version  workerapi.InitialComputerGenerationRequest
	instance string
}

func newPreparationErrorServer(t *testing.T, stage preparationStage) *preparationErrorServer {
	t.Helper()
	f := newInitialPublicationFixture(t)
	s := &preparationErrorServer{f: f, faults: &sqlFaults{TxDB: f.Pool}, keys: &faultingKeys{KeyWrapper: f.keys}, objects: &faultingObjects{UploadStore: f.store}, logs: &lockedBuffer{}, instance: pgvalue.UUIDString(f.runtime)}
	s.handler = f.serve(t, func(cfg *ServerConfig) {
		cfg.TX = s.faults
		cfg.ComputerKeys = s.keys
		cfg.CAS = s.objects
		cfg.Log = slog.New(slog.NewJSONHandler(s.logs, nil))
	})
	credential := seedHostCredential(t, f.Pool, f.worker.HostID)
	if stage >= preparationRootCertified {
		server := httptest.NewServer(s.handler)
		t.Cleanup(server.Close)
		client := credential.client(t, server.URL)
		_, root, object := f.certifyInitialRoot(t, client)
		s.object = object
		s.version = workerapi.InitialComputerGenerationRequest{ComputerInstanceID: s.instance, DesiredVersion: 1, Root: root, Config: oci.RuntimeConfig{User: "root"}}
		if stage >= preparationPublished {
			if _, err := client.PublishInitialComputerGeneration(t.Context(), s.version); err != nil {
				t.Fatal(err)
			}
		}
	}
	s.token = credential.token(t, s.handler)
	s.keys.mu.Lock()
	s.keys.recording = true
	s.keys.mu.Unlock()
	return s
}

// post sends the route's request for the fixture's preparation.
func (s *preparationErrorServer) post(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	var body any
	switch path {
	case initialKeyPath:
		body = workerapi.InitialComputerKeyRequest{ComputerInstanceID: s.instance, DesiredVersion: 1}
	case computerSourcePath:
		body = workerapi.ComputerSourceRequest{ComputerInstanceID: s.instance, DesiredVersion: 1}
	case objectRegisterPath, objectCertifyPath:
		body = s.object
	case initialVersionPath:
		body = s.version
	default:
		t.Fatalf("unknown preparation route %s", path)
	}
	return postWorker(t, s.handler, s.token, path, body)
}

// armAfterUnwrap arms the SQL fault during the next provider unwrap, so the
// first authority read succeeds and only the final revalidation fails.
func (s *preparationErrorServer) armAfterUnwrap(match string, skip int) {
	s.keys.mu.Lock()
	defer s.keys.mu.Unlock()
	s.keys.afterUnwrap = func() { s.faults.arm(match, skip, false) }
}

// requireResponse checks the status, that the response carries neither the
// injected cause nor plaintext the provider returned, that all such plaintext
// was cleared, and that the failure was logged once exactly when it is
// internal.
func (s *preparationErrorServer) requireResponse(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), status)
	}
	if strings.Contains(response.Body.String(), injectedFailureText) {
		t.Fatalf("response exposes cause: %s", response.Body.String())
	}
	var decoded struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil || decoded.Error.Code == "" {
		t.Fatalf("response is not an error: %s", response.Body.String())
	}
	s.keys.mu.Lock()
	defer s.keys.mu.Unlock()
	for i, key := range s.keys.returned {
		if !bytes.Equal(key, make([]byte, len(key))) {
			t.Fatal("rejected plaintext not cleared")
		}
		if copied := s.keys.copies[i]; len(copied) > 0 && strings.Contains(response.Body.String(), base64.StdEncoding.EncodeToString(copied)) {
			t.Fatal("response carries plaintext")
		}
	}
	// A failed commit logs only its transaction stage, so count the error
	// records rather than the injected cause.
	logged := strings.Count(s.logs.String(), `"level":"ERROR"`)
	if status == http.StatusInternalServerError && logged != 1 {
		t.Fatalf("internal failure logged %d times: %s", logged, s.logs.String())
	}
	if status != http.StatusInternalServerError && logged != 0 {
		t.Fatalf("classified failure logged: %s", s.logs.String())
	}
}

// Each failure on the worker's Computer preparation routes is reported by
// its class: an unexpected database failure is a logged 500, a failed key
// provider or object storage a 503, and none leaks its cause or plaintext.
func TestComputerPreparationFailuresReportTheirClass(t *testing.T) {
	for _, test := range []struct {
		name   string
		stage  preparationStage
		path   string
		inject func(*preparationErrorServer)
		status int
		// unwrapped requires the provider to have returned plaintext before
		// the failure.
		unwrapped bool
	}{
		{name: "key first fence", path: initialKeyPath, inject: func(s *preparationErrorServer) { s.faults.arm(firstFenceStatement, 0, false) }, status: http.StatusInternalServerError},
		{name: "key pin write", path: initialKeyPath, inject: func(s *preparationErrorServer) { s.faults.arm(pinWriteStatement, 0, false) }, status: http.StatusInternalServerError},
		{name: "key commit", path: initialKeyPath, inject: func(s *preparationErrorServer) { s.faults.arm("", 0, true) }, status: http.StatusInternalServerError},
		{name: "key final deadline query", path: initialKeyPath, inject: func(s *preparationErrorServer) { s.armAfterUnwrap(deadlineStatement, 1) }, status: http.StatusInternalServerError, unwrapped: true},
		{name: "key final claim read", path: initialKeyPath, inject: func(s *preparationErrorServer) { s.armAfterUnwrap(claimReadStatement, 0) }, status: http.StatusInternalServerError, unwrapped: true},
		{name: "key provider wrap", path: initialKeyPath, inject: func(s *preparationErrorServer) { s.keys.failWrap = true }, status: http.StatusServiceUnavailable},
		{name: "key provider unwrap", path: initialKeyPath, inject: func(s *preparationErrorServer) { s.keys.failUnwrap = true }, status: http.StatusServiceUnavailable, unwrapped: true},
		{name: "source first fence", stage: preparationPublished, path: computerSourcePath, inject: func(s *preparationErrorServer) { s.faults.arm(firstFenceStatement, 0, false) }, status: http.StatusInternalServerError},
		{name: "source key read", stage: preparationPublished, path: computerSourcePath, inject: func(s *preparationErrorServer) { s.faults.arm(sourceKeysStatement, 0, false) }, status: http.StatusInternalServerError},
		{name: "source pin write", stage: preparationPublished, path: computerSourcePath, inject: func(s *preparationErrorServer) { s.faults.arm(pinWriteStatement, 0, false) }, status: http.StatusInternalServerError},
		{name: "source final deadline query", stage: preparationPublished, path: computerSourcePath, inject: func(s *preparationErrorServer) { s.armAfterUnwrap(deadlineStatement, 1) }, status: http.StatusInternalServerError, unwrapped: true},
		{name: "source final claim read", stage: preparationPublished, path: computerSourcePath, inject: func(s *preparationErrorServer) { s.armAfterUnwrap(claimReadStatement, 0) }, status: http.StatusInternalServerError, unwrapped: true},
		{name: "source provider unwrap", stage: preparationPublished, path: computerSourcePath, inject: func(s *preparationErrorServer) { s.keys.failUnwrap = true }, status: http.StatusServiceUnavailable, unwrapped: true},
		{name: "object registration SQL", stage: preparationRootCertified, path: objectRegisterPath, inject: func(s *preparationErrorServer) { s.faults.arm(registrationSQLMatch, 0, false) }, status: http.StatusInternalServerError},
		{name: "object registration first fence", stage: preparationRootCertified, path: objectRegisterPath, inject: func(s *preparationErrorServer) { s.faults.arm(firstFenceStatement, 0, false) }, status: http.StatusInternalServerError},
		{name: "object registration commit", stage: preparationRootCertified, path: objectRegisterPath, inject: func(s *preparationErrorServer) { s.faults.arm("", 0, true) }, status: http.StatusInternalServerError},
		{name: "object certification storage", stage: preparationRootCertified, path: objectCertifyPath, inject: func(s *preparationErrorServer) { s.objects.failStat = true }, status: http.StatusServiceUnavailable},
		{name: "object certification final deadline query", stage: preparationRootCertified, path: objectCertifyPath, inject: func(s *preparationErrorServer) { s.faults.arm(deadlineStatement, 1, false) }, status: http.StatusInternalServerError},
		{name: "version first fence", stage: preparationRootCertified, path: initialVersionPath, inject: func(s *preparationErrorServer) { s.faults.arm(firstFenceStatement, 0, false) }, status: http.StatusInternalServerError},
		{name: "version commit", stage: preparationRootCertified, path: initialVersionPath, inject: func(s *preparationErrorServer) { s.faults.arm("", 0, true) }, status: http.StatusInternalServerError},
		{name: "version final deadline query", stage: preparationRootCertified, path: initialVersionPath, inject: func(s *preparationErrorServer) { s.faults.arm(deadlineStatement, 1, false) }, status: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newPreparationErrorServer(t, test.stage)
			test.inject(s)
			response := s.post(t, test.path)
			s.requireResponse(t, response, test.status)
			if test.unwrapped && len(s.keys.returned) == 0 {
				t.Fatal("failure was not reached after a provider unwrap")
			}
		})
	}
}

// Deterministic outcomes keep their statuses: a malformed request is a 400,
// a claim mismatch a 401, and an expired or revoked preparation a 409, with
// no plaintext in the response.
func TestComputerPreparationRejectionsReportTheirClass(t *testing.T) {
	expire := func(s *preparationErrorServer) {
		dbtest.MustExec(t, t.Context(), s.f.Pool, `UPDATE computer_instances SET preparation_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, s.f.runtime)
	}
	revoke := func(s *preparationErrorServer) {
		dbtest.MustExec(t, t.Context(), s.f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, s.f.runtime)
	}
	staleClaims := func(s *preparationErrorServer) {
		dbtest.MustExec(t, t.Context(), s.f.Pool, `UPDATE worker_hosts SET claim_version=claim_version+1 WHERE id=$1`, pgvalue.UUID(s.f.worker.HostID))
	}
	for _, test := range []struct {
		name   string
		stage  preparationStage
		path   string
		change func(*preparationErrorServer)
		status int
	}{
		{name: "key expired", path: initialKeyPath, change: expire, status: http.StatusConflict},
		{name: "key revoked", path: initialKeyPath, change: revoke, status: http.StatusConflict},
		{name: "key stale claims", path: initialKeyPath, change: staleClaims, status: http.StatusUnauthorized},
		{name: "key malformed instance", path: initialKeyPath, change: func(s *preparationErrorServer) { s.instance = "not-an-instance" }, status: http.StatusBadRequest},
		{name: "source expired", stage: preparationPublished, path: computerSourcePath, change: expire, status: http.StatusConflict},
		{name: "source revoked", stage: preparationPublished, path: computerSourcePath, change: revoke, status: http.StatusConflict},
		{name: "source stale claims", stage: preparationPublished, path: computerSourcePath, change: staleClaims, status: http.StatusUnauthorized},
		{name: "object registration revoked", stage: preparationRootCertified, path: objectRegisterPath, change: revoke, status: http.StatusConflict},
		{name: "object registration malformed inspection", stage: preparationRootCertified, path: objectRegisterPath, change: func(s *preparationErrorServer) { s.object.Inspection.Pack = nil }, status: http.StatusBadRequest},
		{name: "object certification expired", stage: preparationRootCertified, path: objectCertifyPath, change: expire, status: http.StatusConflict},
		{name: "version expired", stage: preparationRootCertified, path: initialVersionPath, change: expire, status: http.StatusConflict},
		{name: "version revoked", stage: preparationRootCertified, path: initialVersionPath, change: revoke, status: http.StatusConflict},
		{name: "version stale claims", stage: preparationRootCertified, path: initialVersionPath, change: staleClaims, status: http.StatusUnauthorized},
		{name: "version malformed root", stage: preparationRootCertified, path: initialVersionPath, change: func(s *preparationErrorServer) { s.version.Root.Page.KeyID = "not-a-key" }, status: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newPreparationErrorServer(t, test.stage)
			test.change(s)
			s.requireResponse(t, s.post(t, test.path), test.status)
		})
	}
}

func postWorker(t *testing.T, handler http.Handler, token, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
