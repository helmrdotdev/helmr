//go:build linux

package computer

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"testing"
)

func seedConfigImage(t *testing.T) []byte {
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
