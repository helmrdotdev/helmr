//go:build linux

package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/workerapi"
	"golang.org/x/sys/unix"
)

func TestValidatePlatformAcquisitionProcess(t *testing.T) {
	process := PlatformAcquisitionProcess{
		BuildPolicyPath:  "/nix/store/policy",
		Encoder:          "/nix/store/encoder",
		Executable:       "/nix/store/helmr-worker",
		GPGV:             "/nix/store/gpgv",
		Patchelf:         "/nix/store/patchelf",
		PlatformStoreURI: "s3://platform",
		UnitCgroupRoot:   "/sys/fs/cgroup/system.slice/helmr-worker.service",
		WorkDir:          "/var/lib/helmr/acquisition",
		XZ:               "/nix/store/xz",
	}
	request := workerapi.PlatformAcquisition{
		DeploymentID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
	}
	if err := validatePlatformAcquisitionProcess(process, request); err != nil {
		t.Fatal(err)
	}
	request.DeploymentID = "60af6067-a253-47b5-915c-2b889fb132c7"
	if err := validatePlatformAcquisitionProcess(process, request); err == nil {
		t.Fatal("UUIDv4 Deployment ID was accepted")
	}
	request.DeploymentID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35"
	process.WorkDir = "relative"
	if err := validatePlatformAcquisitionProcess(process, request); err == nil {
		t.Fatal("relative acquisition work directory was accepted")
	}
}

func TestPlatformAcquisitionEnvironmentExcludesWorkerSecrets(t *testing.T) {
	t.Setenv("AWS_REGION", "us-west-2")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("HELMR_CHECKPOINT_KEY", "secret")
	process := PlatformAcquisitionProcess{
		BuildPolicyPath:  "/policy",
		Encoder:          "/encoder",
		GPGV:             "/gpgv",
		Patchelf:         "/patchelf",
		PlatformStoreURI: "s3://platform",
		WorkDir:          "/work",
		XZ:               "/xz",
	}
	environment := platformAcquisitionEnvironment(process, "/cgroup")
	if !slices.Contains(environment, "AWS_REGION=us-west-2") {
		t.Fatal("AWS region was not forwarded")
	}
	for _, variable := range environment {
		if strings.HasPrefix(variable, "AWS_SECRET_ACCESS_KEY=") ||
			strings.HasPrefix(variable, "HELMR_CHECKPOINT_KEY=") {
			t.Fatalf("worker secret was forwarded: %s", variable)
		}
	}
}

func TestCreatePlatformAcquisitionCgroupRejectsSymlinkRoot(t *testing.T) {
	realRoot := t.TempDir()
	linkRoot := filepath.Join(t.TempDir(), "cgroup")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	if _, _, err := createPlatformAcquisitionCgroup(
		linkRoot,
		"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
	); err == nil {
		t.Fatal("symlinked acquisition cgroup root was accepted")
	}
}

func TestPlatformAcquisitionCgroupIntegration(t *testing.T) {
	if os.Getenv("HELMR_PLATFORM_ACQUISITION_CGROUP_INTEGRATION") != "1" {
		t.Skip("requires a delegated cgroup-v2 systemd service")
	}
	root, err := PrepareVerifierHost()
	if err != nil {
		t.Fatal(err)
	}
	deploymentID := "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35"
	path, processCgroup, err := createPlatformAcquisitionCgroup(root, deploymentID)
	if err != nil {
		t.Fatal(err)
	}
	aggregatePath := filepath.Dir(path)
	for name, want := range platformAcquisitionCgroupLimits {
		raw, err := os.ReadFile(filepath.Join(aggregatePath, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(raw)) != want {
			t.Fatalf("%s = %q, want %q", name, raw, want)
		}
	}
	parent := platformAcquisitionSleeper(t, int(processCgroup.Fd()))
	waitForPlatformAcquisitionCgroupPID(
		t,
		filepath.Join(path, platformAcquisitionProcessLeaf, "cgroup.procs"),
		parent.Process.Pid,
	)

	descendantPath := filepath.Join(path, "conformance")
	if err := os.Mkdir(descendantPath, 0755); err != nil {
		t.Fatal(err)
	}
	descendantCgroup, err := os.Open(descendantPath)
	if err != nil {
		t.Fatal(err)
	}
	descendant := platformAcquisitionSleeper(t, int(descendantCgroup.Fd()))
	waitForPlatformAcquisitionCgroupPID(
		t,
		filepath.Join(descendantPath, "cgroup.procs"),
		descendant.Process.Pid,
	)

	concurrentPath, concurrentCgroup, err := createPlatformAcquisitionCgroup(root, deploymentID)
	if err != nil {
		t.Fatalf("concurrent same-Deployment acquisition failed: %v", err)
	}
	if filepath.Dir(concurrentPath) != aggregatePath {
		t.Fatal("concurrent acquisition escaped the aggregate cgroup")
	}
	if err := concurrentCgroup.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cleanupPlatformAcquisitionCgroup(concurrentPath); err != nil {
		t.Fatal(err)
	}

	if err := killPlatformAcquisitionCgroup(path); err != nil {
		t.Fatal(err)
	}
	waitForPlatformAcquisitionSleeper(t, parent)
	waitForPlatformAcquisitionSleeper(t, descendant)
	if err := processCgroup.Close(); err != nil {
		t.Fatal(err)
	}
	if err := descendantCgroup.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cleanupPlatformAcquisitionCgroup(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("acquisition cgroup remains after cleanup: %v", err)
	}

	retryPath, retryCgroup, err := createPlatformAcquisitionCgroup(root, deploymentID)
	if err != nil {
		t.Fatalf("same-Deployment retry failed: %v", err)
	}
	if retryPath == path {
		t.Fatal("same-Deployment retry reused the prior cgroup identity")
	}
	if filepath.Dir(retryPath) != aggregatePath {
		t.Fatal("same-Deployment retry escaped the aggregate cgroup")
	}
	if err := retryCgroup.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cleanupPlatformAcquisitionCgroup(retryPath); err != nil {
		t.Fatal(err)
	}
}

func TestPlatformAcquisitionCgroupSleeper(t *testing.T) {
	if os.Getenv("HELMR_PLATFORM_ACQUISITION_CGROUP_SLEEPER") != "1" {
		t.Skip("helper process")
	}
	<-context.Background().Done()
}

func platformAcquisitionSleeper(t *testing.T, cgroupFD int) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(
		executable,
		"-test.run=^TestPlatformAcquisitionCgroupSleeper$",
	)
	command.Env = append(
		os.Environ(),
		"HELMR_PLATFORM_ACQUISITION_CGROUP_SLEEPER=1",
	)
	command.SysProcAttr = &syscall.SysProcAttr{
		UseCgroupFD: true,
		CgroupFD:    cgroupFD,
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	return command
}

func waitForPlatformAcquisitionCgroupPID(t *testing.T, path string, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err == nil && slices.Contains(strings.Fields(string(raw)), strconv.Itoa(pid)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("PID %d did not enter %s: %v", pid, path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForPlatformAcquisitionSleeper(t *testing.T, command *exec.Cmd) {
	t.Helper()
	wait := make(chan error, 1)
	go func() {
		wait <- command.Wait()
	}()
	select {
	case err := <-wait:
		if exit, ok := err.(*exec.ExitError); !ok || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != unix.SIGKILL {
			t.Fatalf("cgroup sleeper exit = %v, want SIGKILL", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cgroup sleeper survived parent cgroup kill")
	}
}
