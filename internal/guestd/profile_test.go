package guestd

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/wire"
)

type profileTestConnection struct {
	bytes.Buffer
}

func (connection *profileTestConnection) Close() error {
	return nil
}

type scriptedNetConn struct {
	reader  *bytes.Reader
	written bytes.Buffer
}

func (conn *scriptedNetConn) Read(value []byte) (int, error) {
	return conn.reader.Read(value)
}

func (conn *scriptedNetConn) Write(value []byte) (int, error) {
	return conn.written.Write(value)
}

func (*scriptedNetConn) Close() error                     { return nil }
func (*scriptedNetConn) LocalAddr() net.Addr              { return nil }
func (*scriptedNetConn) RemoteAddr() net.Addr             { return nil }
func (*scriptedNetConn) SetDeadline(time.Time) error      { return nil }
func (*scriptedNetConn) SetReadDeadline(time.Time) error  { return nil }
func (*scriptedNetConn) SetWriteDeadline(time.Time) error { return nil }

type oneShotTestListener struct {
	conn     net.Conn
	accepted int
}

func (listener *oneShotTestListener) Accept() (net.Conn, error) {
	listener.accepted++
	if listener.conn == nil {
		return nil, errors.New("unexpected second accept")
	}
	conn := listener.conn
	listener.conn = nil
	return conn, nil
}

func (listener *oneShotTestListener) Close() error {
	return nil
}

func (listener *oneShotTestListener) Addr() net.Addr {
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
		{name: "build install", value: "build-install", want: buildInstallGuestProfile},
		{name: "build analysis", value: "build-analyze", want: buildAnalyzeGuestProfile},
		{name: "Program proof", value: "program-proof", want: programProofGuestProfile},
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
			wire.StreamHeader{Type: wire.StreamTypeRunImage},
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
		if err == nil || err.Error() != `manager acquisition guest rejects input type "run-image"` {
			t.Fatalf("handleConnection() error = %v", err)
		}
	})

	t.Run("build profile rejects a mismatched stream", func(t *testing.T) {
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
			Config{Profile: "build-install"},
			slogDiscard(),
			newWaitingRunRegistry(),
			newWorkspaceOperationRegistry(),
		)
		if err == nil || err.Error() != `build install guest rejects input type "manager-acquire"` {
			t.Fatalf("handleConnection() error = %v", err)
		}
	})
}

func TestBuildGuestIsOneShot(t *testing.T) {
	t.Parallel()

	server, client := net.Pipe()
	listener := &oneShotTestListener{conn: server}
	result := make(chan error, 1)
	go func() {
		result <- serveOneShotGuest(
			context.Background(),
			listener,
			Config{Profile: "manager-acquire"},
			slogDiscard(),
			newWaitingRunRegistry(),
			newWorkspaceOperationRegistry(),
		)
	}()
	if err := wire.WriteStreamFrameHeader(
		client,
		wire.StreamHeader{Type: wire.StreamTypeRunImage},
		0,
	); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil ||
			!strings.Contains(err.Error(), "manager acquisition guest rejects input") {
			t.Fatalf("serveOneShotGuest() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serveOneShotGuest() did not return")
	}
	if listener.accepted != 1 {
		t.Fatalf("listener accepted %d connections, want 1", listener.accepted)
	}
}

var _ net.Listener = (*oneShotTestListener)(nil)
