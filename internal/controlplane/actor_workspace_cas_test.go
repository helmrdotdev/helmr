package controlplane

import (
	"bytes"
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/cas"
	"io"
)

type actorTurnCAS struct {
	object cas.Object
	body   []byte
}

func (c actorTurnCAS) Stat(context.Context, string) (cas.Object, error) { return c.object, nil }

func (c actorTurnCAS) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(c.body)), nil
}

func (actorTurnCAS) Put(context.Context, string, io.Reader) (cas.Object, error) {
	return cas.Object{}, errors.New("unexpected CAS put")
}

func (actorTurnCAS) Publish(context.Context, string, io.Reader) (cas.Object, error) {
	return cas.Object{}, errors.New("unexpected CAS publish")
}

func (actorTurnCAS) Stage(context.Context, string) (cas.Stage, error) {
	return nil, errors.New("unexpected CAS stage")
}

func (actorTurnCAS) Delete(context.Context, string) error {
	return errors.New("unexpected CAS delete")
}
