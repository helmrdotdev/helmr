package guestd

import (
	"context"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"io"
)

type agentProcessOptions struct {
	Program      programMounts
	ProgramLease *agentProgramLease
	Identity     *agentv1.SessionIdentity
	Secrets      []*agentv1.SessionSecret
	ProtectedEnv map[string]string
	ProxyCA      []byte
}

type agentSessionProcess interface {
	start(context.Context) error
	write(context.Context, *agentv1.GuestCommand) error
	read() (*agentv1.ProgramEvent, error)
	wait(context.Context) error
	logs() (io.Reader, io.Reader)
	converge([]nativeScopeEvidence) error
	freeze(context.Context) error
	verifyFrozen() error
	thaw(context.Context) error
	close(context.Context) error
}
