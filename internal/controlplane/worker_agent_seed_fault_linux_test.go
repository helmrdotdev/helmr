//go:build linux && computerproof

package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	awsv4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/bootseed"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Only this test Worker's existing CAS client uses the proxy. Preparation and
// qualification finish before the response fault is armed. Small demand ranges
// stay intact while a larger background range waits for actual Agent.init work.
type nativeSeedFaultProxy struct {
	mu       sync.Mutex
	targets  map[string]bool
	held     chan struct{}
	release  chan struct{}
	injected chan struct{}
	holdOnce sync.Once
	hitOnce  sync.Once
}

type nativeSeedRoundTrip func(*http.Request) (*http.Response, error)

func (f nativeSeedRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newNativeSeedFaultProxy(ctx context.Context, rawURI string) (*nativeSeedFaultProxy, *httptest.Server, string, error) {
	uri, err := url.Parse(rawURI)
	if err != nil {
		return nil, nil, "", err
	}
	if uri.Scheme != "s3" || uri.Host == "" || !strings.HasPrefix(uri.Path, "/_verification/native-execution/") ||
		!strings.HasSuffix(uri.Path, "/cas") || uri.RawQuery != "" || uri.Fragment != "" {
		return nil, nil, "", errors.New("seed fault requires isolated native-execution CAS")
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	if cfg.Region != "us-east-1" {
		return nil, nil, "", errors.New("seed fault fixture requires its qualified region")
	}
	p := &nativeSeedFaultProxy{targets: make(map[string]bool), held: make(chan struct{}), release: make(chan struct{}), injected: make(chan struct{})}
	upstream, _ := url.Parse("https://s3.us-east-1.amazonaws.com")
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.Transport = nativeSeedRoundTrip(func(r *http.Request) (*http.Response, error) {
		// Preserve the SDK's body and checksum framing. Signed streaming chunks
		// cannot be re-signed as an ordinary request by this fixture.
		hash := r.Header.Get("X-Amz-Content-Sha256")
		if hash == "" || strings.HasPrefix(hash, "STREAMING-AWS4-") {
			return nil, errors.New("unsupported payload signing mode in seed fixture")
		}
		credentials, err := cfg.Credentials.Retrieve(r.Context())
		if err != nil {
			return nil, err
		}
		r.Host = upstream.Host
		// The reverse proxy adds this header, but S3 rewrites it. Signing it
		// makes otherwise unchanged requests fail authentication upstream.
		r.Header.Del("X-Forwarded-For")
		r.Header.Del("Authorization")
		r.Header.Del("X-Amz-Security-Token")
		err = awsv4.NewSigner(func(o *awsv4.SignerOptions) { o.DisableURIPathEscaping = true }).
			SignHTTP(r.Context(), credentials, r, hash, "s3", cfg.Region, time.Now())
		if err != nil {
			return nil, err
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	proxy.ModifyResponse = p.modify
	bucketPath := "/" + uri.Host
	objectPrefix := bucketPath + uri.Path + "/"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inScope := strings.HasPrefix(r.URL.Path, objectPrefix)
		if r.URL.Path == bucketPath || r.URL.Path == bucketPath+"/" {
			inScope = strings.HasPrefix(r.URL.Query().Get("prefix"), strings.TrimPrefix(uri.Path, "/")+"/")
		}
		if !inScope {
			http.Error(w, "outside fixture CAS", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	query := uri.Query()
	query.Set("endpoint", server.URL)
	uri.RawQuery = query.Encode()
	return p, server, uri.String(), nil
}

func (p *nativeSeedFaultProxy) modify(response *http.Response) error {
	p.mu.Lock()
	selected := p.targets[response.Request.URL.Path]
	p.mu.Unlock()
	// A demand request covers at most four 256-KiB chunks plus four tags.
	// Header/index requests are smaller too. Do not stall those reads.
	if !selected || response.Request.Method != http.MethodGet || response.StatusCode != http.StatusPartialContent ||
		response.Request.Header.Get("Range") == "" || response.ContentLength <= (1<<20)+64 {
		return nil
	}
	if response.ContentLength > 8<<20 {
		return errors.New("seed background response exceeds bounded stream")
	}
	p.holdOnce.Do(func() { close(p.held) })
	select {
	case <-response.Request.Context().Done():
		return response.Request.Context().Err()
	case <-p.release:
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, response.ContentLength+1))
	closeErr := response.Body.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	if int64(len(body)) != response.ContentLength {
		return errors.New("seed background response length differs")
	}
	body[0] ^= 1
	response.Body = io.NopCloser(bytes.NewReader(body))
	// Exercise the memory object's AEAD rather than an optional transport checksum.
	for name := range response.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-amz-checksum-") {
			response.Header.Del(name)
		}
	}
	p.hitOnce.Do(func() { close(p.injected) })
	return nil
}

func TestNativeSeedFaultWaitsForInitAndPreservesDemand(t *testing.T) {
	p := &nativeSeedFaultProxy{targets: map[string]bool{"/seed": true}, held: make(chan struct{}), release: make(chan struct{}), injected: make(chan struct{})}
	response := func(size int) *http.Response {
		r := httptest.NewRequest(http.MethodGet, "http://fixture/seed", nil)
		r.Header.Set("Range", fmt.Sprintf("bytes=100-%d", 99+size))
		return &http.Response{Request: r, StatusCode: http.StatusPartialContent, ContentLength: int64(size),
			Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(bytes.Repeat([]byte{42}, size)))}
	}
	demand := response((1 << 20) + 64)
	if err := p.modify(demand); err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 1)
	if _, err := io.ReadFull(demand.Body, first); err != nil || first[0] != 42 {
		t.Fatal("demand was changed or blocked", err)
	}
	demand.Body.Close()
	background := response((1 << 20) + 65)
	done := make(chan error, 1)
	go func() { done <- p.modify(background) }()
	select {
	case <-p.held:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	select {
	case err := <-done:
		t.Fatalf("background escaped before init receipt: %v", err)
	default:
	}
	close(p.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(background.Body)
	background.Body.Close()
	if err != nil || int64(len(body)) != background.ContentLength || body[0] != 43 || !bytes.Equal(body[1:], bytes.Repeat([]byte{42}, len(body)-1)) {
		t.Fatalf("fault changed more than one ciphertext byte: %v", err)
	}
}

func verifyNativeLateSeedFault(t *testing.T, parent context.Context, pool *pgxpool.Pool, c *client.Client, env uuid.UUID, casURI, evidence string, proxy *nativeSeedFaultProxy) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	wait := func(check func() (bool, error)) {
		t.Helper()
		for {
			ok, err := check()
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	// Retiring the seed-only Computer is asynchronous. Its lease must settle
	// before a held response can be attributed to the new Session below.
	wait(func() (bool, error) {
		var stopped bool
		err := pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND fenced_at IS NULL)`, env).Scan(&stopped)
		return stopped, err
	})
	uri, _ := url.Parse(casURI)
	rows, err := pool.Query(ctx, `SELECT manifest FROM boot_seed_builds WHERE environment_id=$1 AND status='succeeded' AND payload_retired_at IS NULL`, env)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var raw []byte
		var m bootseed.Manifest
		if err = rows.Scan(&raw); err == nil {
			err = json.Unmarshal(raw, &m)
		}
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if m.MemoryBytes <= 0 || m.MemoryBytes > 4<<30 {
			rows.Close()
			t.Fatal("seed fault fixture requires at most 4 GiB RAM so index reads stay below the held range")
		}
		key, err := cas.ObjectKey(uri.Path, m.Memory.Digest)
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		proxy.mu.Lock()
		proxy.targets["/"+uri.Host+"/"+key] = true
		proxy.mu.Unlock()
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	input := json.RawMessage(`[{"type":"text","text":"\"recover-check\""}]`)
	started, err := c.CreateSession(ctx, api.CreateSessionRequest{Agent: "peer", Input: input, IdempotencyKey: "late-seed-fault"}, client.EnvironmentScopeOptions{})
	if err != nil || started.Turn == nil {
		t.Fatalf("late seed Session: %v", err)
	}
	turnResult := func(turn api.TurnAdmission) json.RawMessage {
		var raw json.RawMessage
		wait(func() (bool, error) {
			v, err := c.RetrieveSessionTurn(ctx, turn.SessionID, turn.TurnID, client.EnvironmentScopeOptions{})
			if err != nil {
				return false, err
			}
			switch v.Status {
			case "completed":
				raw = v.Result
				return true, nil
			case "failed", "cancelled", "interrupted":
				return false, fmt.Errorf("late seed Turn %s: %s", turn.TurnID, v.Status)
			}
			return false, nil
		})
		return raw
	}
	first := turnResult(*started.Turn)
	var initial struct {
		Nonce string `json:"nonce"`
	}
	if err = json.Unmarshal(first, &initial); err != nil || initial.Nonce == "" {
		t.Fatalf("Agent.init nonce missing: %s: %v", first, err)
	}
	select {
	case <-proxy.held:
	default:
		t.Fatal("no background range was held across Agent.init")
	}
	var computer, allocation, seed string
	var lease, process int64
	var ready bool
	err = pool.QueryRow(ctx, `SELECT p.computer_id::text,p.computer_lease_epoch,p.epoch,l.allocation_id::text,l.boot_seed_build_id::text,
 p.status='ready' AND p.attachment_sequence>0 AND p.fenced_at IS NULL AND l.initialized_at IS NOT NULL AND l.fenced_at IS NULL
 FROM session_processes p JOIN computer_leases l ON(l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch)
 WHERE p.environment_id=$1 AND p.session_id=$2 ORDER BY p.epoch DESC LIMIT 1`, env, started.SessionID).
		Scan(&computer, &lease, &process, &allocation, &seed, &ready)
	if err != nil || !ready {
		t.Fatalf("Agent.init was not identity-bound and ready: %v", err)
	}
	readyAt := time.Now()
	close(proxy.release)
	select {
	case <-proxy.injected:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	wait(func() (bool, error) {
		var closed bool
		err := pool.QueryRow(ctx, `SELECT l.boot_seed_failed_at IS NOT NULL AND l.fenced_at IS NOT NULL AND p.fenced_at IS NOT NULL
 FROM computer_leases l JOIN session_processes p ON(p.environment_id,p.computer_id,p.computer_lease_epoch)=(l.environment_id,l.computer_id,l.epoch)
 WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3 AND p.session_id=$4 AND p.epoch=$5`, env, computer, lease, started.SessionID, process).Scan(&closed)
		return closed, err
	})
	logs, err := os.ReadFile(filepath.Join(evidence, "worker.log"))
	if err != nil {
		t.Fatal(err)
	}
	pid := regexp.MustCompile(`memory pager attached instance_id=` + regexp.QuoteMeta(allocation) + ` pid=([0-9]+)`).FindSubmatch(logs)
	failure := regexp.MustCompile(`(?m)^.*owner=` + regexp.QuoteMeta(computer) + fmt.Sprintf(` epoch=%d .*authenticate memory chunk.*$`, lease))
	if len(pid) != 2 || !failure.Match(logs) {
		t.Fatal("missing real pager authentication failure")
	}
	command, err := os.ReadFile("/proc/" + string(pid[1]) + "/cmdline")
	if (err != nil && !errors.Is(err, os.ErrNotExist)) || bytes.Contains(command, []byte(allocation)) {
		t.Fatalf("failed seed VMM remains: %v", err)
	}
	next, err := c.EnqueueSession(ctx, started.SessionID, api.EnqueueSessionRequest{Input: input, IdempotencyKey: "after-late-seed-fault"}, client.EnvironmentScopeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// A completed Turn can still have an unpreserved filesystem tail when
	// the VM fails. Acknowledge only this loss's explicit recovery-gap hold;
	// enqueuing new work must not silently acknowledge it.
	view, err := c.RetrieveSession(ctx, started.SessionID, client.EnvironmentScopeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var acknowledged []string
	for _, hold := range view.Holds {
		if hold.Kind != "recovery_gap" || hold.RecoveryGap == nil || hold.RecoveryGap.ComputerID != computer || hold.RecoveryGap.LeaseEpoch != lease {
			t.Fatalf("unexpected hold after fenced seed loss: %+v", hold)
		}
		queued, err := c.RetrieveSessionTurn(ctx, next.SessionID, next.TurnID, client.EnvironmentScopeOptions{})
		if err != nil || queued.Status != "queued" {
			t.Fatalf("recovery gap did not hold the next Turn: %+v: %v", queued, err)
		}
		if _, err := c.ResumeSession(ctx, started.SessionID, api.ResumeSessionRequest{HoldID: *hold.ID, IdempotencyKey: "ack-late-seed-gap-" + *hold.ID}, client.EnvironmentScopeOptions{}); err != nil {
			t.Fatal(err)
		}
		acknowledged = append(acknowledged, *hold.ID)
	}
	second := turnResult(next)
	var recovered struct {
		Nonce string `json:"nonce"`
	}
	if err = json.Unmarshal(second, &recovered); err != nil || recovered.Nonce == "" || recovered.Nonce == initial.Nonce {
		t.Fatalf("Agent.init did not run freshly after fenced loss: %s: %v", second, err)
	}
	var valid bool
	var raw []byte
	err = pool.QueryRow(ctx, `SELECT current.epoch>old.epoch AND current.boot_seed_build_id IS NULL AND current.initialized_at IS NOT NULL AND current.disk_only_recovery
 AND old.boot_seed_failed_at IS NOT NULL AND old.fenced_at<=current.initialized_at AND b.status='succeeded'
 AND first_turn.status='completed' AND first_turn.process_epoch=$6 AND next_turn.status='completed' AND next_turn.process_epoch>$6,
 jsonb_build_object('computer',old.computer_id,'seed',old.boot_seed_build_id,'failed_lease',old.epoch,'failed_at',old.boot_seed_failed_at,
 'fenced_at',old.fenced_at,'next_lease',current.epoch,'disk_recovery',current.disk_only_recovery,'shared_seed_status',b.status)
 FROM computer_leases old JOIN LATERAL(SELECT * FROM computer_leases x WHERE x.environment_id=old.environment_id AND x.computer_id=old.computer_id ORDER BY epoch DESC LIMIT 1) current ON true
 JOIN boot_seed_builds b ON(b.environment_id,b.id)=(old.environment_id,old.boot_seed_build_id)
 JOIN turns first_turn ON first_turn.environment_id=old.environment_id AND first_turn.id=$4
 JOIN turns next_turn ON next_turn.environment_id=old.environment_id AND next_turn.id=$5
 WHERE old.environment_id=$1 AND old.computer_id=$2 AND old.epoch=$3`, env, computer, lease, started.Turn.TurnID, next.TurnID, process).Scan(&valid, &raw)
	if err != nil || !valid {
		t.Fatalf("late seed recovery authority: %s: %v", raw, err)
	}
	// Disk-only recovery legitimately bypasses seed selection. This proof owns
	// fresh Agent.init after fencing; the selector test owns seed exclusion.
	proof, err := json.Marshal(map[string]any{"agent_init_completed_at": readyAt, "acknowledged_recovery_gap_holds": acknowledged, "first": first, "recovered": second, "authority": json.RawMessage(raw), "failed_vmm_absent": true, "seed": seed, "proves_seed_selector_exclusion": false})
	if err == nil {
		err = os.WriteFile(filepath.Join(evidence, "late-seed-fault.json"), proof, 0600)
	}
	if err != nil {
		t.Fatal(err)
	}
}
