package blockproof

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func createPersistent(t *testing.T, dir string) *Persistent {
	t.Helper()
	p, e := CreatePersistent(dir, 4*BlockSize, 8)
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func assertPersistent(t *testing.T, p *Persistent, want byte) {
	t.Helper()
	b := make([]byte, 4*BlockSize)
	if _, e := p.ReadAt(b, 0); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(b, bytes.Repeat([]byte{want}, len(b))) {
		t.Fatal("reopened bytes differ")
	}
}
func fill(t *testing.T, p *Persistent, b byte) {
	t.Helper()
	if _, e := p.WriteAt(bytes.Repeat([]byte{b}, 4*BlockSize), 0); e != nil {
		t.Fatal(e)
	}
}
func TestPersistentFlushFailureRetryAndOwnership(t *testing.T) {
	dir := t.TempDir()
	p := createPersistent(t, dir)
	if _, e := OpenPersistent(dir, 8); e == nil {
		t.Fatal("second owner accepted")
	}
	fill(t, p, 3)
	if e := p.Flush(); e != nil {
		t.Fatal(e)
	}
	fill(t, p, 5)
	p.phase = func(s string) error {
		if s == "root-synced" {
			return errors.New("injected pre-rename failure")
		}
		return nil
	}
	if e := p.Flush(); e == nil {
		t.Fatal("flush should fail")
	}
	p.phase = nil
	if e := p.Flush(); e != nil {
		t.Fatal(e)
	} // No dirty blocks remain, but base must still be committed.
	if e := p.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e := p.WriteAt([]byte{8}, 0); !errors.Is(e, os.ErrClosed) {
		t.Fatal(e)
	}
	if e := p.Flush(); !errors.Is(e, os.ErrClosed) {
		t.Fatal(e)
	}
	p, e := OpenPersistent(dir, 8)
	if e != nil {
		t.Fatal(e)
	}
	assertPersistent(t, p, 5)
	p.Close()
}
func TestConcurrentFlushCannotRegressRoot(t *testing.T) {
	dir := t.TempDir()
	p := createPersistent(t, dir)
	defer p.Close()
	fill(t, p, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	once := sync.Once{}
	p.phase = func(s string) error {
		if s == "objects-staged" {
			once.Do(func() { close(entered); <-release })
		}
		return nil
	}
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { first <- p.Flush() }()
	<-entered
	fill(t, p, 2)
	go func() { second <- p.Flush() }()
	close(release)
	if e := <-first; e != nil {
		t.Fatal(e)
	}
	if e := <-second; e != nil {
		t.Fatal(e)
	}
	p.Close()
	r, e := OpenPersistent(dir, 8)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	assertPersistent(t, r, 2)
}
func TestPersistentFailsClosedOnMissingOrCorruptDependencies(t *testing.T) {
	for _, kind := range []string{"missing-root", "corrupt-root", "missing-segment", "corrupt-segment"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			p := createPersistent(t, dir)
			fill(t, p, 1)
			if e := p.Flush(); e != nil {
				t.Fatal(e)
			}
			root, e := os.ReadFile(filepath.Join(dir, "root"))
			if e != nil {
				t.Fatal(e)
			}
			m, e := p.disk.store.load(string(root))
			if e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(dir, "root")
			if kind == "missing-segment" || kind == "corrupt-segment" {
				path = filepath.Join(dir, "objects", m.Blocks[0].Segment)
			}
			if kind == "missing-root" || kind == "missing-segment" {
				e = os.Remove(path)
			} else {
				e = os.WriteFile(path, []byte("corruption"), 0600)
			}
			if e != nil {
				t.Fatal(e)
			}
			if kind == "missing-segment" || kind == "corrupt-segment" {
				if e = p.Flush(); e == nil {
					t.Fatal("flush certified missing/corrupt inherited block")
				}
			}
			p.Close()
			if r, e := OpenPersistent(dir, 8); e == nil {
				r.Close()
				t.Fatal("bad root reopened")
			}
		})
	}
}

// Child prints a synchronization token only at the selected boundary; parent
// kills it there. No timing sleep substitutes for a reached persistence phase.
func TestCrashChild(t *testing.T) {
	if os.Getenv("BLOCK_PROOF_CHILD") != "1" {
		return
	}
	p, e := OpenPersistent(os.Getenv("BLOCK_PROOF_DIR"), 8)
	if e != nil {
		t.Fatal(e)
	}
	target := os.Getenv("BLOCK_PROOF_PHASE")
	fill(t, p, 9)
	wait := func() {
		fmt.Println("boundary-reached")
		for {
			time.Sleep(time.Hour)
		}
	}
	p.phase = func(phase string) error {
		if phase == target {
			wait()
		}
		return nil
	}
	if target == "segment-staged" {
		calls := 0
		p.disk.store.BeforePut = func() error {
			calls++
			if calls == 2 {
				wait()
			}
			return nil
		}
	}
	if e = p.Flush(); e != nil {
		t.Fatal(e)
	}
	if target == "acknowledged" {
		wait()
	}
	t.Fatal("missing crash boundary")
}
func TestProcessCrashReopen(t *testing.T) {
	for _, phase := range []string{"segment-staged", "objects-staged", "root-synced", "root-renamed", "root-committed", "acknowledged"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			p := createPersistent(t, dir)
			fill(t, p, 4)
			if e := p.Flush(); e != nil {
				t.Fatal(e)
			}
			p.Close()
			cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$")
			cmd.Env = append(os.Environ(), "BLOCK_PROOF_CHILD=1", "BLOCK_PROOF_DIR="+dir, "BLOCK_PROOF_PHASE="+phase)
			out, e := cmd.StdoutPipe()
			if e != nil {
				t.Fatal(e)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if e = cmd.Start(); e != nil {
				t.Fatal(e)
			}
			defer func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
			}()
			reached := make(chan bool, 1)
			go func() {
				scanner := bufio.NewScanner(out)
				for scanner.Scan() {
					if scanner.Text() == "boundary-reached" {
						reached <- true
						return
					}
				}
				reached <- false
			}()
			select {
			case ok := <-reached:
				if !ok {
					cmd.Wait()
					t.Fatalf("child failed: %s", stderr.String())
				}
			case <-time.After(15 * time.Second):
				t.Fatal("child boundary timeout")
			}
			if e = cmd.Process.Kill(); e != nil {
				t.Fatal(e)
			}
			if e = cmd.Wait(); e == nil {
				t.Fatal("child unexpectedly exited successfully")
			}
			r, e := OpenPersistent(dir, 8)
			if e != nil {
				t.Fatal(e)
			}
			defer r.Close()
			want := byte(4)
			if phase == "root-renamed" || phase == "root-committed" || phase == "acknowledged" {
				want = 9
			}
			assertPersistent(t, r, want)
		})
	}
}
