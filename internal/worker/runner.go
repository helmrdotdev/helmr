package worker

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type ControlPlaneClient interface {
	DiscoverRunLeases(ctx context.Context) (workerapi.RunLeaseDiscoveryResponse, error)
	ClaimComputerInstance(ctx context.Context) (workerapi.ComputerInstanceClaimResponse, error)
	workerapi.ComputerMaterializerControlPlaneClient
}

type RunLeaseExecutor interface {
	ExecuteRunLease(context.Context, workerapi.RunLeaseWork) error
}

type Materializer interface {
	RunComputerMount(ctx context.Context, mount workerapi.ComputerInstanceAssignment, client workerapi.ComputerMaterializerControlPlaneClient) error
}

type Runner struct {
	client           ControlPlaneClient
	runLeaseExecutor RunLeaseExecutor
	materializer     Materializer
	capabilities     workerapi.Capabilities
	reservations     *reservation.Ledger
	pollEvery        time.Duration
	renewEvery       time.Duration
	renewWait        time.Duration
	releaseWait      time.Duration
	log              *slog.Logger
}

type Option func(*Runner)

func WithPollEvery(duration time.Duration) Option {
	return func(runner *Runner) {
		runner.pollEvery = duration
	}
}

func WithLogger(log *slog.Logger) Option {
	return func(runner *Runner) {
		runner.log = log
	}
}

func WithReservations(reservations *reservation.Ledger) Option {
	return func(runner *Runner) {
		runner.reservations = reservations
	}
}

func NewRunner(client ControlPlaneClient, executor RunLeaseExecutor, materializer Materializer, capabilities workerapi.Capabilities, opts ...Option) (*Runner, error) {
	if client == nil {
		return nil, errors.New("worker client is required")
	}
	if executor == nil {
		return nil, errors.New("worker executor is required")
	}
	if materializer == nil {
		return nil, errors.New("worker materializer is required")
	}
	runner := &Runner{
		client:           client,
		runLeaseExecutor: executor,
		materializer:     materializer,
		capabilities:     capabilities,
		pollEvery:        2 * time.Second,
		renewEvery:       10 * time.Second,
		renewWait:        5 * time.Second,
		releaseWait:      30 * time.Second,
		log:              slog.Default(),
	}
	for _, opt := range opts {
		opt(runner)
	}
	if runner.pollEvery <= 0 {
		return nil, errors.New("worker poll interval must be positive")
	}
	if runner.renewEvery <= 0 {
		return nil, errors.New("worker renew interval must be positive")
	}
	if runner.renewWait <= 0 {
		return nil, errors.New("worker renew timeout must be positive")
	}
	if runner.renewWait >= runner.renewEvery {
		return nil, errors.New("worker renew timeout must be less than renew interval")
	}
	if runner.releaseWait <= 0 {
		return nil, errors.New("worker release timeout must be positive")
	}
	if runner.reservations == nil {
		return nil, errors.New("worker capacity ledger is required")
	}
	if runner.log == nil {
		runner.log = slog.Default()
	}
	return runner, nil
}

func isStaleLease(err error) bool {
	return httpclient.IsStatus(err, http.StatusConflict)
}
