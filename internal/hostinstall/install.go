package hostinstall

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ServiceName is the worker's systemd unit.
const ServiceName = "helmr-worker.service"

// InstalledMarker exists once every install step finished; Install removes it
// first, so an interrupted install is never taken for a complete one.
const InstalledMarker = "/usr/share/helmr/installed"

// Installer installs verified bundles under Root, running system commands
// through Run. The zero Root is the machine's own root.
type Installer struct {
	Root string
	// Run runs a system command and returns its standard output.
	Run func(name string, args ...string) (string, error)
}

// System installs on this machine.
func System() Installer {
	return Installer{Root: "/", Run: func(name string, args ...string) (string, error) {
		out, err := exec.Command(name, args...).Output()
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			err = fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(exit.Stderr)))
		}
		return string(out), err
	}}
}

func (i Installer) path(path string) string { return filepath.Join(i.Root, path) }

// Stopped reports whether the worker service runs nothing, which replacing
// its installed files requires.
func (i Installer) Stopped() (bool, error) {
	state, err := i.Run("systemctl", "show", ServiceName, "-p", "ActiveState", "--value")
	if err != nil {
		return false, err
	}
	// A failed unit runs nothing either, for example after an interrupted
	// install left it restarting a missing worker.
	switch strings.TrimSpace(state) {
	case "inactive", "failed":
		return true, nil
	}
	return false, nil
}

// Install puts verified files in place, prepares the accounts and devices the
// worker runs with, and writes the worker's systemd unit. It refuses while the
// worker service runs.
func (i Installer) Install(v Verified) error {
	stopped, err := i.Stopped()
	if err != nil {
		return fmt.Errorf("read the worker service state: %w", err)
	}
	if !stopped {
		return errors.New("stop and quiesce the worker before replacing its installed files")
	}
	if err := os.Remove(i.path(InstalledMarker)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, dir := range []string{"/usr/local/bin", "/usr/local/libexec/helmr", "/usr/share/helmr", "/var/lib/helmr/images/guest/out", "/run/helmr", "/etc/helmr", "/var/lib/helmr/jailer"} {
		if err := os.MkdirAll(i.path(dir), 0o755); err != nil {
			return err
		}
	}
	files := []struct {
		source, target string
		mode           os.FileMode
	}{
		{v.Host("cpu-template-helper"), "/usr/local/bin/cpu-template-helper", 0o755},
		{v.Host("firecracker"), "/usr/local/bin/firecracker", 0o755},
		{v.Host("jailer"), "/usr/local/bin/jailer", 0o755},
		{v.Host("worker"), "/usr/local/bin/worker", 0o755},
		{v.Host("helmr"), "/usr/local/bin/helmr", 0o755},
		{v.Host("mkfs.ext4"), "/usr/local/libexec/helmr/mkfs.ext4", 0o755},
		{v.Host("mke2fs.conf"), "/usr/share/helmr/mke2fs.conf", 0o444},
		{v.Host(hostManifestName), "/usr/share/helmr/" + hostManifestName, 0o644},
	}
	for _, name := range runtimeFiles {
		files = append(files, struct {
			source, target string
			mode           os.FileMode
		}{v.Runtime(name), "/var/lib/helmr/images/guest/out/" + name, 0o444})
	}
	for _, file := range files {
		if err := installFile(file.source, i.path(file.target), file.mode); err != nil {
			return fmt.Errorf("install %s: %w", file.target, err)
		}
	}
	if err := i.accounts(); err != nil {
		return err
	}
	for path, content := range map[string]string{
		"/etc/udev/rules.d/99-helmr-kvm.rules":    "KERNEL==\"kvm\", GROUP=\"helmr-vmm\", MODE=\"0660\"\n",
		"/etc/systemd/system/" + ServiceName:      unit,
		"/etc/sysctl.d/99-helmr-firecracker.conf": "net.ipv4.ip_forward = 1\nuser.max_user_namespaces = 16384\n",
	} {
		if err := writeFile(i.path(path), []byte(content), 0o644); err != nil {
			return err
		}
	}
	if _, err := os.Stat(i.path("/dev/kvm")); err == nil {
		if _, err := i.Run("chgrp", "helmr-vmm", "/dev/kvm"); err != nil {
			return err
		}
		if _, err := i.Run("chmod", "0660", "/dev/kvm"); err != nil {
			return err
		}
	}
	if _, err := i.Run("sysctl", "-p", "/etc/sysctl.d/99-helmr-firecracker.conf"); err != nil {
		return err
	}
	if _, err := i.Run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	return writeFile(i.path(InstalledMarker), nil, 0o644)
}

// accounts creates the VMM account with its fixed ids and the verifier
// account, or checks the existing ones.
func (i Installer) accounts() error {
	if _, err := i.Run("getent", "group", "helmr-vmm"); err != nil {
		if _, err := i.Run("groupadd", "--system", "--gid", "1001", "helmr-vmm"); err != nil {
			return err
		}
	}
	if _, err := i.Run("id", "helmr-vmm"); err != nil {
		if _, err := i.Run("useradd", "--system", "--uid", "1001", "--gid", "1001", "--groups", "kvm", "--no-create-home", "--shell", "/usr/sbin/nologin", "helmr-vmm"); err != nil {
			return err
		}
	}
	ids, err := i.Run("id", "-u", "helmr-vmm")
	if err != nil {
		return err
	}
	group, err := i.Run("id", "-g", "helmr-vmm")
	if err != nil {
		return err
	}
	if strings.TrimSpace(ids) != "1001" || strings.TrimSpace(group) != "1001" {
		return errors.New("the helmr-vmm account exists with ids other than 1001:1001")
	}
	if _, err := i.Run("getent", "group", "helmr-verifier"); err != nil {
		if _, err := i.Run("groupadd", "--system", "helmr-verifier"); err != nil {
			return err
		}
	}
	if _, err := i.Run("id", "helmr-verifier"); err != nil {
		if _, err := i.Run("useradd", "--system", "--gid", "helmr-verifier", "--no-create-home", "--home-dir", "/nonexistent", "--shell", "/usr/sbin/nologin", "helmr-verifier"); err != nil {
			return err
		}
	}
	return nil
}

const unit = `[Unit]
Description=Helmr worker
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/helmr/worker.env
ExecStart=/usr/local/bin/worker
Restart=on-failure
RestartSec=5
# The worker exits 78 when the Control Plane no longer admits its machine.
RestartPreventExitStatus=78
Delegate=yes
DelegateSubgroup=supervisor
KillMode=mixed
TasksMax=infinity

[Install]
WantedBy=multi-user.target
`

// installFile replaces target with a copy of source through a rename, so a
// running reader keeps the file it opened.
func installFile(source, target string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
