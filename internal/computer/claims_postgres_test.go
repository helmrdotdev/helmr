package computer

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func segmentInspection(label string, key string) blockformat.ObjectInspection {
	return blockformat.ObjectInspection{Segment: &blockformat.Ref{Digest: sha256.Sum256([]byte(label)), Key: key, Kind: blockformat.SegmentKind, Count: 1, Size: 64}}
}

// Initial preparation and saves compare the principal's claim versions after
// their locks: a claim bump asks the worker to re-authenticate.
func TestClaimBumpsReportStaleClaimsOnComparingPaths(t *testing.T) {
	for _, bump := range claimBumps {
		t.Run(bump.name+"/preparation", func(t *testing.T) {
			f := newPreparationFixture(t)
			dbtest.MustExec(t, t.Context(), f.Pool, bump.sql)
			root := disk.GenerationRoot{FormatVersion: 1, LogicalBytes: f.logicalBytes,
				Pack: disk.GenerationPack{Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 1024, Rank: 2},
				Page: disk.GenerationPage{Digest: "sha256:" + strings.Repeat("b", 64), Salt: strings.Repeat("c", 64), KeyID: "01912345-6789-7abc-8def-0123456789ab", Kind: 3, Count: 1, SizeBytes: 128}, Offset: 8}
			for name, operation := range map[string]func() error{
				"initial key": func() error {
					_, err := f.broker.InitialKey(t.Context(), f.principal, f.ref)
					return err
				},
				"initial object": func() error {
					return f.publisher.RegisterInitialObject(t.Context(), f.principal, f.ref, segmentInspection("stale", pgvalue.UUIDString(pgvalue.NewUUIDv7())))
				},
				"initial version": func() error {
					_, err := f.publisher.PublishInitialVersion(t.Context(), f.principal, f.ref, InitialVersion{Root: root, Config: oci.RuntimeConfig{User: "root"}})
					return err
				},
			} {
				if err := operation(); !errors.Is(err, workergroup.ErrStaleClaims) {
					t.Errorf("%s after %s: %v", name, bump.name, err)
				}
			}
		})
		t.Run(bump.name+"/save", func(t *testing.T) {
			f, _, worker, ref := instanceSaveFixture(t)
			if _, err := savePublisher(t, f).BeginSave(t.Context(), worker, ref); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, bump.sql)
			publisher := savePublisher(t, f)
			inspection := segmentInspection("stale save", pgvalue.UUIDString(pgvalue.NewUUIDv7()))
			for name, operation := range map[string]func() error{
				"begin": func() error {
					_, err := publisher.BeginSave(t.Context(), worker, ref)
					return err
				},
				"object registration":  func() error { return publisher.RegisterSaveObject(t.Context(), worker, ref, inspection) },
				"object certification": func() error { return publisher.CertifySaveObject(t.Context(), worker, ref, inspection) },
				"object reuse":         func() error { return publisher.ReuseSaveObject(t.Context(), worker, ref, inspection) },
				"publication": func() error {
					_, err := publisher.PublishSave(t.Context(), worker, ref, disk.GenerationRoot{FormatVersion: 1, LogicalBytes: 1 << 30, Pack: disk.GenerationPack{Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 1024, Rank: 2}, Page: disk.GenerationPage{Digest: "sha256:" + strings.Repeat("b", 64), Salt: strings.Repeat("c", 64), KeyID: "01912345-6789-7abc-8def-0123456789ab", Kind: 3, Count: 1, SizeBytes: 128}, Offset: 8})
					return err
				},
				"abandonment": func() error { return publisher.AbandonSave(t.Context(), worker, ref) },
			} {
				if err := operation(); !errors.Is(err, workergroup.ErrStaleClaims) {
					t.Errorf("%s after %s: %v", name, bump.name, err)
				}
			}
		})
	}
}

// savePublisher returns a publisher over the fixture's database whose object
// storage is unavailable.
func savePublisher(t *testing.T, f runtest.Fixture) Publisher {
	t.Helper()
	publisher, err := NewPublisher(f.Pool, unavailableObjects{})
	if err != nil {
		t.Fatal(err)
	}
	return publisher
}

type unavailableObjects struct{}

func (unavailableObjects) Stat(context.Context, string) (cas.Object, error) {
	return cas.Object{}, errors.New("object storage is not configured")
}
