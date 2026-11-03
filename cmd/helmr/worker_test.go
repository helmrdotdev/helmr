package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/hostinstall"
)

// fakeWorkerHost points the worker commands at a temporary machine root and
// records the system commands they run.
func fakeWorkerHost(t *testing.T) (string, *[]string) {
	t.Helper()
	root := t.TempDir()
	var calls []string
	previousHost, previousLookPath, previousPlatform := workerHost, lookPath, workerPlatform
	workerPlatform = "linux/amd64"
	workerHost = hostinstall.Installer{Root: root, Run: func(name string, args ...string) (string, error) {
		calls = append(calls, strings.Join(append([]string{name}, args...), " "))
		return "", nil
	}}
	lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	t.Cleanup(func() { workerHost, lookPath, workerPlatform = previousHost, previousLookPath, previousPlatform })
	for _, path := range []string{"/dev/kvm", "/sys/fs/cgroup/cgroup.controllers", "/run/systemd/system"} {
		writeHostFile(t, root, path, "")
	}
	writeHostFile(t, root, "/etc/machine-id", "0123456789abcdef0123456789abcdef\n")
	return root, &calls
}

func writeHostFile(t *testing.T, root, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(path)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runWorkerCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(append([]string{"worker"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func TestWorkerPreflightNamesWhatIsMissing(t *testing.T) {
	root, _ := fakeWorkerHost(t)
	if err := os.Remove(filepath.Join(root, "dev/kvm")); err != nil {
		t.Fatal(err)
	}
	lookPath = func(name string) (string, error) {
		if name == "nft" {
			return "", errors.New("missing")
		}
		return "/usr/bin/" + name, nil
	}
	workerPlatform = "linux/arm64"
	err := preflight()
	if err == nil || !strings.Contains(err.Error(), "Linux x86_64") || !strings.Contains(err.Error(), "/dev/kvm: enable hardware virtualization") || !strings.Contains(err.Error(), "nft: install it") {
		t.Fatalf("preflight = %v", err)
	}
}

func TestWorkerUpRequiresOneEnrollmentSource(t *testing.T) {
	fakeWorkerHost(t)
	for _, args := range [][]string{{"up"}, {"up", "--group", "g", "--token-file", "t"}} {
		if _, err := runWorkerCLI(t, args...); err == nil || !strings.Contains(err.Error(), "--group (after helmr login) or --token-file") {
			t.Fatalf("%v = %v", args, err)
		}
	}
}

func TestWorkerUpStartsAnEnrolledInstalledMachine(t *testing.T) {
	root, calls := fakeWorkerHost(t)
	writeHostFile(t, root, workerIdentityPath, `{"worker_host_id":"019a0000-0000-7000-8000-000000000001","machine_key":"`+strings.Repeat("A", 43)+`="}`)
	writeHostFile(t, root, workerEnvPath, "CONTROL_PLANE_URL=https://cp.test\n")
	writeHostFile(t, root, hostinstall.InstalledMarker, "")
	if _, err := runWorkerCLI(t, "up"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*calls, "\n"); got != "systemctl enable --now "+hostinstall.ServiceName {
		t.Fatalf("calls = %q", got)
	}
}

func TestWorkerMachineEnvUsesTheMachineAndOverrides(t *testing.T) {
	root, _ := fakeWorkerHost(t)
	writeHostFile(t, root, "/etc/resolv.conf", "nameserver 127.0.0.53\n")
	writeHostFile(t, root, "/run/systemd/resolve/resolv.conf", "# upstream\nnameserver ::1\nnameserver 10.0.0.2\n")
	overrides := filepath.Join(t.TempDir(), "overrides.env")
	if err := os.WriteFile(overrides, []byte("# sizing\nVM_MEMORY_MIB=8192\nWORKER_NETWORK_BLOCKED_IPV4_CIDRS=[\"10.0.0.0/8\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := machineEnv(workerUpOptions{controlPlaneURL: "https://cp.test", pool: "default", devices: 2, envFile: overrides})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"WORKER_RESOURCE_ID":                "machine-0123456789abcdef0123456789abcdef",
		"WORKER_NETWORK_RESOLVER_IPV4":      "10.0.0.2",
		"WORKER_COMPUTER_DEVICES":           "/dev/nbd0 /dev/nbd1",
		"VM_MEMORY_MIB":                     "8192",
		"WORKER_NETWORK_BLOCKED_IPV4_CIDRS": `["10.0.0.0/8"]`,
		"CONTROL_PLANE_URL":                 "https://cp.test",
	} {
		if env[key] != want {
			t.Fatalf("%s = %q, want %q", key, env[key], want)
		}
	}
	if _, hasToken := env["WORKER_ENROLLMENT_TOKEN_FILE"]; hasToken {
		t.Fatal("the machine environment names an enrollment token")
	}
	parsed, err := readEnvFile(writeTemp(t, renderEnv(env)))
	if err != nil || len(parsed) != len(env) || parsed["VM_MEMORY_MIB"] != "8192" {
		t.Fatalf("rendered environment reads back as %v, %v", parsed, err)
	}
}

func TestWorkerMachineEnvNeedsAReachableResolver(t *testing.T) {
	root, _ := fakeWorkerHost(t)
	writeHostFile(t, root, "/etc/resolv.conf", "nameserver 127.0.0.53\n")
	if _, err := machineEnv(workerUpOptions{pool: "default", devices: 1}); err == nil || !strings.Contains(err.Error(), "--resolver") {
		t.Fatalf("loopback-only resolver = %v", err)
	}
	if _, err := machineEnv(workerUpOptions{pool: "default", devices: 1, resolver: "127.0.0.1"}); err == nil {
		t.Fatal("a loopback --resolver was accepted")
	}
}

func TestWorkerCommandsNeedAnInstalledWorker(t *testing.T) {
	fakeWorkerHost(t)
	for _, command := range []string{"status", "drain", "fence"} {
		if _, err := runWorkerCLI(t, command); err == nil || !strings.Contains(err.Error(), "run helmr worker up") {
			t.Fatalf("%s = %v", command, err)
		}
	}
}

func TestWorkerDownKeepsTheIdentity(t *testing.T) {
	root, calls := fakeWorkerHost(t)
	writeHostFile(t, root, workerIdentityPath, "{}")
	writeHostFile(t, root, "/etc/systemd/system/"+hostinstall.ServiceName, "")
	if _, err := runWorkerCLI(t, "down"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*calls, "\n"); got != "systemctl disable --now "+hostinstall.ServiceName {
		t.Fatalf("calls = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, workerIdentityPath)); err != nil {
		t.Fatal("down removed the identity")
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWorkerMachineEnvKeepsManagedSettings(t *testing.T) {
	root, _ := fakeWorkerHost(t)
	writeHostFile(t, root, "/etc/resolv.conf", "nameserver 10.0.0.2\n")
	for _, key := range []string{"WORKER_WORK_DIR", "WORKER_IDENTITY_PATH", "CONTROL_PLANE_URL"} {
		overrides := writeTemp(t, key+"=/elsewhere\n")
		if _, err := machineEnv(workerUpOptions{pool: "default", devices: 1, envFile: overrides}); err == nil || !strings.Contains(err.Error(), "cannot set "+key) {
			t.Fatalf("override of %s = %v", key, err)
		}
	}
}

func TestWorkerRemoveWaitsForTheDrain(t *testing.T) {
	fakeWorkerHost(t)
	if _, err := runWorkerCLI(t, "remove", "--wait-timeout", "0"); err == nil || !strings.Contains(err.Error(), "must be positive") {
		t.Fatalf("remove without waiting = %v", err)
	}
}

func TestWorkerReadsQuotedEnvironmentFiles(t *testing.T) {
	env, err := readEnvFile(writeTemp(t, "A=\"http://cp.test\"\nB=\"a \\\"q\\\" \\\\ z\"\nC='lit\\eral'\nD=plain\n"))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"A": "http://cp.test", "B": `a "q" \ z`, "C": `lit\eral`, "D": "plain"} {
		if env[key] != want {
			t.Fatalf("%s = %q, want %q", key, env[key], want)
		}
	}
}

func TestWorkerRemoveOnlyUninstallsTheUpLayout(t *testing.T) {
	fakeWorkerHost(t)
	if _, err := runWorkerCLI(t, "remove", "--env", "/etc/other.env"); err == nil || !strings.Contains(err.Error(), "only a worker that helmr worker up installed") {
		t.Fatalf("remove with another layout = %v", err)
	}
}

// A machine enrolled before its install finished has no unit; remove still
// deletes its identity and names the host to revoke.
func TestWorkerRemoveWithoutAnInstalledUnit(t *testing.T) {
	root, calls := fakeWorkerHost(t)
	workerHost.Run = func(name string, args ...string) (string, error) {
		call := strings.Join(append([]string{name}, args...), " ")
		*calls = append(*calls, call)
		if strings.HasPrefix(call, "systemctl disable") || strings.HasPrefix(call, "systemctl stop") {
			return "", errors.New("unit not loaded")
		}
		return "", nil
	}
	writeHostFile(t, root, workerIdentityPath, `{"worker_host_id":"019a0000-0000-7000-8000-000000000001","machine_key":"`+strings.Repeat("A", 43)+`="}`)
	if err := os.Chmod(filepath.Join(root, workerIdentityPath), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runWorkerCLI(t, "remove")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "revoke worker host 019a0000-0000-7000-8000-000000000001") {
		t.Fatalf("output = %q", out)
	}
	if _, err := os.Stat(filepath.Join(root, workerIdentityPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("remove kept the identity")
	}
}
