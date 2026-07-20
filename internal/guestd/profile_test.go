package guestd

import (
	"bytes"
	"context"
	"errors"
	"io"
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

type oneShotTestListener struct {
	conn     net.Conn
	accepted int
}

type orderedTestListener struct {
	name   string
	conn   net.Conn
	events *[]string
}

func (listener *orderedTestListener) Accept() (net.Conn, error) {
	*listener.events = append(*listener.events, listener.name)
	if listener.conn == nil {
		return nil, errors.New("unexpected second accept")
	}
	conn := listener.conn
	listener.conn = nil
	return conn, nil
}

func (*orderedTestListener) Close() error {
	return nil
}

func (*orderedTestListener) Addr() net.Addr {
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

	t.Run("dependency requires dedicated admission", func(t *testing.T) {
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
		if err == nil || err.Error() !=
			"dependency guest connections require dedicated admission" {
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
		wire.StreamHeader{Type: wire.StreamTypeCompileTaskBundle},
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

func TestResolveBootstrapPrecedesManagerAdmission(t *testing.T) {
	token := bytes.Repeat([]byte{0x5a}, 32)
	record := make([]byte, 37)
	copy(record[:4], []byte("HRB0"))
	record[4] = 0x01
	copy(record[5:], token)
	bootstrapConn := &scriptedNetConn{reader: bytes.NewReader(record)}
	managerConn := &scriptedNetConn{reader: bytes.NewReader(nil)}
	var events []string

	err := serveDependencyGuest(
		context.Background(),
		&orderedTestListener{
			name:   "manager",
			conn:   managerConn,
			events: &events,
		},
		&orderedTestListener{
			name:   "bootstrap",
			conn:   bootstrapConn,
			events: &events,
		},
	)
	if err == nil || !strings.Contains(err.Error(), "read manager request header") {
		t.Fatalf("serveDependencyGuest() error = %v", err)
	}
	if len(events) != 2 ||
		events[0] != "bootstrap" ||
		events[1] != "manager" {
		t.Fatalf("accept order = %v", events)
	}
	if bootstrapConn.written.String() != "HRA0" {
		t.Fatalf(
			"bootstrap acknowledgement = %q",
			bootstrapConn.written.String(),
		)
	}
}

var _ io.ReadWriteCloser = (*profileTestConnection)(nil)
var _ net.Listener = (*oneShotTestListener)(nil)
