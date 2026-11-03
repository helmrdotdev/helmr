package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/hostinstall"
	"github.com/helmrdotdev/helmr/internal/release"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerhost"
	"github.com/spf13/cobra"
)

// The worker's fixed layout on a machine it runs on.
const (
	workerEnvPath      = "/etc/helmr/worker.env"
	workerWorkDir      = "/var/lib/helmr"
	workerIdentityPath = "/var/lib/helmr/" + workerhost.IdentityFileName
)

// workerHost is the machine the worker commands act on; tests replace it.
var workerHost = hostinstall.System()

// workerEnvFile is the environment file the commands read the installed
// worker's settings from.
var workerEnvFile = workerEnvPath

func workerCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "worker",
		Short: "Run this Linux machine as a worker for a worker group.",
		Long: "Run this Linux machine as a worker for a worker group. The commands act on the worker " +
			"installed on this machine and need root.",
	}
	root.PersistentFlags().StringVar(&workerEnvFile, "env", workerEnvPath, "the installed worker's environment file, for status, drain, fence and remove")
	root.AddCommand(workerUpCommand(), workerStatusCommand(), workerDrainCommand(), workerFenceCommand(),
		workerDownCommand(), workerRemoveCommand(), workerLogsCommand(), workerInstallCommand())
	return root
}

// workerControlPlane reads how the installed worker reaches its Control
// Plane from the worker's environment file.
func workerControlPlane() (config.WorkerControlPlane, error) {
	env, err := readEnvFile(hostPath(workerEnvFile))
	if errors.Is(err, os.ErrNotExist) {
		return config.WorkerControlPlane{}, errors.New("no worker is installed on this machine; run helmr worker up")
	}
	if err != nil {
		return config.WorkerControlPlane{}, err
	}
	cfg := config.WorkerControlPlane{
		ControlPlaneURL:    env["CONTROL_PLANE_URL"],
		WorkerIdentityPath: env["WORKER_IDENTITY_PATH"],
		WorkDir:            env["WORKER_WORK_DIR"],
		PollEvery:          2 * time.Second,
	}
	if cfg.ControlPlaneURL == "" {
		return cfg, fmt.Errorf("%s names no CONTROL_PLANE_URL", workerEnvFile)
	}
	return cfg, nil
}

func workerStatusCommand() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show this machine's worker status; exits non-zero unless it is active and ready.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := workerControlPlane()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			status, err := workerhost.Status(ctx, cfg)
			if err != nil {
				return err
			}
			if jsonOutput {
				if err := writeJSON(cmd.OutOrStdout(), status); err != nil {
					return err
				}
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "worker host: %s\ngroup: %s\nstatus: %s\nactive instances: %d\nready: %t\n",
					status.WorkerHostID, status.WorkerGroupID, status.Status, status.ActiveInstances, status.Readiness.Ready)
			}
			if status.Status != workerapi.StatusActive {
				return fmt.Errorf("worker status is %s", status.Status)
			}
			if !status.Readiness.Ready {
				reason := status.Readiness.PausedReason
				if reason == "" {
					reason = "unavailable"
				}
				return fmt.Errorf("worker admission is not ready: %s", reason)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print the status as JSON")
	return cmd
}

func workerDrainCommand() *cobra.Command {
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "drain",
		Short: "Stop new work on this machine and wait for running work to finish.",
		Long: "Stop new work on this machine and wait for running work to finish. Once drained, the " +
			"Control Plane retires the machine's key; bring the machine back with helmr worker remove and up.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := workerControlPlane()
			if err != nil {
				return err
			}
			status, err := workerhost.Drain(cmd.Context(), cfg, wait)
			if err != nil {
				return err
			}
			if wait > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "worker host %s drained\n", status.WorkerHostID)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "worker host %s draining with %d active instances\n", status.WorkerHostID, status.ActiveInstances)
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&wait, "wait-timeout", 30*time.Minute, "how long to wait for running work to finish; 0 returns at once")
	return cmd
}

func workerFenceCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "fence",
		Short: "Record that this machine's work is lost, so it resumes elsewhere in the group now.",
		Long: "Record that this machine's work is lost, so the Control Plane resumes it elsewhere in the group " +
			"now instead of waiting for the machine to return. Run it when the machine is being destroyed: " +
			"from a provider termination hook after a drain did not finish, or by hand before wiping the machine.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := workerControlPlane()
			if err != nil {
				return err
			}
			if err := workerhost.Fence(cmd.Context(), cfg); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "worker fenced")
			return nil
		},
	}
}

func workerDownCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Stop the worker and keep this machine enrolled.",
		Long: "Stop the worker and keep this machine enrolled. The group sees the machine as lost until " +
			"helmr worker up starts it again with the same key.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireRoot(); err != nil {
				return err
			}
			if err := stopWorkerService(); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "worker stopped; helmr worker up starts it again")
			return nil
		},
	}
}

func workerRemoveCommand() *cobra.Command {
	var groupID string
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Drain the worker, uninstall it and delete this machine's worker identity.",
		Long: "Drain the worker, uninstall it and delete this machine's worker identity. A drained " +
			"machine's key is retired by the Control Plane. When the worker cannot drain, --group with " +
			"helmr login revokes the machine's key; otherwise revoke it from the group's workers page.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireRoot(); err != nil {
				return err
			}
			if wait <= 0 {
				return errors.New("--wait-timeout must be positive: remove waits for the drain to finish")
			}
			if workerEnvFile != workerEnvPath {
				return fmt.Errorf("remove uninstalls only a worker that helmr worker up installed, with %s", workerEnvPath)
			}
			out := cmd.OutOrStdout()
			identity, identityErr := workerhost.ReadIdentity(hostPath(workerIdentityPath))
			drained := false
			if cfg, err := workerControlPlane(); err == nil && identityErr == nil {
				if _, err := workerhost.Drain(cmd.Context(), cfg, wait); err == nil {
					drained = true
				} else {
					fmt.Fprintf(cmd.ErrOrStderr(), "drain did not finish: %v\n", err)
				}
			}
			if err := stopWorkerService(); err != nil {
				return err
			}
			if identityErr == nil && !drained {
				if groupID == "" {
					fmt.Fprintf(out, "revoke worker host %s from its group's workers page, or rerun with --group\n", identity.WorkerHostID)
				} else {
					cp, err := sessionControlPlaneClient(cmd)
					if err != nil {
						return err
					}
					if err := cp.RevokeWorkerHost(cmd.Context(), groupID, identity.WorkerHostID); err != nil {
						return fmt.Errorf("revoke worker host %s: %w", identity.WorkerHostID, err)
					}
					fmt.Fprintf(out, "worker host %s revoked\n", identity.WorkerHostID)
				}
			}
			for _, path := range []string{
				"/etc/systemd/system/" + hostinstall.ServiceName, workerEnvPath, workerWorkDir,
				"/usr/local/bin/worker", "/usr/local/bin/firecracker", "/usr/local/bin/jailer",
				"/usr/local/bin/cpu-template-helper", "/usr/local/libexec/helmr", "/usr/share/helmr",
				"/etc/modules-load.d/helmr-computer.conf", "/etc/modprobe.d/helmr-computer.conf",
				"/etc/udev/rules.d/99-helmr-kvm.rules", "/etc/sysctl.d/99-helmr-firecracker.conf",
			} {
				if err := os.RemoveAll(hostPath(path)); err != nil {
					return err
				}
			}
			if _, err := workerHost.Run("systemctl", "daemon-reload"); err != nil {
				return err
			}
			fmt.Fprintln(out, "worker removed")
			return nil
		},
	}
	cmd.Flags().StringVar(&groupID, "group", "", "worker group ID, to revoke the machine's key when it cannot drain")
	cmd.Flags().DurationVar(&wait, "wait-timeout", 30*time.Minute, "how long to wait for running work to finish")
	return cmd
}

func workerLogsCommand() *cobra.Command {
	var follow bool
	var lines int
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show the worker's logs from the system journal.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			args := []string{"--unit", hostinstall.ServiceName, "--lines", fmt.Sprint(lines), "--no-pager"}
			if follow {
				args = append(args, "--follow")
			}
			journal := exec.CommandContext(cmd.Context(), "journalctl", args...)
			journal.Stdout, journal.Stderr = cmd.OutOrStdout(), cmd.ErrOrStderr()
			return journal.Run()
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new log lines")
	cmd.Flags().IntVarP(&lines, "lines", "n", 200, "number of recent lines to show")
	return cmd
}

func workerInstallCommand() *cobra.Command {
	var bundles hostinstall.Bundles
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install a release's worker bundles from local files, pinned by digest.",
		Long: "Install a release's worker bundles from local files, pinned by digest, without enrolling " +
			"or starting the worker. Machine image builds use it; helmr worker up installs the same way " +
			"from the Control Plane's release.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireRoot(); err != nil {
				return err
			}
			if bundles.HostPath == "" || bundles.RuntimePath == "" {
				return errors.New("--host-bundle and --runtime-bundle are required")
			}
			if err := installBundles(bundles, os.TempDir()); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "installed worker host and runtime artifacts")
			return nil
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&bundles.HostPath, "host-bundle", "", "path of "+release.HostArtifacts)
	flags.StringVar(&bundles.Host.Digest, "host-bundle-digest", "", "sha256 digest of the host bundle")
	flags.StringVar(&bundles.HostManifest.Digest, "host-manifest-digest", "", "sha256 digest of "+release.HostArtifactsManifest)
	flags.StringVar(&bundles.RuntimePath, "runtime-bundle", "", "path of "+release.RuntimeArtifacts)
	flags.StringVar(&bundles.Runtime.Digest, "runtime-bundle-digest", "", "sha256 digest of the runtime bundle")
	flags.StringVar(&bundles.RuntimeManifest.Digest, "runtime-manifest-digest", "", "sha256 digest of "+release.RuntimeArtifactsManifest)
	return cmd
}

// stopWorkerService stops and disables the worker unit when one is installed;
// systemd refuses both for a unit file that does not exist.
func stopWorkerService() error {
	if _, err := os.Stat(hostPath("/etc/systemd/system/" + hostinstall.ServiceName)); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	_, err := workerHost.Run("systemctl", "disable", "--now", hostinstall.ServiceName)
	return err
}

// installBundles verifies bundles into a private directory under staging and
// installs them.
func installBundles(bundles hostinstall.Bundles, staging string) error {
	verified, err := hostinstall.Verify(bundles, staging)
	if err != nil {
		return err
	}
	defer verified.Close()
	return workerHost.Install(verified)
}

func requireRoot() error {
	if os.Geteuid() != 0 && workerHost.Root == "/" {
		return errors.New("helmr worker needs root; run it with sudo")
	}
	return nil
}

// readEnvFile reads a KEY=VALUE environment file, skipping blank lines and
// comments.
func readEnvFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("%s:%d is not KEY=VALUE", path, line)
		}
		env[key] = unquoteEnv(value)
	}
	return env, scanner.Err()
}

// unquoteEnv reads a value the way systemd's EnvironmentFile does: a value in
// double quotes takes backslash escapes, one in single quotes is literal.
func unquoteEnv(value string) string {
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1]
	}
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return value
	}
	var b strings.Builder
	inner := value[1 : len(value)-1]
	for i := 0; i < len(inner); i++ {
		if inner[i] == '\\' && i+1 < len(inner) {
			i++
		}
		b.WriteByte(inner[i])
	}
	return b.String()
}
