package computerhost

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	cass3 "github.com/helmrdotdev/helmr/internal/cas/s3"
	"github.com/helmrdotdev/helmr/internal/vm"
)

// This opt-in diagnostic reports a measured envelope, not a hard RSS guarantee.
// The HTTP server and encrypted fixture live outside the measured subprocess.
func TestCheckpointMemoryTransportEnvelope(t *testing.T) {
	if os.Getenv("HELMR_MEMORY_TRANSPORT_CHILD") != "" {
		memoryTransportChild(t)
		return
	}
	if os.Getenv("HELMR_MEMORY_TRANSPORT_PROOF") != "1" {
		t.Skip("set HELMR_MEMORY_TRANSPORT_PROOF=1 for isolated transport allocation evidence")
	}
	for _, kind := range []string{"raw", "compressed", "mixed", "zero"} {
		t.Run(kind, func(t *testing.T) {
			raw := make([]byte, 32*vm.MemoryReadBytes)
			switch kind {
			case "raw", "mixed":
				if _, err := rand.Read(raw); err != nil {
					t.Fatal(err)
				}
				if kind == "mixed" {
					for at := 0; at < len(raw); at += vm.MemoryReadBytes {
						clear(raw[at : at+64*1024])
					}
				}
			case "compressed":
				for i := range raw {
					raw[i] = byte(i % 31)
				}
			}
			_, ranges, descriptor, header := memoryFixture(t, raw)
			hashes := make([]string, len(raw)/vm.MemoryReadBytes)
			for i := range hashes {
				h := sha256.Sum256(raw[i*vm.MemoryReadBytes : (i+1)*vm.MemoryReadBytes])
				hashes[i] = hex.EncodeToString(h[:])
			}
			directory := t.TempDir()
			object := filepath.Join(directory, "object")
			if err := os.WriteFile(object, ranges.data, 0600); err != nil {
				t.Fatal(err)
			}
			// File-backed serving keeps the encrypted object out of the measured process.
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				f, err := os.Open(object)
				if err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
				defer f.Close()
				http.ServeContent(w, r, "object", time.Unix(0, 0), f)
			}))
			defer server.Close()
			cert := filepath.Join(directory, "ca.pem")
			if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
				t.Fatal(err)
			}
			config := memoryTransportFixture{URI: "s3://fixture/cas?endpoint=" + url.QueryEscape(server.URL), Descriptor: descriptor, Header: header, Size: int64(len(raw)), Hashes: hashes}
			data, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(directory, "fixture.json")
			if err := os.WriteFile(file, data, 0600); err != nil {
				t.Fatal(err)
			}
			empty := filepath.Join(directory, "empty")
			if err := os.WriteFile(empty, nil, 0600); err != nil {
				t.Fatal(err)
			}
			for _, gc := range []string{"100", "20", "off"} {
				cmd := exec.Command(os.Args[0], "-test.run=^TestCheckpointMemoryTransportEnvelope$", "-test.v", "-test.timeout=2m")
				cmd.Env = append(os.Environ(), "HELMR_MEMORY_TRANSPORT_CHILD="+file, "GOGC="+gc, "GOMEMLIMIT=off", "AWS_ACCESS_KEY_ID=fixture", "AWS_SECRET_ACCESS_KEY=fixture", "AWS_SESSION_TOKEN=", "AWS_PROFILE=", "AWS_REGION=us-east-1", "AWS_DEFAULT_REGION=us-east-1", "AWS_EC2_METADATA_DISABLED=true", "AWS_CONFIG_FILE="+empty, "AWS_SHARED_CREDENTIALS_FILE="+empty, "AWS_CA_BUNDLE="+cert)
				output, err := cmd.CombinedOutput()
				t.Logf("kind=%s gc=%s\n%s", kind, gc, output)
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

type memoryTransportFixture struct {
	URI        string
	Descriptor cas.Descriptor
	Header     string
	Size       int64
	Hashes     []string
}
type memoryEnvelope struct {
	HeapAlloc, HeapInuse, RuntimeResident, TotalAlloc uint64
	NumGC                                             uint32
	RSS                                               int64
}

func readMemoryEnvelope() memoryEnvelope {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	r := memoryEnvelope{HeapAlloc: m.HeapAlloc, HeapInuse: m.HeapInuse, RuntimeResident: m.Sys - m.HeapReleased, TotalAlloc: m.TotalAlloc, NumGC: m.NumGC, RSS: -1}
	if data, err := os.ReadFile("/proc/self/statm"); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) > 1 {
			pages, e := strconv.ParseInt(fields[1], 10, 64)
			if e == nil {
				r.RSS = pages * int64(os.Getpagesize())
			}
		}
	}
	return r
}
func memoryTransportChild(t *testing.T) {
	data, err := os.ReadFile(os.Getenv("HELMR_MEMORY_TRANSPORT_CHILD"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture memoryTransportFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	// Worker-wide client/cipher construction is outside per-restore accounting.
	store, err := cass3.New(t.Context(), fixture.URI)
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := NewCheckpointEncryptor(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	baseline := readMemoryEnvelope()
	peak := baseline
	var mu sync.Mutex
	sample := func() {
		m := readMemoryEnvelope()
		mu.Lock()
		defer mu.Unlock()
		peak.HeapAlloc = max(peak.HeapAlloc, m.HeapAlloc)
		peak.HeapInuse = max(peak.HeapInuse, m.HeapInuse)
		peak.RuntimeResident = max(peak.RuntimeResident, m.RuntimeResident)
		peak.RSS = max(peak.RSS, m.RSS)
		peak.TotalAlloc = max(peak.TotalAlloc, m.TotalAlloc)
		peak.NumGC = max(peak.NumGC, m.NumGC)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	var stopOnce sync.Once
	stopSampling := func() { stopOnce.Do(func() { close(stop); <-done }) }
	defer stopSampling()
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sample()
			case <-stop:
				return
			}
		}
	}()
	source, err := openCheckpointMemory(t.Context(), store, cipher, fixture.Descriptor, fixture.Header, "environment/computer/checkpoint/memory", fixture.Size)
	if err != nil {
		t.Fatal(err)
	}
	validate := func(at int64, b []byte) error {
		h := sha256.Sum256(b)
		if hex.EncodeToString(h[:]) != fixture.Hashes[at/vm.MemoryReadBytes] {
			return fmt.Errorf("content mismatch at %d", at)
		}
		return nil
	}
	start := make(chan struct{})
	results := make(chan error, 4)
	var active, peakJobs atomic.Int64
	job := func(work func() error) {
		go func() {
			<-start
			n := active.Add(1)
			for p := peakJobs.Load(); n > p; p = peakJobs.Load() {
				if peakJobs.CompareAndSwap(p, n) {
					break
				}
			}
			defer active.Add(-1)
			results <- work()
		}()
	}
	for i := 0; i < 3; i++ {
		job(func() error {
			buffer := make([]byte, vm.MemoryReadBytes)
			defer clear(buffer)
			for at := int64(0); at < fixture.Size; at += vm.MemoryReadBytes {
				if _, err := source.ReadAt(t.Context(), buffer, at); err != nil {
					return err
				}
				if err := validate(at, buffer); err != nil {
					return err
				}
				sample()
			}
			return nil
		})
	}
	job(func() error {
		for at := int64(0); at < fixture.Size; {
			next, err := source.ReadWindows(t.Context(), at, func(at int64, b []byte) error { sample(); return validate(at, b) })
			if err != nil {
				return err
			}
			if next <= at {
				return fmt.Errorf("stream made no progress")
			}
			at = next
		}
		return nil
	})
	close(start)
	for i := 0; i < 4; i++ {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	sample()
	stopSampling()
	beforeGC := readMemoryEnvelope()
	runtime.GC()
	afterGC := readMemoryEnvelope()
	runtime.KeepAlive(source)
	allowance, err := vm.MemoryRestoreOverhead(fixture.Size)
	if err != nil {
		t.Fatal(err)
	}
	report := struct {
		Baseline, Peak, BeforeForcedGC, AfterForcedGC memoryEnvelope
		Reservation                                   int64
		PeakJobs                                      int64
		Stats                                         vm.MemoryReadStats
	}{baseline, peak, beforeGC, afterGC, allowance, peakJobs.Load(), source.MemoryReadStats()}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("memory_transport_envelope=%s", encoded)
	if peakJobs.Load() != 4 {
		t.Fatal("four caller jobs did not overlap")
	}
}
