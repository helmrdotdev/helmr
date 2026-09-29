package oci

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"os"
	"reflect"
	"testing"
)

func TestReadVerifiedConfigVerifiesFullImageAndPreservesSettings(t *testing.T) {
	body := append(verifiedConfigImage(t), []byte("trailing envelope")...)
	path := t.TempDir() + "/image.tar"
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	artifact := struct {
		Digest    string
		SizeBytes int64
	}{Digest: sha256sum.DigestBytes(body), SizeBytes: int64(len(body))}
	got, err := ReadVerifiedConfig(context.Background(), path, artifact.Digest, artifact.SizeBytes)
	if err != nil {
		t.Fatal(err)
	}
	want := RuntimeConfig{Env: []string{"A=one", "A=two", "EMPTY="}, WorkingDir: "/workspace", User: "1000:1000", Entrypoint: []string{"/bin/sh", "-c"}, Cmd: []string{"echo hello"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("config = %v", got)
	}
	parsed, err := ReadConfig(bytes.NewReader(body))
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
			value, err := ReadVerifiedConfig(ctx, file, a.Digest, a.SizeBytes)
			if err == nil || !reflect.DeepEqual(value, RuntimeConfig{}) {
				t.Fatalf("config=%v err=%v", value, err)
			}
			if change == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

func verifiedConfigImage(t *testing.T) []byte {
	t.Helper()
	config := []byte(`{"config":{"Env":["A=one","A=two","EMPTY="],"WorkingDir":"/workspace","User":"1000:1000","Entrypoint":["/bin/sh","-c"],"Cmd":["echo hello"]}}`)
	marshal := func(v any) []byte {
		b, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	descriptor := func(b []byte, media string) Descriptor {
		return Descriptor{Digest: sha256sum.DigestBytes(b), Size: int64(len(b)), MediaType: media}
	}
	manifest := marshal(Manifest{Config: descriptor(config, "application/vnd.image.config.v1+json")})
	index := marshal(Index{Manifests: []Descriptor{descriptor(manifest, "application/vnd.image.manifest.v1+json")}})
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
