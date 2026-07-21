//go:build linux

package guestd

import (
	"bufio"
	"bytes"
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
