package computerhost

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk"
)

type saveHostFixture struct {
	mu              sync.Mutex
	steps           []string
	fail            string
	failed          bool
	computer        string
	root            disk.VersionRoot
	blocked, joined chan struct{}
}

func (f *saveHostFixture) step(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, name)
	if f.fail == name && !f.failed {
		f.failed = true
		return errors.New("lost " + name + " response")
	}
	return nil
}
func (f *saveHostFixture) Publish(context.Context, cas.Descriptor, *os.File) (cas.Object, error) {
	return cas.Object{}, errors.New("unexpected upload in lifecycle fixture")
}

type saveHostCapture struct{ f *saveHostFixture }

func (c saveHostCapture) Root() disk.VersionRoot { return c.f.root }
func (c saveHostCapture) Publish(ctx context.Context, _ disk.ContinuationPublication) error {
	if c.f.blocked != nil {
		close(c.f.blocked)
		<-ctx.Done()
		<-c.f.joined
		return ctx.Err()
	}
	if err := c.f.step("objects"); err != nil {
		return err
	}
	if c.f.fail == "abandon" {
		return errors.New("upload failed before commit")
	}
	return nil
}
func (c saveHostCapture) Adopt(context.Context, int) error            { return c.f.step("adopt") }
func (c saveHostCapture) Collect(context.Context, int) (int64, error) { return 0, nil }
func (c saveHostCapture) Release()                                    { _ = c.f.step("release") }
