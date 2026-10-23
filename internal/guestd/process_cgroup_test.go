package guestd

import (
	"strings"
	"testing"
)

func TestExecCgroupLeafUsesCommandIdentity(t *testing.T) {
	for _, test := range []struct{ id, want string }{
		{"command-1", "exec-a1f09a8d759d13102ef80be512730d03"},
		{"command-2", "exec-4fae32044660fc6bd41b70d215584f60"},
		{"command-1\x00suffix", "exec-985a72691c78b9d381d23bac7146f74f"},
	} {
		t.Run(test.id, func(t *testing.T) {
			for range 2 {
				leaf, err := execCgroupLeafName(test.id)
				if err != nil || leaf != test.want {
					t.Fatalf("exec leaf: got %q, want %q: %v", leaf, test.want, err)
				}
				if err := validateProcessCgroupLeaf(leaf); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestSessionCgroupLeafUsesSessionAndEpoch(t *testing.T) {
	for _, test := range []struct {
		id    string
		epoch int64
		want  string
	}{
		{"command-1", 1, "session-c659e2270079dd35e178a43759fe6821"},
		{"session-2", 1, "session-bc8a1a5f3b4f86b445f262f9a652d996"},
		{"command-1", 2, "session-e3add7da9dba7447540deb9ffb3dbc1d"},
	} {
		t.Run(test.want, func(t *testing.T) {
			for range 2 {
				leaf, err := sessionCgroupLeafName(test.id, test.epoch)
				if err != nil || leaf != test.want {
					t.Fatalf("session leaf: got %q, want %q: %v", leaf, test.want, err)
				}
				if err := validateProcessCgroupLeaf(leaf); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestCgroupLeafRejectsIncompleteIdentity(t *testing.T) {
	for _, id := range []string{"", " \t\n"} {
		if leaf, err := execCgroupLeafName(id); err == nil || leaf != "" {
			t.Fatalf("accepted blank command identity %q: %q %v", id, leaf, err)
		}
	}
	for _, test := range []struct {
		id    string
		epoch int64
	}{{"", 1}, {" \t\n", 1}, {"session\x00suffix", 1}, {"session", 0}, {"session", -1}} {
		if leaf, err := sessionCgroupLeafName(test.id, test.epoch); err == nil || leaf != "" {
			t.Fatalf("accepted incomplete Session identity %q/%d: %q %v", test.id, test.epoch, leaf, err)
		}
	}
}

func TestValidateProcessCgroupLeaf(t *testing.T) {
	for _, prefix := range []string{"run-", "exec-", "session-"} {
		if err := validateProcessCgroupLeaf(prefix + "0123456789abcdef0123456789ABCDEF"); err != nil {
			t.Fatalf("valid %s leaf: %v", prefix, err)
		}
	}
	for _, leaf := range []string{
		"run", "../run-deadbeef", "run-not-hex",
		"other-" + strings.Repeat("a", 32),
		"exec-" + strings.Repeat("a", 31),
		"exec-" + strings.Repeat("a", 33),
		"session-" + strings.Repeat("g", 32),
		"../exec-" + strings.Repeat("a", 32),
	} {
		if err := validateProcessCgroupLeaf(leaf); err == nil {
			t.Fatalf("untrusted cgroup leaf %q was accepted", leaf)
		}
	}
}
