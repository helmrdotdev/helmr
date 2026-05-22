package guestd

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type resolvedRuntimeUser struct {
	Name string
	UID  uint32
	GID  uint32
	Home string
}

type passwdEntry struct {
	Name string
	UID  uint32
	GID  uint32
	Home string
}

type groupEntry struct {
	Name string
	GID  uint32
}

func resolveRuntimeUser(imageRoot string, raw string) (*resolvedRuntimeUser, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return &resolvedRuntimeUser{Name: "root", UID: 0, GID: 0, Home: "/root"}, nil
	}
	identity, err := resolveUserSpec(imageRoot, raw)
	if err != nil {
		if isRootUserSpec(raw) {
			return &resolvedRuntimeUser{Name: "root", UID: 0, GID: 0, Home: "/root"}, nil
		}
		return nil, err
	}
	if identity.UID == 0 && identity.Home == "/tmp" {
		identity.Home = "/root"
		if identity.Name == "0" {
			identity.Name = "root"
		}
	}
	return identity, nil
}

func isRootUserSpec(raw string) bool {
	user, group, ok := strings.Cut(strings.TrimSpace(raw), ":")
	if strings.TrimSpace(user) != "root" && strings.TrimSpace(user) != "0" {
		return false
	}
	if !ok {
		return true
	}
	group = strings.TrimSpace(group)
	return group == "root" || group == "0"
}

func resolveUserSpec(imageRoot string, raw string) (*resolvedRuntimeUser, error) {
	user, group, err := splitUserSpec(raw)
	if err != nil {
		return nil, err
	}
	passwd, err := readPasswdEntries(imageRoot)
	if err != nil {
		return nil, err
	}
	var name string
	var uid uint32
	var gid uint32
	var home string
	if parsedUID, ok := parseID(user); ok {
		uid = parsedUID
		gid = 0
		name = user
		for _, entry := range passwd {
			if entry.UID == uid {
				name = entry.Name
				gid = entry.GID
				home = entry.Home
				break
			}
		}
	} else {
		entry, ok := findPasswd(passwd, user)
		if !ok {
			return nil, fmt.Errorf("user %q was not found in /etc/passwd", user)
		}
		name = entry.Name
		uid = entry.UID
		gid = entry.GID
		home = entry.Home
	}
	if group != "" {
		resolved, err := resolveGroup(imageRoot, group)
		if err != nil {
			return nil, err
		}
		gid = resolved
	}
	if home == "" {
		home = "/tmp"
	}
	return &resolvedRuntimeUser{Name: name, UID: uid, GID: gid, Home: home}, nil
}

func splitUserSpec(raw string) (string, string, error) {
	if raw == "" {
		return "", "", errors.New("OCI User is empty")
	}
	parts := strings.Split(raw, ":")
	if len(parts) > 2 {
		return "", "", fmt.Errorf("OCI User %q contains more than one ':'", raw)
	}
	if strings.TrimSpace(parts[0]) == "" {
		return "", "", fmt.Errorf("OCI User %q has an empty user field", raw)
	}
	if len(parts) == 2 && strings.TrimSpace(parts[1]) == "" {
		return "", "", fmt.Errorf("OCI User %q has an empty group field", raw)
	}
	group := ""
	if len(parts) == 2 {
		group = strings.TrimSpace(parts[1])
	}
	return strings.TrimSpace(parts[0]), group, nil
}

func resolveGroup(imageRoot string, raw string) (uint32, error) {
	if gid, ok := parseID(raw); ok {
		return gid, nil
	}
	groups, err := readGroupEntries(imageRoot)
	if err != nil {
		return 0, err
	}
	for _, entry := range groups {
		if entry.Name == raw {
			return entry.GID, nil
		}
	}
	return 0, fmt.Errorf("group %q was not found in /etc/group", raw)
}

func parseID(raw string) (uint32, bool) {
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(value), true
}

func readPasswdEntries(imageRoot string) ([]passwdEntry, error) {
	file, err := os.Open(filepath.Join(imageRoot, "etc", "passwd"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var entries []passwdEntry
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 7 {
			continue
		}
		uid, ok := parseID(parts[2])
		if !ok {
			continue
		}
		gid, ok := parseID(parts[3])
		if !ok {
			continue
		}
		entries = append(entries, passwdEntry{Name: parts[0], UID: uid, GID: gid, Home: parts[5]})
	}
	return entries, scanner.Err()
}

func readGroupEntries(imageRoot string) ([]groupEntry, error) {
	file, err := os.Open(filepath.Join(imageRoot, "etc", "group"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var entries []groupEntry
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 3 {
			continue
		}
		gid, ok := parseID(parts[2])
		if !ok {
			continue
		}
		entries = append(entries, groupEntry{Name: parts[0], GID: gid})
	}
	return entries, scanner.Err()
}

func findPasswd(entries []passwdEntry, name string) (passwdEntry, bool) {
	for _, entry := range entries {
		if entry.Name == name {
			return entry, true
		}
	}
	return passwdEntry{}, false
}
