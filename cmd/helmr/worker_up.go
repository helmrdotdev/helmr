package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/hostinstall"
	"github.com/helmrdotdev/helmr/internal/hostrelease"
	"github.com/helmrdotdev/helmr/internal/workerclient"
	"github.com/helmrdotdev/helmr/internal/workerhost"
	"github.com/spf13/cobra"
	"uuid"
)

type workerUpOptions struct {
	groupID, tokenFile, controlPlaneURL string
	pool, resolver, envFile             string
	devices                             int
}

func workerUpCommand() *cobra.Command {
	var opts workerUpOptions
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Enroll this machine in a worker group, install the worker and start it.",
		Long: "Enroll this machine in a worker group, install the Control Plane's release of the worker " +
			"and start it. With --group, helmr login (as root) mints a single-use enrollment token; " +
			"--token-file uses one you were given. On an enrolled machine, up starts the stopped worker.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireRoot(); err != nil {
				return err
			}
			if err := preflight(); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			identity, err := workerhost.ReadIdentity(hostPath(workerIdentityPath))
			enrolled := err == nil
			env, envErr := readEnvFile(hostPath(workerEnvPath))
			if enrolled && envErr != nil {
				return fmt.Errorf("this machine has a worker identity but no %s; run helmr worker remove, then helmr worker up", workerEnvPath)
			}
			if !enrolled {
				if env, identity, err = enrollMachine(cmd, opts); err != nil {
					return err
				}
				fmt.Fprintf(out, "enrolled as worker host %s\n", identity.WorkerHostID)
			}
			version := "the installed release"
			if !workerInstalled() {
				if version, err = installRelease(cmd, env, identity); err != nil {
					return err
				}
			}
			if _, err := workerHost.Run("systemctl", "enable", "--now", hostinstall.ServiceName); err != nil {
				return err
			}
			fmt.Fprintf(out, "started the worker on %s; helmr worker status shows when it is ready\n", version)
			return nil
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&opts.groupID, "group", "", "worker group ID to join, minting a single-use enrollment token with your helmr login")
	flags.StringVar(&opts.tokenFile, "token-file", "", "file holding an enrollment token (mode 0400 or 0600)")
	flags.StringVar(&opts.controlPlaneURL, "control-plane-url", "", "the Control Plane's URL; defaults to your helmr login's")
	flags.StringVar(&opts.pool, "pool", "default", "worker pool to join; machines of one shape share a pool")
	flags.StringVar(&opts.resolver, "resolver", "", "IPv4 DNS resolver for guests; defaults to the machine's first non-loopback nameserver")
	flags.IntVar(&opts.devices, "computer-devices", 16, "number of NBD devices the worker owns")
	flags.StringVar(&opts.envFile, "env-file", "", "KEY=VALUE file of worker settings applied over the defaults")
	return cmd
}

// enrollMachine writes the worker's environment and enrolls the machine with
// a single-use token, minted through the operator's login or read from a file.
func enrollMachine(cmd *cobra.Command, opts workerUpOptions) (map[string]string, workerhost.Identity, error) {
	if (opts.groupID == "") == (opts.tokenFile == "") {
		return nil, workerhost.Identity{}, errors.New("give --group (after helmr login) or --token-file")
	}
	tokenFile := opts.tokenFile
	if opts.groupID != "" {
		cp, err := sessionControlPlaneClient(cmd)
		if err != nil {
			return nil, workerhost.Identity{}, err
		}
		issued, err := cp.IssueWorkerEnrollmentToken(cmd.Context(), opts.groupID)
		if err != nil {
			return nil, workerhost.Identity{}, fmt.Errorf("issue an enrollment token: %w", err)
		}
		if opts.controlPlaneURL == "" {
			opts.controlPlaneURL = cp.BaseURL()
		}
		tokenFile = hostPath("/run/helmr/worker-enrollment-token")
		if err := writePrivate(tokenFile, []byte(issued.EnrollmentToken), 0o400); err != nil {
			return nil, workerhost.Identity{}, err
		}
		defer os.Remove(tokenFile)
	}
	if opts.controlPlaneURL == "" {
		opts.controlPlaneURL = cliControlPlaneURL(cmd)
	}
	if opts.controlPlaneURL == "" {
		return nil, workerhost.Identity{}, errors.New("--token-file needs the Control Plane's URL in --control-plane-url, --api-url or HELMR_API_URL")
	}
	env, err := machineEnv(opts)
	if err != nil {
		return nil, workerhost.Identity{}, err
	}
	if err := prepareComputerDevices(opts.devices); err != nil {
		return nil, workerhost.Identity{}, err
	}
	if err := writePrivate(hostPath(workerEnvPath), []byte(renderEnv(env)), 0o600); err != nil {
		return nil, workerhost.Identity{}, err
	}
	identity, err := workerhost.ResolveIdentity(cmd.Context(), config.Worker{
		ControlPlaneURL:           env["CONTROL_PLANE_URL"],
		WorkerResourceID:          env["WORKER_RESOURCE_ID"],
		WorkerPoolName:            env["WORKER_POOL_NAME"],
		WorkerEnrollmentTokenFile: tokenFile,
		WorkerIdentityPath:        hostPath(workerIdentityPath),
		JailerChrootDir:           hostPath(env["JAILER_CHROOT_DIR"]),
	}, hostPath(workerWorkDir))
	return env, identity, err
}

func workerInstalled() bool {
	for _, path := range []string{"/usr/local/bin/worker", "/etc/systemd/system/" + hostinstall.ServiceName} {
		if _, err := os.Stat(hostPath(path)); err != nil {
			return false
		}
	}
	return true
}

// installRelease fetches the Control Plane's release as this host, verifies it
// and installs it. The fetch runs under a service of its own, so the worker's
// first start opens the next epoch.
func installRelease(cmd *cobra.Command, env map[string]string, identity workerhost.Identity) (string, error) {
	source, err := workerclient.New(env["CONTROL_PLANE_URL"], workerclient.WithAuth(identity.WorkerHostID, identity.Key()), workerclient.WithService(uuid.NewV7().String()))
	if err != nil {
		return "", err
	}
	download, err := os.MkdirTemp(hostPath(workerWorkDir), ".release-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(download)
	fetched, err := hostrelease.Fetch(cmd.Context(), source, &http.Client{}, download)
	if errors.Is(err, hostrelease.ErrNoRelease) {
		return "", errors.New("this Control Plane is built from source and names no release; install the worker bundles you built with helmr worker install, then rerun helmr worker up")
	}
	if err != nil {
		return "", fmt.Errorf("fetch the Control Plane's release: %w", err)
	}
	if err := installBundles(hostinstall.FromRelease(fetched.Index, fetched.Dir)); err != nil {
		return "", err
	}
	return fetched.Index.Version, nil
}

// lookPath finds a command the worker needs; tests replace it.
var lookPath = exec.LookPath

func hostPath(path string) string { return filepath.Join(workerHost.Root, path) }

// requirement is one thing the machine needs, with what to do when missing.
type requirement struct {
	ok      func() bool
	missing string
}

func preflight() error {
	exists := func(path string) func() bool {
		return func() bool { _, err := os.Stat(hostPath(path)); return err == nil }
	}
	command := func(name string) func() bool {
		return func() bool { _, err := lookPath(name); return err == nil }
	}
	requirements := []requirement{
		{exists("/dev/kvm"), "/dev/kvm: enable hardware virtualization (or nested virtualization) and load the kvm module"},
		{exists("/sys/fs/cgroup/cgroup.controllers"), "cgroup v2: boot with the unified cgroup hierarchy"},
		{exists("/run/systemd/system"), "systemd: the worker runs as a systemd service"},
		{exists("/etc/machine-id"), "/etc/machine-id: run systemd-machine-id-setup"},
	}
	for _, name := range []string{"systemctl", "journalctl", "modprobe", "ip", "nft", "sysctl", "getent", "groupadd", "useradd"} {
		requirements = append(requirements, requirement{command(name), name + ": install it (ip comes with iproute2, nft with nftables)"})
	}
	var missing []string
	for _, r := range requirements {
		if !r.ok() {
			missing = append(missing, "  - "+r.missing)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("this machine cannot run a worker yet:\n%s", strings.Join(missing, "\n"))
	}
	return nil
}

// machineEnv is the worker's environment for this machine: the reference
// defaults, this machine's identity and resolver, then the operator's overrides.
func machineEnv(opts workerUpOptions) (map[string]string, error) {
	machineID, err := os.ReadFile(hostPath("/etc/machine-id"))
	if err != nil {
		return nil, err
	}
	resolver := opts.resolver
	if resolver == "" {
		if resolver, err = machineResolver(); err != nil {
			return nil, err
		}
	}
	if address, err := netip.ParseAddr(resolver); err != nil || !address.Is4() || address.IsLoopback() {
		return nil, fmt.Errorf("resolver %q must be a non-loopback IPv4 address", resolver)
	}
	if opts.devices < 1 || opts.devices > 1000 {
		return nil, errors.New("--computer-devices must be between 1 and 1000")
	}
	var devices []string
	for i := range opts.devices {
		devices = append(devices, fmt.Sprintf("/dev/nbd%d", i))
	}
	env := map[string]string{
		"CONTROL_PLANE_URL":                     opts.controlPlaneURL,
		"WORKER_RESOURCE_ID":                    "machine-" + strings.TrimSpace(string(machineID)),
		"WORKER_POOL_NAME":                      opts.pool,
		"WORKER_COMPUTER_DEVICES":               strings.Join(devices, " "),
		"WORKER_COMPUTER_STAGING_MIB":           "65536",
		"FIRECRACKER_PATH":                      "/usr/local/bin/firecracker",
		"CPU_TEMPLATE_HELPER_PATH":              "/usr/local/bin/cpu-template-helper",
		"JAILER_PATH":                           "/usr/local/bin/jailer",
		"MKFS_EXT4_PATH":                        "/usr/local/libexec/helmr/mkfs.ext4",
		"MKE2FS_CONFIG_PATH":                    "/usr/share/helmr/mke2fs.conf",
		"JAILER_UID":                            "1001",
		"JAILER_GID":                            "1001",
		"JAILER_CGROUP_VERSION":                 "2",
		"WORKER_NETWORK_LINK_POOL":              "169.254.64.0/18",
		"WORKER_NETWORK_TRANSLATION_POOL":       "100.96.0.0/16",
		"WORKER_NETWORK_RESOLVER_IPV4":          resolver,
		"WORKER_NETWORK_BLOCKED_IPV4_CIDRS":     "[]",
		"WORKER_NETWORK_PRIVATE_BUSINESS_CIDRS": "[]",
		"WORKER_WORK_DIR":                       workerWorkDir,
		"WORKER_IDENTITY_PATH":                  workerIdentityPath,
		"WORKER_IMAGES_DIR":                     "/var/lib/helmr/images",
		"JAILER_CHROOT_DIR":                     "/var/lib/helmr/jailer",
	}
	if opts.envFile != "" {
		overrides, err := readEnvFile(opts.envFile)
		if err != nil {
			return nil, err
		}
		for key, value := range overrides {
			env[key] = value
		}
	}
	return env, nil
}

// machineResolver is the first non-loopback IPv4 nameserver the machine uses,
// preferring systemd-resolved's upstream list over its local stub.
func machineResolver() (string, error) {
	for _, path := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		raw, err := os.ReadFile(hostPath(path))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 || fields[0] != "nameserver" {
				continue
			}
			if address, err := netip.ParseAddr(fields[1]); err == nil && address.Is4() && !address.IsLoopback() {
				return address.String(), nil
			}
		}
	}
	return "", errors.New("this machine has no non-loopback IPv4 nameserver; give one with --resolver")
}

// prepareComputerDevices loads the NBD module with the worker's device count
// and checks that every device exists and is not connected to anything else.
func prepareComputerDevices(count int) error {
	for path, content := range map[string]string{
		"/etc/modules-load.d/helmr-computer.conf": "nbd\n",
		"/etc/modprobe.d/helmr-computer.conf":     fmt.Sprintf("options nbd nbds_max=%d max_part=0\n", count),
	} {
		if err := writePrivate(hostPath(path), []byte(content), 0o644); err != nil {
			return err
		}
	}
	if _, err := workerHost.Run("modprobe", "nbd", fmt.Sprintf("nbds_max=%d", count), "max_part=0"); err != nil {
		return fmt.Errorf("load the nbd module: %w", err)
	}
	for i := range count {
		if _, err := os.Stat(hostPath(fmt.Sprintf("/dev/nbd%d", i))); err != nil {
			return fmt.Errorf("/dev/nbd%d is missing; if nbd was already loaded with fewer devices, unload it (modprobe -r nbd) and rerun", i)
		}
		if _, err := os.Stat(hostPath(fmt.Sprintf("/sys/block/nbd%d/pid", i))); err == nil {
			return fmt.Errorf("/dev/nbd%d is in use by another process; the worker needs its devices to itself", i)
		}
	}
	return nil
}

func renderEnv(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var b strings.Builder
	b.WriteString("# Written by helmr worker up.\n")
	for _, key := range keys {
		fmt.Fprintf(&b, "%s=%s\n", key, env[key])
	}
	return b.String()
}

// writePrivate replaces path with data through a rename.
func writePrivate(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
