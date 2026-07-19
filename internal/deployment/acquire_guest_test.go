package deployment

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
)

func TestGuestManagerNormalizerUsesClosedGuestProfile(t *testing.T) {
	archiveBytes := managerTestTGZ(t, []managerTestTarEntry{
		{name: "package/package.json", mode: 0644, content: []byte(`{"name":"npm"}`)},
		{name: "package/bin/npm-cli.js", mode: 0755, content: []byte("npm")},
	})
	archive, source := managerGuestSource(t, archiveBytes)
	provisional := managerDownloadFile(t)
	connector := &managerAcquireTestConnector{scratch: t.TempDir()}
	selector := NewManagerSelector(
		PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
		ArchitectureX8664,
	)

	terminal, err := (GuestManagerNormalizer{Connector: connector}).Normalize(
		context.Background(),
		selector,
		source,
		archive,
		provisional,
	)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != ManagerAcquireStatusOK {
		t.Fatalf("status = %q", terminal.Status)
	}
	if connector.request.OwnerKind != vm.OwnerBuild ||
		connector.request.Resources != compute.ManagerAcquireResources() ||
		connector.request.PIDsMax != managerAcquirePIDsMax ||
		!connector.request.Networkless ||
		connector.request.Network.Internet ||
		len(connector.request.Network.Allow) != 0 ||
		len(connector.request.Network.Deny) != 0 {
		t.Fatalf("connect request = %+v", connector.request)
	}

	reader := tar.NewReader(provisional)
	var names []string
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
	if len(names) != 5 || names[4] != "lib/npm/package.json" {
		t.Fatalf("normalized entries = %#v", names)
	}
}

func TestGuestManagerNormalizerReturnsUnsupportedStatus(t *testing.T) {
	archiveBytes := managerTestTGZ(t, []managerTestTarEntry{
		{name: "package/package.json", mode: 0644, content: []byte(`{}`)},
	})
	archive, source := managerGuestSource(t, archiveBytes)
	provisional := managerDownloadFile(t)
	connector := &managerAcquireTestConnector{scratch: t.TempDir()}

	terminal, err := (GuestManagerNormalizer{Connector: connector}).Normalize(
		context.Background(),
		NewManagerSelector(
			PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
			ArchitectureX8664,
		),
		source,
		archive,
		provisional,
	)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != ManagerAcquireStatusUnsupportedLayout {
		t.Fatalf("status = %q", terminal.Status)
	}
	info, err := provisional.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("failed provisional size = %d", info.Size())
	}
}

func managerGuestSource(t *testing.T, content []byte) (*os.File, ManagerSource) {
	t.Helper()
	archive := managerDownloadFile(t)
	if _, err := archive.Write(content); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	return archive, ManagerSource{
		Digest:    "sha256:" + hex.EncodeToString(sum[:]),
		Origin:    managerNPMReleaseOriginRoot + "npm-11.4.2.tgz",
		SizeBytes: int64(len(content)),
	}
}

type managerAcquireTestConnector struct {
	scratch string
	request vm.ConnectRequest
}

func (connector *managerAcquireTestConnector) Connect(
	_ context.Context,
	request vm.ConnectRequest,
) (vm.Session, error) {
	if connector == nil {
		return nil, errors.New("connector is nil")
	}
	connector.request = request
	host, guest := net.Pipe()
	session := &managerAcquireTestSession{
		host: host,
		done: make(chan error, 1),
	}
	go func() {
		defer guest.Close()
		header, bodyLen, err := wire.ReadStreamFrameHeader(guest)
		if err != nil {
			session.done <- err
			return
		}
		if header.Type != wire.StreamTypeManagerAcquire {
			session.done <- errors.New("unexpected manager acquisition stream")
			return
		}
		archive, err := os.CreateTemp(connector.scratch, "manager-source-*")
		if err != nil {
			session.done <- err
			return
		}
		defer archive.Close()
		input := &io.LimitedReader{R: guest, N: int64(bodyLen)}
		acquire, err := ReadManagerAcquireRequest(input, archive)
		if err == nil && input.N != 0 {
			err = errors.New("manager acquisition request has trailing input")
		}
		if err == nil {
			err = NormalizeManagerArchive(guest, acquire, archive, connector.scratch)
		}
		session.done <- err
	}()
	return session, nil
}

type managerAcquireTestSession struct {
	host      net.Conn
	done      chan error
	closeOnce sync.Once
	closeErr  error
}

func (session *managerAcquireTestSession) Stream() vm.Stream {
	return managerAcquireTestStream{Conn: session.host}
}

func (session *managerAcquireTestSession) OpenStream(context.Context) (vm.Stream, error) {
	return nil, errors.New("open stream is unsupported")
}

func (session *managerAcquireTestSession) Wait(context.Context) error {
	return nil
}

func (session *managerAcquireTestSession) Close(context.Context) error {
	session.closeOnce.Do(func() {
		session.closeErr = errors.Join(session.host.Close(), <-session.done)
	})
	return session.closeErr
}

type managerAcquireTestStream struct {
	net.Conn
}

func (managerAcquireTestStream) CloseWrite() error {
	return nil
}
