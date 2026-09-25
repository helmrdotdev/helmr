package controlplane

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifactgc"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

type computerGraphReclaimStore struct {
	t     *testing.T
	q     db.Querier
	calls int
	fail  bool
}

func (s *computerGraphReclaimStore) RetiredUploads(context.Context, string) ([]string, error) {
	return nil, nil
}
func (s *computerGraphReclaimStore) ReclaimUpload(context.Context, string, string) error {
	return errors.New("unexpected multipart upload")
}
func (s *computerGraphReclaimStore) ReclaimVersions(ctx context.Context, digest string) error {
	s.calls++
	// The remote side effect sees a committed irreversible retirement fence.
	_, err := s.q.UpsertCasObject(ctx, db.UpsertCasObjectParams{OrgID: pgvalue.NewUUIDv7(), Digest: digest, SizeBytes: 1, MediaType: "application/octet-stream"})
	if err == nil {
		s.t.Fatal("physical deletion preceded retirement commit")
	}
	if s.fail {
		return errors.New("injected storage failure")
	}
	return nil
}

func TestComputerGraphCollectionRetainsRootsAndLivePublishers(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "abandoned", true: "published"}[published], func(t *testing.T) {
			f, fence, input := generationPublicationFixture(t)
			if published {
				if _, err := f.server.publishInitialComputerGeneration(t.Context(), fence, input); err != nil {
					t.Fatal(err)
				}
			}
			store := &computerGraphReclaimStore{t: t, q: f.server.db}
			collector, err := artifactgc.New(f.Pool, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			if err = collector.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			if store.calls != 0 {
				t.Fatal("live publisher collected")
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1,observed_state='failed',reserved_run_id=NULL,reserved_attempt_number=NULL,reserved_workspace_version_id=NULL,terminal_at=clock_timestamp(),terminal_reason_code='fixture' WHERE id=$1`, f.runtime)
			if err = collector.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			if store.calls != 0 {
				t.Fatal("logical revocation collected live bytes")
			}
			// Explicit test evidence for the physical exclusion boundary.
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET reclaimed_at=clock_timestamp(),reclaim_evidence='{"proof":"fixture"}' WHERE id=$1`, f.runtime)
			if err = collector.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			var objects int
			if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_objects WHERE digest=$1`, input.Root.Pack.Digest).Scan(&objects); err != nil {
				t.Fatal(err)
			}
			if published {
				if objects != 1 || store.calls != 0 {
					t.Fatalf("published root collected: %d %d", objects, store.calls)
				}
			} else if objects != 0 || store.calls != 1 {
				t.Fatalf("orphan not collected: %d %d", objects, store.calls)
			}
		})
	}
}

func TestComputerGraphCollectionRetainsOtherOrganizationAndStorageRetry(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "remote retry", true: "shared membership"}[shared], func(t *testing.T) {
			f, _, input := generationPublicationFixture(t)
			if shared {
				if _, err := f.server.db.UpsertCasObject(t.Context(), db.UpsertCasObjectParams{OrgID: pgvalue.NewUUIDv7(), Digest: input.Root.Pack.Digest, SizeBytes: input.Root.Pack.SizeBytes, MediaType: "application/octet-stream"}); err != nil {
					t.Fatal(err)
				}
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1,observed_state='failed',reserved_run_id=NULL,reserved_attempt_number=NULL,reserved_workspace_version_id=NULL,terminal_at=clock_timestamp(),terminal_reason_code='fixture',reclaimed_at=clock_timestamp(),reclaim_evidence='{"proof":"fixture"}' WHERE id=$1`, f.runtime)
			store := &computerGraphReclaimStore{t: t, q: f.server.db, fail: true}
			collector, err := artifactgc.New(f.Pool, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			err = collector.Reconcile(t.Context())
			if shared {
				if err != nil || store.calls != 0 {
					t.Fatalf("shared membership lost: %d %v", store.calls, err)
				}
				return
			}
			if err == nil || store.calls != 1 {
				t.Fatalf("storage failure lost: %d %v", store.calls, err)
			}
			store.fail = false
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE cas_blobs SET next_reclaim_at=now() WHERE digest=$1`, input.Root.Pack.Digest)
			// The graph row is gone, but its durable tombstone still schedules retry.
			if err = collector.Reconcile(t.Context()); err != nil || store.calls != 2 {
				t.Fatalf("retry lost: %d %v", store.calls, err)
			}
		})
	}
}
