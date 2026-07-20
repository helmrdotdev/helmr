package deployment

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/helmrdotdev/helmr/internal/vm"
)

func TestPrepareResolveRelayBindsBeforeBootstrap(t *testing.T) {
	random := bytes.Repeat([]byte{0x5a}, relayTokenBytes)
	stream := &relayTestStream{
		response: bytes.NewReader(resolveBootstrapAck[:]),
	}
	endpoint := &relayTestEndpoint{}
	session := &relayTestPreparedSession{
		stream:   stream,
		endpoint: endpoint,
	}

	token, gotEndpoint, err := prepareResolveRelay(
		context.Background(),
		session,
		bytes.NewReader(random),
	)
	if err != nil {
		t.Fatal(err)
	}
	if gotEndpoint != endpoint {
		t.Fatal("prepareResolveRelay returned a different endpoint")
	}
	if !slices.Equal(token[:], random) {
		t.Fatal("prepareResolveRelay returned the wrong token")
	}
	if !slices.Equal(session.events, []string{"bind", "dial"}) {
		t.Fatalf("events = %v, want [bind dial]", session.events)
	}
	if !stream.writeClosed {
		t.Fatal("bootstrap request was not half-closed")
	}
	request := stream.request.Bytes()
	if len(request) != 4+1+relayTokenBytes ||
		!bytes.Equal(request[:4], resolveBootstrapMagic[:]) ||
		request[4] != resolveOperation ||
		!bytes.Equal(request[5:], random) {
		t.Fatalf("bootstrap request = %x", request)
	}
}

func TestPrepareResolveRelayClosesEndpointOnBootstrapFailure(t *testing.T) {
	endpoint := &relayTestEndpoint{}
	session := &relayTestPreparedSession{
		stream: &relayTestStream{
			response: bytes.NewReader([]byte("bad!")),
		},
		endpoint: endpoint,
	}
	random := bytes.Repeat([]byte{0x5a}, relayTokenBytes)

	_, _, err := prepareResolveRelay(
		context.Background(),
		session,
		bytes.NewReader(random),
	)
	if err == nil {
		t.Fatal("prepareResolveRelay accepted an invalid acknowledgement")
	}
	if !endpoint.closed {
		t.Fatal("prepareResolveRelay left its endpoint open")
	}
	if bytes.Contains([]byte(err.Error()), random) {
		t.Fatal("prepareResolveRelay leaked the token in its error")
	}
}

func TestAcceptResolveBootstrapRequiresExactTerminatedRecord(t *testing.T) {
	token := bytes.Repeat([]byte{0xa5}, relayTokenBytes)
	valid := make([]byte, 4+1+relayTokenBytes)
	copy(valid[:4], resolveBootstrapMagic[:])
	valid[4] = resolveOperation
	copy(valid[5:], token)

	for _, test := range []struct {
		name  string
		input []byte
		ok    bool
	}{
		{name: "exact", input: valid, ok: true},
		{name: "truncated", input: valid[:len(valid)-1]},
		{name: "extended", input: append(slices.Clone(valid), 0)},
		{
			name:  "wrong magic",
			input: append([]byte("BAD!"), valid[4:]...),
		},
		{
			name: "wrong operation",
			input: append(
				append(slices.Clone(valid[:4]), byte(0xff)),
				valid[5:]...,
			),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection := &relayBootstrapBuffer{
				Reader: bytes.NewReader(test.input),
			}
			got, err := AcceptResolveBootstrap(connection)
			if test.ok {
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got[:], token) {
					t.Fatal("bootstrap returned the wrong token")
				}
				if !bytes.Equal(connection.written.Bytes(), resolveBootstrapAck[:]) {
					t.Fatalf(
						"bootstrap acknowledgement = %q",
						connection.written.Bytes(),
					)
				}
				return
			}
			if err == nil {
				t.Fatal("bootstrap accepted an invalid record")
			}
			if bytes.Contains([]byte(err.Error()), token) {
				t.Fatal("bootstrap leaked the token in its error")
			}
			if connection.written.Len() != 0 {
				t.Fatal("bootstrap acknowledged an invalid record")
			}
		})
	}
}

type relayTestPreparedSession struct {
	stream   vm.Stream
	endpoint vm.HostEndpoint
	events   []string
}

func (*relayTestPreparedSession) Open(context.Context) (vm.Session, error) {
	return nil, errors.New("open is unsupported")
}

func (session *relayTestPreparedSession) DialGuest(
	context.Context,
	uint32,
) (vm.Stream, error) {
	session.events = append(session.events, "dial")
	return session.stream, nil
}

func (session *relayTestPreparedSession) BindHost(
	context.Context,
	uint32,
) (vm.HostEndpoint, error) {
	session.events = append(session.events, "bind")
	return session.endpoint, nil
}

func (*relayTestPreparedSession) Close(context.Context) error {
	return nil
}

type relayTestEndpoint struct {
	closed bool
}

func (*relayTestEndpoint) Accept(context.Context) (vm.Stream, error) {
	return nil, errors.New("accept is unsupported")
}

func (endpoint *relayTestEndpoint) Close() error {
	endpoint.closed = true
	return nil
}

type relayTestStream struct {
	request     bytes.Buffer
	response    *bytes.Reader
	writeClosed bool
	closed      bool
}

func (stream *relayTestStream) Read(value []byte) (int, error) {
	if stream.response == nil {
		return 0, io.EOF
	}
	return stream.response.Read(value)
}

func (stream *relayTestStream) Write(value []byte) (int, error) {
	return stream.request.Write(value)
}

func (stream *relayTestStream) CloseWrite() error {
	stream.writeClosed = true
	return nil
}

func (stream *relayTestStream) Close() error {
	stream.closed = true
	return nil
}

type relayBootstrapBuffer struct {
	*bytes.Reader
	written bytes.Buffer
}

func (buffer *relayBootstrapBuffer) Write(value []byte) (int, error) {
	return buffer.written.Write(value)
}
