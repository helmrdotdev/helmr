package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
)

func TestWorkerObservationRecoversAfterHungRequest(t *testing.T) {
	for _, draining := range []bool{false, true} {
		name := "periodic"
		if draining {
			name = "drain completion"
		}
		t.Run(name, func(t *testing.T) {
			var observations atomic.Int32
			canceled := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				switch r.URL.Path {
				case "/worker/v1/instance/credential":
					_ = json.NewEncoder(w).Encode(workerapi.HostCredentialResponse{WorkerEpoch: 7, Credential: "credential", ExpiresInSeconds: 3600})
				case "/worker/v1/instance/observations":
					if observations.Add(1) == 1 {
						<-r.Context().Done()
						close(canceled)
						return
					}
					_ = json.NewEncoder(w).Encode(workerapi.StatusResponse{Status: workerapi.StatusDraining})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client, err := workerclient.New(server.URL, workerclient.WithAuth("host", "secret"), workerclient.WithService("service"))
			if err != nil {
				t.Fatal(err)
			}
			s, err := New(Config{Recover: emptyPhysicalRecovery, ControlPlane: client, ObservationEvery: 100 * time.Millisecond, PollEvery: 10 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			statuses := make(chan workerapi.StatusResponse, 1)
			fatalWork := make(chan error, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				if draining {
					if err := s.waitForDrainReady(ctx, RecoveryEvidence{}, fatalWork); err != nil {
						fatalWork <- err
					} else {
						statuses <- workerapi.StatusResponse{Status: workerapi.StatusDraining}
					}
					cancel()
				} else {
					s.observe(ctx, RecoveryEvidence{}, func(status workerapi.StatusResponse) { statuses <- status; cancel() }, fatalWork)
				}
			}()
			defer func() {
				cancel()
				if draining {
					select {
					case fatalWork <- errors.New("test cleanup"):
					default:
					}
				}
				<-done
			}()
			select {
			case <-canceled:
			case <-ctx.Done():
				t.Fatal("hung observation never canceled")
			}
			select {
			case status := <-statuses:
				if status.Status != workerapi.StatusDraining {
					t.Fatalf("status = %+v", status)
				}
			case err := <-fatalWork:
				t.Fatalf("observation timeout treated as authority rejection: %v", err)
			case <-time.After(2 * time.Second):
				t.Fatal("observations did not resume after hung request")
			}

		})
	}
}

type observationResources struct {
	sample workerapi.ResourceObservation
}

func (r *observationResources) ApplyResourcePolicy(policy workerapi.ResourcePolicy) {
	r.sample.PolicyRevision = policy.Revision
}
func (r *observationResources) ResourceObservation() workerapi.ResourceObservation { return r.sample }
func (*observationResources) ApplyPreparationKeys([]workerapi.ReadyLeafKey)        {}
func (*observationResources) CloseOptionalAdmission()                              {}

type policyObservationControl struct {
	testControlPlane
	observe func(context.Context, workerapi.Observation) (workerapi.StatusResponse, error)
}

func (c *policyObservationControl) ObserveWorker(ctx context.Context, observation workerapi.Observation) (workerapi.StatusResponse, error) {
	return c.observe(ctx, observation)
}

func TestWorkerAppliedPolicyObservation(t *testing.T) {
	const interval = 30 * time.Second
	type step struct {
		at                 time.Duration
		reported, returned int64
		drain, hang        bool
		err                error
	}
	advance := step{at: 0, reported: 1, returned: 2}
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{"churn", []step{advance, {at: interval / 2, reported: 2, returned: 3}, {at: interval, reported: 3, returned: 3}}},
		{"unchanged", []step{{reported: 1, returned: 1}, {at: interval, reported: 1, returned: 1}}},
		{"first drain", []step{{reported: 1, returned: 2, drain: true}, {at: interval, reported: 2, returned: 2, drain: true}}},
		{"follow-up drain", []step{advance, {at: interval / 2, reported: 2, returned: 2, drain: true}}},
		{"timeout", []step{advance, {at: interval / 2, reported: 2, hang: true}, {at: interval, reported: 2, returned: 2}}},
		{"revoked", []step{advance, {at: interval / 2, reported: 2, err: testAuthorityRejectedError{}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				resources := &observationResources{sample: workerapi.ResourceObservation{PolicyRevision: 1}}
				start := time.Now()
				calls := 0
				var wantStatuses, statuses []string
				cp := &policyObservationControl{}
				cp.observe = func(req context.Context, obs workerapi.Observation) (workerapi.StatusResponse, error) {
					if calls >= len(tc.steps) {
						t.Fatal("unexpected observation")
					}
					want := tc.steps[calls]
					calls++
					if time.Since(start) != want.at || obs.Resources.Sequence != int64(calls) || obs.Resources.PolicyRevision != want.reported {
						t.Fatalf("observation %d at %v = %+v; want time %v, revision %d", calls, time.Since(start), obs.Resources, want.at, want.reported)
					}
					if calls == 1 {
						resources.sample.OptionalDiskBytes = 64
						resources.sample.ReclaimPending = true
						time.Sleep(interval / 2)
					} else if obs.Resources.OptionalDiskBytes != 64 || !obs.Resources.ReclaimPending {
						t.Fatalf("stale occupancy in %+v", obs.Resources)
					}
					if want.hang {
						<-req.Done()
						return workerapi.StatusResponse{}, req.Err()
					}
					if want.err != nil {
						return workerapi.StatusResponse{}, want.err
					}
					if calls == len(tc.steps) {
						cancel()
					}
					status := workerapi.StatusActive
					if want.drain {
						status = workerapi.StatusDraining
					}
					wantStatuses = append(wantStatuses, string(status))
					return workerapi.StatusResponse{Status: status, ResourcePolicy: workerapi.ResourcePolicy{Revision: want.returned}}, nil
				}
				healthChecks := 0
				s, err := New(Config{Recover: emptyPhysicalRecovery, ControlPlane: cp, Resources: resources, ObservationEvery: interval,
					Observation: func(Status, Snapshot, RecoveryEvidence) workerapi.Observation {
						healthChecks++
						return workerapi.Observation{}
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				s.state.Store(StatusActive)
				fatal := make(chan error, 1)
				s.observe(ctx, RecoveryEvidence{}, func(status workerapi.StatusResponse) { statuses = append(statuses, string(status.Status)) }, fatal)
				if calls != len(tc.steps) || !slices.Equal(statuses, wantStatuses) {
					t.Fatalf("%d observations, statuses %v; want %d, %v", calls, statuses, len(tc.steps), wantStatuses)
				}
				wantChecks := 1
				if tc.steps[len(tc.steps)-1].at == interval {
					wantChecks++
				}
				if healthChecks != wantChecks {
					t.Fatalf("health checks = %d, want %d", healthChecks, wantChecks)
				}
				wantErr := tc.steps[len(tc.steps)-1].err
				select {
				case err := <-fatal:
					if !errors.Is(err, wantErr) {
						t.Fatalf("fatal = %v, want %v", err, wantErr)
					}
				default:
					if wantErr != nil {
						t.Fatal("authority rejection was lost")
					}
				}
			})
		})
	}
}
