package guestd

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/wire"
)

type profileTestConnection struct {
	bytes.Buffer
}

func (connection *profileTestConnection) Close() error {
	return nil
}

func TestParseGuestProfile(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		value   string
		want    guestProfile
		wantErr string
	}{
		{name: "ordinary", want: ordinaryGuestProfile},
		{name: "manager acquisition", value: "manager-acquire", want: managerAcquireGuestProfile},
		{name: "dependency", value: "dependency", want: dependencyGuestProfile},
		{name: "unknown", value: "runtime", wantErr: `unsupported guest profile "runtime"`},
		{name: "whitespace", value: " dependency", wantErr: `unsupported guest profile " dependency"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseGuestProfile(test.value)
			if test.wantErr != "" {
				if err == nil || err.Error() != test.wantErr {
					t.Fatalf("parseGuestProfile() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGuestProfile() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("parseGuestProfile() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestGuestProfileProtocolIsolation(t *testing.T) {
	t.Parallel()

	t.Run("ordinary rejects acquisition", func(t *testing.T) {
		t.Parallel()

		connection := &profileTestConnection{}
		if err := wire.WriteStreamFrameHeader(
			connection,
			wire.StreamHeader{Type: wire.StreamTypeManagerAcquire},
			1,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Write([]byte{0}); err != nil {
			t.Fatal(err)
		}
		_, err := handleConnection(
			context.Background(),
			connection,
			Config{},
			slogDiscard(),
			newWaitingRunRegistry(),
			newWorkspaceOperationRegistry(),
		)
		if err == nil || err.Error() != "ordinary guest rejects manager acquisition" {
			t.Fatalf("handleConnection() error = %v", err)
		}
	})

	t.Run("acquisition rejects ordinary stream", func(t *testing.T) {
		t.Parallel()

		connection := &profileTestConnection{}
		if err := wire.WriteStreamFrameHeader(
			connection,
			wire.StreamHeader{Type: wire.StreamTypeCompileTaskBundle},
			0,
		); err != nil {
			t.Fatal(err)
		}
		_, err := handleConnection(
			context.Background(),
			connection,
			Config{Profile: "manager-acquire"},
			slogDiscard(),
			newWaitingRunRegistry(),
			newWorkspaceOperationRegistry(),
		)
		if err == nil || err.Error() != `manager acquisition guest rejects input type "compile-task-bundle"` {
			t.Fatalf("handleConnection() error = %v", err)
		}
	})

	t.Run("dependency uses exact manager reader", func(t *testing.T) {
		t.Parallel()

		connection := &profileTestConnection{}
		if err := wire.WriteStreamFrameHeader(
			connection,
			wire.StreamHeader{Type: wire.StreamTypeManagerAcquire},
			0,
		); err != nil {
			t.Fatal(err)
		}
		_, err := handleConnection(
			context.Background(),
			connection,
			Config{Profile: "dependency"},
			slogDiscard(),
			newWaitingRunRegistry(),
			newWorkspaceOperationRegistry(),
		)
		if err == nil || !strings.Contains(err.Error(), "manager request header is not the exact v0 header") {
			t.Fatalf("handleConnection() error = %v", err)
		}
	})
}

var _ io.ReadWriteCloser = (*profileTestConnection)(nil)
