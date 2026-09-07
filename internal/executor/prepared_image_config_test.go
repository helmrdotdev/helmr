package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/oci"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

func preparedConfigImage(t *testing.T) []byte {
	t.Helper()
	config := []byte(`{"config":{"Env":["A=one","A=two","EMPTY="],"WorkingDir":"/workspace","User":"1000:1000","Entrypoint":["/bin/sh","-c"],"Cmd":["echo hello"]}}`)
	marshal := func(v any) []byte {
		b, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	descriptor := func(b []byte, media string) oci.Descriptor {
		return oci.Descriptor{Digest: sha256sum.DigestBytes(b), Size: int64(len(b)), MediaType: media}
	}
	manifest := marshal(oci.Manifest{Config: descriptor(config, "application/vnd.oci.image.config.v1+json")})
	index := marshal(oci.Index{Manifests: []oci.Descriptor{descriptor(manifest, "application/vnd.oci.image.manifest.v1+json")}})
	var result bytes.Buffer
	tw := tar.NewWriter(&result)
	for _, entry := range []struct {
		name string
		body []byte
	}{
		{"oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)}, {"index.json", index},
		{"blobs/sha256/" + sha256sum.HexBytes(config), config}, {"blobs/sha256/" + sha256sum.HexBytes(manifest), manifest},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Mode: 0600, Size: int64(len(entry.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

func TestPreparedImageConfigVerifiesFullImageAndPreservesSettings(t *testing.T) {
	body := append(preparedConfigImage(t), []byte("trailing envelope")...)
	path := t.TempDir() + "/image.tar"
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	artifact := workerapi.CASObject{Digest: sha256sum.DigestBytes(body), SizeBytes: int64(len(body))}
	got, err := readPreparedImageConfig(context.Background(), path, artifact)
	if err != nil {
		t.Fatal(err)
	}
	want := &workspacev0.RuntimeImageConfig{Env: []string{"A=one", "A=two", "EMPTY="}, WorkingDir: "/workspace", User: "1000:1000", Entrypoint: []string{"/bin/sh", "-c"}, Cmd: []string{"echo hello"}}
	if !proto.Equal(got, want) {
		t.Fatalf("config = %v", got)
	}
	parsed, err := oci.ReadConfig(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.Env, got.Env) || parsed.User != got.User {
		t.Fatal("OCI config parity failed")
	}
	for _, change := range []string{"digest", "size-small", "size-large", "trailer", "truncated", "cancelled"} {
		t.Run(change, func(t *testing.T) {
			b := append([]byte(nil), body...)
			a := artifact
			ctx := context.Background()
			switch change {
			case "digest":
				a.Digest = sha256sum.DigestBytes([]byte("other"))
			case "size-small":
				a.SizeBytes--
			case "size-large":
				a.SizeBytes++
			case "trailer":
				b[len(b)-1] ^= 1
			case "truncated":
				b = b[:len(b)-1]
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			file := t.TempDir() + "/image.tar"
			if err := os.WriteFile(file, b, 0600); err != nil {
				t.Fatal(err)
			}
			value, err := readPreparedImageConfig(ctx, file, a)
			if err == nil || value != nil {
				t.Fatalf("config=%v err=%v", value, err)
			}
			if change == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

func TestPrepareGuestRuntimeTransfersConfigOrImage(t *testing.T) {
	for _, mounted := range []bool{false, true} {
		t.Run(map[bool]string{false: "image", true: "mounted"}[mounted], func(t *testing.T) {
			body := preparedConfigImage(t)
			path := t.TempDir() + "/image.tar"
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			mount := workerapi.WorkspaceMount{WorkspaceID: "workspace", WorkspaceMountPath: "/workspace", WorkspaceImage: workerapi.CASObject{Digest: sha256sum.DigestBytes(body), SizeBytes: int64(len(body))}}
			var config *workspacev0.RuntimeImageConfig
			if mounted {
				var err error
				config, err = readPreparedImageConfig(context.Background(), path, mount.WorkspaceImage)
				if err != nil {
					t.Fatal(err)
				}
				path = "/does-not-exist"
			}
			var reply bytes.Buffer
			if err := frameio.WriteProtoFrame(&reply, &workspacev0.PrepareWorkspaceRuntimeResponse{State: "prepared", RuntimeInstanceId: "runtime"}); err != nil {
				t.Fatal(err)
			}
			stream := &scriptedGuestStream{read: bytes.NewReader(reply.Bytes())}
			pool := &PreparedRuntimePool{}
			if err := pool.prepareGuestRuntime(context.Background(), fakeGuestSession{stream: stream}, "runtime", mount, path, config); err != nil {
				t.Fatal(err)
			}
			input := bytes.NewReader(stream.written.Bytes())
			if _, _, err := wire.ReadStreamFrameHeader(input); err != nil {
				t.Fatal(err)
			}
			var request workspacev0.PrepareWorkspaceRuntimeRequest
			if err := frameio.ReadProtoFrame(input, &request); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(request.GetMountedImageConfig(), config) {
				t.Fatalf("config = %v", request.GetMountedImageConfig())
			}
			if mounted {
				if input.Len() != 0 {
					t.Fatalf("unexpected image transfer: %d bytes", input.Len())
				}
				return
			}
			header, size, err := wire.ReadStreamFrameHeader(input)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(input)
			if err != nil {
				t.Fatal(err)
			}
			if header.Type != wire.StreamTypeRunImage || size != uint64(len(body)) || !bytes.Equal(got, body) {
				t.Fatal("image frame changed")
			}
		})
	}
}
