//go:build linux

package nbd

import (
	"bufio"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestQuarantineSurvivesStoppedDriver(t *testing.T) {
	if os.Getenv("HELMR_NBD_QUARANTINE_CHILD") == "1" {
		os.Stdout.WriteString("parked\n")
		quarantine()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestQuarantineSurvivesStoppedDriver$")
	cmd.Env = append(os.Environ(), "HELMR_NBD_QUARANTINE_CHILD=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill(); <-done }()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "parked\n" {
		t.Fatalf("park readiness %q %v", line, err)
	}
	select {
	case err := <-done:
		done <- err
		t.Fatalf("quarantine exited without release: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}
