package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

const abortPath = "/worker/v1/agent-computers/source-abort"

var errLostReply = errors.New("dedicated verification reply loss")
var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type replyMember struct {
	SessionID    string `json:"session_id"`
	ProcessEpoch int64  `json:"process_epoch"`
}
type replyTarget struct {
	Mode             string        `json:"mode"`
	ComputerID       string        `json:"computer_id"`
	InstanceID       string        `json:"instance_id"`
	WriterGeneration uint64        `json:"writer_generation"`
	CheckpointID     string        `json:"checkpoint_id,omitempty"`
	Members          []replyMember `json:"members,omitempty"`
}
type replyEvent struct {
	Kind      string    `json:"kind"`
	At        time.Time `json:"at"`
	Dropped   bool      `json:"dropped"`
	Installed bool      `json:"installed,omitempty"`
	Activated bool      `json:"activated,omitempty"`
	Version   int64     `json:"desired_version,omitempty"`
	Digest    string    `json:"digest,omitempty"`
	Stopped   []string  `json:"stopped_session_ids,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// Raw protocol authority remains private. The observer emits selected identities
// and digests, never credentials, headers, grants or serialized messages.
type replyFault struct {
	mu        sync.Mutex
	jailRoot  string
	target    *replyTarget
	capture   *agentv1.ComputerSessionCapture
	baseline  *agentv1.ComputerSessionInstallation
	installed bool
	controls  *agentv1.ComputerSessionControls
	events    []replyEvent
	stage     int
	failure   string
	relay     *guestRelay
	gate      chan struct{}
	released  bool
}

func (f *replyFault) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failure == "" {
		f.failure = err.Error()
	}
}
func (f *replyFault) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gate != nil && !f.released {
		close(f.gate)
		f.released = true
	}
}
func (f *replyFault) control(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var target replyTarget
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&target) != nil {
			http.Error(w, "invalid reply target", 400)
			return
		}
		if err := f.arm(target); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	case http.MethodPatch:
		f.mu.Lock()
		ready := f.capture != nil && f.gate != nil
		f.mu.Unlock()
		if !ready {
			http.Error(w, "capture not held", http.StatusConflict)
			return
		}
		f.release()
	case http.MethodDelete:
		f.release()
		f.mu.Lock()
		relay := f.relay
		f.mu.Unlock()
		if relay == nil {
			http.Error(w, "no relay", http.StatusConflict)
			return
		}
		if err := relay.restore(); err != nil {
			f.fail(err)
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	case http.MethodGet:
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	state := map[string]any{"target": f.target, "events": f.events, "stage": f.stage, "failure": f.failure, "released": f.released}
	if f.relay != nil {
		state["relay"] = f.relay.status()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
}
func (f *replyFault) arm(target replyTarget) error {
	if !canonicalUUID.MatchString(target.ComputerID) || !canonicalUUID.MatchString(target.InstanceID) {
		return errors.New("require exact Computer and Instance UUIDs")
	}
	if target.Mode != "passthrough" && target.Mode != "drop" {
		return errors.New("require passthrough or drop mode")
	}
	if target.Mode == "drop" && (target.CheckpointID != "" || target.WriterGeneration == 0 || len(target.Members) != 2) {
		return errors.New("drop mode requires source generation and both Session processes before capture")
	}
	for _, m := range target.Members {
		if !canonicalUUID.MatchString(m.SessionID) || m.ProcessEpoch <= 0 {
			return errors.New("invalid Session process")
		}
	}
	if len(target.Members) == 2 && target.Members[0].SessionID == target.Members[1].SessionID {
		return errors.New("duplicate Session")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.target != nil {
		return errors.New("reply case already armed")
	}
	relay, err := installGuestRelay(filepath.Join(f.jailRoot, target.InstanceID, "root", "vsock.sock"), f)
	if err != nil {
		return err
	}
	f.target, f.relay, f.gate = &target, relay, make(chan struct{})
	go relay.serve()
	return nil
}
func messageDigest(p proto.Message) []byte {
	raw, _ := (proto.MarshalOptions{Deterministic: true}).Marshal(p)
	sum := sha256.Sum256(raw)
	return sum[:]
}
func digest(p proto.Message) string { return hex.EncodeToString(messageDigest(p)) }

func (f *replyFault) record(event replyEvent, stage int) bool {
	if len(f.events) >= 100 {
		f.failure = "reply fault exceeded bounded case observations"
		return false
	}
	event.At = time.Now().UTC()
	event.Dropped = stage >= 0 && f.stage == stage
	if event.Dropped {
		f.stage++
	}
	f.events = append(f.events, event)
	return event.Dropped
}
func (f *replyFault) selected(c *agentv1.ComputerSessionCapture) bool {
	if f.target == nil || f.target.Mode != "drop" || c == nil {
		return false
	}
	e := c.GetEnvelope()
	if e.GetComputerId() != f.target.ComputerID || e.GetComputerInstanceId() != f.target.InstanceID || e.GetWriterGeneration() != f.target.WriterGeneration || len(c.Sessions) != len(f.target.Members) {
		return false
	}
	for _, m := range f.target.Members {
		found := false
		for _, s := range c.Sessions {
			if s.SessionId == m.SessionID && s.ProcessEpoch == m.ProcessEpoch {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return f.target.CheckpointID == "" || c.CheckpointId == f.target.CheckpointID
}
func (f *replyFault) installation(p *agentv1.ComputerSessionInstallation) bool {
	if p == nil || !p.SourceAbort || !proto.Equal(p.Capture, f.capture) || p.DesiredVersion != f.capture.DesiredVersion+1 {
		return false
	}
	// The continuation envelope refreshes operation authority; only its source
	// identity, not its expiring request credentials, is fixed by capture.
	e := p.GetEnvelope()
	return e.GetComputerId() == f.target.ComputerID && e.GetComputerInstanceId() == f.target.InstanceID && e.GetWriterGeneration() == f.target.WriterGeneration && e.GetOperationId() == f.capture.CheckpointId
}

type replyRequestKey struct{}
type replyRequest struct {
	prepare      *workerapi.AgentComputerSourceAbortRequest
	installation *workerapi.AgentComputerInstallationRequest
}

func (f *replyFault) proxy(upstream string) http.Handler {
	proxy := &httputil.ReverseProxy{Rewrite: func(p *httputil.ProxyRequest) {
		p.Out.URL.Scheme, p.Out.URL.Host = "http", upstream
		p.Out.Host = upstream
	}, ModifyResponse: f.response, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
		if errors.Is(err, errLostReply) {
			panic(http.ErrAbortHandler)
		}
		http.Error(w, "Control Plane forwarding failed", http.StatusBadGateway)
	}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && (r.URL.Path == abortPath+"/prepare" || r.URL.Path == abortPath+"/complete") {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 17<<20))
			if err != nil {
				http.Error(w, "invalid request", 400)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			request := replyRequest{}
			if r.URL.Path == abortPath+"/prepare" {
				var p workerapi.AgentComputerSourceAbortRequest
				if json.Unmarshal(body, &p) == nil {
					request.prepare = &p
				}
			} else {
				var p workerapi.AgentComputerInstallationRequest
				if json.Unmarshal(body, &p) == nil {
					request.installation = &p
				}
			}
			r = r.WithContext(context.WithValue(r.Context(), replyRequestKey{}, request))
		}
		proxy.ServeHTTP(w, r)
	})
}
func (f *replyFault) response(r *http.Response) error {
	request, ok := r.Request.Context().Value(replyRequestKey{}).(replyRequest)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !ok || f.failure != "" || f.capture == nil || r.StatusCode < 200 || r.StatusCode >= 300 {
		return nil
	}
	if request.prepare != nil {
		q := request.prepare
		if q.CheckpointID != f.capture.CheckpointId || uint64(q.LeaseEpoch) != f.target.WriterGeneration {
			return nil
		}
		original := r.Body
		body, err := io.ReadAll(io.LimitReader(original, (17<<20)+1))
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(body), original), original}
		if err != nil || len(body) > 17<<20 {
			f.failure = "could not read complete upstream receipt"
			return nil
		}
		_ = original.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		var response workerapi.AgentComputerInstallationResponse
		p := new(agentv1.ComputerSessionInstallation)
		if json.Unmarshal(body, &response) != nil || proto.Unmarshal(response.Installation, p) != nil || !f.installation(p) {
			f.failure = "upstream preparation changed source capture"
			return nil
		}
		if f.installed && !proto.Equal(p, f.baseline) {
			f.failure = "upstream replaced installed authority"
			return nil
		}
		f.baseline = proto.Clone(p).(*agentv1.ComputerSessionInstallation)
		stopped := []string{}
		for _, s := range p.StoppedSessions {
			stopped = append(stopped, s.SessionId)
		}
		slices.Sort(stopped)
		if f.record(replyEvent{Kind: "cp-prepare", Version: p.DesiredVersion, Digest: digest(p), Stopped: stopped}, 1) {
			return errLostReply
		}
	} else if request.installation != nil {
		p := new(agentv1.ComputerSessionInstallation)
		if proto.Unmarshal(request.installation.Installation, p) != nil || !f.selected(p.Capture) {
			return nil
		}
		receipt := new(agentv1.ComputerSessionReceipt)
		if !f.installed || !proto.Equal(p, f.baseline) || proto.Unmarshal(request.installation.Receipt, receipt) != nil || !receipt.Activated || !receipt.ActivationStarted || receipt.Frozen || !receipt.Installed || receipt.Error != "" || receipt.ErrorCode != 0 || receipt.CheckpointId != f.capture.CheckpointId || receipt.DesiredVersion != p.DesiredVersion || f.controls == nil || !bytes.Equal(receipt.ControlsDigest, messageDigest(f.controls)) {
			f.failure = "completion changed installed authority or activation receipt"
			return nil
		}
		if f.record(replyEvent{Kind: "cp-complete", Version: p.DesiredVersion, Digest: digest(p)}, 4) {
			return errLostReply
		}
		if f.stage == 5 && f.relay != nil {
			if err := f.relay.restore(); err != nil {
				f.failure = err.Error()
			}
		}
	}
	return nil
}
