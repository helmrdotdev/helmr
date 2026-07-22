//go:build linux

package guestd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProgramCgroupContainsAndKillsCompleteTree(t *testing.T) {
	if os.Getenv("HELMR_PRIVILEGED_PROGRAM_TEST") != "1" {
		t.Skip("set HELMR_PRIVILEGED_PROGRAM_TEST=1 in a disposable privileged Linux guest")
	}
	if os.Geteuid() != 0 {
		t.Fatal("privileged Program cgroup test requires root")
	}
	if _, err := os.Stat(dependencyCgroupRoot); errors.Is(err, os.ErrNotExist) {
		prepareDependencyTestCgroup(t)
	} else if err != nil {
		t.Fatal(err)
	}

	cgroup, err := createProgramCgroup()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cgroup.kill()
		_ = cgroup.waitEmpty()
		_ = cgroup.close()
	}()

	cmd := exec.Command(os.Args[0], "-test.run=TestProgramCgroupNamespaceHelper")
	cmd.Env = append(os.Environ(), "HELMR_PROGRAM_CGROUP_HELPER=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	if err := cgroup.attach(cmd); err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	line := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			line <- scanner.Text()
			return
		}
		line <- ""
	}()
	select {
	case value := <-line:
		if value != "cgroup=0::/" {
			t.Fatalf("Program cgroup namespace identity = %q", value)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Program did not enter its cgroup namespace")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(filepath.Join(dependencyCgroupRoot, programCgroupLeaf, "cgroup.procs"))
		if err != nil {
			t.Fatal(err)
		}
		if len(bytes.Fields(raw)) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Program descendants were not contained: %q", strings.TrimSpace(string(raw)))
		}
		time.Sleep(10 * time.Millisecond)
	}
	transitionCtx, cancelTransition := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelTransition()
	if err := cgroup.freeze(transitionCtx); err != nil {
		t.Fatal(err)
	}
	if frozen, populated, err := cgroup.(*linuxProgramCgroup).state(); err != nil || !frozen || !populated {
		t.Fatalf("Program cgroup frozen=%v populated=%v error=%v", frozen, populated, err)
	}
	if err := cgroup.thaw(transitionCtx); err != nil {
		t.Fatal(err)
	}
	if frozen, populated, err := cgroup.(*linuxProgramCgroup).state(); err != nil || frozen || !populated {
		t.Fatalf("Program cgroup frozen=%v populated=%v after thaw error=%v", frozen, populated, err)
	}

	if err := cgroup.kill(); err != nil {
		t.Fatal(err)
	}
	if err := cgroup.waitEmpty(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("Program tree exited without cgroup termination")
	}
}

func TestParseProgramCgroupState(t *testing.T) {
	for _, test := range []struct {
		name      string
		body      string
		frozen    bool
		populated bool
		ok        bool
	}{
		{name: "frozen", body: "populated 1\nfrozen 1\n", frozen: true, populated: true, ok: true},
		{name: "thawed", body: "frozen 0\npopulated 1\n", populated: true, ok: true},
		{name: "empty", body: "populated 0\nfrozen 1\n", frozen: true, ok: true},
		{name: "missing frozen", body: "populated 1\n"},
		{name: "missing populated", body: "frozen 1\n"},
		{name: "invalid", body: "frozen yes\n"},
		{name: "duplicate frozen", body: "populated 1\nfrozen 0\nfrozen 1\n"},
		{name: "duplicate populated", body: "populated 0\npopulated 1\nfrozen 1\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			frozen, populated, err := parseProgramCgroupState([]byte(test.body))
			if (err == nil) != test.ok || (err == nil && (frozen != test.frozen || populated != test.populated)) {
				t.Fatalf("parseProgramCgroupState() = %v, %v, %v", frozen, populated, err)
			}
		})
	}
}

func TestProgramCgroupTransitionRejectsEmptyFrozenCgroup(t *testing.T) {
	complete, err := programCgroupTransitionComplete(true, false, true)
	if err == nil || complete {
		t.Fatalf("programCgroupTransitionComplete() = %v, %v", complete, err)
	}
}

func TestProgramCgroupNamespaceHelper(t *testing.T) {
	if os.Getenv("HELMR_PROGRAM_CGROUP_HELPER") != "1" {
		return
	}
	if err := enterProgramCgroupNamespace(); err != nil {
		t.Fatal(err)
	}
	child := exec.Command("/bin/sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stdout.WriteString("cgroup=" + strings.TrimSpace(string(raw)) + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
}
