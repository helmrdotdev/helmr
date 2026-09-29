package oci

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"testing"
	"testing/iotest"
)

func TestInspectMetadataOrderAndConcurrentOwnership(t *testing.T) {
	config := []byte(`{"config":{"Env":["A=one","B=two"],"WorkingDir":"/workspace","User":"1000","Entrypoint":["/bin/sh"],"Cmd":["-c","echo ready"]}}`)
	want, err := DecodeConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	image := ociTar(t, []ociTestLayer{
		{mediaType: "application/vnd.oci.image.layer.v1.tar", body: bytes.Repeat([]byte("first"), 1700)},
		{mediaType: "application/vnd.oci.image.layer.v1.tar", body: bytes.Repeat([]byte("second"), 2900)},
	}, config)
	type entry struct {
		name string
		body []byte
	}
	var entries []entry
	reader := tar.NewReader(bytes.NewReader(image))
	for {
		h, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry{h.Name, b})
	}

	for _, failure := range []string{"missing-index", "missing-config", "duplicate-index", "duplicate-config", "unsafe-path"} {
		t.Run(failure, func(t *testing.T) {
			var archive bytes.Buffer
			writer := tar.NewWriter(&archive)
			for _, item := range entries {
				isIndex := item.name == "index.json"
				isConfig := item.name == "blobs/sha256/"+sha256Hex(config)
				if (failure == "missing-index" && isIndex) || (failure == "missing-config" && isConfig) {
					continue
				}
				writeTarFile(t, writer, item.name, item.body)
				if (failure == "duplicate-index" && isIndex) || (failure == "duplicate-config" && isConfig) {
					writeTarFile(t, writer, item.name, item.body)
				}
			}
			if failure == "unsafe-path" {
				writeTarFile(t, writer, "../escape", nil)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := Inspect(bytes.NewReader(archive.Bytes())); err == nil {
				t.Fatal("invalid archive accepted")
			}
		})
	}
	if _, err := Inspect(bytes.NewReader(ociTar(t, nil, []byte("not JSON")))); err == nil {
		t.Fatal("malformed metadata accepted")
	}
	for _, reverse := range []bool{false, true} {
		var archive bytes.Buffer
		writer := tar.NewWriter(&archive)
		for i := range entries {
			j := i
			if reverse {
				j = len(entries) - 1 - i
			}
			writeTarFile(t, writer, entries[j].name, entries[j].body)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		body := archive.Bytes()
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			t.Parallel()
			for range 3 {
				got, err := Inspect(bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got.Config, want) {
					t.Fatalf("config = %+v, want %+v", got.Config, want)
				}
			}
		})
	}
}

func TestInspectBlobRetentionAndAllocationBound(t *testing.T) {
	for _, size := range []int{0, maxJSONBlobBytes, maxJSONBlobBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			body := bytes.Repeat([]byte(" "), size)
			if size > 0 {
				copy(body, []byte("{}"))
			}
			digest := sha256Hex(body)
			var archive bytes.Buffer
			writer := tar.NewWriter(&archive)
			writeTarFile(t, writer, "index.json", []byte(`{}`))
			writeTarFile(t, writer, "blobs/sha256/"+digest, body)
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, blobs, err := inspectBlobs(bytes.NewReader(archive.Bytes()))
			runtime.ReadMemStats(&after)
			if err != nil {
				t.Fatal(err)
			}
			blob := blobs[digest]
			retained := size > 0 && size <= maxJSONBlobBytes
			if retained && !bytes.Equal(blob.body, body) {
				t.Fatal("retained content changed")
			}
			if !retained && blob.body != nil {
				t.Fatal("empty or oversized blob retained a body")
			}
			// Allow allocator/race-instrumentation overhead, but reject geometric
			// growth plus cloning or retention proportional to an oversized layer.
			budget := uint64(2 << 20)
			if retained {
				budget += 2 * uint64(size)
			}
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("allocated_bytes=%d budget_bytes=%d", allocated, budget)
			if allocated > budget {
				t.Fatalf("allocated %d bytes, budget %d", allocated, budget)
			}
			descriptor := Descriptor{Digest: "sha256:" + digest, Size: int64(size), MediaType: "application/vnd.oci.image.layer.v1.tar"}
			if err := validateLayerDescriptor(blobs, descriptor); err != nil {
				t.Fatal(err)
			}
			mismatched := descriptor
			mismatched.Size++
			if _, err := readMetadataBlob(blobs, mismatched); err == nil {
				t.Fatal("metadata size mismatch accepted")
			}
			if err := validateLayerDescriptor(blobs, mismatched); err == nil {
				t.Fatal("layer size mismatch accepted")
			}
			metadata, err := readMetadataBlob(blobs, descriptor)
			if retained {
				if _, err := DecodeConfig(metadata); err != nil {
					t.Fatal(err)
				}
			}
			if (err == nil) != retained {
				t.Fatalf("metadata admission for size %d: %v", size, err)
			}
		})
	}
}

func TestInspectTruncatedBlobAllocationBound(t *testing.T) {
	for _, size := range []int64{maxJSONBlobBytes, maxJSONBlobBytes + 1, 1 << 30} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			var header bytes.Buffer
			writer := tar.NewWriter(&header)
			if err := writer.WriteHeader(&tar.Header{Name: "blobs/sha256/" + sha256Hex(nil), Mode: 0600, Size: size}); err != nil {
				t.Fatal(err)
			}
			// Intentionally omit the declared body and archive terminator.
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, _, err := inspectBlobs(bytes.NewReader(header.Bytes()))
			runtime.ReadMemStats(&after)
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("error = %v", err)
			}
			budget := uint64(2 << 20)
			if size <= maxJSONBlobBytes {
				budget += 2 * uint64(size)
			}
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("allocated_bytes=%d budget_bytes=%d", allocated, budget)
			if allocated > budget {
				t.Fatalf("allocated %d bytes for truncated body, budget %d", allocated, budget)
			}
			injected := errors.New("source read failed")
			_, _, err = inspectBlobs(io.MultiReader(bytes.NewReader(header.Bytes()), iotest.ErrReader(injected)))
			if !errors.Is(err, injected) {
				t.Fatalf("read error = %v", err)
			}
		})
	}
}
