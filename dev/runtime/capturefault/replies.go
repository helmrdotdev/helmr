package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const abortPath = "/worker/v1/computer/checkpoints/abort"

var errLostReply = errors.New("dedicated verification reply loss")
var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type replyTarget struct {
	Mode         string   `json:"mode"`
	ComputerID   string   `json:"computer_id"`
	InstanceID   string   `json:"instance_id"`
	CheckpointID string   `json:"checkpoint_id"`
	RunIDs       []string `json:"run_ids"`
}

type replyEvent struct {
	Kind        string                         `json:"kind"`
	At          time.Time                      `json:"at"`
	Dropped     bool                           `json:"dropped"`
	Error       string                         `json:"error,omitempty"`
	Disposition string                         `json:"disposition,omitempty"`
	Version     int64                          `json:"abort_desired_version,omitempty"`
	Members     []workerapi.CaptureAbortMember `json:"members,omitempty"`
}

// This state belongs to one fresh case. It records selected identities, never
// authentication headers or write capabilities. Restart between cases.
type replyFault struct {
	mu       sync.Mutex
	jailRoot string
	target   *replyTarget
	baseline *workerapi.CaptureAbortResponse
	events   []replyEvent
	stage    int
	failure  string
	relay    *guestRelay
}

func (f *replyFault) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failure == "" {
		f.failure = err.Error()
	}
}

func (f *replyFault) control(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var target replyTarget
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&target); err != nil {
			http.Error(w, "invalid reply target", 400)
			return
		}
		if err := f.arm(target); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
	} else if r.Method == http.MethodDelete {
		f.mu.Lock()
		relay := f.relay
		f.mu.Unlock()
		if relay == nil {
			http.Error(w, "no relay", 409)
			return
		}
		if err := relay.restore(); err != nil {
			f.fail(err)
			http.Error(w, err.Error(), 409)
			return
		}
	} else if r.Method != http.MethodGet {
		http.Error(w, "method", 405)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	state := map[string]any{"target": f.target, "events": f.events, "stage": f.stage, "failure": f.failure}
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
	if target.Mode == "drop" && (!canonicalUUID.MatchString(target.CheckpointID) || len(target.RunIDs) != 2) {
		return errors.New("drop mode requires the checkpoint and both captured Runs")
	}
	for _, id := range target.RunIDs {
		if !canonicalUUID.MatchString(id) {
			return errors.New("invalid Run UUID")
		}
	}
	slices.Sort(target.RunIDs)
	if len(slices.Compact(slices.Clone(target.RunIDs))) != len(target.RunIDs) {
		return errors.New("duplicate Run UUID")
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
	f.target, f.relay = &target, relay
	go relay.serve()
	return nil
}

type replyRequestKey struct{}

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
		if r.Method == http.MethodPost && (r.URL.Path == abortPath || r.URL.Path == abortPath+"/complete") {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
			if err != nil {
				http.Error(w, "invalid request", 400)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var request workerapi.CaptureAbortRequest
			if json.Unmarshal(body, &request) == nil {
				r = r.WithContext(context.WithValue(r.Context(), replyRequestKey{}, request))
			}
		}
		proxy.ServeHTTP(w, r)
	})
}

func (f *replyFault) response(r *http.Response) error {
	request, ok := r.Request.Context().Value(replyRequestKey{}).(workerapi.CaptureAbortRequest)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !ok || f.failure != "" || f.target == nil || f.target.Mode != "drop" || request.CheckpointID != f.target.CheckpointID || request.ComputerInstanceID != f.target.InstanceID {
		return nil
	}
	if r.StatusCode != http.StatusOK {
		return nil // An upstream failure is never an applied-but-lost receipt.
	}
	original := r.Body
	body, err := io.ReadAll(io.LimitReader(original, (1<<20)+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(body), original), original}
	if err != nil || len(body) > 1<<20 {
		f.failure = "could not read complete upstream receipt"
		return nil
	}
	_ = original.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	event := replyEvent{At: time.Now().UTC()}
	if r.Request.URL.Path == abortPath {
		var receipt workerapi.CaptureAbortResponse
		if err := json.Unmarshal(body, &receipt); err != nil || receipt.ComputerID != f.target.ComputerID || receipt.ComputerInstanceID != request.ComputerInstanceID || receipt.CheckpointID != request.CheckpointID || receipt.DesiredVersion != request.DesiredVersion || receipt.WorkerEpoch != request.WorkerEpoch || receipt.AbortDesiredVersion != request.DesiredVersion+1 {
			f.failure = "upstream abort receipt changed target"
			return nil
		}
		if receipt.Disposition != workerapi.CaptureAborted && receipt.Disposition != workerapi.CaptureAbortAcknowledged {
			f.failure = "upstream did not abort the selected capture"
			return nil
		}
		ids := make([]string, 0, len(receipt.Members))
		for _, member := range receipt.Members {
			ids = append(ids, member.RunID)
		}
		slices.Sort(ids)
		if receipt.Disposition == workerapi.CaptureAborted && !slices.Equal(ids, f.target.RunIDs) {
			f.failure = "abort receipt changed captured member set"
			return nil
		}
		if f.baseline != nil && (receipt.WorkerHostID != f.baseline.WorkerHostID || receipt.WriterGeneration != f.baseline.WriterGeneration || receipt.MembershipRevision != f.baseline.MembershipRevision || receipt.VMPlatformID != f.baseline.VMPlatformID || receipt.AbortDesiredVersion != f.baseline.AbortDesiredVersion) {
			f.failure = "abort replay changed source fence"
			return nil
		}
		if f.baseline != nil && receipt.Disposition == workerapi.CaptureAborted {
			for _, member := range receipt.Members {
				matched := false
				for _, prior := range f.baseline.Members {
					if member.RunID == prior.RunID && member.AttemptNumber == prior.AttemptNumber && member.RunWaitID == prior.RunWaitID && member.Lease == prior.Lease && member.BaseComputerDiskVersionID == prior.BaseComputerDiskVersionID && member.Cancelled == prior.Cancelled {
						matched = true
					}
				}
				if !matched {
					f.failure = "abort replay changed member authority"
					return nil
				}
			}
		}
		if f.baseline == nil {
			// Keep only the fence used for guest matching, never the capability.
			receipt.WriteCapability = ""
			f.baseline = &receipt
		}
		event.Kind, event.Disposition, event.Version, event.Members = "cp-abort", receipt.Disposition, receipt.AbortDesiredVersion, receipt.Members
		if f.stage == 0 && receipt.Disposition == workerapi.CaptureAborted {
			event.Dropped = true
			f.stage++
		}
		if receipt.Disposition == workerapi.CaptureAbortAcknowledged && f.stage == 4 {
			if err := f.relay.restore(); err != nil {
				f.failure = fmt.Sprintf("restore source socket: %v", err)
				return nil
			}
		}
	} else {
		var receipt workerapi.ComputerCheckpointResponse
		if err := json.Unmarshal(body, &receipt); err != nil || f.baseline == nil || receipt.ComputerInstanceID != request.ComputerInstanceID || receipt.CheckpointID != request.CheckpointID || receipt.WorkerEpoch != request.WorkerEpoch || receipt.DesiredVersion != f.baseline.AbortDesiredVersion {
			f.failure = "upstream completion receipt changed target"
			return nil
		}
		event.Kind, event.Version = "cp-complete", receipt.DesiredVersion
		if f.stage == 3 {
			event.Dropped = true
			f.stage++
		}
	}
	f.events = append(f.events, event)
	if len(f.events) > 100 {
		f.failure = "reply fault exceeded bounded case observations"
		return nil
	}
	if event.Dropped {
		return errLostReply
	}
	return nil
}
