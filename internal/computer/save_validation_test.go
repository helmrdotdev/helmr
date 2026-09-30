package computer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// forbiddenDatabase fails the test on any statement or transaction.
type forbiddenDatabase struct{ t *testing.T }

func (d forbiddenDatabase) Begin(context.Context) (pgx.Tx, error) {
	d.t.Error("save validation began a transaction")
	return nil, errors.New("database access is forbidden")
}

func (d forbiddenDatabase) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	d.t.Error("save validation executed a statement")
	return pgconn.CommandTag{}, errors.New("database access is forbidden")
}

func (d forbiddenDatabase) Query(context.Context, string, ...any) (pgx.Rows, error) {
	d.t.Error("save validation ran a query")
	return nil, errors.New("database access is forbidden")
}

func (d forbiddenDatabase) QueryRow(context.Context, string, ...any) pgx.Row {
	d.t.Error("save validation read a row")
	return forbiddenRow{}
}

type forbiddenRow struct{}

func (forbiddenRow) Scan(...any) error { return errors.New("database access is forbidden") }

type forbiddenObjects struct{ t *testing.T }

func (o forbiddenObjects) Stat(context.Context, string) (cas.Object, error) {
	o.t.Error("save validation read object storage")
	return cas.Object{}, errors.New("object storage access is forbidden")
}

// A save reference without a positive sequence or writer generation is
// rejected as input before any database or storage access.
func TestSaveOperationsValidateBeforeDatabaseAccess(t *testing.T) {
	publisher, err := NewPublisher(forbiddenDatabase{t}, forbiddenObjects{t})
	if err != nil {
		t.Fatal(err)
	}
	principal := workergroup.HostPrincipal{HostID: uuid.NewV7(), GroupID: uuid.NewV7(), Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	inspection := segmentInspection("invalid save", uuid.NewV7().String())
	root := disk.GenerationRoot{FormatVersion: 1, LogicalBytes: 1 << 30,
		Pack: disk.GenerationPack{Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 1024, Rank: 2},
		Page: disk.GenerationPage{Digest: "sha256:" + strings.Repeat("b", 64), Salt: strings.Repeat("c", 64), KeyID: "01912345-6789-7abc-8def-0123456789ab", Kind: 3, Count: 1, SizeBytes: 128}, Offset: 8}
	for name, ref := range map[string]SaveRef{
		"sequence":   {EnvironmentID: uuid.NewV7(), InstanceID: uuid.NewV7(), SaveID: uuid.NewV7(), Sequence: 0, WriterGeneration: 1},
		"generation": {EnvironmentID: uuid.NewV7(), InstanceID: uuid.NewV7(), SaveID: uuid.NewV7(), Sequence: 1, WriterGeneration: 0},
	} {
		for operation, call := range map[string]func() error{
			"begin": func() error {
				_, err := publisher.BeginSave(t.Context(), principal, ref)
				return err
			},
			"object registration":  func() error { return publisher.RegisterSaveObject(t.Context(), principal, ref, inspection) },
			"object certification": func() error { return publisher.CertifySaveObject(t.Context(), principal, ref, inspection) },
			"object reuse":         func() error { return publisher.ReuseSaveObject(t.Context(), principal, ref, inspection) },
			"publication": func() error {
				_, err := publisher.PublishSave(t.Context(), principal, ref, root)
				return err
			},
			"adoption":    func() error { return publisher.AdoptSave(t.Context(), principal, ref, root) },
			"abandonment": func() error { return publisher.AbandonSave(t.Context(), principal, ref) },
		} {
			var input InputError
			if err := call(); !errors.As(err, &input) {
				t.Errorf("%s with invalid %s: %v", operation, name, err)
			}
		}
	}
}
