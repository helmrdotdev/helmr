package guestd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveUserSpecNamedNumericAndGroup(t *testing.T) {
	root := guestRootWithUsers(t)

	user, err := resolveUserSpec(root, "agent:staff")
	if err != nil {
		t.Fatal(err)
	}
	if user.Name != "agent" || user.UID != 1001 || user.GID != 2001 || user.Home != "/home/agent" {
		t.Fatalf("user = %+v", user)
	}

	user, err = resolveUserSpec(root, "1002")
	if err != nil {
		t.Fatal(err)
	}
	if user.Name != "runner" || user.UID != 1002 || user.GID != 1002 || user.Home != "/home/runner" {
		t.Fatalf("numeric user = %+v", user)
	}

	user, err = resolveUserSpec(root, "1234:2345")
	if err != nil {
		t.Fatal(err)
	}
	if user.UID != 1234 || user.GID != 2345 || user.Home != "/tmp" {
		t.Fatalf("numeric user:group = %+v", user)
	}
}

func TestResolveRuntimeUserAcceptsNumericUserWithoutGroupOrPasswdEntry(t *testing.T) {
	root := guestRootWithUsers(t)
	user, err := resolveRuntimeUser(root, "1234")
	if err != nil {
		t.Fatal(err)
	}
	if user.UID != 1234 || user.GID != 0 || user.Home != "/tmp" {
		t.Fatalf("user = %+v", user)
	}
}

func TestResolveRuntimeUserAcceptsRoot(t *testing.T) {
	root := guestRootWithUsers(t)
	for _, raw := range []string{"root", "0", "0:0", "root:root", "agent:0"} {
		t.Run(raw, func(t *testing.T) {
			user, err := resolveRuntimeUser(root, raw)
			if err != nil {
				t.Fatal(err)
			}
			if raw != "agent:0" && (user.UID != 0 || user.GID != 0) {
				t.Fatalf("user = %+v", user)
			}
			if raw == "agent:0" && (user.UID != 1001 || user.GID != 0) {
				t.Fatalf("user = %+v", user)
			}
		})
	}
}

func TestResolveRuntimeUserAcceptsRootWithoutPasswd(t *testing.T) {
	for _, raw := range []string{"root", "root:root", "0:root"} {
		t.Run(raw, func(t *testing.T) {
			user, err := resolveRuntimeUser(t.TempDir(), raw)
			if err != nil {
				t.Fatal(err)
			}
			if user.Name != "root" || user.UID != 0 || user.GID != 0 || user.Home != "/root" {
				t.Fatalf("user = %+v", user)
			}
		})
	}
}

func TestResolveRuntimeUserDefaultsToRoot(t *testing.T) {
	root := t.TempDir()
	user, err := resolveRuntimeUser(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if user.Name != "root" || user.UID != 0 || user.GID != 0 || user.Home != "/root" {
		t.Fatalf("user = %+v", user)
	}
}

func guestRootWithUsers(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	etc := filepath.Join(root, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etc, "passwd"), []byte(strings.Join([]string{
		"root:x:0:0:root:/root:/bin/sh",
		"agent:x:1001:1001:agent:/home/agent:/bin/sh",
		"runner:x:1002:1002:runner:/home/runner:/bin/sh",
		"",
	}, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etc, "group"), []byte(strings.Join([]string{
		"root:x:0:",
		"agent:x:1001:",
		"runner:x:1002:",
		"staff:x:2001:",
		"",
	}, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}
