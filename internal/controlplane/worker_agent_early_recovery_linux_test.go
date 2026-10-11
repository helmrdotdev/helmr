//go:build linux && computerproof

package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	cass3 "github.com/helmrdotdev/helmr/internal/cas/s3"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const nativeStatFaultHeader = "X-Helmr-Proof-Stat-Fault"
const nativeStatWindowHeader = "X-Helmr-Proof-Stat-Window"

type nativeStatFaultKey struct{}
type nativeStatFault struct {
	digest   string
	response http.ResponseWriter
	window   *nativeStatWindow
}
type nativeFaultStore struct{ *cass3.Store }

// The child composes the actual service with one request-scoped storage error.
// Other requests and production binaries have no fault control surface.
func (s nativeFaultStore) Stat(ctx context.Context, digest string) (cas.Object, error) {
	if fault, ok := ctx.Value(nativeStatFaultKey{}).(nativeStatFault); ok && fault.digest == digest {
		if fault.window == nil || fault.window.unavailable(fault.response) {
			fault.response.Header().Set(nativeStatFaultHeader, "observed")
			return cas.Object{}, cas.ErrUnavailable
		}
	}
	return s.Store.Stat(ctx, digest)
}
func nativeStorageFaultHandler(next http.Handler) http.Handler {
	var windows sync.Map
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if digest := r.Header.Get(nativeStatFaultHeader); digest != "" {
			fault := nativeStatFault{digest: digest, response: w}
			if key := r.Header.Get(nativeStatWindowHeader); key != "" {
				window, _ := windows.LoadOrStore(key+":"+digest, &nativeStatWindow{})
				fault.window = window.(*nativeStatWindow)
			}
			r = r.WithContext(context.WithValue(r.Context(), nativeStatFaultKey{}, fault))
		}
		next.ServeHTTP(w, r)
	})
}

// Starts only when the selected real storage call is reached. Retries retain
// this monotonic deadline; unrelated requests never acquire a held global gate.
type nativeStatWindow struct {
	mu      sync.Mutex
	started time.Time
}

func (s *nativeStatWindow) unavailable(w http.ResponseWriter) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started.IsZero() {
		s.started = time.Now()
	}
	w.Header().Set(nativeStatWindowHeader+"-Start", s.started.UTC().Format(time.RFC3339Nano))
	w.Header().Set(nativeStatWindowHeader+"-End", s.started.Add(2*time.Second).UTC().Format(time.RFC3339Nano))
	return time.Since(s.started) < 2*time.Second
}

type nativeEarlyRecovery struct {
	RootPack               string
	Capture, PartialUpload nativeRecoveryStage
}
type nativeRecoveryStage struct {
	RequestDigest, ObjectDigest string
	Attempts                    int
	ReconciledAt                time.Time
	Restart                     *nativePublicationRestart
}

func (g *nativePublicationGate) serveEarlyRecovery(w http.ResponseWriter, r *http.Request) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var publication workerapi.AgentSavePublication
	var object workerapi.AgentSaveObject
	capture := r.URL.Path == "/worker/v1/computer-saves/capture"
	var save string
	if capture {
		if err := json.Unmarshal(body, &publication); err != nil {
			return err
		}
		save = publication.Save.SaveID
	} else {
		if err := json.Unmarshal(body, &object); err != nil {
			return err
		}
		save = object.Save.SaveID
	}
	var native bool
	err = g.pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM computer_saves s
 JOIN turns t ON (t.environment_id,t.id)=(s.environment_id,s.turn_id)
 JOIN sessions se ON (se.environment_id,se.id)=(t.environment_id,t.session_id)
 JOIN agents a ON (a.environment_id,a.id)=(se.environment_id,se.agent_id)
 WHERE s.environment_id=$1 AND s.id=$2 AND a.name IN ('codex','claude'))`, g.environment, save).Scan(&native)
	if err != nil {
		return err
	}
	if !native {
		g.next.ServeHTTP(w, r)
		return nil
	}
	// Serialize fault windows, including their before/after retention observations.
	g.restartMu.Lock()
	defer g.restartMu.Unlock()
	g.mu.Lock()
	if g.recovery == nil {
		g.recovery = map[string]*nativeEarlyRecovery{}
	}
	recovery := g.recovery[save]
	if recovery == nil {
		recovery = &nativeEarlyRecovery{}
		g.recovery[save] = recovery
	}
	g.mu.Unlock()
	stage := &recovery.Capture
	digest := ""
	if !capture {
		if object.Inspection.Segment == nil {
			g.next.ServeHTTP(w, r)
			return nil
		}
		digest = fmt.Sprintf("sha256:%x", object.Inspection.Segment.Digest)
		stage = &recovery.PartialUpload
		if stage.ObjectDigest != "" && stage.ObjectDigest != digest {
			g.next.ServeHTTP(w, r)
			return nil
		}
		if stage.Attempts == 0 {
			var certified, rootCertified, pinned bool
			var certifiedBytes int64
			err = g.pool.QueryRow(r.Context(), `SELECT
    EXISTS(SELECT 1 FROM computer_objects WHERE environment_id=$1 AND digest=$2 AND certified),
    EXISTS(SELECT 1 FROM computer_objects WHERE environment_id=$1 AND digest=$3 AND certified),
 EXISTS(SELECT 1 FROM computer_object_pins WHERE environment_id=$1 AND save_id=$4 AND digest=$2),
 COALESCE((SELECT sum(o.size_bytes) FROM computer_object_pins p JOIN computer_objects o ON (o.environment_id,o.digest)=(p.environment_id,p.digest) WHERE p.environment_id=$1 AND p.save_id=$4 AND o.certified),0)`, g.environment, digest, recovery.RootPack, save).Scan(&certified, &rootCertified, &pinned, &certifiedBytes)
			if err != nil {
				return err
			}
			if certified || certifiedBytes == 0 {
				g.next.ServeHTTP(w, r)
				return nil
			}
			if !pinned || rootCertified || recovery.RootPack == "" {
				return errors.New("upload fault did not precede root certification")
			}
			// Inspect the real object before injecting an unavailable Stat in the child.
			// This is a transient storage-call failure, not a claim of a natural S3 outage.
			store, err := cass3.New(r.Context(), g.restart.casURI)
			if err != nil {
				return err
			}
			stored, err := store.Stat(r.Context(), digest)
			if err != nil {
				return err
			}
			if stored.SizeBytes != object.Inspection.Segment.Size {
				return errors.New("partial upload object length mismatch")
			}
			stage.ObjectDigest = digest
		}
	} else {
		recovery.RootPack = publication.Root.Pack.Digest
	}
	requestDigest := fmt.Sprintf("%x", sha256.Sum256(body))
	if stage.RequestDigest != "" && stage.RequestDigest != requestDigest {
		return errors.New("early save retry changed exact request")
	}
	stage.RequestDigest = requestDigest
	stage.Attempts++
	if stage.Attempts == 1 && !capture {
		r.Header.Set(nativeStatFaultHeader, digest)
	}
	response := httptest.NewRecorder()
	g.next.ServeHTTP(response, r)
	if stage.Attempts == 1 {
		if capture && response.Code != http.StatusNoContent {
			return fmt.Errorf("capture before lost acknowledgement: %d", response.Code)
		}
		if !capture && (response.Code != http.StatusServiceUnavailable || response.Header().Get(nativeStatFaultHeader) != "observed") {
			return fmt.Errorf("storage fault not observed: %d", response.Code)
		}
		proof := &nativePublicationRestart{BeforePublicationCommit: true}
		var state string
		proof.Before, proof.LeaseExpiresAt, state, err = g.restartIdentity(r.Context(), save, !capture)
		if err != nil {
			return err
		}
		if state != "captured" {
			return fmt.Errorf("early save state: %s", state)
		}
		if !capture && (proof.Before.CertifiedPinnedObjectBytes <= 0 || proof.Before.CertifiedPinnedObjectBytes >= proof.Before.PinnedObjectBytes) {
			return errors.New("partial upload does not retain a mixed certified and uncertified object set")
		}
		if err := g.restartSavedCut(r.Context(), save, proof, "captured", !capture); err != nil {
			return err
		}
		if !capture {
			var pending bool
			if err := g.pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM computer_object_pins p JOIN computer_objects o ON (o.environment_id,o.digest)=(p.environment_id,p.digest) WHERE p.environment_id=$1 AND p.save_id=$2 AND p.digest=$3 AND NOT o.certified)`, g.environment, save, digest).Scan(&pending); err != nil {
				return err
			}
			if !pending {
				return errors.New("restart lost the pinned uncertified upload")
			}
		}
		stage.Restart = proof
		http.Error(w, "save owner restarted; retry same operation", http.StatusServiceUnavailable)
		return nil
	}
	if response.Code == http.StatusNoContent {
		stage.ReconciledAt = time.Now().UTC()
	}
	for name, values := range response.Header() {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(response.Code)
	_, err = io.Copy(w, response.Body)
	return err
}
